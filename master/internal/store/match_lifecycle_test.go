package store_test

// Iteration-2 store behavior: AllocateServer dispatch on successful
// allocation, the matches lifecycle (pending → running → finished|aborted,
// players_peak from heartbeats) and the crash-loop input hygiene
// (node_lost failures excluded, clean one-shot exits produce no input).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Successful fresh allocation → exactly one AllocateServer to the server's
// node; the idempotent repeat and no_capacity produce none.
func TestAllocateSendsAllocateServer(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	rec := &testdb.CommandRecorder{}
	st.SetCommandSender(rec)
	ctx := context.Background()

	serverID := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	matchID := uuid.NewString()

	alloc, err := st.Allocate(ctx, "game", "dev", "eu", nil, matchID, 2)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	cmds := rec.Take()
	if len(cmds) != 1 {
		t.Fatalf("want exactly 1 command, got %d", len(cmds))
	}
	if cmds[0].NodeID != f.NodeID {
		t.Fatalf("command went to node %s, want %s", cmds[0].NodeID, f.NodeID)
	}
	a := cmds[0].Msg.GetAllocate()
	if a == nil {
		t.Fatalf("want AllocateServer, got %+v", cmds[0].Msg)
	}
	if a.GetServerId() != serverID || a.GetServerId() != alloc.ServerID ||
		a.GetMatchId() != matchID || a.GetPlayersExpected() != 2 {
		t.Fatalf("AllocateServer fields: %+v (server %s, match %s)", a, serverID, matchID)
	}

	// Idempotent repeat: same server back, no duplicate command (the hub
	// already tracks the pending one).
	again, err := st.Allocate(ctx, "game", "dev", "eu", nil, matchID, 2)
	if err != nil || again.ServerID != alloc.ServerID {
		t.Fatalf("repeat: %+v, %v", again, err)
	}
	if cmds := rec.Take(); len(cmds) != 0 {
		t.Fatalf("idempotent repeat must not re-send, got %d commands", len(cmds))
	}

	// no_capacity → no command.
	if _, err := st.Allocate(ctx, "game", "dev", "eu", nil, uuid.NewString(), 2); !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("want no_capacity, got %v", err)
	}
	if cmds := rec.Take(); len(cmds) != 0 {
		t.Fatalf("no_capacity must not send, got %d commands", len(cmds))
	}
}

// M-5: the allocation idempotency fast-path (findByMatch) is env-scoped. The
// same match_id in a different env must NOT hand back the first env's server —
// it misses the fast-path and falls through to the normal claim (here prod has
// no ready servers → no_capacity, rather than leaking the dev server).
func TestAllocateIdempotencyEnvScoped(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // project game (dev+prod seeded), dev node
	ctx := context.Background()

	devSrv := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	matchID := uuid.NewString()

	// Claim the dev server under match_id in env=dev.
	a, err := st.Allocate(ctx, "game", "dev", "eu", nil, matchID, 0)
	if err != nil || a.ServerID != devSrv {
		t.Fatalf("dev allocate: %+v %v (want %s)", a, err, devSrv)
	}

	// SAME match_id, env=prod: the fast-path must not return the dev server. prod
	// has no ready servers → the normal claim path lands on no_capacity (before the
	// env-scope fix findByMatch would have leaked the dev server here).
	if _, err := st.Allocate(ctx, "game", "prod", "eu", nil, matchID, 0); !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("cross-env reuse of match_id must miss the fast-path and hit no_capacity, got %v", err)
	}
}

