package store_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

func TestNodeTokenAuth(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)

	node, err := st.AuthNodeToken(ctx, f.NodeToken)
	if err != nil {
		t.Fatalf("auth with valid token: %v", err)
	}
	if node.ID != f.NodeID || node.Region != "eu" {
		t.Fatalf("unexpected node: %+v", node)
	}
	if _, err := st.AuthNodeToken(ctx, f.NodeToken+"x"); !errors.Is(err, store.ErrBadToken) {
		t.Fatalf("tampered token: want ErrBadToken, got %v", err)
	}
	if _, err := st.AuthNodeToken(ctx, "bnt_"+uuid.NewString()+".deadbeef"); !errors.Is(err, store.ErrBadToken) {
		t.Fatalf("unknown node: want ErrBadToken, got %v", err)
	}

	nodes, err := st.ListNodes(ctx, store.NodeFilter{})
	if err != nil || len(nodes) != 1 {
		t.Fatalf("list nodes: %v, %d", err, len(nodes))
	}
}

func TestAPIKeys(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	n, err := st.CountActiveAPIKeys(ctx)
	if err != nil || n != 0 {
		t.Fatalf("expected empty api_keys, got %d (%v)", n, err)
	}
	k, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ci", Scopes: []string{"deploy", "readonly"}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	got, err := st.AuthAPIKey(ctx, secret)
	if err != nil {
		t.Fatalf("auth key: %v", err)
	}
	if got.ID != k.ID || len(got.Scopes) != 2 {
		t.Fatalf("unexpected key: %+v", got)
	}
	if _, err := st.AuthAPIKey(ctx, secret+"x"); !errors.Is(err, store.ErrBadAPIKey) {
		t.Fatalf("tampered key: want ErrBadAPIKey, got %v", err)
	}
}

// TestAPIKeyPurge covers PurgeAPIKey's tri-state result (registries v1
// design §6, docs/superpowers/specs/2026-07-09-registries-design.md): an
// active key refuses (notRevoked=true, nothing deleted); a revoked key is
// hard-deleted (purged=true); an unknown or already-purged id reports both
// false with no error — retrying purge is never a destructive escalation.
func TestAPIKeyPurge(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	active, _, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "active", Scopes: []string{"readonly"}})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	revoked, _, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "revoked", Scopes: []string{"readonly"}})
	if err != nil {
		t.Fatalf("create revoked: %v", err)
	}
	if _, _, err := st.RevokeAPIKey(ctx, revoked.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Active key: purge refuses, nothing deleted.
	if k, purged, notRevoked, err := st.PurgeAPIKey(ctx, active.ID); err != nil || purged || !notRevoked {
		t.Fatalf("purge active: k=%+v purged=%v notRevoked=%v err=%v", k, purged, notRevoked, err)
	}
	keys, err := st.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAPIKey(keys, active.ID) {
		t.Fatalf("purge must not delete an active key")
	}

	// Revoked key: purge deletes the row and returns its last known fields.
	k, purged, notRevoked, err := st.PurgeAPIKey(ctx, revoked.ID)
	if err != nil || !purged || notRevoked {
		t.Fatalf("purge revoked: k=%+v purged=%v notRevoked=%v err=%v", k, purged, notRevoked, err)
	}
	if k.Name != "revoked" {
		t.Fatalf("purge returned wrong key: %+v", k)
	}
	keys, err = st.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hasAPIKey(keys, revoked.ID) {
		t.Fatalf("purged key still present in ListAPIKeys")
	}

	// Retry on the now-gone id: both false, no error — not a destructive
	// escalation on a repeated ?purge=true.
	if k, purged, notRevoked, err := st.PurgeAPIKey(ctx, revoked.ID); err != nil || purged || notRevoked {
		t.Fatalf("retry purge: k=%+v purged=%v notRevoked=%v err=%v", k, purged, notRevoked, err)
	}

	// Unknown id: same as retry — both false, no error.
	if k, purged, notRevoked, err := st.PurgeAPIKey(ctx, uuid.NewString()); err != nil || purged || notRevoked {
		t.Fatalf("purge unknown: k=%+v purged=%v notRevoked=%v err=%v", k, purged, notRevoked, err)
	}
}

func hasAPIKey(keys []store.APIKey, id string) bool {
	for _, k := range keys {
		if k.ID == id {
			return true
		}
	}
	return false
}

