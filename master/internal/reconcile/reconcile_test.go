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

// TestReconcileNodeDrain covers node drain (итерация 4, master.md §6): ready
// servers on a drained node are reaped (draining + Stop) and the warm pool is
// rebuilt on an active node, while an allocated server plays its match out.
func TestReconcileNodeDrain(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // node A
	nodeB := f.AddNode(t, "node-2", "203.0.113.20", 10)
	f.UpsertFleet(t, 2, 50) // buffer 2
	r, sender := newReconciler(st)
	ctx := context.Background()

	// Warm pool of 2 ready + one allocated (live match), all on node A.
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 26001, 0)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 26002, 0)
	allocA := f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 26003, 0)

	// Buffer already satisfied → steady state is quiet.
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cmds := sender.take(); len(cmds) != 0 {
		t.Fatalf("buffer full — want quiet, got %d commands", len(cmds))
	}

	// Drain node A.
	node, err := st.DrainNode(ctx, f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node.State != "draining" {
		t.Fatalf("DrainNode: want state draining, got %s", node.State)
	}

	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	starts, stops := map[string]int{}, map[string]int{}
	for _, c := range sender.take() {
		if c.Msg.GetStart() != nil {
			starts[c.NodeID]++
		}
		if c.Msg.GetStop() != nil {
			stops[c.NodeID]++
		}
	}
	if stops[f.NodeID] != 2 || len(stops) != 1 {
		t.Fatalf("want 2 Stop on the drained node, got %v", stops)
	}
	if starts[nodeB] != 2 || len(starts) != 1 {
		t.Fatalf("want 2 Start on the active node B, got %v", starts)
	}
	// The allocated server keeps playing — never touched by drain.
	if sv, err := st.GetServer(ctx, allocA); err != nil || sv.State != "allocated" {
		t.Fatalf("allocated server must keep playing, got state=%s err=%v", sv.State, err)
	}
	if states := f.ServerStates(t); states["draining"] != 2 || states["creating"] != 2 || states["allocated"] != 1 {
		t.Fatalf("unexpected states after drain: %+v", states)
	}

	// node_drain event recorded.
	if n, err := st.CountEvents(ctx, store.EventNodeDrain); err != nil || n != 1 {
		t.Fatalf("want 1 node_drain event, got %d err=%v", n, err)
	}

	// Undrain: node active again, idempotent.
	if node, err := st.UndrainNode(ctx, f.NodeID); err != nil || node.State != "active" {
		t.Fatalf("UndrainNode: want active, got state=%s err=%v", node.State, err)
	}
	if n, err := st.CountEvents(ctx, store.EventNodeUndrain); err != nil || n != 1 {
		t.Fatalf("want 1 node_undrain event, got %d err=%v", n, err)
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

// Спред с follow-ups итерации 5 (анти-аффинити, спека §1) сделал наименее
// занятую ноду первой — bin-pack (busier-node-first) отменён (см.
// TestReconcilePlacementSpreads). Здесь фиксируем комплементарную клаузу
// u.used < capacity_slots: нода на ПОЛНОЙ ёмкости исключается из размещения,
// даже когда спред предпочёл бы её (у неё меньше used) — буфер уходит на ноду
// со свободным слотом (раньше это была часть 2 dense-packing-теста).
func TestReconcilePlacementSkipsFullNode(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 2)                    // node A, capacity 2
	nodeB := f.AddNode(t, "node-2", "203.0.113.11", 10) // node B, capacity 10
	// node A is full (2/2) and, by used count, the spread-preferred node;
	// node B carries more (3 used) but still has free slots.
	f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 20001, 0)
	f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 20002, 0)
	f.InsertServer(t, nodeB, f.VersionID, "allocated", 20003, 0)
	f.InsertServer(t, nodeB, f.VersionID, "allocated", 20004, 0)
	f.InsertServer(t, nodeB, f.VersionID, "allocated", 20005, 0)
	f.UpsertFleet(t, 2, 50)
	r, sender := newReconciler(st)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	starts := map[string]int{}
	for _, c := range sender.take() {
		if c.Msg.GetStart() != nil {
			starts[c.NodeID]++
		}
	}
	// The full node A (used=2=capacity) is excluded even though spread would
	// pick it first; both buffer servers land on node B.
	if starts[nodeB] != 2 || starts[f.NodeID] != 0 {
		t.Fatalf("full node A must be skipped, buffer must land on node B, got %+v", starts)
	}
}

