package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ufna/birdman/agent/internal/config"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// --- fakes ---

type fakeHandle struct {
	mu      sync.Mutex
	exit    chan Exit
	signals []syscall.Signal
	killed  bool
	deleted bool
	states  []string // recorded SetState values "state/matchID"
}

func newFakeHandle() *fakeHandle { return &fakeHandle{exit: make(chan Exit, 1)} }

func (h *fakeHandle) Wait() <-chan Exit { return h.exit }
func (h *fakeHandle) Signal(_ context.Context, sig syscall.Signal) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.signals = append(h.signals, sig)
	return nil
}
func (h *fakeHandle) Kill(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.killed = true
	return nil
}
func (h *fakeHandle) Delete(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deleted = true
	return nil
}
func (h *fakeHandle) SetState(_ context.Context, state, matchID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.states = append(h.states, state+"/"+matchID)
	return nil
}
func (h *fakeHandle) isDeleted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deleted
}
func (h *fakeHandle) gotSignal(sig syscall.Signal) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.signals {
		if s == sig {
			return true
		}
	}
	return false
}
func (h *fakeHandle) lastState() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.states) == 0 {
		return ""
	}
	return h.states[len(h.states)-1]
}

type fakeRuntime struct {
	mu       sync.Mutex
	started  []StartSpec
	handles  map[string]*fakeHandle
	restored []RestoredServer
	startErr error
	pulls    []string
	pullErr  error
}

func newFakeRuntime() *fakeRuntime { return &fakeRuntime{handles: map[string]*fakeHandle{}} }

func (r *fakeRuntime) Pull(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pulls = append(r.pulls, ref)
	return r.pullErr
}

func (r *fakeRuntime) Start(_ context.Context, spec StartSpec) (Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return nil, r.startErr
	}
	h := newFakeHandle()
	r.started = append(r.started, spec)
	r.handles[spec.ID] = h
	return h, nil
}

func (r *fakeRuntime) Restore(context.Context) ([]RestoredServer, error) {
	return r.restored, nil
}

func (r *fakeRuntime) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started)
}

func (r *fakeRuntime) handle(id string) *fakeHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handles[id]
}

type sinkEvent struct{ serverID, kind, detail string }

type fakeSink struct {
	mu     sync.Mutex
	events []sinkEvent
	pulls  []string // "status ref"
}

func (s *fakeSink) ServerEvent(serverID, kind, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{serverID, kind, detail})
}
func (s *fakeSink) PullReport(_, imageRef, status, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls = append(s.pulls, status+" "+imageRef)
}
func (s *fakeSink) has(serverID, kind string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.serverID == serverID && e.kind == kind {
			return true
		}
	}
	return false
}

// fakeLiba dials the per-server agent socket like a game server would.
type fakeLiba struct {
	t    *testing.T
	conn net.Conn
}