// 100 goroutines race for a single ready server: exactly one allocation
// succeeds, the rest get no_capacity (docs/specs/master.md §3).
func TestAllocateConcurrentSingleServer(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 100)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)

	const n = 100
	var ok, noCap, other int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.Allocate(context.Background(), "game", "dev", "eu", nil, uuid.NewString(), 0, nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, store.ErrNoCapacity):
				noCap++
			default:
				other++
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || noCap != n-1 || other != 0 {
		t.Fatalf("want 1 success / %d no_capacity, got %d / %d / %d other", n-1, ok, noCap, other)
	}
}

// 100 goroutines × 100 ready servers → 100 allocations of 100 distinct
// servers.
func TestAllocateConcurrentHundredServers(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 200)
	for i := 0; i < 100; i++ {
		f.InsertServer(t, f.NodeID, f.VersionID, "ready", int32(20000+i), 0)
	}

	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := st.Allocate(context.Background(), "game", "dev", "eu", nil, uuid.NewString(), 0, nil)
			if err != nil {
				t.Errorf("allocate: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[a.ServerID] {
				t.Errorf("server %s allocated twice", a.ServerID)
			}
			seen[a.ServerID] = true
		}()
	}
	wg.Wait()
	if len(seen) != 100 {
		t.Fatalf("want 100 distinct servers, got %d", len(seen))
	}
}

// Repeated and concurrent requests with the same match_id return the same
// server (idempotency backed by the partial unique index).
func TestAllocateIdempotentByMatchID(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 200)
	for i := 0; i < 100; i++ {
		f.InsertServer(t, f.NodeID, f.VersionID, "ready", int32(21000+i), 0)
	}
	ctx := context.Background()

	matchID := uuid.NewString()
	first, err := st.Allocate(ctx, "game", "dev", "eu", nil, matchID, 0, nil)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	again, err := st.Allocate(ctx, "game", "dev", "eu", nil, matchID, 0, nil)
	if err != nil || again.ServerID != first.ServerID {
		t.Fatalf("sequential repeat: want %s, got %+v (%v)", first.ServerID, again, err)
	}

	// Concurrent burst with one fresh match_id: all callers must converge on
	// one server.
	matchID2 := uuid.NewString()
	var mu sync.Mutex
	got := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := st.Allocate(context.Background(), "game", "dev", "eu", nil, matchID2, 0, nil)
			if err != nil {
				t.Errorf("concurrent idempotent allocate: %v", err)
				return
			}
			mu.Lock()
			got[a.ServerID] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(got) != 1 {
		t.Fatalf("same match_id resolved to %d servers: %v", len(got), got)
	}

	var allocated int
	if err := st.Pool.QueryRow(ctx,
		`select count(*) from servers where state = 'allocated'`).Scan(&allocated); err != nil {
		t.Fatal(err)
	}
	if allocated != 2 {
		t.Fatalf("want exactly 2 allocated servers, got %d", allocated)
	}
}

func TestAllocateFilters(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 100)
	ctx := context.Background()

	// No ready servers at all.
	if _, err := st.Allocate(ctx, "game", "dev", "eu", nil, uuid.NewString(), 0, nil); !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("empty pool: want no_capacity, got %v", err)
	}
	// Unknown project.
	if _, err := st.Allocate(ctx, "nope", "dev", "eu", nil, uuid.NewString(), 0, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown project: want not_found, got %v", err)
	}

	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)

	// Wrong region.
	if _, err := st.Allocate(ctx, "game", "dev", "us", nil, uuid.NewString(), 0, nil); !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("wrong region: want no_capacity, got %v", err)
	}
	// Version filter mismatch.
	v2 := f.AddVersion(t, "2.0.0", "dev")
	if _, err := st.Allocate(ctx, "game", "dev", "eu", &v2, uuid.NewString(), 0, nil); !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("version mismatch: want no_capacity, got %v", err)
	}
	// Stale heartbeat excludes the node.
	f.SetHeartbeatAge(t, f.NodeID, 11*time.Second)
	if _, err := st.Allocate(ctx, "game", "dev", "eu", nil, uuid.NewString(), 0, nil); !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("stale heartbeat: want no_capacity, got %v", err)
	}
	// Fresh again → allocable, and version filter matches.
	f.SetHeartbeatAge(t, f.NodeID, 0)
	a, err := st.Allocate(ctx, "game", "dev", "eu", &f.VersionID, uuid.NewString(), 0, nil)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if a.Host != "203.0.113.10" || a.Port != 20001 {
		t.Fatalf("unexpected endpoint: %+v", a)
	}
}