// TestReconcilePlacementSpreads: анти-аффинити размещения буфера (спека
// follow-ups итерации 5 §1). Два активных узла региона (по 2 слота).
// buffer_ready=2 → по одному creating на каждом узле (спред), а не 2+0 на
// одном: смерть любого узла теряет минимум ready. buffer_ready=3 → третий
// слот идёт на менее занятый узел (2+1), но заведомо не 3+0.
func TestReconcilePlacementSpreads(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 2)                   // node A, capacity 2
	nodeB := f.AddNode(t, "node-2", "203.0.113.11", 2) // node B, capacity 2
	f.UpsertFleet(t, 2, 4)                             // buffer 2, max 4
	r, _ := newReconciler(st)
	ctx := context.Background()

	// buffer 2 over two empty nodes → one warm server on each (anti-affinity),
	// never 2+0 on a single node.
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if per := placedPerNode(t, f); per[f.NodeID] != 1 || per[nodeB] != 1 {
		t.Fatalf("buffer 2 must spread 1+1 across nodes, got %+v", per)
	}

	// Grow the buffer to 3: the extra server lands on the less-busy node →
	// 2+1, never 3+0 (and each node caps at 2 slots anyway).
	f.UpsertFleet(t, 3, 4)
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	per := placedPerNode(t, f)
	if per[f.NodeID]+per[nodeB] != 3 {
		t.Fatalf("buffer 3 must place 3 warm servers, got %+v", per)
	}
	if per[f.NodeID] == 3 || per[nodeB] == 3 {
		t.Fatalf("buffer must not pile 3+0 on one node, got %+v", per)
	}
	if per[f.NodeID] < 1 || per[nodeB] < 1 {
		t.Fatalf("both nodes must carry buffer, got %+v", per)
	}
}

// Node silent >10s → quarantine; >30s → its servers failed; heartbeat return
// → active again (docs/specs/protocol.md §1 Lease).
func TestQuarantineLifecycle(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 20002, 0)
	lease := reconcile.NewLeaseChecker(st, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), 10*time.Minute)
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
	v2 := f.AddVersion(t, "2.0.0", "dev")
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0) // old version, ready
	buffer, maxServers := int32(0), int32(50)
	if _, err := st.UpsertFleet(context.Background(), store.UpsertFleetParams{
		Project: f.Project, Env: f.Env, Region: f.Region, ActiveVersion: &v2,
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
	lease := reconcile.NewLeaseChecker(st, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), 10*time.Minute)
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

// --- multi-version window (итерация 3, master.md §5) ---

// flipActive deploys v2 through the real store flip: v2 → prepulling →
// active, the old active version → deprecated (deprecated_at = now).
func flipActive(t *testing.T, st *store.Store, versionID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Pool.Exec(ctx,
		`update versions set state = 'prepulling' where id = $1::uuid`, versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateVersion(ctx, versionID, "prepulling", store.EventDeployActivated, nil); err != nil {
		t.Fatal(err)
	}
}

// In the window both buffers are kept: full buffer_ready for the active
// version, min(2, buffer_ready) for the deprecated one.
func TestWindowKeepsBothBuffers(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 20)
	f.UpsertFleet(t, 3, 50) // active 1.0.0, buffer 3
	v2 := f.AddVersion(t, "1.1.0", "dev")
	flipActive(t, st, v2) // 1.0.0 → deprecated
	r, sender := newReconciler(st)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	byImage := map[string]int{}
	for _, c := range sender.take() {
		if s := c.Msg.GetStart(); s != nil {
			byImage[s.GetImageRef()]++
		}
	}
	if byImage["ghcr.io/example/game-server:1.1.0"] != 3 {
		t.Fatalf("active buffer: want 3 starts of 1.1.0, got %+v", byImage)
	}
	if byImage["ghcr.io/example/game-server:1.0.0"] != 2 {
		t.Fatalf("deprecated buffer: want min(2,3)=2 starts of 1.0.0, got %+v", byImage)
	}

	// Steady state stays quiet.
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cmds := sender.take(); len(cmds) != 0 {
		t.Fatalf("window steady state must be quiet, got %d commands", len(cmds))
	}
}

// Deprecated ready servers beyond min(2, buffer) are stopped (oldest first);
// allocated ones keep playing.
func TestWindowDeprecatedSurplusStopped(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 20)
	buffer := int32(0)
	if _, err := st.UpsertFleet(context.Background(), store.UpsertFleetParams{
		Project: f.Project, Env: f.Env, Region: f.Region, ActiveVersion: &f.VersionID, BufferReady: &buffer,
	}); err != nil {
		t.Fatal(err)
	}
	v2 := f.AddVersion(t, "1.1.0", "dev")
	flipActive(t, st, v2)

	// buffer_ready 0 → deprecated target min(2,0)=0: every ready v1 goes.
	oldest := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 2*time.Hour)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20002, time.Hour)
	alive := f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 20003, 2*time.Hour)
	r, sender := newReconciler(st)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped := map[string]bool{}
	for _, c := range sender.take() {
		if s := c.Msg.GetStop(); s != nil {
			stopped[s.GetServerId()] = true
		}
		if d := c.Msg.GetDrainServer(); d != nil {
			t.Fatalf("in-window deprecated allocated server must not be drained: %+v", d)
		}
	}
	if len(stopped) != 2 || !stopped[oldest] {
		t.Fatalf("want both ready v1 stopped (incl. oldest), got %v", stopped)
	}
	sv, err := st.GetServer(context.Background(), alive)
	if err != nil || sv.State != "allocated" {
		t.Fatalf("allocated deprecated server must keep playing: %+v %v", sv, err)
	}
}

