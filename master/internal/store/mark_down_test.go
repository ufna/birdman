package store_test

// Iteration-5 follow-up (ревизия): авто-down длительно молчащих quarantine-нод
// + событие node_down (оператор/алерты отличают «моргнула» от «лежит давно»),
// поднятие down → active heartbeat'ом живой сессии (touchNode), и сохранённая
// ревокация: dead (ручной терминал) heartbeat'ом НЕ поднимается.

import (
	"context"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestMarkDownNodes: a quarantined node silent past node_down_after_min goes
// down with a node_down event (payload silent_for_s); the pass is idempotent
// (no duplicate event); a node still within the threshold stays quarantine; a
// heartbeat of a live session lifts down → active with node_recovered; and a
// manually revoked node (state 'dead') is NOT lifted by a heartbeat — the
// revocation contract survives the auto-down feature.
func TestMarkDownNodes(t *testing.T) {
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

	// Node A: quarantined and silent 15m > 10m threshold → down.
	quarantine(f.NodeID, 15*time.Minute)
	// Node B: quarantined but only 30s silent → must stay quarantine.
	nodeB := f.AddNode(t, "node-2", "203.0.113.11", 10)
	quarantine(nodeB, 30*time.Second)

	n, err := st.MarkDownNodes(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("mark down: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 node marked down, got %d", n)
	}
	if got := nodeState(t, st, f.NodeID); got != "down" {
		t.Fatalf("node A: want down, got %s", got)
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

	// Idempotent: nothing newly down, no duplicate node_down.
	if n, err := st.MarkDownNodes(ctx, 10*time.Minute); err != nil || n != 0 {
		t.Fatalf("second pass: want 0, got %d (%v)", n, err)
	}
	if c, _ := st.CountEvents(ctx, store.EventNodeDown); c != 1 {
		t.Fatalf("want exactly 1 node_down event, got %d", c)
	}

	// A heartbeat of a live session lifts the down node back to active and
	// records node_recovered (touchNode down → active — same contract as the
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

	// Revocation stays revoked: a manually dead node is NOT resurrected by a
	// heartbeat (in production agentlink refuses it a session in every auth
	// mode; even a racing in-flight heartbeat must not lift the state) and no
	// node_recovered is emitted.
	if _, err := st.Pool.Exec(ctx,
		`update nodes set state = 'dead' where id = $1::uuid`, f.NodeID); err != nil {
		t.Fatalf("revoke node: %v", err)
	}
	if err := st.ApplyHeartbeat(ctx, f.NodeID, nil); err != nil {
		t.Fatalf("heartbeat on dead node: %v", err)
	}
	if got := nodeState(t, st, f.NodeID); got != "dead" {
		t.Fatalf("revoked node must stay dead after a heartbeat, got %s", got)
	}
	if c, _ := st.CountEvents(ctx, store.EventNodeRecovered); c != 1 {
		t.Fatalf("heartbeat on a dead node must not emit node_recovered, got %d", c)
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
	events, err := st.ListEvents(context.Background(), 200, "")
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
