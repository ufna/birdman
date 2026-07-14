package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ufna/birdman/agent/internal/config"
	"github.com/ufna/birdman/agent/internal/imagegc"
	"github.com/ufna/birdman/agent/internal/runtime"
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
	pid     uint32
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
func (h *fakeHandle) Pid() uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pid
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
	mu          sync.Mutex
	started     []StartSpec
	handles     map[string]*fakeHandle
	restored    []RestoredServer
	startErr    error
	pulls       []string
	pullLookups []runtime.CredLookup // one per Pull call, same index as pulls
	pullErr     error

	// image-store surface (RemoveImage handler + imagegc, environments v1 §6б):
	images    []imagegc.Image // images present in the runtime store
	usedRefs  map[string]bool // refs backing a live container (UsedImageRefs)
	deleted   []string        // DeleteImage calls, in order
	deleteErr error
	usedErr   error
	// usedGate, when non-nil, blocks UsedImageRefs until closed — lets a test
	// prove the RemoveImage handler does its runtime work in a goroutine and
	// never blocks the command dispatch (recv) loop.
	usedGate chan struct{}
}

func newFakeRuntime() *fakeRuntime { return &fakeRuntime{handles: map[string]*fakeHandle{}} }

func (r *fakeRuntime) Images(context.Context) ([]imagegc.Image, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.images), nil
}

func (r *fakeRuntime) ImagePresent(_ context.Context, ref string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, img := range r.images {
		if img.Name == ref {
			return true, nil
		}
	}
	return false, nil
}

func (r *fakeRuntime) UsedImageRefs(context.Context) (map[string]bool, error) {
	if r.usedGate != nil {
		<-r.usedGate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.usedErr != nil {
		return nil, r.usedErr
	}
	out := make(map[string]bool, len(r.usedRefs))
	for k, v := range r.usedRefs {
		out[k] = v
	}
	return out, nil
}

func (r *fakeRuntime) DeleteImage(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleteErr != nil {
		return r.deleteErr
	}
	r.deleted = append(r.deleted, name)
	for i, img := range r.images {
		if img.Name == name {
			r.images = slices.Delete(r.images, i, i+1)
			break
		}
	}
	return nil
}

func (r *fakeRuntime) wasDeleted(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.deleted, name)
}

func (r *fakeRuntime) deleteCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deleted)
}

func (r *fakeRuntime) Pull(_ context.Context, ref string, lookup runtime.CredLookup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pulls = append(r.pulls, ref)
	r.pullLookups = append(r.pullLookups, lookup)
	return r.pullErr
}

// lastPullLookup returns the CredLookup passed to the most recent Pull call
// (PrePull path) — nil if Pull was never called.
func (r *fakeRuntime) lastPullLookup() runtime.CredLookup {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pullLookups) == 0 {
		return nil
	}
	return r.pullLookups[len(r.pullLookups)-1]
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

type logChunk struct {
	serverID string
	data     []byte
	eof      bool
}

// imageReport is one RemoveImage RESULT the manager reported back to the master
// (proto ImageReport, environments v1 §6б).
type imageReport struct{ cmdID, ref, status, detail string }

type fakeSink struct {
	mu           sync.Mutex
	events       []sinkEvent
	pulls        []string // "status ref"
	imageReports []imageReport
	chunks       []logChunk
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
func (s *fakeSink) ImageReport(cmdID, imageRef, status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imageReports = append(s.imageReports, imageReport{cmdID, imageRef, status, detail})
}

// imageReportsFor returns every ImageReport sent for ref — the handler owes the
// master EXACTLY ONE per RemoveImage, so tests assert the count too.
func (s *fakeSink) imageReportsFor(ref string) []imageReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []imageReport
	for _, r := range s.imageReports {
		if r.ref == ref {
			out = append(out, r)
		}
	}
	return out
}

