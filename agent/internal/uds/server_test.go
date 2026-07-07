package uds

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeLiba — клиентская сторона контракта, зеркало mockAgent из
// examples/stub-server/main_test.go: те же фреймы, но тест смотрит
// с агентской стороны сокета.
type fakeLiba struct {
	t    *testing.T
	conn net.Conn
	sc   *bufio.Scanner
}

func dialLiba(t *testing.T, path string) *fakeLiba {
	t.Helper()
	var conn net.Conn
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.Dial("unix", path)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() })
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	return &fakeLiba{t: t, conn: conn, sc: sc}
}

func (f *fakeLiba) send(typ string, data map[string]any) {
	f.t.Helper()
	frame := map[string]any{"v": 1, "type": typ}
	if data != nil {
		frame["data"] = data
	}
	b, _ := json.Marshal(frame)
	if _, err := f.conn.Write(append(b, '\n')); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeLiba) sendRaw(line string) {
	f.t.Helper()
	if _, err := f.conn.Write([]byte(line + "\n")); err != nil {
		f.t.Fatal(err)
	}
}

// expect читает фреймы агента, пропуская ping (keepalive-шум), пока не
// встретит typ.
func (f *fakeLiba) expect(typ string) map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.conn.SetReadDeadline(deadline)
		if !f.sc.Scan() {
			f.t.Fatalf("liba: expected %q, socket closed: %v", typ, f.sc.Err())
		}
		var fr struct {
			V    int            `json:"v"`
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(f.sc.Bytes(), &fr); err != nil {
			f.t.Fatalf("bad frame %q: %v", f.sc.Text(), err)
		}
		if fr.V != 1 {
			f.t.Fatalf("frame without v=1: %s", f.sc.Text())
		}
		if fr.Type == "ping" && typ != "ping" {
			continue
		}
		if fr.Type != typ {
			f.t.Fatalf("liba: expected %q, got %q (%s)", typ, fr.Type, f.sc.Text())
		}
		return fr.Data
	}
	f.t.Fatalf("liba: timeout waiting for %q", typ)
	return nil
}

type rec struct {
	kind string
	str1 string
	str2 string
	num  int
	val  float64
}

func recorder() (Events, chan rec) {
	ch := make(chan rec, 64)
	return Events{
		OnHello:      func(v string) { ch <- rec{kind: "hello", str1: v} },
		OnReady:      func() { ch <- rec{kind: "ready"} },
		OnPlayers:    func(n int) { ch <- rec{kind: "players", num: n} },
		OnMatchStart: func(id string) { ch <- rec{kind: "match_start", str1: id} },
		OnMatchEnd:   func(id, res string) { ch <- rec{kind: "match_end", str1: id, str2: res} },
		OnMetric:     func(n string, v float64) { ch <- rec{kind: "metric", str1: n, val: v} },
		OnLog:        func(l, m string) { ch <- rec{kind: "log", str1: l, str2: m} },
		OnPong:       func() { ch <- rec{kind: "pong"} },
	}, ch
}

func expectRec(t *testing.T, ch chan rec, kind string) rec {
	t.Helper()
	select {
	case r := <-ch:
		if r.kind != kind {
			t.Fatalf("event %q, want %q (%+v)", r.kind, kind, r)
		}
		return r
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for event %q", kind)
		return rec{}
	}
}

