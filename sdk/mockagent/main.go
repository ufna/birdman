// mockagent — крошечный локальный «агент» для отладки интеграции игр с
// birdman SDK (docs/specs/sdk.md §4): слушает unix socket liba-контракта
// (docs/specs/protocol.md §2), печатает все входящие фреймы и шлёт
// allocated/drain/ping по командам из stdin.
//
// Поведение повторяет настоящий агент (agent/internal/uds/server.go):
// per-server сокет, новое подключение заменяет старое, последний
// allocated/drain реплеится реконнектящейся liba, keepalive ping раз в 10с.
//
//	go run . -socket /tmp/agent.sock
//	BIRDMAN_SOCKET=/tmp/agent.sock BIRDMAN_SERVER_ID=dev-1 BIRDMAN_PORT=7777 ./your-server
//
// Команды stdin:
//	allocate <match_id> [players_expected]   выдать матч (реплеится при реконнекте)
//	drain <deadline_s> [reason...]           доиграть и завершиться
//	ping                                     keepalive вручную
//	quit                                     выйти
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type frame struct {
	V    int             `json:"v"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type agent struct {
	mu            sync.Mutex
	conn          net.Conn
	lastAllocated *frame
	lastDrain     *frame
}

func main() {
	socket := flag.String("socket", "./agent.sock", "unix socket path (game side: BIRDMAN_SOCKET)")
	pingEvery := flag.Duration("ping", 10*time.Second, "keepalive ping interval (0 = off)")
	flag.Parse()

	if err := os.Remove(*socket); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "mockagent: remove stale socket: %v\n", err)
		os.Exit(1)
	}
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mockagent: %v\n", err)
		os.Exit(1)
	}
	_ = os.Chmod(*socket, 0o666) // как настоящий агент: игра может бежать не под root
	defer ln.Close()
	fmt.Printf("mockagent: listening on %s (commands: allocate <id> [n] | drain <s> [reason] | ping | quit)\n", *socket)

	a := &agent{}
	go a.acceptLoop(ln)
	if *pingEvery > 0 {
		go func() {
			for range time.Tick(*pingEvery) {
				a.send("ping", map[string]any{})
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { a.stdinLoop(); close(done) }()
	select {
	case <-sig:
	case <-done:
	}
	fmt.Println("mockagent: bye")
}

func (a *agent) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		a.mu.Lock()
		if a.conn != nil {
			a.conn.Close() // новое подключение заменяет старое
		}
		a.conn = conn
		replay := []*frame{}
		if a.lastAllocated != nil {
			replay = append(replay, a.lastAllocated)
		}
		if a.lastDrain != nil {
			replay = append(replay, a.lastDrain)
		}
		a.mu.Unlock()
		fmt.Println("mockagent: liba connected")
		for _, f := range replay {
			a.writeFrame(conn, f)
			fmt.Printf("-> %s (replay) %s\n", f.Type, string(f.Data))
		}
		go a.readLoop(conn)
	}
}

func (a *agent) readLoop(conn net.Conn) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 256*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			fmt.Printf("<- bad frame: %s\n", line)
			continue
		}
		if f.V != 1 {
			fmt.Printf("<- ignored (v=%d): %s\n", f.V, line)
			continue
		}
		fmt.Printf("<- %s %s\n", f.Type, string(f.Data))
	}
	a.mu.Lock()
	if a.conn == conn {
		a.conn = nil
		fmt.Println("mockagent: liba disconnected")
	}
	a.mu.Unlock()
	conn.Close()
}

func (a *agent) stdinLoop() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "allocate":
			if len(fields) < 2 {
				fmt.Println("usage: allocate <match_id> [players_expected]")
				continue
			}
			n := 0
			if len(fields) > 2 {
				n, _ = strconv.Atoi(fields[2])
			}
			f := a.remember("allocated", map[string]any{"match_id": fields[1], "players_expected": n})
			a.sendFrame(f)
		case "drain":
			if len(fields) < 2 {
				fmt.Println("usage: drain <deadline_s> [reason...]")
				continue
			}
			sec, _ := strconv.Atoi(fields[1])
			reason := "manual"
			if len(fields) > 2 {
				reason = strings.Join(fields[2:], " ")
			}
			f := a.remember("drain", map[string]any{"deadline_s": sec, "reason": reason})
			a.sendFrame(f)
		case "ping":
			a.send("ping", map[string]any{})
		case "quit", "exit":
			return
		default:
			fmt.Println("commands: allocate <match_id> [players_expected] | drain <deadline_s> [reason...] | ping | quit")
		}
	}
}

// remember строит фрейм и запоминает его для реплея (как настоящий агент).
func (a *agent) remember(typ string, data map[string]any) *frame {
	raw, _ := json.Marshal(data)
	f := &frame{V: 1, Type: typ, Data: raw}
	a.mu.Lock()
	switch typ {
	case "allocated":
		a.lastAllocated = f
	case "drain":
		a.lastDrain = f
	}
	a.mu.Unlock()
	return f
}

func (a *agent) send(typ string, data map[string]any) {
	raw, _ := json.Marshal(data)
	a.sendFrame(&frame{V: 1, Type: typ, Data: raw})
}

func (a *agent) sendFrame(f *frame) {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		fmt.Printf("-> %s dropped: liba is not connected (delivered on reconnect if allocate/drain)\n", f.Type)
		return
	}
	if err := a.writeFrame(conn, f); err != nil {
		fmt.Printf("-> %s failed: %v\n", f.Type, err)
		return
	}
	fmt.Printf("-> %s %s\n", f.Type, string(f.Data))
}

func (a *agent) writeFrame(conn net.Conn, f *frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Write(append(b, '\n'))
	if err != nil {
		conn.Close()
		a.mu.Lock()
		if a.conn == conn {
			a.conn = nil
		}
		a.mu.Unlock()
	}
	return err
}
