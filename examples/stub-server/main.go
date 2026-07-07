// stub-server — минимальный «игровой сервер» birdman и референс интеграции:
// UDP-чат + серверная сторона SDK-контракта (liba): NDJSON поверх unix socket,
// см. docs/specs/protocol.md §2. Без BIRDMAN_SOCKET работает standalone —
// аналог no-op режима SDK: та же игра, никакого агента.
//
// UDP-протокол (текст, один датаграм = одна команда):
//   PING          → PONG server=<id> players=<n> state=<ready|allocated|match|draining>
//   JOIN <name>   → WELCOME <name> players=<n>  (+ broadcast JOINED <name>)
//   SAY <text>    → broadcast MSG <name>: <text>
//   LEAVE         → BYE <name>                  (+ broadcast LEFT <name>)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	playerTimeout = 60 * time.Second
	sweepEvery    = 5 * time.Second
	tickEvery     = 5 * time.Second
)

func main() { os.Exit(run()) }

func run() int {
	cfg := configFromEnv()
	g := newGame(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return g.run(ctx)
}

type config struct {
	Port     int
	Socket   string
	ServerID string
}

func configFromEnv() config {
	port := 7777
	if v := os.Getenv("BIRDMAN_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &port)
	}
	id := os.Getenv("BIRDMAN_SERVER_ID")
	if id == "" {
		id, _ = os.Hostname()
	}
	return config{Port: port, Socket: os.Getenv("BIRDMAN_SOCKET"), ServerID: id}
}

type player struct {
	name string
	udp  *net.UDPAddr
	seen time.Time
}

type game struct {
	cfg  config
	conn *net.UDPConn
	liba *libaClient // nil => standalone

	mu        sync.Mutex
	players   map[string]*player // key: удалённый addr:port
	allocated bool
	matchID   string
	matchLive bool
	draining  bool

	exit chan int
}

func newGame(cfg config) *game {
	return &game{cfg: cfg, players: map[string]*player{}, exit: make(chan int, 1)}
}

func (g *game) run(ctx context.Context) int {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: g.cfg.Port})
	if err != nil {
		log.Printf("[stub] fatal: listen udp :%d: %v", g.cfg.Port, err)
		return 1
	}
	g.mu.Lock()
	g.conn = conn
	g.mu.Unlock()
	defer conn.Close()
	log.Printf("[stub] server_id=%s listening udp %s", g.cfg.ServerID, conn.LocalAddr())

	if g.cfg.Socket != "" {
		g.liba = newLibaClient(g.cfg.Socket, g)
		go g.liba.run(ctx)
	} else {
		log.Printf("[stub] BIRDMAN_SOCKET is not set — standalone (unmanaged) mode")
	}

	go g.readLoop()
	go g.housekeeping(ctx)

	select {
	case code := <-g.exit:
		return code
	case <-ctx.Done():
		log.Printf("[stub] signal received, shutting down")
		return 0
	}
}

func (g *game) localAddr() *net.UDPAddr {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn == nil {
		return nil
	}
	return g.conn.LocalAddr().(*net.UDPAddr)
}

func (g *game) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, raddr, err := g.conn.ReadFromUDP(buf)
		if err != nil {
			return // сокет закрыт — выходим
		}
		g.handlePacket(strings.TrimSpace(string(buf[:n])), raddr)
	}
}

func (g *game) reply(addr *net.UDPAddr, s string) { g.conn.WriteToUDP([]byte(s+"\n"), addr) }

// broadcast — вызывающий держит g.mu
func (g *game) broadcast(s string) {
	for _, p := range g.players {
		g.conn.WriteToUDP([]byte(s+"\n"), p.udp)
	}
}

func (g *game) handlePacket(msg string, raddr *net.UDPAddr) {
	fields := strings.Fields(msg)
	if len(fields) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := raddr.String()
	if p, ok := g.players[key]; ok {
		p.seen = time.Now()
	}
	switch strings.ToUpper(fields[0]) {
	case "PING":
		g.reply(raddr, fmt.Sprintf("PONG server=%s players=%d state=%s",
			g.cfg.ServerID, len(g.players), g.stateLocked()))
	case "JOIN":
		if g.draining {
			g.reply(raddr, "ERR draining, no new players")
			return
		}
		name := "anon"
		if len(fields) > 1 {
			name = fields[1]
		}
		g.players[key] = &player{name: name, udp: raddr, seen: time.Now()}
		g.reply(raddr, fmt.Sprintf("WELCOME %s players=%d", name, len(g.players)))
		g.broadcast("JOINED " + name)
		g.onPlayersChangedLocked()
	case "SAY":
		p, ok := g.players[key]
		if !ok {
			g.reply(raddr, "ERR join first")
			return
		}
		g.broadcast(fmt.Sprintf("MSG %s: %s", p.name, strings.Join(fields[1:], " ")))
	case "LEAVE":
		if p, ok := g.players[key]; ok {
			delete(g.players, key)
			g.reply(raddr, "BYE "+p.name)
			g.broadcast("LEFT " + p.name)
			g.onPlayersChangedLocked()
		}
	default:
		g.reply(raddr, "ERR unknown command (PING|JOIN <name>|SAY <text>|LEAVE)")
	}
}