// awaitImageReport waits for the one ImageReport the RemoveImage handler owes for
// ref and returns it. The handler does its runtime work in a goroutine (it must
// never block the recv loop), so the report lands asynchronously.
func awaitImageReport(t *testing.T, sink *fakeSink, ref string) imageReport {
	t.Helper()
	eventually(t, "ImageReport for "+ref, func() bool { return len(sink.imageReportsFor(ref)) > 0 })
	got := sink.imageReportsFor(ref)
	if len(got) != 1 {
		t.Fatalf("RemoveImage owes the master EXACTLY ONE report for %s, got %d: %+v", ref, len(got), got)
	}
	return got[0]
}
func (s *fakeSink) LogChunk(_ context.Context, _, serverID string, data []byte, eof bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = append(s.chunks, logChunk{serverID, append([]byte(nil), data...), eof})
	return true
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

// tailText concatenates the payloads of all LogChunk calls for serverID.
func (s *fakeSink) tailText(serverID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b []byte
	for _, c := range s.chunks {
		if c.serverID == serverID {
			b = append(b, c.data...)
		}
	}
	return string(b)
}

// tailSawEOF reports whether a terminating (eof) chunk was sent for serverID.
func (s *fakeSink) tailSawEOF(serverID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.chunks {
		if c.serverID == serverID && c.eof {
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

// libaFrame is one decoded agent→liba frame.
type libaFrame struct {
	V    int            `json:"v"`
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// readFrames drains agent→liba frames into the returned channel (closed on
// disconnect). Call once per connection.
func (f *fakeLiba) readFrames() <-chan libaFrame {
	ch := make(chan libaFrame, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(f.conn)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			var fr libaFrame
			if json.Unmarshal(sc.Bytes(), &fr) == nil {
				select {
				case ch <- fr:
				default: // slow test — drop rather than deadlock
				}
			}
		}
	}()
	return ch
}

// awaitFrame waits for the next frame of the given type (skipping others,
// e.g. pings).
func awaitFrame(t *testing.T, ch <-chan libaFrame, typ string) libaFrame {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case fr, ok := <-ch:
			if !ok {
				t.Fatalf("liba connection closed while waiting for %q", typ)
			}
			if fr.Type == typ {
				return fr
			}
		case <-deadline:
			t.Fatalf("no %q frame in time", typ)
		}
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
		Region:               "dev",
		CapacitySlots:        4,
		PortRange:            []int{20000, 20010},
		LimitsDefault:        config.Limits{CPUMillis: 500, MemMB: 128},
		LogDir:               t.TempDir(),
		DataDir:              t.TempDir(),
		LogMaxSizeMB:         100,
		LogRetentionDays:     7,
		DiskGCWatermarkPct:   80, // agent.md §6
		DiskFullWatermarkPct: 90, // agent.md §6 (0 would reject every start)
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

// writeToken writes content to a fresh temp file and returns its path — a
// registry_auth.token_file stand-in.
func writeToken(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// logCapture records every m.logf call so tests can assert on log content
// (e.g. "the WARN fired exactly once", "the token never appears").
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) Printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *logCapture) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func (c *logCapture) count(substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
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

// AllocateServer → `allocated` frame in liba, state ready → allocated with
// the match id persisted to labels; the frame is cached and replayed to a
// reconnecting liba; a clean exit after the match leaves `stopped`, not
// `failed` (итерация 2).
func TestAllocateDeliversToLiba(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)
	m.Start(context.Background(), &agentlinkv1.StartServer{ServerId: "s1", ImageRef: "img:1", CmdId: "c1"})
	eventually(t, "started", func() bool { return rt.startCount() == 1 })
	rt.mu.Lock()
	sockPath := rt.started[0].SocketPath
	rt.mu.Unlock()

	liba := dialLiba(t, sockPath)
	frames := liba.readFrames()
	liba.send("hello", map[string]any{"sdk_version": "stub/1"})
	liba.send("ready", nil)
	eventually(t, "ready", stateIs(m, "s1", "ready"))

	// Allocate of an unknown server is an ignored no-op.
	m.Allocate(context.Background(), &agentlinkv1.AllocateServer{
		ServerId: "nope", MatchId: "m-0", CmdId: "a0",
	})

	m.Allocate(context.Background(), &agentlinkv1.AllocateServer{
		ServerId: "s1", MatchId: "m-42", PlayersExpected: 2, CmdId: "a1",
	})
	eventually(t, "allocated state", stateIs(m, "s1", "allocated"))
	if s := snapshotOf(m, "s1"); s.GetMatchId() != "m-42" {
		t.Fatalf("snapshot match id: %+v", s)
	}
	eventually(t, "allocated label persisted", func() bool {
		return rt.handle("s1").lastState() == "allocated/m-42"
	})
	fr := awaitFrame(t, frames, "allocated")
	if fr.V != 1 || fr.Data["match_id"] != "m-42" || fr.Data["players_expected"] != float64(2) {
		t.Fatalf("allocated frame: %+v", fr)
	}

	// Replayed command (agent restart lost the cmd_id cache) stays idempotent
	// and re-delivers the frame.
	m.Allocate(context.Background(), &agentlinkv1.AllocateServer{
		ServerId: "s1", MatchId: "m-42", PlayersExpected: 2, CmdId: "a2",
	})
	if fr := awaitFrame(t, frames, "allocated"); fr.Data["match_id"] != "m-42" {
		t.Fatalf("replayed frame: %+v", fr)
	}
	if s := snapshotOf(m, "s1"); s.GetState() != "allocated" {
		t.Fatalf("idempotent allocate broke the state: %s", s.GetState())
	}

	// liba reconnect: the last allocated frame is replayed from the cache.
	liba.conn.Close()
	liba2 := dialLiba(t, sockPath)
	frames2 := liba2.readFrames()
	fr = awaitFrame(t, frames2, "allocated")
	if fr.Data["match_id"] != "m-42" || fr.Data["players_expected"] != float64(2) {
		t.Fatalf("reconnect replay: %+v", fr)
	}

	// Match plays out; the one-shot dedik exits 0 → stopped (master reaps it),
	// NOT failed.
	liba2.send("players", map[string]any{"count": 2})
	liba2.send("match_start", map[string]any{"match_id": "m-42"})
	liba2.send("match_end", map[string]any{"match_id": "m-42", "result": "completed"})
	rt.handle("s1").exit <- Exit{Code: 0}
	eventually(t, "stopped after clean exit", stateIs(m, "s1", "stopped"))
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

// DrainServer (итерация 3): the dedik gets the `drain{deadline_s, reason}`
// frame, moves to draining (label persisted), the frame is replayed to a
// reconnecting liba, no signals are sent, and the clean self-exit after the
// match leaves `stopped` (master reaps it).
func TestDrainServerDeliversToLiba(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)
	m.Start(context.Background(), &agentlinkv1.StartServer{ServerId: "s1", ImageRef: "img:1", CmdId: "c1"})
	eventually(t, "started", func() bool { return rt.startCount() == 1 })
	rt.mu.Lock()
	sockPath := rt.started[0].SocketPath
	rt.mu.Unlock()

	liba := dialLiba(t, sockPath)
	frames := liba.readFrames()
	liba.send("ready", nil)
	eventually(t, "ready", stateIs(m, "s1", "ready"))
	m.Allocate(context.Background(), &agentlinkv1.AllocateServer{
		ServerId: "s1", MatchId: "m-1", PlayersExpected: 2, CmdId: "a1",
	})
	eventually(t, "allocated", stateIs(m, "s1", "allocated"))

	// Drain of an unknown server is an ignored no-op.
	m.DrainServer(context.Background(), &agentlinkv1.DrainServer{
		ServerId: "nope", DeadlineS: 30, Reason: "deploy", CmdId: "d0",
	})

	m.DrainServer(context.Background(), &agentlinkv1.DrainServer{
		ServerId: "s1", DeadlineS: 300, Reason: "deploy 1.1.0", CmdId: "d1",
	})
	eventually(t, "draining state", stateIs(m, "s1", "draining"))
	fr := awaitFrame(t, frames, "drain")
	if fr.V != 1 || fr.Data["deadline_s"] != float64(300) || fr.Data["reason"] != "deploy 1.1.0" {
		t.Fatalf("drain frame: %+v", fr)
	}
	eventually(t, "draining label persisted", func() bool {
		return rt.handle("s1").lastState() == "draining/m-1"
	})
	// No signal: the dedik plays the match out on its own.
	if rt.handle("s1").gotSignal(syscall.SIGTERM) {
		t.Fatal("DrainServer must not signal the container")
	}

	// Replayed command (cmd_id cache lost) is idempotent: frame re-delivered.
	m.DrainServer(context.Background(), &agentlinkv1.DrainServer{
		ServerId: "s1", DeadlineS: 300, Reason: "deploy 1.1.0", CmdId: "d2",
	})
	if fr := awaitFrame(t, frames, "drain"); fr.Data["reason"] != "deploy 1.1.0" {
		t.Fatalf("replayed drain frame: %+v", fr)
	}
	if s := snapshotOf(m, "s1"); s.GetState() != "draining" {
		t.Fatalf("idempotent drain broke the state: %s", s.GetState())
	}

	// liba reconnect: the last drain frame is replayed from the cache.
	liba.conn.Close()
	liba2 := dialLiba(t, sockPath)
	frames2 := liba2.readFrames()
	if fr := awaitFrame(t, frames2, "drain"); fr.Data["deadline_s"] != float64(300) {
		t.Fatalf("reconnect replay: %+v", fr)
	}

	// The match ends, the one-shot dedik exits 0 → stopped, not failed.
	liba2.send("match_end", map[string]any{"match_id": "m-1", "result": "completed"})
	rt.handle("s1").exit <- Exit{Code: 0}
	eventually(t, "stopped after drain", stateIs(m, "s1", "stopped"))
}

// TestNodeDrainRejectsStart covers the node-level drain (итерация 4,
// master.md §6 / agent.md §7): once drained the agent rejects new StartServer
// with a `failed` event and never touches the runtime; undrain restores normal
// starts. Both commands are idempotent.
func TestNodeDrainRejectsStart(t *testing.T) {
	rt := newFakeRuntime()
	m, sink, _ := testManager(t, rt)

	m.Drain(context.Background(), &agentlinkv1.Drain{CmdId: "d1", Reason: "maintenance"})
	if !m.Draining() {
		t.Fatal("Draining() must be true after Drain")
	}
	m.Drain(context.Background(), &agentlinkv1.Drain{CmdId: "d2"}) // idempotent

	m.Start(context.Background(), &agentlinkv1.StartServer{ServerId: "s1", ImageRef: "img:1", CmdId: "c1"})
	eventually(t, "failed event for drained start", func() bool { return sink.has("s1", "failed") })
	eventually(t, "s1 failed", stateIs(m, "s1", "failed"))
	if rt.startCount() != 0 {
		t.Fatal("drain must not touch the runtime")
	}

	m.Undrain(context.Background(), &agentlinkv1.Undrain{})
	if m.Draining() {
		t.Fatal("Draining() must be false after Undrain")
	}
	m.Undrain(context.Background(), &agentlinkv1.Undrain{}) // idempotent

	m.Start(context.Background(), &agentlinkv1.StartServer{ServerId: "s2", ImageRef: "img:1", CmdId: "c2"})
	eventually(t, "runtime started after undrain", func() bool { return rt.startCount() == 1 })
}

// TestTailLogsReportsNoLogs covers TailLogs (agent.md §5): a tail for a server
// with no log files on disk streams a human-readable "no logs" chunk and
// terminates the stream with an eof chunk instead of hanging.
func TestTailLogsReportsNoLogs(t *testing.T) {
	rt := newFakeRuntime()
	m, sink, _ := testManager(t, rt)
	m.TailLogs(context.Background(), &agentlinkv1.TailLogs{CmdId: "t1", ServerId: "ghost"})
	eventually(t, "no-logs chunk", func() bool {
		return strings.Contains(sink.tailText("ghost"), "no logs")
	})
	eventually(t, "tail eof", func() bool { return sink.tailSawEOF("ghost") })
}

// TestSetRegistriesHandlerStoresSnapshot covers the Handler-side of
// registries v1 (docs/superpowers/specs/2026-07-09-registries-design.md
// §2/§3): dispatching SetRegistries makes the credential immediately
// resolvable through the chain, and a second (different) snapshot replaces
// the first wholesale rather than merging.
func TestSetRegistriesHandlerStoresSnapshot(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)

	m.SetRegistries(context.Background(), &agentlinkv1.SetRegistries{
		CmdId: "sr-1",
		Registries: []*agentlinkv1.RegistryCred{
			{Host: "ghcr.io", Username: "u1", Token: "t1"},
		},
	})
	u, tok, source, err := m.resolveRegistryAuth("ghcr.io")
	if err != nil || source != "master" || u != "u1" || tok != "t1" {
		t.Fatalf("after first SetRegistries: u=%q tok=%q source=%q err=%v", u, tok, source, err)
	}

	// A second, different snapshot replaces the first: ghcr.io is gone.
	m.SetRegistries(context.Background(), &agentlinkv1.SetRegistries{
		CmdId: "sr-2",
		Registries: []*agentlinkv1.RegistryCred{
			{Host: "registry.example.com:5000", Username: "u2", Token: "t2"},
		},
	})
	if _, _, source, err := m.resolveRegistryAuth("ghcr.io"); err != nil || source != "anonymous" {
		t.Fatal("second SetRegistries must wipe the first snapshot, not merge with it")
	}
	u, tok, source, err = m.resolveRegistryAuth("registry.example.com:5000")
	if err != nil || source != "master" || u != "u2" || tok != "t2" {
		t.Fatalf("after second SetRegistries: u=%q tok=%q source=%q err=%v", u, tok, source, err)
	}
}

// TestRegistryAuthChainPrecedence covers the full pull-auth chain (§3):
// master snapshot (exact host) beats the legacy agent.yaml file cred; a
// legacy host mismatch or an empty chain falls through to anonymous.
func TestRegistryAuthChainPrecedence(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)

	// Nothing configured anywhere → anonymous.
	if _, _, source, err := m.resolveRegistryAuth("ghcr.io"); err != nil || source != "anonymous" {
		t.Fatalf("empty chain: err=%v source=%q, want nil/anonymous", err, source)
	}

	// Legacy configured for a specific host: a DIFFERENT host still misses.
	m.cfg.RegistryAuth = &config.RegistryAuth{
		Username: "legacy-user", TokenFile: writeToken(t, "legacy-tok"), Host: "registry.example.com",
	}
	if _, _, source, err := m.resolveRegistryAuth("ghcr.io"); err != nil || source != "anonymous" {
		t.Fatalf("legacy host mismatch: err=%v source=%q, want nil/anonymous", err, source)
	}
	// Legacy host match.
	u, tok, source, err := m.resolveRegistryAuth("registry.example.com")
	if err != nil || source != "legacy" || u != "legacy-user" || tok != "legacy-tok" {
		t.Fatalf("legacy hit: u=%q tok=%q source=%q err=%v", u, tok, source, err)
	}

	// Master snapshot for the SAME host beats the legacy fallback.
	m.registries.Set([]*agentlinkv1.RegistryCred{
		{Host: "registry.example.com", Username: "master-user", Token: "master-tok"},
	})
	u, tok, source, err = m.resolveRegistryAuth("registry.example.com")
	if err != nil || source != "master" || u != "master-user" || tok != "master-tok" {
		t.Fatalf("master beats legacy: u=%q tok=%q source=%q err=%v", u, tok, source, err)
	}

	// Master credential for one host must not leak to an unrelated one that
	// neither master nor legacy know — still anonymous (the anti-exfiltration
	// property the whole design closes).
	if _, _, source, err := m.resolveRegistryAuth("evil.example.com"); err != nil || source != "anonymous" {
		t.Fatalf("unrelated host: err=%v source=%q, want nil/anonymous", err, source)
	}
}

// TestLegacyDefaultHostWarnsOnce covers the legacy registry_auth fallback
// without an explicit host (§3): it defaults to ghcr.io, and the WARN about
// that default fires exactly once per process no matter how many pulls
// consult the chain.
func TestLegacyDefaultHostWarnsOnce(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)
	cap := &logCapture{}
	m.logf = cap.Printf
	m.cfg.RegistryAuth = &config.RegistryAuth{Username: "u", TokenFile: writeToken(t, "tok")} // Host left unset

	u, tok, source, err := m.resolveRegistryAuth("ghcr.io")
	if err != nil || source != "legacy" || u != "u" || tok != "tok" {
		t.Fatalf("legacy default host hit: u=%q tok=%q source=%q err=%v", u, tok, source, err)
	}

	// Consult the chain repeatedly (a miss and two more hits): the WARN must
	// not fire again.
	m.resolveRegistryAuth("evil.example.com")
	m.resolveRegistryAuth("ghcr.io")
	m.resolveRegistryAuth("ghcr.io")

	if n := cap.count("registry_auth без host"); n != 1 {
		t.Fatalf("WARN must fire exactly once per process, fired %d time(s):\n%s", n, cap.all())
	}
}

