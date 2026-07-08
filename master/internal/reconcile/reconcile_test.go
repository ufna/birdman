package reconcile_test

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/reconcile"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

// fakeSender is the in-memory AgentLink transport used instead of gRPC.
type fakeSender struct {
	mu   sync.Mutex
	cmds []fakeCmd
}

type fakeCmd struct {
	NodeID string
	Msg    *agentlinkv1.MasterMsg
}

func (f *fakeSender) Send(nodeID string, msg *agentlinkv1.MasterMsg) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, fakeCmd{NodeID: nodeID, Msg: msg})
	return uuid.NewString()
}

func (f *fakeSender) take() []fakeCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.cmds
	f.cmds = nil
	return out
}

func (f *fakeSender) countStarts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if c.Msg.GetStart() != nil {
			starts++
		}
		if c.Msg.GetStop() != nil {
			stops++
		}
	}
	return
}

func newReconciler(st *store.Store) (*reconcile.Reconciler, *fakeSender) {
	sender := &fakeSender{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return reconcile.New(st, sender, log), sender
}

func TestReconcileCreatesBuffer(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 3, 50)
	r, sender := newReconciler(st)
	ctx := context.Background()

	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cmds := sender.take()
	if len(cmds) != 3 {
		t.Fatalf("want 3 StartServer commands, got %d", len(cmds))
	}
	for _, c := range cmds {
		start := c.Msg.GetStart()
		if start == nil || c.NodeID != f.NodeID {
			t.Fatalf("unexpected command: %+v", c)
		}
		if start.GetImageRef() != "ghcr.io/example/game-server:1.0.0" {
			t.Fatalf("wrong image: %s", start.GetImageRef())
		}
		if start.GetPort() != 0 {
			t.Fatalf("agent must pick the port, got %d", start.GetPort())
		}
	}
	if states := f.ServerStates(t); states["creating"] != 3 {
		t.Fatalf("want 3 creating servers, got %+v", states)
	}

	// Second pass: desired state already met — no new commands.
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cmds := sender.take(); len(cmds) != 0 {
		t.Fatalf("steady state must be quiet, got %d commands", len(cmds))
	}

	// Agent reports all ready → still quiet.
	reports := readyReports(t, f)
	if err := st.ApplyHeartbeat(ctx, f.NodeID, reports); err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cmds := sender.take(); len(cmds) != 0 {
		t.Fatalf("ready buffer must be quiet, got %d commands", len(cmds))
	}
}

// Killing a server restores the buffer on the next pass (acceptance ит. 1).
func TestReconcileRestoresAfterFailure(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	r, sender := newReconciler(st)
	ctx := context.Background()

	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sender.take()
	if err := st.ApplyHeartbeat(ctx, f.NodeID, readyReports(t, f)); err != nil {
		t.Fatal(err)
	}

	// Agent reports one server dead.
	victim := anyServerID(t, f, "ready")
	if err := st.ApplyServerEvent(ctx, f.NodeID, victim, "failed", "exit 1"); err != nil {
		t.Fatal(err)
	}

	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cmds := sender.take()
	if len(cmds) != 1 || cmds[0].Msg.GetStart() == nil {
		t.Fatalf("want exactly 1 replacement StartServer, got %+v", cmds)
	}
	states := f.ServerStates(t)
	if states["creating"] != 1 || states["ready"] != 1 || states["failed"] != 1 {
		t.Fatalf("unexpected states: %+v", states)
	}
}

func TestReconcileSurplusStopsOldest(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	oldest := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 3*time.Hour)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20002, 2*time.Hour)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20003, time.Hour)
	f.UpsertFleet(t, 1, 50)
	r, sender := newReconciler(st)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmds := sender.take()
	if len(cmds) != 2 {
		t.Fatalf("want 2 StopServer, got %d", len(cmds))
	}
	stopped := map[string]bool{}
	for _, c := range cmds {
		stop := c.Msg.GetStop()
		if stop == nil || stop.GetGraceS() <= 0 {
			t.Fatalf("unexpected command: %+v", c)
		}
		stopped[stop.GetServerId()] = true
	}
	if !stopped[oldest] {
		t.Fatalf("oldest ready server must be reaped first, stopped: %v", stopped)
	}
	states := f.ServerStates(t)
	if states["draining"] != 2 || states["ready"] != 1 {
		t.Fatalf("unexpected states: %+v", states)
	}
}