func dialLiba(t *testing.T, path string) *fakeLiba {
	t.Helper()
	var conn net.Conn
	var err error
	deadline := time.Now().Add(5 * time.Second)
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
	return &fakeLiba{t: t, conn: conn}
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

// --- helpers ---

func testManager(t *testing.T, rt Runtime) (*Manager, *fakeSink, string) {
	t.Helper()
	// Short base dir: unix socket paths must stay under ~104 bytes.
	sockDir, err := os.MkdirTemp("", "bdsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	cfg := &config.Config{
		Region:        "dev",
		CapacitySlots: 4,
		PortRange:     []int{20000, 20010},
		LimitsDefault: config.Limits{CPUMillis: 500, MemMB: 128},
		LogDir:        t.TempDir(),
		DataDir:       t.TempDir(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sink := &fakeSink{}
	m, err := NewManager(ctx, Options{
		Config: cfg, Runtime: rt, Sink: sink, SocketDir: sockDir, Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, sink, sockDir
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met in time: %s", what)
}

func snapshotOf(m *Manager, id string) *agentlinkv1.ServerState {
	for _, s := range m.Snapshot() {
		if s.GetServerId() == id {
			return s
		}
	}
	return nil
}

func stateIs(m *Manager, id, state string) func() bool {
	return func() bool {
		s := snapshotOf(m, id)
		return s != nil && s.GetState() == state
	}
}

// --- tests ---

func TestStartServerToReady(t *testing.T) {
	rt := newFakeRuntime()
	m, sink, _ := testManager(t, rt)

	m.Start(context.Background(), &agentlinkv1.StartServer{
		ServerId: "s1", ImageRef: "ghcr.io/x/img:1", CmdId: "c1",
		Env: map[string]string{"FOO": "bar"},
	})
	eventually(t, "container started", func() bool { return rt.startCount() == 1 })
	rt.mu.Lock()
	spec := rt.started[0]
	rt.mu.Unlock()
	if spec.Port < 20000 || spec.Port > 20010 {
		t.Fatalf("port %d out of pool range", spec.Port)
	}
	if spec.Region != "dev" || spec.CPUMillis != 500 || spec.MemMB != 128 || spec.Env["FOO"] != "bar" {
		t.Fatalf("start spec: %+v", spec)
	}
	if filepath.Base(spec.SocketPath) != "agent.sock" {
		t.Fatalf("socket path %q must end in agent.sock", spec.SocketPath)
	}
	eventually(t, "starting state", stateIs(m, "s1", "starting"))

	liba := dialLiba(t, spec.SocketPath)
	liba.send("hello", map[string]any{"sdk_version": "test/1"})
	liba.send("ready", nil)
	eventually(t, "ready state", stateIs(m, "s1", "ready"))
	if !sink.has("s1", "ready") {
		t.Fatal("ready ServerEvent not emitted")
	}
	eventually(t, "state label persisted", func() bool {
		return rt.handle("s1").lastState() == "ready/"
	})

	liba.send("players", map[string]any{"count": 3})
	liba.send("metric", map[string]any{"name": "tick_ms", "value": 16.6})
	eventually(t, "players and tick in snapshot", func() bool {
		s := snapshotOf(m, "s1")
		return s != nil && s.GetPlayers() == 3 && s.GetTickMs() > 16
	})
	if s := snapshotOf(m, "s1"); s.GetVersion() != "ghcr.io/x/img:1" || s.GetPort() != int32(spec.Port) {
		t.Fatalf("snapshot: %+v", s)
	}
}

// A replayed StartServer for a known server id must not start a second
// container (idempotency across cmd_id cache loss, e.g. agent restart).
func TestStartIdempotentByServerID(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)
	cmd := &agentlinkv1.StartServer{ServerId: "s1", ImageRef: "img:1", CmdId: "c1"}
	m.Start(context.Background(), cmd)
	eventually(t, "first start", func() bool { return rt.startCount() == 1 })
	m.Start(context.Background(), cmd)
	time.Sleep(50 * time.Millisecond)
	if rt.startCount() != 1 {
		t.Fatalf("duplicate StartServer started %d containers", rt.startCount())
	}
}

func TestStopServerGraceful(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)
	m.Start(context.Background(), &agentlinkv1.StartServer{ServerId: "s1", ImageRef: "img:1", CmdId: "c1"})
	eventually(t, "started", func() bool { return rt.startCount() == 1 })
	rt.mu.Lock()
	sockPath := rt.started[0].SocketPath
	rt.mu.Unlock()
	liba := dialLiba(t, sockPath)
	liba.send("ready", nil)
	eventually(t, "ready", stateIs(m, "s1", "ready"))

	m.Stop(context.Background(), &agentlinkv1.StopServer{ServerId: "s1", GraceS: 30, CmdId: "c2"})
	h := rt.handle("s1")
	eventually(t, "SIGTERM sent", func() bool { return h.gotSignal(syscall.SIGTERM) })
	eventually(t, "draining state", stateIs(m, "s1", "draining"))
	h.exit <- Exit{Code: 0}
	eventually(t, "stopped state", stateIs(m, "s1", "stopped"))
	eventually(t, "container deleted", h.isDeleted)
	eventually(t, "socket dir removed", func() bool {
		_, err := os.Stat(filepath.Dir(sockPath))
		return errors.Is(err, os.ErrNotExist)
	})

	// Stop of an unknown server is an idempotent no-op.
	m.Stop(context.Background(), &agentlinkv1.StopServer{ServerId: "nope", CmdId: "c3"})
}

func TestCrashReportsFailed(t *testing.T) {
	rt := newFakeRuntime()
	m, sink, _ := testManager(t, rt)
	m.Start(context.Background(), &agentlinkv1.StartServer{ServerId: "s1", ImageRef: "img:1", CmdId: "c1"})
	eventually(t, "started", func() bool { return rt.startCount() == 1 })
	rt.mu.Lock()
	sockPath := rt.started[0].SocketPath
	rt.mu.Unlock()
	liba := dialLiba(t, sockPath)
	liba.send("ready", nil)
	eventually(t, "ready", stateIs(m, "s1", "ready"))

	rt.handle("s1").exit <- Exit{Code: 137}
	eventually(t, "failed state", stateIs(m, "s1", "failed"))
	eventually(t, "failed event", func() bool { return sink.has("s1", "failed") })
	eventually(t, "container deleted", rt.handle("s1").isDeleted)
}

func TestStartFailureBecomesFailedTombstone(t *testing.T) {
	rt := newFakeRuntime()
	rt.startErr = errors.New("image pull denied")
	m, sink, _ := testManager(t, rt)
	m.Start(context.Background(), &agentlinkv1.StartServer{ServerId: "s1", ImageRef: "img:1", CmdId: "c1"})
	eventually(t, "failed tombstone in snapshot", stateIs(m, "s1", "failed"))
	eventually(t, "failed event", func() bool { return sink.has("s1", "failed") })
}

func TestRestoreRebuildsMap(t *testing.T) {
	rt := newFakeRuntime()
	h1, h2, h3, h4 := newFakeHandle(), newFakeHandle(), newFakeHandle(), newFakeHandle()
	rt.restored = []RestoredServer{
		{Handle: h1, ID: "r1", Port: 20005, ImageRef: "img:1", State: "ready", Running: true},
		{Handle: h2, ID: "r2", Port: 20006, ImageRef: "img:1", State: "allocated", MatchID: "m-1", Running: true},
		{Handle: h3, ID: "r3", Port: 20007, ImageRef: "img:1", State: "starting", Running: false, ExitCode: 2},
		{Handle: h4, ID: "r4", Port: 20008, ImageRef: "img:1", State: "draining", Running: false, ExitCode: 0},
	}
	m, sink, _ := testManager(t, rt)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Live servers are back at their recorded states.
	eventually(t, "r1 ready", stateIs(m, "r1", "ready"))
	eventually(t, "r2 allocated", stateIs(m, "r2", "allocated"))
	if s := snapshotOf(m, "r2"); s.GetMatchId() != "m-1" || s.GetPort() != 20006 {
		t.Fatalf("r2 snapshot: %+v", s)
	}
	// Dead ones are cleaned up: crash reported, drained one silently reaped.
	if snapshotOf(m, "r3") != nil || snapshotOf(m, "r4") != nil {
		t.Fatal("dead containers must not enter the map")
	}
	if !sink.has("r3", "failed") {
		t.Fatal("dead non-draining container must be reported failed")
	}
	if sink.has("r4", "failed") {
		t.Fatal("dead draining container must not be reported failed")
	}
	eventually(t, "dead containers deleted", func() bool { return h3.isDeleted() && h4.isDeleted() })

	// The replayed StartServer for a restored id is a no-op.
	m.Start(context.Background(), &agentlinkv1.StartServer{ServerId: "r1", ImageRef: "img:1", CmdId: "c9"})
	time.Sleep(50 * time.Millisecond)
	if rt.startCount() != 0 {
		t.Fatal("restored server must not be started again")
	}

	// liba can reconnect to the recreated socket and refresh state.
	liba := dialLiba(t, m.socketPath("r1"))
	liba.send("ready", nil) // re-sent ready on reconnect is ignored (already ready)
	liba.send("players", map[string]any{"count": 2})
	eventually(t, "players after reconnect", func() bool {
		s := snapshotOf(m, "r1")
		return s != nil && s.GetPlayers() == 2
	})

	// A restored server crashing later is reported failed.
	h1.exit <- Exit{Code: 139}
	eventually(t, "r1 failed after crash", stateIs(m, "r1", "failed"))
	if !sink.has("r1", "failed") {
		t.Fatal("crash of restored server must be reported")
	}
}

func TestPrePullReports(t *testing.T) {
	rt := newFakeRuntime()
	m, sink, _ := testManager(t, rt)
	m.PrePull(context.Background(), &agentlinkv1.PrePull{CmdId: "p1", ImageRef: "img:2"})
	eventually(t, "pulled report", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.pulls) == 2 && sink.pulls[0] == "pulling img:2" && sink.pulls[1] == "pulled img:2"
	})

	rt.pullErr = errors.New("denied")
	m.PrePull(context.Background(), &agentlinkv1.PrePull{CmdId: "p2", ImageRef: "img:3"})
	eventually(t, "failed report", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.pulls) == 4 && sink.pulls[3] == "failed img:3"
	})
}

func TestDrainAndUnsupportedAreAckOnly(t *testing.T) {
	rt := newFakeRuntime()
	m, sink, _ := testManager(t, rt)
	m.Drain(context.Background(), &agentlinkv1.Drain{CmdId: "d1", Reason: "maintenance"})
	m.Unsupported(context.Background(), "tail_logs", "t1", "s1")
	m.Unsupported(context.Background(), "upgrade_agent", "u1", "")
	eventually(t, "unsupported event for tail_logs", func() bool {
		return sink.has("s1", "unsupported")
	})
	if rt.startCount() != 0 {
		t.Fatal("drain/unsupported must not touch the runtime")
	}
}