// TestPrePullUsesRegistryAuthChain covers requirement §3/§5: the PrePull path
// (not just StartServer) must resolve credentials through the very same
// chain, host-matched.
func TestPrePullUsesRegistryAuthChain(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)
	m.registries.Set([]*agentlinkv1.RegistryCred{
		{Host: "ghcr.io", Username: "u1", Token: "t1"},
	})

	m.PrePull(context.Background(), &agentlinkv1.PrePull{CmdId: "p1", ImageRef: "ghcr.io/x/y:1"})
	eventually(t, "pull recorded", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.pulls) == 1
	})

	lookup := rt.lastPullLookup()
	if lookup == nil {
		t.Fatal("PrePull did not pass a CredLookup to Runtime.Pull")
	}
	u, tok, err := lookup("ghcr.io")
	if err != nil || u != "u1" || tok != "t1" {
		t.Fatalf("PrePull lookup(ghcr.io) = (%q, %q, %v), want (u1, t1, nil) — PrePull must use the same chain as StartServer", u, tok, err)
	}
	if u, _, err := lookup("evil.example.com"); err != nil || u != "" {
		t.Fatal("PrePull lookup must not match an unrelated host")
	}
}

// TestLegacyBrokenTokenFileFailsPull is the Fix-1 regression test (task
// review of the host-match work, registries-v1): a legacy registry_auth host
// match whose token_file cannot be read must FAIL the pull with that error —
// never silently degrade to an anonymous pull, which would mask a day-one
// token_file typo behind a confusing registry 401 later. Master snapshot is
// left empty so only the legacy path is in play.
func TestLegacyBrokenTokenFileFailsPull(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)
	m.cfg.RegistryAuth = &config.RegistryAuth{
		Username:  "legacy-user",
		TokenFile: filepath.Join(t.TempDir(), "missing-token"), // never written — read fails
		Host:      "ghcr.io",
	}

	m.PrePull(context.Background(), &agentlinkv1.PrePull{CmdId: "p1", ImageRef: "ghcr.io/x/y:1"})
	eventually(t, "pull recorded", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.pulls) == 1
	})

	lookup := rt.lastPullLookup()
	if lookup == nil {
		t.Fatal("PrePull did not pass a CredLookup to Runtime.Pull")
	}

	// The legacy host matches — the callback must fail, not go anonymous.
	u, tok, err := lookup("ghcr.io")
	if err == nil {
		t.Fatalf("legacy host match with unreadable token_file: got (%q, %q, nil), want a non-nil error", u, tok)
	}
	if !strings.Contains(err.Error(), "read registry token") || !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("error must carry the actionable file-read cause (surfaces via PullReport/ServerEvent detail), got: %v", err)
	}

	// Regression: a host that does not match the legacy config at all is
	// still a clean anonymous pull, not an error.
	if u, _, err := lookup("evil.example.com"); err != nil || u != "" {
		t.Fatalf("unrelated host must stay anonymous: u=%q err=%v", u, err)
	}
}