func TestReconcileRespectsMaxServers(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 5, 3)
	r, sender := newReconciler(st)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if starts, _ := sender.countStarts(); starts != 3 {
		t.Fatalf("max_servers=3 must cap creations, got %d", starts)
	}
}

// First-fit: the node with more used slots receives the new server (dense
// packing, docs/specs/master.md §2).
func TestReconcileFirstFitDensePacking(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	nodeB := f.AddNode(t, "node-2", "203.0.113.11", 10)
	// node-1 is busier: two allocated servers.
	f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 20001, 0)
	f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 20002, 0)
	f.UpsertFleet(t, 1, 50)
	r, sender := newReconciler(st)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmds := sender.take()
	if len(cmds) != 1 || cmds[0].Msg.GetStart() == nil {
		t.Fatalf("want 1 StartServer, got %+v", cmds)
	}
	if cmds[0].NodeID != f.NodeID {
		t.Fatalf("dense packing: want busier node %s, got %s (empty %s)", f.NodeID, cmds[0].NodeID, nodeB)
	}

	// Fill node-1 to capacity → next servers go to node-2.
	for i := 0; i < 7; i++ {
		f.InsertServer(t, f.NodeID, f.VersionID, "allocated", int32(20100+i), 0)
	}
	buffer := int32(2)
	if _, err := st.UpsertFleet(context.Background(), store.UpsertFleetParams{
		Project: f.Project, Region: f.Region, ActiveVersion: &f.VersionID, BufferReady: &buffer,
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range sender.take() {
		if c.Msg.GetStart() != nil && c.NodeID != nodeB {
			t.Fatalf("full node must overflow to node-2, got %s", c.NodeID)
		}
	}
}

// Node silent >10s → quarantine; >30s → its servers failed; heartbeat return
// → active again (docs/specs/protocol.md §1 Lease).
func TestQuarantineLifecycle(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 20002, 0)
	lease := reconcile.NewLeaseChecker(st, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ctx := context.Background()

	// Fresh heartbeat → nothing happens.
	if err := lease.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state := nodeState(t, st, f.NodeID); state != "active" {
		t.Fatalf("fresh node must stay active, got %s", state)
	}

	// 11s of silence → quarantine, servers untouched.
	f.SetHeartbeatAge(t, f.NodeID, 11*time.Second)
	if err := lease.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state := nodeState(t, st, f.NodeID); state != "quarantine" {
		t.Fatalf("want quarantine, got %s", state)
	}
	if n, _ := st.CountEvents(ctx, store.EventNodeQuarantine); n != 1 {
		t.Fatalf("want 1 node_quarantine event, got %d", n)
	}
	if states := f.ServerStates(t); states["failed"] != 0 {
		t.Fatalf("servers must survive early quarantine: %+v", states)
	}

	// 31s of silence → servers failed.
	f.SetHeartbeatAge(t, f.NodeID, 31*time.Second)
	if err := lease.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	states := f.ServerStates(t)
	if states["failed"] != 2 {
		t.Fatalf("want 2 failed servers, got %+v", states)
	}

	// Heartbeat returns → active again + recovery event.
	if err := st.ApplyHeartbeat(ctx, f.NodeID, nil); err != nil {
		t.Fatal(err)
	}
	if state := nodeState(t, st, f.NodeID); state != "active" {
		t.Fatalf("returned node must be active, got %s", state)
	}
	if n, _ := st.CountEvents(ctx, store.EventNodeRecovered); n != 1 {
		t.Fatalf("want 1 node_recovered event, got %d", n)
	}
}

// ≥3 failures of one (version,node) within 10 min pause creations for that
// pair and emit a single crash_loop event (docs/specs/master.md §2).
func TestCrashLoopPausesCreation(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 1, 50)
	r, sender := newReconciler(st)
	ctx := context.Background()

	// Three create→fail cycles.
	for i := 0; i < 3; i++ {
		if err := r.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		cmds := sender.take()
		if len(cmds) != 1 || cmds[0].Msg.GetStart() == nil {
			t.Fatalf("cycle %d: want 1 StartServer, got %+v", i, cmds)
		}
		f.MarkFailed(t, cmds[0].Msg.GetStart().GetServerId(), 0)
	}

	// Fourth pass: the pair is paused — no creations despite the deficit.
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cmds := sender.take(); len(cmds) != 0 {
		t.Fatalf("crash-looping pair must be paused, got %+v", cmds)
	}
	if n, _ := st.CountEvents(ctx, store.EventCrashLoop); n != 1 {
		t.Fatalf("want exactly 1 crash_loop event, got %d", n)
	}
	// And it stays paused on repeated passes without duplicate events.
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountEvents(ctx, store.EventCrashLoop); n != 1 {
		t.Fatalf("crash_loop event must not repeat, got %d", n)
	}

	// A second healthy node takes over the buffer.
	nodeB := f.AddNode(t, "node-2", "203.0.113.11", 10)
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cmds := sender.take()
	if len(cmds) != 1 || cmds[0].NodeID != nodeB {
		t.Fatalf("buffer must move to the healthy node, got %+v", cmds)
	}
}

