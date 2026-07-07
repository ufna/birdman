package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Hello map reconciliation (protocol.md §1 Lease, «сверка карты серверов»):
// servers failed by the lease checker while the node was silent are
// resurrected when the returning agent reports them alive — same ids, no
// wasteful re-creation. Regular heartbeats must NOT resurrect (terminal
// guard against stale reports), and master-reaped servers stay reaped.
func TestHelloResurrectsLeaseFailedServers(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "dev", 10)

	ready := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	allocated := f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 20002, 0)
	matchID := uuid.NewString()
	if _, err := st.Pool.Exec(ctx,
		`update servers set match_id = $2::uuid where id = $1::uuid`, allocated, matchID); err != nil {
		t.Fatal(err)
	}
	reaped := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 20003, 0)

	// The node goes silent: lease checker fails its live servers.
	f.MarkFailed(t, ready, time.Second)
	f.MarkFailed(t, allocated, time.Second)

	// A regular heartbeat must not resurrect a failed server.
	if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
		{ServerID: ready, State: "ready", Port: 20001},
	}); err != nil {
		t.Fatal(err)
	}
	if sv, _ := st.GetServer(ctx, ready); sv.State != "failed" {
		t.Fatalf("heartbeat resurrected a failed server: %s", sv.State)
	}

	// Hello with the recovered map resurrects the live ones.
	if err := st.HelloSync(ctx, f.NodeID, "node-1", 10, "0.1.0", []store.ServerReport{
		{ServerID: ready, State: "ready", Port: 20001},
		{ServerID: allocated, State: "allocated", Port: 20002, MatchID: matchID},
		{ServerID: reaped, State: "ready", Port: 20003}, // master reaped it — must stick
	}); err != nil {
		t.Fatal(err)
	}
	if sv, _ := st.GetServer(ctx, ready); sv.State != "ready" {
		t.Fatalf("hello must resurrect failed→ready, got %s", sv.State)
	}
	sv, err := st.GetServer(ctx, allocated)
	if err != nil {
		t.Fatal(err)
	}
	if sv.State != "allocated" {
		t.Fatalf("hello must resurrect failed→allocated, got %s", sv.State)
	}
	if sv.MatchID == nil || *sv.MatchID != matchID {
		t.Fatalf("resurrected allocation lost its match: %v", sv.MatchID)
	}
	if sv, _ := st.GetServer(ctx, reaped); sv.State != "reaped" {
		t.Fatalf("reaped must never resurrect, got %s", sv.State)
	}

	events, err := st.ListEvents(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	recovered := 0
	for _, e := range events {
		if e.Kind == store.EventServerRecovered {
			recovered++
		}
	}
	if recovered != 2 {
		t.Fatalf("want 2 server_recovered events, got %d", recovered)
	}

	// The resurrected server is allocatable again.
	alloc, err := st.Allocate(ctx, f.Project, f.Region, nil, uuid.NewString())
	if err != nil {
		t.Fatalf("allocate after resurrection: %v", err)
	}
	if alloc.ServerID != ready {
		t.Fatalf("expected the resurrected ready server %s, got %s", ready, alloc.ServerID)
	}
}