// TestStartServerUsesRegistryAuthChain mirrors TestPrePullUsesRegistryAuthChain
// for the StartServer path (StartSpec.Lookup).
func TestStartServerUsesRegistryAuthChain(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)
	m.registries.Set([]*agentlinkv1.RegistryCred{
		{Host: "ghcr.io", Username: "u1", Token: "t1"},
	})

	m.Start(context.Background(), &agentlinkv1.StartServer{ServerId: "s1", ImageRef: "ghcr.io/x/y:1", CmdId: "c1"})
	eventually(t, "started", func() bool { return rt.startCount() == 1 })
	rt.mu.Lock()
	lookup := rt.started[0].Lookup
	rt.mu.Unlock()
	if lookup == nil {
		t.Fatal("StartServer did not pass a Lookup in StartSpec")
	}
	u, tok, err := lookup("ghcr.io")
	if err != nil || u != "u1" || tok != "t1" {
		t.Fatalf("StartServer lookup(ghcr.io) = (%q, %q, %v), want (u1, t1, nil)", u, tok, err)
	}
}

// --- RemoveImage (environments v1 §6б) ---
//
// КАЖДАЯ ветка хендлера ОБЯЗАНА доложить результат мастеру одним ImageReport
// (removed|absent|busy|error). Ack подтверждает лишь ПОЛУЧЕНИЕ команды, поэтому
// без отчёта мастер штамповал маркер versions.image_cleanup_at вслепую: пропущенное
// удаление (образ занят дренящимся контейнером, ошибка рантайма) терялось навсегда,
// а образ доживал до watermark-GC. Тесты ниже фиксируют статус на всех четырёх.