// Full happy path of the matchmaker route: pending (RecordMatch) →
// match_start → running with started_at → players peak from heartbeats →
// match_end completed → finished; the one-shot dedik exits 0, its stopped
// report reaps the server and feeds nothing into crash-loop detection.
func TestMatchLifecycleRunningFinished(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	serverID := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	matchID := uuid.NewString()
	if _, err := st.Allocate(ctx, "game", "dev", "eu", nil, matchID, 2); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := st.RecordMatch(ctx, matchID, "game", "eu", serverID, f.VersionID, f.Env); err != nil {
		t.Fatalf("record match: %v", err)
	}
	m, err := st.GetMatch(ctx, matchID)
	if err != nil || m.State != "pending" || m.StartedAt != nil {
		t.Fatalf("fresh match: %+v, %v", m, err)
	}

	// match_start → running, started_at set.
	if err := st.ApplyServerEvent(ctx, f.NodeID, serverID, "match_start", matchID); err != nil {
		t.Fatal(err)
	}
	m, _ = st.GetMatch(ctx, matchID)
	if m.State != "running" || m.StartedAt == nil {
		t.Fatalf("after match_start: %+v", m)
	}
	started := *m.StartedAt

	// Heartbeats drive players_peak: 1 → 2 → 1 peaks at 2.
	for _, players := range []int32{1, 2, 1} {
		if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
			{ServerID: serverID, State: "allocated", Players: players, MatchID: matchID, Port: 20001},
		}); err != nil {
			t.Fatal(err)
		}
	}
	m, _ = st.GetMatch(ctx, matchID)
	if m.PlayersPeak != 2 {
		t.Fatalf("players_peak: want 2, got %d", m.PlayersPeak)
	}

	// Duplicate match_start (agent replay) must not reset started_at.
	if err := st.ApplyServerEvent(ctx, f.NodeID, serverID, "match_start", matchID); err != nil {
		t.Fatal(err)
	}
	if m, _ = st.GetMatch(ctx, matchID); !m.StartedAt.Equal(started) {
		t.Fatalf("duplicate match_start moved started_at: %v → %v", started, m.StartedAt)
	}

	// match_end completed → finished with ended_at; peak survives.
	if err := st.ApplyServerEvent(ctx, f.NodeID, serverID, "match_end", matchID+" completed"); err != nil {
		t.Fatal(err)
	}
	m, _ = st.GetMatch(ctx, matchID)
	if m.State != "finished" || m.EndedAt == nil || m.PlayersPeak != 2 {
		t.Fatalf("after match_end: %+v", m)
	}

	// The dedik exits 0 → agent tombstone reports stopped → reaped, NOT
	// failed: a normal end of a one-shot server.
	if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
		{ServerID: serverID, State: "stopped", MatchID: matchID, Port: 20001},
	}); err != nil {
		t.Fatal(err)
	}
	if sv, _ := st.GetServer(ctx, serverID); sv.State != "reaped" {
		t.Fatalf("clean exit: want reaped, got %s", sv.State)
	}
	// …and therefore no crash-loop input at all.
	if n, _ := st.CountEvents(ctx, store.EventServerFailed); n != 0 {
		t.Fatalf("clean cycle produced %d server_failed events", n)
	}
	failures, err := st.RecentFailedTimes(ctx, f.VersionID, "eu", 25*time.Minute)
	if err != nil || len(failures) != 0 {
		t.Fatalf("crash-loop input must be empty: %v, %v", failures, err)
	}
	// The match events landed in the audit feed with the match reference.
	for _, kind := range []string{"match_start", "match_end"} {
		if n, _ := st.CountEvents(ctx, kind); n < 1 {
			t.Fatalf("missing %s event", kind)
		}
	}
}

// REST /v1/allocate path: nobody wrote a matches row — match_start creates it
// on the fly; a liba `aborted` result lands as state aborted.
func TestMatchStartCreatesRowAndAbortedResult(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	serverID := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	matchID := uuid.NewString()
	if _, err := st.Allocate(ctx, "game", "dev", "eu", nil, matchID, 0); err != nil {
		t.Fatalf("allocate: %v", err)
	}

	if err := st.ApplyServerEvent(ctx, f.NodeID, serverID, "match_start", matchID); err != nil {
		t.Fatal(err)
	}
	m, err := st.GetMatch(ctx, matchID)
	if err != nil {
		t.Fatalf("match row must exist after match_start: %v", err)
	}
	if m.State != "running" || m.ServerID != serverID || m.Region != "eu" || m.VersionID != f.VersionID {
		t.Fatalf("upserted match: %+v", m)
	}

	if err := st.ApplyServerEvent(ctx, f.NodeID, serverID, "match_end", matchID+" aborted"); err != nil {
		t.Fatal(err)
	}
	if m, _ = st.GetMatch(ctx, matchID); m.State != "aborted" || m.EndedAt == nil {
		t.Fatalf("aborted result: %+v", m)
	}
}

