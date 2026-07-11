package store_test

// Iteration-5 follow-up: авто-dead длительно молчащих quarantine-нод + событие
// node_down (оператор/алерты отличают «моргнула» от «умерла»), плюс
// симметричное поднятие dead → active вернувшимся heartbeat'ом (touchNode).

import (
	"context"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestMarkDeadNodes: a quarantined node silent past node_dead_after_min is
// finalized dead with a node_down event (payload silent_for_s); the pass is
// idempotent (no duplicate event); a node still within the threshold stays
// quarantine; and a returning heartbeat lifts dead → active with node_recovered.
func TestMarkDeadNodes(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	quarantine := func(nodeID string, age time.Duration) {
		t.Helper()
		f.SetHeartbeatAge(t, nodeID, age)
		if _, err := st.Pool.Exec(ctx,
			`update nodes set state = 'quarantine' where id = $1::uuid`, nodeID); err != nil {
			t.Fatalf("quarantine %s: %v", nodeID, err)
		}
	}

	// Node A: quarantined and silent 15m > 10m threshold → dead.
	quarantine(f.NodeID, 15*time.Minute)
	// Node B: quarantined but only 30s silent → must stay quarantine.
	nodeB := f.AddNode(t, "node-2", "203.0.113.11", 10)
	quarantine(nodeB, 30*time.Second)

	n, err := st.MarkDeadNodes(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("mark dead: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 node marked dead, got %d", n)
	}
	if got := nodeState(t, st, f.NodeID); got != "dead" {
		t.Fatalf("node A: want dead, got %s", got)
	}
	if got := nodeState(t, st, nodeB); got != "quarantine" {
		t.Fatalf("node B (recent): want quarantine, got %s", got)
	}

	// node_down event carries the node ref and how long it was silent.
	down := lastEvent(t, st, store.EventNodeDown)
	if down.NodeID == nil || *down.NodeID != f.NodeID {
		t.Fatalf("node_down node_id = %v, want %s", down.NodeID, f.NodeID)
	}
	silent, ok := down.Payload["silent_for_s"].(float64)
	if !ok || silent < 900 {
		t.Fatalf("node_down silent_for_s = %v (ok=%v), want >= 900", down.Payload["silent_for_s"], ok)
	}

	// Idempotent: nothing new is dead, no duplicate node_down.
	if n, err := st.MarkDeadNodes(ctx, 10*time.Minute); err != nil || n != 0 {
		t.Fatalf("second pass: want 0, got %d (%v)", n, err)
	}
	if c, _ := st.CountEvents(ctx, store.EventNodeDown); c != 1 {
		t.Fatalf("want exactly 1 node_down event, got %d", c)
	}

	// A returning heartbeat on the dead node lifts it back to active and
	// records node_recovered (touchNode dead → active — same contract as the
	// quarantine → active recovery).
	if err := st.ApplyHeartbeat(ctx, f.NodeID, nil); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if got := nodeState(t, st, f.NodeID); got != "active" {
		t.Fatalf("after heartbeat: want active, got %s", got)
	}
	if c, _ := st.CountEvents(ctx, store.EventNodeRecovered); c != 1 {
		t.Fatalf("want 1 node_recovered event, got %d", c)
	}
}

func nodeState(t *testing.T, st *store.Store, nodeID string) string {
	t.Helper()
	var state string
	if err := st.Pool.QueryRow(context.Background(),
		`select state from nodes where id = $1::uuid`, nodeID).Scan(&state); err != nil {
		t.Fatalf("node state: %v", err)
	}
	return state
}

func lastEvent(t *testing.T, st *store.Store, kind string) store.Event {
	t.Helper()
	events, err := st.ListEvents(context.Background(), 200)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range events {
		if e.Kind == kind {
			return e
		}
	}
	t.Fatalf("no %s event found", kind)
	return store.Event{}
}