// TestRemoveImageDeletesAndUntouches: an unused, present image is deleted
// synchronously and its ref is dropped from the GC protected set (РЕВИЗИЯ M12
// — a dead ref must not keep occupying a protection slot). Unrelated protected
// refs are untouched. Reports `removed`.
func TestRemoveImageDeletesAndUntouches(t *testing.T) {
	rt := newFakeRuntime()
	rt.images = []imagegc.Image{{Name: "img:1"}, {Name: "keep:2"}}
	rt.usedRefs = map[string]bool{}
	m, sink, _ := testManager(t, rt)

	gc := imagegc.New(imagegc.Options{DiskUsage: func() (uint64, uint64) { return 0, 0 }, Logf: t.Logf})
	m.touchImage = gc.Touch
	m.untouchImage = gc.Untouch
	gc.Touch("img:1")
	gc.Touch("keep:2")

	m.RemoveImage(context.Background(), &agentlinkv1.RemoveImage{CmdId: "r1", ImageRef: "img:1"})
	eventually(t, "img:1 deleted", func() bool { return rt.wasDeleted("img:1") })
	eventually(t, "img:1 untouched from GC set", func() bool { return !gc.Protected()["img:1"] })
	if !gc.Protected()["keep:2"] {
		t.Fatal("Untouch must drop only the removed ref, keep:2 must stay protected")
	}
	got := awaitImageReport(t, sink, "img:1")
	if got.status != "removed" || got.cmdID != "r1" || got.detail != "" {
		t.Fatalf("deleted image must report removed/cmd r1/no detail, got %+v", got)
	}
}