// Servers stuck in creating (lost StartServer, master restart) fail by
// timeout and the deficit is re-created (уточнено в v0).
func TestStuckCreatingFailsByTimeout(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 1, 50)
	r, sender := newReconciler(st)
	ctx := context.Background()

	stuck := f.InsertServer(t, f.NodeID, f.VersionID, "creating", 0, 3*time.Minute)
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sv, err := st.GetServer(ctx, stuck)
	if err != nil {
		t.Fatal(err)
	}
	if sv.State != "failed" {
		t.Fatalf("stuck creating must fail, got %s", sv.State)
	}
	if starts, _ := sender.countStarts(); starts != 1 {
		t.Fatalf("deficit must be re-created, got %d starts", starts)
	}
}

// Ready servers of non-active versions are reaped.
func TestStaleVersionReaped(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	v2 := f.AddVersion(t, "2.0.0")
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0) // old version, ready
	buffer, maxServers := int32(0), int32(50)
	if _, err := st.UpsertFleet(context.Background(), store.UpsertFleetParams{
		Project: f.Project, Region: f.Region, ActiveVersion: &v2,
		BufferReady: &buffer, MaxServers: &maxServers,
	}); err != nil {
		t.Fatal(err)
	}
	r, sender := newReconciler(st)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, stops := sender.countStarts()
	if stops != 1 {
		t.Fatalf("stale-version ready server must be stopped, got %d stops", stops)
	}
	if states := f.ServerStates(t); states["draining"] != 1 {
		t.Fatalf("unexpected states: %+v", states)
	}
}