func TestHeartbeatTransitions(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	id := f.InsertServer(t, f.NodeID, f.VersionID, "creating", 0, 0)

	// pulling → still creating, stats updated.
	if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
		{ServerID: id, State: "pulling"},
	}); err != nil {
		t.Fatal(err)
	}
	if sv, _ := st.GetServer(ctx, id); sv.State != "creating" {
		t.Fatalf("pulling: want creating, got %s", sv.State)
	}

	// ready with port → ready, port recorded.
	if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
		{ServerID: id, State: "ready", Port: 23456},
	}); err != nil {
		t.Fatal(err)
	}
	sv, _ := st.GetServer(ctx, id)
	if sv.State != "ready" || sv.Port != 23456 {
		t.Fatalf("ready: got %s port %d", sv.State, sv.Port)
	}

	// Allocation wins over a stale 'ready' report (no downgrade).
	if _, err := st.Allocate(ctx, "game", "dev", "eu", nil, uuid.NewString(), 0, nil); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
		{ServerID: id, State: "ready", Port: 23456, Players: 3},
	}); err != nil {
		t.Fatal(err)
	}
	sv, _ = st.GetServer(ctx, id)
	if sv.State != "allocated" || sv.Players != 3 {
		t.Fatalf("no-downgrade: got %s players %d", sv.State, sv.Players)
	}

	// failed report → failed + server_failed event.
	if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
		{ServerID: id, State: "failed"},
	}); err != nil {
		t.Fatal(err)
	}
	if sv, _ = st.GetServer(ctx, id); sv.State != "failed" {
		t.Fatalf("failed: got %s", sv.State)
	}
	if n, _ := st.CountEvents(ctx, store.EventServerFailed); n != 1 {
		t.Fatalf("want 1 server_failed event, got %d", n)
	}

	// Draining server missing from the report → reaped.
	id2 := f.InsertServer(t, f.NodeID, f.VersionID, "draining", 20002, 0)
	if err := st.ApplyHeartbeat(ctx, f.NodeID, nil); err != nil {
		t.Fatal(err)
	}
	if sv, _ := st.GetServer(ctx, id2); sv.State != "reaped" {
		t.Fatalf("absent draining: want reaped, got %s", sv.State)
	}

	// Foreign server id in a report is ignored.
	other := f.AddNode(t, "node-2", "203.0.113.11", 10)
	id3 := f.InsertServer(t, other, f.VersionID, "creating", 0, 0)
	if err := st.ApplyHeartbeat(ctx, f.NodeID, []store.ServerReport{
		{ServerID: id3, State: "failed"},
	}); err != nil {
		t.Fatal(err)
	}
	if sv, _ := st.GetServer(ctx, id3); sv.State != "creating" {
		t.Fatalf("foreign report must be ignored, got %s", sv.State)
	}
}

func TestApplyServerEvent(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	id := f.InsertServer(t, f.NodeID, f.VersionID, "creating", 0, 0)
	if err := st.ApplyServerEvent(ctx, f.NodeID, id, "ready", ""); err != nil {
		t.Fatal(err)
	}
	if sv, _ := st.GetServer(ctx, id); sv.State != "ready" {
		t.Fatalf("ready event: got %s", sv.State)
	}
	if err := st.ApplyServerEvent(ctx, f.NodeID, id, "oom", "killed"); err != nil {
		t.Fatal(err)
	}
	if sv, _ := st.GetServer(ctx, id); sv.State != "failed" {
		t.Fatalf("oom event: got %s", sv.State)
	}
	if n, _ := st.CountEvents(ctx, store.EventServerFailed); n != 1 {
		t.Fatalf("want 1 server_failed event, got %d", n)
	}
}

// khl-3jb.5: the external matchmaker's per-match payload rides the claim into
// AllocateServer verbatim — the agent forwards it as allocated.metadata and
// the replay carries it, so this is the seam that must not drop it.
func TestAllocateCarriesMetadata(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	rec := &testdb.CommandRecorder{}
	st.SetCommandSender(rec)
	ctx := context.Background()

	md := map[string]string{"join_secret": "cafe", "mode": "ranked"}
	if _, err := st.Allocate(ctx, "game", "dev", "eu", nil, uuid.NewString(), 0, md); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	allocs := rec.Allocates()
	if len(allocs) != 1 {
		t.Fatalf("want 1 AllocateServer, got %d", len(allocs))
	}
	got := allocs[0].Msg.GetAllocate().GetMetadata()
	if got["join_secret"] != "cafe" || got["mode"] != "ranked" {
		t.Fatalf("metadata did not ride through: %v", got)
	}
}