// reap_ttl_min closes the window: the deprecated version goes disabled, its
// ready buffer is reaped and its LIVE match gets a per-server drain (exactly
// once); the dedik plays out and exits itself.
func TestWindowReapTTLDrainsLiveMatch(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 20)
	buffer, maxServers, reapTTL := int32(1), int32(50), int32(30)
	if _, err := st.UpsertFleet(context.Background(), store.UpsertFleetParams{
		Project: f.Project, Env: f.Env, Region: f.Region, ActiveVersion: &f.VersionID,
		BufferReady: &buffer, MaxServers: &maxServers, ReapTTLMin: &reapTTL,
	}); err != nil {
		t.Fatal(err)
	}
	v2 := f.AddVersion(t, "1.1.0", "dev")
	flipActive(t, st, v2)
	ctx := context.Background()

	// v1 (deprecated): one ready + one live match; v2: a live match too.
	v1ready := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	v1match := f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 20002, 0)
	v2match := f.InsertServer(t, f.NodeID, v2, "allocated", 20003, 0)
	r, sender := newReconciler(st)

	// Window still open (fresh deprecated_at): nothing v1-related is touched.
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sender.take()
	if got := versionState(t, st, f.VersionID); got != "deprecated" {
		t.Fatalf("window must be open, got %s", got)
	}

	// Fast-forward past the TTL → the window closes on the next pass.
	if err := st.SetVersionDeprecatedAt(ctx, f.VersionID,
		time.Now().Add(-time.Duration(reapTTL+1)*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := versionState(t, st, f.VersionID); got != "disabled" {
		t.Fatalf("expired deprecated version: want disabled, got %s", got)
	}
	if n, _ := st.CountEvents(ctx, store.EventVersionDisabled); n != 1 {
		t.Fatalf("want 1 version_disabled event, got %d", n)
	}

	var drains, stops int
	for _, c := range sender.take() {
		if d := c.Msg.GetDrainServer(); d != nil {
			drains++
			if d.GetServerId() != v1match {
				t.Fatalf("drained wrong server: %s (want %s)", d.GetServerId(), v1match)
			}
			if d.GetDeadlineS() != store.DrainDeadlineS || d.GetReason() == "" {
				t.Fatalf("drain command: %+v", d)
			}
		}
		if s := c.Msg.GetStop(); s != nil {
			stops++
			if s.GetServerId() != v1ready {
				t.Fatalf("stopped wrong server: %s (want %s)", s.GetServerId(), v1ready)
			}
		}
	}
	if drains != 1 || stops != 1 {
		t.Fatalf("want 1 drain + 1 stop, got %d/%d", drains, stops)
	}
	if n, _ := st.CountEvents(ctx, store.EventServerDrain); n != 1 {
		t.Fatalf("want 1 server_drain event, got %d", n)
	}
	sv, err := st.GetServer(ctx, v1match)
	if err != nil || sv.State != "draining" {
		t.Fatalf("drained server state: %+v %v", sv, err)
	}
	if sv, _ := st.GetServer(ctx, v2match); sv.State != "allocated" {
		t.Fatalf("active-version match must be untouched, got %s", sv.State)
	}

	// The drain fires exactly once: the next pass is quiet about v1match.
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range sender.take() {
		if d := c.Msg.GetDrainServer(); d != nil {
			t.Fatalf("duplicate drain: %+v", d)
		}
	}

	// The dedik finishes and exits: stopped report → reaped, match closed by
	// its own match_end as usual (agent side covered in agent tests).
	if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
		{ServerID: v1match, State: "stopped"},
	}); err != nil {
		t.Fatal(err)
	}
	if sv, _ := st.GetServer(ctx, v1match); sv.State != "reaped" {
		t.Fatalf("drained server after exit: want reaped, got %s", sv.State)
	}
}

func versionState(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	v, err := st.GetVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v.State
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

// placedPerNode counts warm-pool servers (creating+ready) per node for the
// fixture project — the buffer's physical spread across the region.
func placedPerNode(t *testing.T, f *testdb.Fixture) map[string]int {
	t.Helper()
	rows, err := f.St.Pool.Query(context.Background(), `
		select s.node_id::text, count(*)::int from servers s
		join projects p on p.id = s.project_id
		where p.slug = $1 and s.state in ('creating','ready') group by 1`, f.Project)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var node string
		var n int
		if err := rows.Scan(&node, &n); err != nil {
			t.Fatal(err)
		}
		out[node] = n
	}
	return out
}