func (g *game) stateLocked() string {
	switch {
	case g.draining:
		return "draining"
	case g.matchLive:
		return "match"
	case g.allocated:
		return "allocated"
	default:
		return "ready"
	}
}

// onPlayersChangedLocked — вызывающий держит g.mu
func (g *game) onPlayersChangedLocked() {
	n := len(g.players)
	if g.liba != nil {
		g.liba.send("players", map[string]any{"count": n})
	}
	if n > 0 && g.allocated && !g.matchLive {
		g.matchLive = true
		log.Printf("[stub] match %s started", g.matchID)
		if g.liba != nil {
			g.liba.send("match_start", map[string]any{"match_id": g.matchID})
		}
	}
	if n == 0 && g.matchLive {
		g.finishLocked("completed")
	}
	if n == 0 && g.draining && !g.matchLive {
		g.exitAsync(0)
	}
}

// finishLocked — вызывающий держит g.mu. В managed-режиме дедик одноразовый:
// после match_end процесс завершается, слот пересоздаёт reconcile master'а.
func (g *game) finishLocked(result string) {
	g.matchLive = false
	log.Printf("[stub] match %s ended: %s", g.matchID, result)
	if g.liba != nil {
		g.liba.send("match_end", map[string]any{"match_id": g.matchID, "result": result})
		g.exitAsync(0)
	} else {
		g.allocated = false
		g.matchID = ""
	}
}

func (g *game) exitAsync(code int) {
	select {
	case g.exit <- code:
	default:
	}
}

func (g *game) housekeeping(ctx context.Context) {
	sweep := time.NewTicker(sweepEvery)
	tick := time.NewTicker(tickEvery)
	defer sweep.Stop()
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			g.mu.Lock()
			changed := false
			for k, p := range g.players {
				if time.Since(p.seen) > playerTimeout {
					delete(g.players, k)
					g.broadcast(fmt.Sprintf("LEFT %s (timeout)", p.name))
					changed = true
				}
			}
			if changed {
				g.onPlayersChangedLocked()
			}
			g.mu.Unlock()
		case <-tick.C:
			if g.liba != nil {
				g.liba.send("metric", map[string]any{"name": "tick_ms", "value": 16.6 + rand.Float64()})
			}
		}
	}
}

// --- события от агента ---

func (g *game) onAllocated(matchID string, playersExpected int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allocated = true
	g.matchID = matchID
	log.Printf("[stub] allocated: match_id=%s players_expected=%d", matchID, playersExpected)
}

func (g *game) onDrain(deadlineS float64, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.draining = true
	log.Printf("[stub] drain requested: deadline=%.0fs reason=%q", deadlineS, reason)
	if !g.matchLive {
		g.exitAsync(0)
		return
	}
	if deadlineS > 0 {
		time.AfterFunc(time.Duration(deadlineS*float64(time.Second)), func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.matchLive {
				g.finishLocked("aborted")
			} else {
				g.exitAsync(0)
			}
		})
	}
}

// --- liba: клиент agent-сокета (NDJSON, envelope {"v":1,"type":...,"data":...}) ---

type wireIn struct {
	V    int             `json:"v"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type libaClient struct {
	path string
	g    *game

	mu   sync.Mutex
	conn net.Conn
}

func newLibaClient(path string, g *game) *libaClient { return &libaClient{path: path, g: g} }

func (c *libaClient) run(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := net.Dial("unix", c.path)
		if err != nil {
			log.Printf("[liba] connect %s: %v (retry in 2s)", c.path, err)
			select {
			case <-time.After(2 * time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
		log.Printf("[liba] connected to agent socket %s", c.path)
		c.send("hello", map[string]any{"sdk_version": "stub/0.1.0"})
		c.send("ready", nil)
		c.g.mu.Lock()
		n := len(c.g.players)
		c.g.mu.Unlock()
		if n > 0 {
			c.send("players", map[string]any{"count": n})
		}
		c.readLoop(conn) // блокируется до разрыва
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}
}

func (c *libaClient) readLoop(conn net.Conn) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		var m wireIn
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			log.Printf("[liba] bad frame: %v", err)
			continue
		}
		switch m.Type {
		case "allocated":
			var d struct {
				MatchID         string `json:"match_id"`
				PlayersExpected int    `json:"players_expected"`
			}
			json.Unmarshal(m.Data, &d)
			c.g.onAllocated(d.MatchID, d.PlayersExpected)
		case "drain":
			var d struct {
				DeadlineS float64 `json:"deadline_s"`
				Reason    string  `json:"reason"`
			}
			json.Unmarshal(m.Data, &d)
			c.g.onDrain(d.DeadlineS, d.Reason)
		case "ping":
			c.send("pong", nil)
		default:
			// forward-compat: неизвестные типы игнорируем (protocol.md §2)
		}
	}
	log.Printf("[liba] agent socket closed")
}

func (c *libaClient) send(typ string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return
	}
	frame := map[string]any{"v": 1, "type": typ}
	if data != nil {
		frame["data"] = data
	}
	b, _ := json.Marshal(frame)
	c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		log.Printf("[liba] write %s: %v", typ, err)
		c.conn.Close()
		c.conn = nil
	}
}