// TestRemoveImageAbsentNoop: a command for an image not present on the node is
// a clean no-op — no delete is attempted (idempotency under at-least-once
// replay after the image was already collected). Reports `absent`, which the
// master accepts exactly like `removed`: the image is not on the node, which is
// the whole point of the command. That equivalence is what makes the sweep's
// re-send safe.
func TestRemoveImageAbsentNoop(t *testing.T) {
	rt := newFakeRuntime()
	rt.images = []imagegc.Image{{Name: "other:9"}} // img:1 not present
	rt.usedRefs = map[string]bool{}
	m, sink, _ := testManager(t, rt)
	cap := &logCapture{}
	m.logf = cap.Printf

	m.RemoveImage(context.Background(), &agentlinkv1.RemoveImage{CmdId: "r1", ImageRef: "img:1"})
	eventually(t, "no-op logged", func() bool { return cap.count("no-op") >= 1 })
	if rt.deleteCount() != 0 {
		t.Fatalf("absent image must not be deleted, deletes=%d", rt.deleteCount())
	}
	if got := awaitImageReport(t, sink, "img:1"); got.status != "absent" || got.detail != "" {
		t.Fatalf("absent image must report absent with no detail, got %+v", got)
	}
}

// TestRemoveImageBusySkips: an image backing a live container is skipped, never
// deleted — the version's servers are still draining. The master IS told now
// (`busy`): it used to be kept in the dark, stamp its «одна догоняющая команда»
// marker anyway, and never retry — so the image survived to the watermark GC.
func TestRemoveImageBusySkips(t *testing.T) {
	rt := newFakeRuntime()
	rt.images = []imagegc.Image{{Name: "img:1"}}
	rt.usedRefs = map[string]bool{"img:1": true}
	m, sink, _ := testManager(t, rt)
	cap := &logCapture{}
	m.logf = cap.Printf

	m.RemoveImage(context.Background(), &agentlinkv1.RemoveImage{CmdId: "r1", ImageRef: "img:1"})
	eventually(t, "busy skip logged", func() bool { return cap.count("in use") >= 1 })
	if rt.deleteCount() != 0 {
		t.Fatalf("busy image must not be deleted, deletes=%d", rt.deleteCount())
	}
	if got := awaitImageReport(t, sink, "img:1"); got.status != "busy" {
		t.Fatalf("image backing a live container must report busy, got %+v", got)
	}
}