func listen(t *testing.T, ev Events) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.sock")
	// logf=nil: серверные горутины могут логировать после конца теста
	s, err := Listen(path, ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// Полный обмен: hello/ready/players/match_start/metric/log/match_end/pong
// от liba; allocated/drain/ping от агента.
func TestLifecycleFrames(t *testing.T) {
	ev, ch := recorder()
	s, path := listen(t, ev)
	liba := dialLiba(t, path)

	liba.send("hello", map[string]any{"sdk_version": "stub/0.1.0"})
	if r := expectRec(t, ch, "hello"); r.str1 != "stub/0.1.0" {
		t.Fatalf("%+v", r)
	}
	liba.send("ready", nil)
	expectRec(t, ch, "ready")

	if err := s.SendAllocated("m-1", 2); err != nil {
		t.Fatal(err)
	}
	d := liba.expect("allocated")
	if d["match_id"] != "m-1" || d["players_expected"].(float64) != 2 {
		t.Fatalf("allocated data: %v", d)
	}

	liba.send("players", map[string]any{"count": 3})
	if r := expectRec(t, ch, "players"); r.num != 3 {
		t.Fatalf("%+v", r)
	}
	liba.send("match_start", map[string]any{"match_id": "m-1"})
	if r := expectRec(t, ch, "match_start"); r.str1 != "m-1" {
		t.Fatalf("%+v", r)
	}
	liba.send("metric", map[string]any{"name": "tick_ms", "value": 16.6})
	if r := expectRec(t, ch, "metric"); r.str1 != "tick_ms" || r.val != 16.6 {
		t.Fatalf("%+v", r)
	}
	liba.send("log", map[string]any{"level": "info", "msg": "spawned"})
	if r := expectRec(t, ch, "log"); r.str1 != "info" || r.str2 != "spawned" {
		t.Fatalf("%+v", r)
	}
	liba.send("match_end", map[string]any{"match_id": "m-1", "result": "completed"})
	if r := expectRec(t, ch, "match_end"); r.str1 != "m-1" || r.str2 != "completed" {
		t.Fatalf("%+v", r)
	}

	if err := s.SendDrain(20, "deploy"); err != nil {
		t.Fatal(err)
	}
	d = liba.expect("drain")
	if d["deadline_s"].(float64) != 20 || d["reason"] != "deploy" {
		t.Fatalf("drain data: %v", d)
	}

	if err := s.SendPing(); err != nil {
		t.Fatal(err)
	}
	liba.expect("ping")
	liba.send("pong", nil)
	expectRec(t, ch, "pong")

	if s.LastHeard().IsZero() {
		t.Fatal("LastHeard must be set after frames")
	}
	if !s.Connected() {
		t.Fatal("Connected must be true")
	}
}

// allocated до коннекта liba: Send возвращает ErrNotConnected, но фрейм
// доставляется при подключении.
func TestPendingAllocatedDeliveredOnConnect(t *testing.T) {
	ev, _ := recorder()
	s, path := listen(t, ev)
	if err := s.SendAllocated("m-7", 4); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
	liba := dialLiba(t, path)
	d := liba.expect("allocated")
	if d["match_id"] != "m-7" || d["players_expected"].(float64) != 4 {
		t.Fatalf("allocated data: %v", d)
	}
}

// Реконнект liba: агент реплеит последние allocated и drain (protocol.md §2).
func TestReconnectReplaysAllocatedAndDrain(t *testing.T) {
	ev, ch := recorder()
	s, path := listen(t, ev)

	liba1 := dialLiba(t, path)
	liba1.send("hello", map[string]any{"sdk_version": "x"})
	expectRec(t, ch, "hello")
	if err := s.SendAllocated("m-9", 2); err != nil {
		t.Fatal(err)
	}
	liba1.expect("allocated")
	if err := s.SendDrain(15, "deploy"); err != nil {
		t.Fatal(err)
	}
	liba1.expect("drain")
	liba1.conn.Close()

	liba2 := dialLiba(t, path)
	d := liba2.expect("allocated")
	if d["match_id"] != "m-9" {
		t.Fatalf("replayed allocated: %v", d)
	}
	d = liba2.expect("drain")
	if d["reason"] != "deploy" || d["deadline_s"].(float64) != 15 {
		t.Fatalf("replayed drain: %v", d)
	}
}

// Мусор, чужой мажор envelope и неизвестные типы игнорируются (forward-compat).
func TestIgnoresGarbageAndUnknown(t *testing.T) {
	ev, ch := recorder()
	_, path := listen(t, ev)
	liba := dialLiba(t, path)
	liba.sendRaw("this is not json")
	liba.sendRaw(`{"v":2,"type":"ready","data":{}}`)
	liba.sendRaw(`{"v":1,"type":"from_the_future","data":{"x":1}}`)
	liba.send("hello", map[string]any{"sdk_version": "z"})
	// если бы ready с v=2 диспатчился, в канале он лежал бы раньше hello
	if r := expectRec(t, ch, "hello"); r.str1 != "z" {
		t.Fatalf("%+v", r)
	}
}

// Сокет должен быть доступен non-root процессу в контейнере
// (identity = сокет, см. Listen).
func TestSocketPermissions(t *testing.T) {
	ev, _ := recorder()
	_, path := listen(t, ev)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o666 {
		t.Fatalf("socket mode %o, want 0666", perm)
	}
}