// Node loss fails ≥3 servers at once (reason node_lost) — that says nothing
// about the (version, node) pair: when the node returns, the buffer must be
// re-created immediately, with no crash_loop event (ложнопозитив acceptance
// итерации 1).
func TestNodeLostDoesNotPauseCreations(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 3, 50)
	r, sender := newReconciler(st)
	lease := reconcile.NewLeaseChecker(st, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ctx := context.Background()

	// Build the warm pool and make it ready.
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cmds := sender.take(); len(cmds) != 3 {
		t.Fatalf("want 3 starts, got %d", len(cmds))
	}
	if err := st.ApplyHeartbeat(ctx, f.NodeID, readyReports(t, f)); err != nil {
		t.Fatal(err)
	}

	// The node goes dark: quarantine + its servers failed (node_lost).
	f.SetHeartbeatAge(t, f.NodeID, 31*time.Second)
	if err := lease.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if states := f.ServerStates(t); states["failed"] != 3 {
		t.Fatalf("want 3 node_lost failures, got %+v", states)
	}

	// The node returns (fresh heartbeat, servers gone for real — say the box
	// rebooted): reconcile must rebuild the buffer at once.
	if err := st.ApplyHeartbeat(ctx, f.NodeID, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	starts, _ := sender.countStarts()
	if starts != 3 {
		t.Fatalf("node_lost must not pause creations: want 3 starts, got %d", starts)
	}
	if n, _ := st.CountEvents(ctx, store.EventCrashLoop); n != 0 {
		t.Fatalf("node_lost produced a false crash_loop event (%d)", n)
	}
}

// A one-shot dedik finishing its match (match_end → clean exit 0 → agent
// reports stopped) is a NORMAL end: the server is reaped, the slot re-created
// by reconcile — and repeated healthy cycles never look like a crash loop.
func TestCleanMatchCycleDoesNotFeedCrashLoop(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 1, 50)
	r, sender := newReconciler(st)
	ctx := context.Background()

	for cycle := 0; cycle < 3; cycle++ {
		// Reconcile creates the buffer server; agent brings it ready.
		if err := r.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		cmds := sender.take()
		if len(cmds) != 1 || cmds[0].Msg.GetStart() == nil {
			t.Fatalf("cycle %d: want 1 StartServer, got %+v", cycle, cmds)
		}
		serverID := cmds[0].Msg.GetStart().GetServerId()
		if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
			{ServerID: serverID, State: "ready", Port: int32(21000 + cycle)},
		}); err != nil {
			t.Fatal(err)
		}

		// Match: allocate → start → end → clean exit (stopped report).
		matchID := uuid.NewString()
		if _, err := st.Allocate(ctx, "game", "eu", nil, matchID, 2); err != nil {
			t.Fatalf("cycle %d: allocate: %v", cycle, err)
		}
		if err := st.ApplyServerEvent(ctx, f.NodeID, serverID, "match_start", matchID); err != nil {
			t.Fatal(err)
		}
		if err := st.ApplyServerEvent(ctx, f.NodeID, serverID, "match_end", matchID+" completed"); err != nil {
			t.Fatal(err)
		}
		if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
			{ServerID: serverID, State: "stopped", MatchID: matchID},
		}); err != nil {
			t.Fatal(err)
		}
		sv, err := st.GetServer(ctx, serverID)
		if err != nil {
			t.Fatal(err)
		}
		if sv.State != "reaped" {
			t.Fatalf("cycle %d: clean exit must reap, got %s", cycle, sv.State)
		}
	}

	// Replacement for the last cycle still comes; zero crash-loop signals.
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if starts, _ := sender.countStarts(); starts != 1 {
		t.Fatalf("replacement after clean cycle: want 1 start, got %d", starts)
	}
	if n, _ := st.CountEvents(ctx, store.EventCrashLoop); n != 0 {
		t.Fatalf("clean cycles fed crash-loop: %d events", n)
	}
	if n, _ := st.CountEvents(ctx, store.EventServerFailed); n != 0 {
		t.Fatalf("clean cycles produced %d server_failed events", n)
	}
	states := f.ServerStates(t)
	if states["reaped"] != 3 || states["failed"] != 0 {
		t.Fatalf("unexpected states after 3 cycles: %+v", states)
	}
}

// --- helpers ---

func readyReports(t *testing.T, f *testdb.Fixture) []store.ServerReport {
	t.Helper()
	rows, err := f.St.Pool.Query(context.Background(),
		`select id::text from servers where node_id = $1::uuid and state = 'creating'`, f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []store.ServerReport
	port := int32(25000)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, store.ServerReport{ServerID: id, State: "ready", Port: port})
		port++
	}
	return out
}

func anyServerID(t *testing.T, f *testdb.Fixture, state string) string {
	t.Helper()
	var id string
	err := f.St.Pool.QueryRow(context.Background(),
		`select id::text from servers where node_id = $1::uuid and state = $2 limit 1`,
		f.NodeID, state).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nodeState(t *testing.T, st *store.Store, nodeID string) string {
	t.Helper()
	var state string
	if err := st.Pool.QueryRow(context.Background(),
		`select state from nodes where id = $1::uuid`, nodeID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
