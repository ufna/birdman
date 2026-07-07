package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type mockAgent struct {
	t    *testing.T
	ln   net.Listener
	conn net.Conn
	sc   *bufio.Scanner
}

func newMockAgent(t *testing.T) (*mockAgent, string) {
	dir, err := os.MkdirTemp("", "bird")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return &mockAgent{t: t, ln: ln}, path
}

func (m *mockAgent) accept() {
	conn, err := m.ln.Accept()
	if err != nil {
		m.t.Fatal(err)
	}
	m.conn = conn
	m.sc = bufio.NewScanner(conn)
}

// expect читает фреймы, пропуская шумовые (metric/log/pong), пока не встретит typ.
func (m *mockAgent) expect(typ string) map[string]any {
	m.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.conn.SetReadDeadline(deadline)
		if !m.sc.Scan() {
			m.t.Fatalf("agent: expected %q, socket closed: %v", typ, m.sc.Err())
		}
		var f struct {
			V    int            `json:"v"`
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(m.sc.Bytes(), &f); err != nil {
			m.t.Fatalf("bad frame %q: %v", m.sc.Text(), err)
		}
		if f.V != 1 {
			m.t.Fatalf("frame without v=1: %s", m.sc.Text())
		}
		if f.Type == "metric" || f.Type == "log" || f.Type == "pong" {
			continue
		}
		if f.Type != typ {
			m.t.Fatalf("agent: expected %q, got %q (%s)", typ, f.Type, m.sc.Text())
		}
		return f.Data
	}
	m.t.Fatalf("agent: timeout waiting for %q", typ)
	return nil
}

func (m *mockAgent) send(typ string, data map[string]any) {
	frame := map[string]any{"v": 1, "type": typ, "data": data}
	b, _ := json.Marshal(frame)
	if _, err := m.conn.Write(append(b, '\n')); err != nil {
		m.t.Fatal(err)
	}
}

func waitUDP(t *testing.T, g *game) *net.UDPAddr {
	t.Helper()
	for i := 0; i < 200; i++ {
		if a := g.localAddr(); a != nil {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("udp not bound")
	return nil
}

func waitState(t *testing.T, cl func(string) string, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(cl("PING"), "state="+want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("state never became %q", want)
}

// udpClient шлёт команду и возвращает первый прямой ответ (broadcast-строки пропускает).
func udpClient(t *testing.T, to *net.UDPAddr) func(string) string {
	t.Helper()
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: to.Port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	buf := make([]byte, 2048)
	return func(msg string) string {
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			n, err := conn.Read(buf)
			if err != nil {
				t.Fatalf("%s: read: %v", msg, err)
			}
			s := strings.TrimSpace(string(buf[:n]))
			if strings.HasPrefix(s, "JOINED ") || strings.HasPrefix(s, "LEFT ") || strings.HasPrefix(s, "MSG ") {
				continue
			}
			return s
		}
	}
}

// Полный managed-цикл: hello → ready → allocated → JOIN → match_start →
// players → LEAVE → match_end(completed) → процесс завершается с кодом 0.
func TestManagedLifecycle(t *testing.T) {
	mock, sock := newMockAgent(t)

	g := newGame(config{Port: 0, Socket: sock, ServerID: "test-1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- g.run(ctx) }()

	mock.accept()
	if d := mock.expect("hello"); d["sdk_version"] == "" {
		t.Fatal("hello without sdk_version")
	}
	mock.expect("ready")

	laddr := waitUDP(t, g)
	mock.send("allocated", map[string]any{"match_id": "m-42", "players_expected": 2})

	cl := udpClient(t, laddr)
	waitState(t, cl, "allocated") // allocated обрабатывается асинхронно
	if got := cl("JOIN alice"); !strings.HasPrefix(got, "WELCOME alice") {
		t.Fatalf("JOIN → %q", got)
	}
	if d := mock.expect("players"); d["count"].(float64) != 1 {
		t.Fatalf("players != 1: %v", d)
	}
	if d := mock.expect("match_start"); d["match_id"] != "m-42" {
		t.Fatalf("match_start: %v", d)
	}
	if got := cl("PING"); !strings.Contains(got, "players=1") || !strings.Contains(got, "state=match") {
		t.Fatalf("PING → %q", got)
	}
	if got := cl("LEAVE"); !strings.HasPrefix(got, "BYE") {
		t.Fatalf("LEAVE → %q", got)
	}
	if d := mock.expect("players"); d["count"].(float64) != 0 {
		t.Fatalf("players != 0: %v", d)
	}
	if d := mock.expect("match_end"); d["result"] != "completed" {
		t.Fatalf("match_end: %v", d)
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after match_end")
	}
}

// Drain без живого матча — немедленный выход.
func TestDrainIdleExits(t *testing.T) {
	mock, sock := newMockAgent(t)
	g := newGame(config{Port: 0, Socket: sock, ServerID: "test-2"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- g.run(ctx) }()

	mock.accept()
	mock.expect("hello")
	mock.expect("ready")
	mock.send("drain", map[string]any{"deadline_s": 5, "reason": "deploy"})

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle server did not exit on drain")
	}
}

// Standalone: без BIRDMAN_SOCKET игра работает и не завершается после матча.
func TestStandalone(t *testing.T) {
	g := newGame(config{Port: 0, ServerID: "solo"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.run(ctx)

	cl := udpClient(t, waitUDP(t, g))
	if got := cl("PING"); !strings.Contains(got, "state=ready") {
		t.Fatalf("PING → %q", got)
	}
	if got := cl("JOIN bob"); !strings.HasPrefix(got, "WELCOME bob") {
		t.Fatalf("JOIN → %q", got)
	}
	if got := cl("LEAVE"); !strings.HasPrefix(got, "BYE") {
		t.Fatalf("LEAVE → %q", got)
	}
	if got := cl("PING"); !strings.Contains(got, "players=0") {
		t.Fatalf("PING after LEAVE → %q", got)
	}
}
