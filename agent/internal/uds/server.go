// Package uds implements the agent side of the liba↔agent contract
// (docs/specs/protocol.md §2): NDJSON envelopes {"v":1,"type":...,"data":{...}}
// over a per-server unix socket. The agent listens, the game (liba) dials;
// identity is the socket itself — no tokens.
package uds

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// Version is the envelope major version this agent speaks.
const Version = 1

const (
	maxLine      = 256 * 1024
	writeTimeout = 3 * time.Second
)

// ErrNotConnected is returned by Send* when liba has no open connection.
// allocated/drain frames are still remembered and delivered on connect.
var ErrNotConnected = errors.New("liba is not connected")

// Events are callbacks for liba → agent messages; nil fields are skipped.
// Callbacks run on the connection reader goroutine — keep them fast.
type Events struct {
	OnHello      func(sdkVersion string)
	OnReady      func()
	OnPlayers    func(count int)
	OnMatchStart func(matchID string)
	OnMatchEnd   func(matchID, result string)
	OnMetric     func(name string, value float64)
	OnLog        func(level, msg string)
	OnPong       func()
	OnConnect    func()
	OnDisconnect func()
}

type inFrame struct {
	V    int             `json:"v"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type outFrame struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Server owns one per-server unix socket. Reconnects are allowed: a new
// connection replaces the previous one and the last allocated/drain frames
// are replayed (protocol.md §2).
type Server struct {
	path string
	ev   Events
	logf func(format string, args ...any)

	wmu sync.Mutex // serializes frame writes to the current connection

	mu            sync.Mutex
	ln            net.Listener
	conn          net.Conn
	lastAllocated *outFrame
	lastDrain     *outFrame
	lastHeard     time.Time
	closed        bool
}

// Listen creates the socket (removing a stale file first) and starts
// accepting connections. The socket file is chmod 0666: game images may run
// as non-root (e.g. distroless nonroot), and only this server's container
// ever sees the socket via its bind mount.
func Listen(path string, ev Events, logf func(string, ...any)) (*Server, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	// Длина пути unix-сокета ограничена sockaddr_un.sun_path: 104 байта на
	// macOS, 108 на Linux. Берём МЕНЬШИЙ порог НАМЕРЕННО, а не по недосмотру:
	// путь, не влезающий на маке, обязан отвергаться одинаково на обеих
	// системах — иначе конфигурация, работающая в Linux-CI, падала бы у
	// разработчика на маке, то есть ровно тем способом, от которого этот
	// предупреждающий текст и защищает. Ядро сообщает о превышении невнятным
	// «invalid argument», и человек ищет ошибку в правах, занятом сокете и чём
	// угодно, кроме длины.
	if len(path) >= 104 {
		return nil, fmt.Errorf("socket path %s is %d bytes: длиннее лимита sun_path (~104), выберите каталог короче", path, len(path))
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod socket %s: %w", path, err)
	}
	s := &Server{path: path, ev: ev, logf: logf, ln: ln}
	go s.acceptLoop()
	return s, nil
}

// Path returns the host path of the socket.
func (s *Server) Path() string { return s.path }

// Connected reports whether a liba connection is currently open.
func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// LastHeard returns the time of the last valid frame from liba (zero if never).
func (s *Server) LastHeard() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastHeard
}

// Close stops accepting, closes the current connection and unlinks the socket.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	var err error
	if ln != nil {
		err = ln.Close() // unlinks the socket file
	}
	if conn != nil {
		conn.Close()
	}
	return err
}

// SendAllocated tells liba the master allocated a match to this server
// (v0 run-once: the --allocate flag plays the master). The frame is
// remembered and replayed to a (re)connecting liba. metadata (may be nil) is
// the external matchmaker's per-match payload, forwarded verbatim — the
// agent neither reads nor invents it, and the replay carries it because the
// frame itself is what is remembered.
func (s *Server) SendAllocated(matchID string, playersExpected int, metadata map[string]string) error {
	data := map[string]any{
		"match_id":         matchID,
		"players_expected": playersExpected,
	}
	if len(metadata) > 0 {
		data["metadata"] = metadata
	}
	f := &outFrame{V: Version, Type: "allocated", Data: data}
	s.mu.Lock()
	s.lastAllocated = f
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	return s.writeTo(conn, f)
}

// SendDrain asks liba to finish the current match and exit within deadlineS
// seconds. Remembered and replayed like allocated.
func (s *Server) SendDrain(deadlineS int, reason string) error {
	f := &outFrame{V: Version, Type: "drain", Data: map[string]any{
		"deadline_s": deadlineS,
		"reason":     reason,
	}}
	s.mu.Lock()
	s.lastDrain = f
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	return s.writeTo(conn, f)
}

// SendPing sends a keepalive; liba answers pong.
func (s *Server) SendPing() error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	return s.writeTo(conn, &outFrame{V: Version, Type: "ping", Data: map[string]any{}})
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				s.logf("[uds] accept: %v", err)
			}
			return
		}
		s.mu.Lock()
		old := s.conn
		s.conn = conn
		s.lastHeard = time.Now()
		var replay []*outFrame
		if s.lastAllocated != nil {
			replay = append(replay, s.lastAllocated)
		}
		if s.lastDrain != nil {
			replay = append(replay, s.lastDrain)
		}
		s.mu.Unlock()
		if old != nil {
			old.Close()
		}
		if s.ev.OnConnect != nil {
			s.ev.OnConnect()
		}
		for _, f := range replay {
			if err := s.writeTo(conn, f); err != nil {
				s.logf("[uds] replay %s: %v", f.Type, err)
			} else {
				s.logf("[uds] delivered pending %s to liba", f.Type)
			}
		}
		go s.readLoop(conn)
	}
}

func (s *Server) readLoop(conn net.Conn) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var f inFrame
		if err := json.Unmarshal(line, &f); err != nil {
			s.logf("[uds] bad frame: %v", err)
			continue
		}
		if f.V != Version {
			s.logf("[uds] envelope v=%d ignored (agent speaks v%d)", f.V, Version)
			continue
		}
		s.mu.Lock()
		s.lastHeard = time.Now()
		s.mu.Unlock()
		s.dispatch(f)
	}
	s.mu.Lock()
	wasCurrent := s.conn == conn
	if wasCurrent {
		s.conn = nil
	}
	closed := s.closed
	s.mu.Unlock()
	conn.Close()
	if wasCurrent && !closed && s.ev.OnDisconnect != nil {
		s.ev.OnDisconnect()
	}
}

func (s *Server) dispatch(f inFrame) {
	unmarshal := func(v any) bool {
		if len(f.Data) == 0 {
			return true // data absent → zero values
		}
		if err := json.Unmarshal(f.Data, v); err != nil {
			s.logf("[uds] bad %s data: %v", f.Type, err)
			return false
		}
		return true
	}
	switch f.Type {
	case "hello":
		var d struct {
			SDKVersion string `json:"sdk_version"`
		}
		if unmarshal(&d) && s.ev.OnHello != nil {
			s.ev.OnHello(d.SDKVersion)
		}
	case "ready":
		if s.ev.OnReady != nil {
			s.ev.OnReady()
		}
	case "players":
		var d struct {
			Count int `json:"count"`
		}
		if unmarshal(&d) && s.ev.OnPlayers != nil {
			s.ev.OnPlayers(d.Count)
		}
	case "match_start":
		var d struct {
			MatchID string `json:"match_id"`
		}
		if unmarshal(&d) && s.ev.OnMatchStart != nil {
			s.ev.OnMatchStart(d.MatchID)
		}
	case "match_end":
		var d struct {
			MatchID string `json:"match_id"`
			Result  string `json:"result"`
		}
		if unmarshal(&d) && s.ev.OnMatchEnd != nil {
			s.ev.OnMatchEnd(d.MatchID, d.Result)
		}
	case "metric":
		var d struct {
			Name  string  `json:"name"`
			Value float64 `json:"value"`
		}
		if unmarshal(&d) && s.ev.OnMetric != nil {
			s.ev.OnMetric(d.Name, d.Value)
		}
	case "log":
		var d struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if unmarshal(&d) && s.ev.OnLog != nil {
			s.ev.OnLog(d.Level, d.Msg)
		}
	case "pong":
		if s.ev.OnPong != nil {
			s.ev.OnPong()
		}
	default:
		// forward-compat: неизвестные типы игнорируем (protocol.md §2)
	}
}

func (s *Server) writeTo(conn net.Conn, f *outFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.wmu.Lock()
	defer s.wmu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := conn.Write(b); err != nil {
		conn.Close()
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.mu.Unlock()
		return fmt.Errorf("write %s: %w", f.Type, err)
	}
	return nil
}