// TestRemoveImageErrorReported: a runtime failure reports `error` + detail, so the
// master leaves the version UNMARKED and the 60s sweep re-sends — instead of
// recording «done» over a removal that never happened. Both failing probes of the
// handler are covered: the delete itself, and the used-refs lookup that precedes it.
func TestRemoveImageErrorReported(t *testing.T) {
	t.Run("delete fails", func(t *testing.T) {
		rt := newFakeRuntime()
		rt.images = []imagegc.Image{{Name: "img:1"}}
		rt.usedRefs = map[string]bool{}
		rt.deleteErr = errors.New("containerd: image is dangling")
		m, sink, _ := testManager(t, rt)

		m.RemoveImage(context.Background(), &agentlinkv1.RemoveImage{CmdId: "r1", ImageRef: "img:1"})
		got := awaitImageReport(t, sink, "img:1")
		if got.status != "error" || !strings.Contains(got.detail, "dangling") {
			t.Fatalf("failed delete must report error with the runtime's text in detail, got %+v", got)
		}
	})
	t.Run("used-refs probe fails", func(t *testing.T) {
		rt := newFakeRuntime()
		rt.images = []imagegc.Image{{Name: "img:1"}}
		rt.usedErr = errors.New("list containers: connection refused")
		m, sink, _ := testManager(t, rt)

		m.RemoveImage(context.Background(), &agentlinkv1.RemoveImage{CmdId: "r1", ImageRef: "img:1"})
		got := awaitImageReport(t, sink, "img:1")
		if got.status != "error" || !strings.Contains(got.detail, "connection refused") {
			t.Fatalf("failed used-refs probe must report error with detail, got %+v", got)
		}
		if rt.deleteCount() != 0 {
			t.Fatalf("a failed used-refs probe must not delete anything, deletes=%d", rt.deleteCount())
		}
	})
}