// A dedik dying mid-match can never send match_end — the failure event closes
// the match as aborted.
func TestServerFailureAbortsMatch(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	serverID := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	matchID := uuid.NewString()
	if _, err := st.Allocate(ctx, "game", "dev", "eu", nil, matchID, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyServerEvent(ctx, f.NodeID, serverID, "match_start", matchID); err != nil {
		t.Fatal(err)
	}

	if err := st.ApplyServerEvent(ctx, f.NodeID, serverID, "failed", "exit 139"); err != nil {
		t.Fatal(err)
	}
	m, _ := st.GetMatch(ctx, matchID)
	if m.State != "aborted" || m.EndedAt == nil {
		t.Fatalf("crash mid-match: want aborted, got %+v", m)
	}
}

// node_lost mass-failures: excluded from the crash-loop input, and the match
// resurrects together with its server when the node comes back (Hello map).
func TestNodeLostFailuresExcludedAndMatchResurrects(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	// Three allocated servers with running matches on a node that goes dark.
	var serverIDs, matchIDs []string
	for i := 0; i < 3; i++ {
		id := f.InsertServer(t, f.NodeID, f.VersionID, "ready", int32(20001+i), 0)
		mid := uuid.NewString()
		if _, err := st.Allocate(ctx, "game", "dev", "eu", nil, mid, 2); err != nil {
			t.Fatal(err)
		}
		if err := st.ApplyServerEvent(ctx, f.NodeID, id, "match_start", mid); err != nil {
			t.Fatal(err)
		}
		serverIDs, matchIDs = append(serverIDs, id), append(matchIDs, mid)
	}

	f.SetHeartbeatAge(t, f.NodeID, 31*time.Second)
	if _, err := st.QuarantineStaleNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := st.FailQuarantinedServers(ctx); err != nil || n != 3 {
		t.Fatalf("fail quarantined: %d, %v", n, err)
	}

	// All matches aborted (unobservable), yet the crash-loop input is empty.
	for _, mid := range matchIDs {
		if m, _ := st.GetMatch(ctx, mid); m.State != "aborted" {
			t.Fatalf("match %s: want aborted on node loss, got %s", mid, m.State)
		}
	}
	if n, _ := st.CountEvents(ctx, store.EventServerFailed); n != 3 {
		t.Fatalf("want 3 server_failed events, got %d", n)
	}
	failures, err := st.RecentFailedTimes(ctx, f.VersionID, "eu", 25*time.Minute)
	if err != nil || len(failures) != 0 {
		t.Fatalf("node_lost failures must not feed crash-loop: %v, %v", failures, err)
	}

	// The node returns with the dediks alive: servers AND their matches are
	// back (server_recovered path).
	reports := make([]store.ServerReport, 0, 3)
	for i, id := range serverIDs {
		reports = append(reports, store.ServerReport{
			ServerID: id, State: "allocated", Players: 2, MatchID: matchIDs[i], Port: int32(20001 + i),
		})
	}
	if err := st.HelloSync(ctx, f.NodeID, "node-1", 10, "test", reports); err != nil {
		t.Fatal(err)
	}
	for i, mid := range matchIDs {
		m, _ := st.GetMatch(ctx, mid)
		if m.State != "running" || m.EndedAt != nil {
			t.Fatalf("match %s must resurrect with its server: %+v", mid, m)
		}
		if sv, _ := st.GetServer(ctx, serverIDs[i]); sv.State != "allocated" {
			t.Fatalf("server %s: want allocated, got %s", serverIDs[i], sv.State)
		}
	}

	// A real (non-node_lost) failure does count.
	f.MarkFailed(t, serverIDs[0], 0)
	failures, err = st.RecentFailedTimes(ctx, f.VersionID, "eu", 25*time.Minute)
	if err != nil || len(failures[f.NodeID]) != 1 {
		t.Fatalf("agent_report failure must count: %v, %v", failures, err)
	}
	// ListMatches sees the whole picture.
	all, err := st.ListMatches(ctx, store.MatchFilter{Project: "game"})
	if err != nil || len(all) != 3 {
		t.Fatalf("list matches: %d, %v", len(all), err)
	}
	running, err := st.ListMatches(ctx, store.MatchFilter{Project: "game", State: "running"})
	if err != nil || len(running) != 3 {
		t.Fatalf("running matches: %d, %v", len(running), err)
	}
}

// RecordMatch writes matches.env from its parameter, not the migration default
// 'dev' (W-I1): a prod allocation must not be recorded as a dev match. Passing
// env='prod' over a dev fixture server proves the value is the parameter's.
func TestRecordMatchWritesEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	serverID := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	matchID := uuid.NewString()
	if _, err := st.Allocate(ctx, "game", "dev", "eu", nil, matchID, 2); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := st.RecordMatch(ctx, matchID, "game", "eu", serverID, f.VersionID, "prod"); err != nil {
		t.Fatalf("record match: %v", err)
	}
	var env string
	if err := st.Pool.QueryRow(ctx,
		`select env from matches where id = $1::uuid`, matchID).Scan(&env); err != nil {
		t.Fatalf("read match env: %v", err)
	}
	if env != "prod" {
		t.Fatalf("matches.env: want prod (explicit param), got %q", env)
	}
}

// The on-the-fly matches row that match_start upserts for a REST-allocated
// server takes its env from the SERVER row (invariant I6), never the migration
// default: a prod server's match must record env='prod', not 'dev' (W-I1).
func TestMatchStartRecordsServerEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	// prod-половина: нода в prod, prod-версия, ready-сервер (env сервера = env ноды).
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatalf("move node to prod: %v", err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	serverID := f.InsertServer(t, prodNode, prodV, "ready", 20002, 0)

	matchID := uuid.NewString()
	if _, err := st.Allocate(ctx, "game", "prod", "eu", nil, matchID, 0); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	// REST-путь: строки matches ещё нет — её создаёт match_start (env из сервера).
	if err := st.ApplyServerEvent(ctx, prodNode, serverID, "match_start", matchID); err != nil {
		t.Fatal(err)
	}
	var env string
	if err := st.Pool.QueryRow(ctx,
		`select env from matches where id = $1::uuid`, matchID).Scan(&env); err != nil {
		t.Fatalf("read match env: %v", err)
	}
	if env != "prod" {
		t.Fatalf("matches.env from match_start: want prod (server row), got %q", env)
	}
}