// TestRemoveImageDoesNotBlockDispatch: the handler must return immediately and
// do its runtime work (UsedImageRefs/DeleteImage) in a goroutine — synchronous
// work would stall the recv loop that acks every command (РЕВИЗИЯ I1, PrePull
// pattern). UsedImageRefs is gated so a synchronous implementation would hang
// the RemoveImage call itself.
func TestRemoveImageDoesNotBlockDispatch(t *testing.T) {
	rt := newFakeRuntime()
	rt.images = []imagegc.Image{{Name: "img:1"}}
	rt.usedRefs = map[string]bool{}
	rt.usedGate = make(chan struct{}) // UsedImageRefs blocks until released
	m, _, _ := testManager(t, rt)

	returned := make(chan struct{})
	go func() {
		m.RemoveImage(context.Background(), &agentlinkv1.RemoveImage{CmdId: "r1", ImageRef: "img:1"})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("RemoveImage blocked dispatch: it did the runtime work synchronously")
	}

	// Release the gate — the goroutine now completes the deletion.
	close(rt.usedGate)
	eventually(t, "delete happens after gate release", func() bool { return rt.wasDeleted("img:1") })
}

// TestRemoveImageEmptyRefIgnored: an empty image_ref is dropped SYNCHRONOUSLY
// (before the goroutine) — the guard logs and returns on the calling goroutine,
// so no runtime work is scheduled at all. Asserting the synchronous log line +
// zero deletes is deterministic; no sleep to race the (never-spawned) goroutine.
func TestRemoveImageEmptyRefIgnored(t *testing.T) {
	rt := newFakeRuntime()
	m, sink, _ := testManager(t, rt)
	cap := &logCapture{}
	m.logf = cap.Printf
	m.RemoveImage(context.Background(), &agentlinkv1.RemoveImage{CmdId: "r1", ImageRef: ""})
	if cap.count("empty image_ref") == 0 {
		t.Fatal("empty image_ref must be ignored synchronously with a log line")
	}
	if rt.deleteCount() != 0 {
		t.Fatal("empty image_ref must be ignored without touching the runtime")
	}
	// Even the command the master never sends gets its one report: «ровно один отчёт
	// на команду» keeps the master's per-node bookkeeping unambiguous.
	got := sink.imageReportsFor("")
	if len(got) != 1 || got[0].status != "error" || got[0].detail == "" {
		t.Fatalf("empty image_ref must still report error+detail exactly once, got %+v", got)
	}
}

// TestPullLogNeverContainsToken covers the per-pull observability log (§3/§6):
// host+source are logged, the token never is.
func TestPullLogNeverContainsToken(t *testing.T) {
	rt := newFakeRuntime()
	m, _, _ := testManager(t, rt)
	cap := &logCapture{}
	m.logf = cap.Printf
	const secretToken = "super-secret-token-xyz"
	m.registries.Set([]*agentlinkv1.RegistryCred{
		{Host: "ghcr.io", Username: "u1", Token: secretToken},
	})

	m.PrePull(context.Background(), &agentlinkv1.PrePull{CmdId: "p1", ImageRef: "ghcr.io/x/y:1"})
	eventually(t, "pull recorded", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.pulls) == 1
	})

	all := cap.all()
	if !strings.Contains(all, "host=ghcr.io source=master") {
		t.Fatalf("expected a host=ghcr.io source=master log line, got:\n%s", all)
	}
	if strings.Contains(all, secretToken) {
		t.Fatal("pull log must never contain the token")
	}
}
