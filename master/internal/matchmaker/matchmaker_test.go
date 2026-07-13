package matchmaker_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

func newMM(t *testing.T, st *store.Store, cfg matchmaker.Config) *matchmaker.Matchmaker {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return matchmaker.New(st, metrics.New(st, log), cfg, log)
}

func regions(pairs ...any) []matchmaker.RegionPing {
	out := make([]matchmaker.RegionPing, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, matchmaker.RegionPing{
			Region: pairs[i].(string), RTTms: int32(pairs[i+1].(int)),
		})
	}
	return out
}

func submit(t *testing.T, mm *matchmaker.Matchmaker, player, version string, regs []matchmaker.RegionPing) matchmaker.Ticket {
	t.Helper()
	tk, err := mm.Submit(context.Background(), matchmaker.SubmitParams{
		PlayerID: player, ClientVersion: version, Regions: regs,
	})
	if err != nil {
		t.Fatalf("submit %s: %v", player, err)
	}
	return tk
}

func runOnce(t *testing.T, mm *matchmaker.Matchmaker) {
	t.Helper()
	if err := mm.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}
}

func get(t *testing.T, mm *matchmaker.Matchmaker, id string) matchmaker.Ticket {
	t.Helper()
	tk, ok := mm.Get(id)
	if !ok {
		t.Fatalf("ticket %s disappeared", id)
	}
	return tk
}

// Two tickets in one region form one match: both get the same host:port and
// match_id, the server is allocated exactly once (acceptance of iteration 2).
func TestTwoTicketsOneMatch(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	mm := newMM(t, st, matchmaker.Config{})

	t1 := submit(t, mm, "p1", "1.0.0", regions("eu", 10))
	// Same MAJOR.MINOR, different patch — must land in the same bucket.
	t2 := submit(t, mm, "p2", "1.0.9", regions("eu", 30))
	if t1.Status != matchmaker.StatusQueued || t2.Status != matchmaker.StatusQueued {
		t.Fatalf("want both queued, got %s / %s", t1.Status, t2.Status)
	}
	runOnce(t, mm)

	g1, g2 := get(t, mm, t1.ID), get(t, mm, t2.ID)
	if g1.Status != matchmaker.StatusMatched || g2.Status != matchmaker.StatusMatched {
		t.Fatalf("want both matched, got %s / %s", g1.Status, g2.Status)
	}
	if g1.Match.MatchID != g2.Match.MatchID {
		t.Fatalf("different matches: %s vs %s", g1.Match.MatchID, g2.Match.MatchID)
	}
	if g1.Match.Host != "203.0.113.10" || g1.Match.Port != 20001 ||
		g2.Match.Host != g1.Match.Host || g2.Match.Port != g1.Match.Port {
		t.Fatalf("bad endpoints: %+v vs %+v", g1.Match, g2.Match)
	}
	if g1.Match.JoinToken != "" {
		t.Fatalf("join token must be off by default, got %q", g1.Match.JoinToken)
	}

	ctx := context.Background()
	var allocated int
	if err := st.Pool.QueryRow(ctx,
		`select count(*) from servers where state = 'allocated'`).Scan(&allocated); err != nil {
		t.Fatal(err)
	}
	if allocated != 1 {
		t.Fatalf("want exactly 1 allocated server, got %d", allocated)
	}
	var matches int
	if err := st.Pool.QueryRow(ctx,
		`select count(*) from matches where id = $1::uuid`, g1.Match.MatchID).Scan(&matches); err != nil {
		t.Fatal(err)
	}
	if matches != 1 {
		t.Fatalf("matches row missing for %s", g1.Match.MatchID)
	}
}

// A formed match reaches the dedik: the matchmaker's allocation dispatches
// AllocateServer to the server's node with players_expected = group size
// (итерация 2 — liba gets `allocated{match_id, players_expected}`).
func TestMatchNotifiesAgentWithPlayersExpected(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	rec := &testdb.CommandRecorder{}
	st.SetCommandSender(rec)
	mm := newMM(t, st, matchmaker.Config{})

	t1 := submit(t, mm, "p1", "1.0.0", regions("eu", 10))
	submit(t, mm, "p2", "1.0.0", regions("eu", 20))
	runOnce(t, mm)

	g1 := get(t, mm, t1.ID)
	if g1.Status != matchmaker.StatusMatched {
		t.Fatalf("want matched, got %s", g1.Status)
	}
	allocs := rec.Allocates()
	if len(allocs) != 1 {
		t.Fatalf("want exactly 1 AllocateServer, got %d", len(allocs))
	}
	if allocs[0].NodeID != f.NodeID {
		t.Fatalf("command node: %s, want %s", allocs[0].NodeID, f.NodeID)
	}
	a := allocs[0].Msg.GetAllocate()
	if a.GetMatchId() != g1.Match.MatchID || a.GetPlayersExpected() != 2 {
		t.Fatalf("AllocateServer: %+v (match %s)", a, g1.Match.MatchID)
	}
	var serverID string
	if err := st.Pool.QueryRow(context.Background(),
		`select id::text from servers where match_id = $1::uuid`, g1.Match.MatchID).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if a.GetServerId() != serverID {
		t.Fatalf("AllocateServer server_id %s, want %s", a.GetServerId(), serverID)
	}
}

// A new ticket for the same player cancels the previous queued one.
func TestAntiDupPlayer(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	mm := newMM(t, st, matchmaker.Config{})

	t1 := submit(t, mm, "p1", "1.0.0", regions("eu", 10))
	t2 := submit(t, mm, "p1", "1.0.0", regions("eu", 10))

	// The old ticket is cancelled and its long-poll wakes immediately.
	started := time.Now()
	g1, ok := mm.Wait(context.Background(), t1.ID, 5*time.Second)
	if !ok || g1.Status != matchmaker.StatusCancelled {
		t.Fatalf("old ticket: want cancelled, got %v %s", ok, g1.Status)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("cancelled long-poll did not wake immediately")
	}
	if g2 := get(t, mm, t2.ID); g2.Status != matchmaker.StatusQueued {
		t.Fatalf("new ticket: want queued, got %s", g2.Status)
	}
}

func TestCancel(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	mm := newMM(t, st, matchmaker.Config{})

	t1 := submit(t, mm, "p1", "1.0.0", regions("eu", 10))
	c, ok := mm.Cancel(t1.ID)
	if !ok || c.Status != matchmaker.StatusCancelled {
		t.Fatalf("cancel: %v %s", ok, c.Status)
	}
	// Idempotent.
	c, ok = mm.Cancel(t1.ID)
	if !ok || c.Status != matchmaker.StatusCancelled {
		t.Fatalf("second cancel: %v %s", ok, c.Status)
	}
	if _, ok := mm.Cancel("00000000-0000-0000-0000-000000000000"); ok {
		t.Fatal("cancel of unknown ticket must report not found")
	}
	// A cancelled ticket does not match anymore.
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	submit(t, mm, "p2", "1.0.0", regions("eu", 10))
	runOnce(t, mm)
	if g := get(t, mm, t1.ID); g.Status != matchmaker.StatusCancelled {
		t.Fatalf("cancelled ticket resurrected: %s", g.Status)
	}
}

// Queued longer than TicketTTL → expired.
func TestTicketTTLExpired(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50) // fleet exists, but no ready servers → no match
	mm := newMM(t, st, matchmaker.Config{TicketTTL: 50 * time.Millisecond})

	t1 := submit(t, mm, "p1", "1.0.0", regions("eu", 10))
	runOnce(t, mm)
	if g := get(t, mm, t1.ID); g.Status != matchmaker.StatusQueued {
		t.Fatalf("want queued before TTL, got %s", g.Status)
	}
	time.Sleep(80 * time.Millisecond)
	runOnce(t, mm)
	if g := get(t, mm, t1.ID); g.Status != matchmaker.StatusExpired {
		t.Fatalf("want expired after TTL, got %s", g.Status)
	}
}

// Incompatible client version → update_required (ops.md §3, default
// MAJOR.MINOR rule): rejected at submit and when the active version changes
// under a queued ticket.
func TestUpdateRequired(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // version 1.0.0 (prod)
	f.UpsertFleet(t, 2, 50)
	mm := newMM(t, st, matchmaker.Config{})

	for _, v := range []string{"0.9.0", "1.1.0", "2.0.0"} {
		if tk := submit(t, mm, "p-"+v, v, regions("eu", 10)); tk.Status != matchmaker.StatusUpdateRequired {
			t.Fatalf("client %s: want update_required, got %s", v, tk.Status)
		}
	}
	// Compatible patch difference stays queued.
	t1 := submit(t, mm, "p1", "1.0.7", regions("eu", 10))
	if t1.Status != matchmaker.StatusQueued {
		t.Fatalf("1.0.7 vs 1.0.0: want queued, got %s", t1.Status)
	}

	// Active version swap to 2.0.0 → the queued 1.0.x ticket becomes stale.
	v2 := f.AddVersion(t, "2.0.0", "dev")
	if _, err := st.UpsertFleet(context.Background(), store.UpsertFleetParams{
		Project: f.Project, Env: f.Env, Region: f.Region, ActiveVersion: &v2,
	}); err != nil {
		t.Fatal(err)
	}
	runOnce(t, mm)
	if g := get(t, mm, t1.ID); g.Status != matchmaker.StatusUpdateRequired {
		t.Fatalf("after version swap: want update_required, got %s", g.Status)
	}
}

// Without any active version (fleet not set up yet) tickets keep waiting —
// no false update_required.
func TestNoActiveVersionKeepsQueued(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10) // no fleet config
	mm := newMM(t, st, matchmaker.Config{})

	t1 := submit(t, mm, "p1", "1.0.0", regions("eu", 10))
	runOnce(t, mm)
	if g := get(t, mm, t1.ID); g.Status != matchmaker.StatusQueued {
		t.Fatalf("want queued with no active versions, got %s", g.Status)
	}
}

// Region widening (master.md §4): players of two different regions match
// after widen_after_s on the region with the lowest median group rtt.
func TestWidenAcrossRegions(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	ctx := context.Background()

	usNode, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "game", Region: "us", Hostname: "node-us",
		PublicIP: "203.0.113.20", CapacitySlots: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.SetHeartbeatAge(t, usNode.ID, 0)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: f.Env, Region: "us", ActiveVersion: &f.VersionID,
	}); err != nil {
		t.Fatal(err)
	}
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	f.InsertServer(t, usNode.ID, f.VersionID, "ready", 21001, 0)

	widen := 250 * time.Millisecond
	mm := newMM(t, st, matchmaker.Config{WidenAfter: widen})

	t1 := submit(t, mm, "p1", "1.0.0", regions("eu", 10, "us", 80))
	t2 := submit(t, mm, "p2", "1.0.0", regions("us", 10, "eu", 80))

	// Before widening each player is eligible only in their best region.
	runOnce(t, mm)
	if g := get(t, mm, t1.ID); g.Status != matchmaker.StatusQueued {
		t.Fatalf("matched before widen_after_s: %s", g.Status)
	}

	time.Sleep(widen + 250*time.Millisecond)
	runOnce(t, mm)
	g1, g2 := get(t, mm, t1.ID), get(t, mm, t2.ID)
	if g1.Status != matchmaker.StatusMatched || g2.Status != matchmaker.StatusMatched {
		t.Fatalf("want both matched after widen, got %s / %s", g1.Status, g2.Status)
	}
	if g1.Match.MatchID != g2.Match.MatchID {
		t.Fatalf("different matches after widen")
	}
}

// no_capacity keeps tickets queued; they match as soon as a ready server
// appears (reconcile refills the pool in production).
func TestNoCapacityThenMatch(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50) // no ready servers yet
	mm := newMM(t, st, matchmaker.Config{})

	t1 := submit(t, mm, "p1", "1.0.0", regions("eu", 10))
	t2 := submit(t, mm, "p2", "1.0.0", regions("eu", 20))
	runOnce(t, mm)
	runOnce(t, mm) // retries do not drop or duplicate anything
	if g1, g2 := get(t, mm, t1.ID), get(t, mm, t2.ID); g1.Status != matchmaker.StatusQueued ||
		g2.Status != matchmaker.StatusQueued {
		t.Fatalf("want both queued on no_capacity, got %s / %s", g1.Status, g2.Status)
	}

	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	runOnce(t, mm)
	g1, g2 := get(t, mm, t1.ID), get(t, mm, t2.ID)
	if g1.Status != matchmaker.StatusMatched || g2.Status != matchmaker.StatusMatched {
		t.Fatalf("want both matched after capacity, got %s / %s", g1.Status, g2.Status)
	}
	if g1.Match.Port != 20001 || g2.Match.Port != 20001 {
		t.Fatalf("wrong server: %+v / %+v", g1.Match, g2.Match)
	}
}

// 50 players in parallel with match_size=2 → exactly 25 matches, every player
// in exactly one match, no server used twice.
func TestConcurrentMatching(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 100)
	f.UpsertFleet(t, 2, 100)
	for i := range 25 {
		f.InsertServer(t, f.NodeID, f.VersionID, "ready", int32(20001+i), 0)
	}
	mm := newMM(t, st, matchmaker.Config{Tick: 10 * time.Millisecond})
	go mm.Run(t.Context())

	const players = 50
	results := make(chan matchmaker.Ticket, players)
	errs := make(chan error, players)
	var wg sync.WaitGroup
	for i := range players {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tk, err := mm.Submit(context.Background(), matchmaker.SubmitParams{
				PlayerID:      fmt.Sprintf("p%02d", i),
				ClientVersion: "1.0.0", Regions: regions("eu", 10+i),
			})
			if err != nil {
				errs <- err
				return
			}
			got, ok := mm.Wait(context.Background(), tk.ID, 30*time.Second)
			if !ok {
				errs <- context.DeadlineExceeded
				return
			}
			results <- got
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("player failed: %v", err)
	}

	byMatch := map[string][]matchmaker.Ticket{}
	seenPlayers := map[string]bool{}
	n := 0
	for tk := range results {
		n++
		if tk.Status != matchmaker.StatusMatched {
			t.Fatalf("ticket %s: want matched, got %s", tk.ID, tk.Status)
		}
		if seenPlayers[tk.PlayerID] {
			t.Fatalf("player %s matched twice", tk.PlayerID)
		}
		seenPlayers[tk.PlayerID] = true
		byMatch[tk.Match.MatchID] = append(byMatch[tk.Match.MatchID], tk)
	}
	if n != players {
		t.Fatalf("want %d results, got %d", players, n)
	}
	if len(byMatch) != players/2 {
		t.Fatalf("want %d matches, got %d", players/2, len(byMatch))
	}
	seenPorts := map[int32]string{}
	for id, group := range byMatch {
		if len(group) != 2 {
			t.Fatalf("match %s has %d players", id, len(group))
		}
		if group[0].Match.Port != group[1].Match.Port || group[0].Match.Host != group[1].Match.Host {
			t.Fatalf("match %s: endpoints differ", id)
		}
		if prev, ok := seenPorts[group[0].Match.Port]; ok {
			t.Fatalf("server port %d reused by matches %s and %s", group[0].Match.Port, prev, id)
		}
		seenPorts[group[0].Match.Port] = id
	}
	var allocated int
	if err := st.Pool.QueryRow(context.Background(),
		`select count(*) from servers where state = 'allocated'`).Scan(&allocated); err != nil {
		t.Fatal(err)
	}
	if allocated != players/2 {
		t.Fatalf("want %d allocated servers, got %d", players/2, allocated)
	}
}

// Join tokens: issued to every participant when enabled; verifiable HMAC.
func TestJoinTokenIssued(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	secret := []byte("test-secret")
	mm := newMM(t, st, matchmaker.Config{
		JoinTokenEnabled: true, JoinTokenSecret: secret, JoinTokenTTL: time.Minute,
	})

	t1 := submit(t, mm, "p1", "1.0.0", regions("eu", 10))
	t2 := submit(t, mm, "p2", "1.0.0", regions("eu", 10))
	runOnce(t, mm)
	g1, g2 := get(t, mm, t1.ID), get(t, mm, t2.ID)
	if g1.Match == nil || g1.Match.JoinToken == "" || g2.Match == nil || g2.Match.JoinToken == "" {
		t.Fatalf("join tokens missing: %+v / %+v", g1.Match, g2.Match)
	}
	matchID, playerID, err := matchmaker.VerifyJoinToken(secret, g1.Match.JoinToken, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if matchID != g1.Match.MatchID || playerID != "p1" {
		t.Fatalf("claims: %s %s", matchID, playerID)
	}
}

// --- multi-version window (итерация 3, master.md §5 + ops.md §3) ---

// deprecateAndActivate flips v2 active through the real store path,
// leaving the previous active version deprecated (the window is open).
func deprecateAndActivate(t *testing.T, st *store.Store, versionID string) {
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

// In the window old clients (compat with deprecated only) match onto the
// deprecated version, new clients onto the active one; a client covered by
// the active version never lands on the deprecated one.
func TestWindowRoutesClientsByCompat(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // 1.0.0 active via fleet
	f.UpsertFleet(t, 2, 50)
	v2 := f.AddVersion(t, "1.1.0", "dev")
	deprecateAndActivate(t, st, v2) // active 1.1.0, deprecated 1.0.0

	oldSrv := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	newSrv := f.InsertServer(t, f.NodeID, v2, "ready", 20002, 0)
	mm := newMM(t, st, matchmaker.Config{})

	tOld1 := submit(t, mm, "old1", "1.0.3", regions("eu", 10))
	tOld2 := submit(t, mm, "old2", "1.0.5", regions("eu", 10))
	tNew1 := submit(t, mm, "new1", "1.1.0", regions("eu", 10))
	tNew2 := submit(t, mm, "new2", "1.1.2", regions("eu", 10))
	for _, tk := range []matchmaker.Ticket{tOld1, tOld2, tNew1, tNew2} {
		if tk.Status != matchmaker.StatusQueued {
			t.Fatalf("ticket %s: want queued, got %s", tk.PlayerID, tk.Status)
		}
	}
	runOnce(t, mm)

	gOld, gNew := get(t, mm, tOld1.ID), get(t, mm, tNew1.ID)
	if gOld.Status != matchmaker.StatusMatched || gNew.Status != matchmaker.StatusMatched {
		t.Fatalf("want both matched, got %s / %s", gOld.Status, gNew.Status)
	}
	if gOld.Match.Port != 20001 {
		t.Fatalf("old clients must land on the deprecated server 20001, got %d", gOld.Match.Port)
	}
	if gNew.Match.Port != 20002 {
		t.Fatalf("new clients must land on the active server 20002, got %d", gNew.Match.Port)
	}
	assertServerVersion(t, st, oldSrv, f.VersionID)
	assertServerVersion(t, st, newSrv, v2)
}

// update_required comes only when the client is compatible with NO live
// version: deprecated still counts until it is disabled.
func TestWindowUpdateRequiredOnlyWhenNoLiveVersion(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	v2 := f.AddVersion(t, "1.1.0", "dev")
	deprecateAndActivate(t, st, v2)
	mm := newMM(t, st, matchmaker.Config{})

	// Old client: compatible with the deprecated 1.0.0 → queued, not rejected.
	tOld := submit(t, mm, "old", "1.0.9", regions("eu", 10))
	if tOld.Status != matchmaker.StatusQueued {
		t.Fatalf("old client in window: want queued, got %s", tOld.Status)
	}
	// Ancient client: no live version at all → update_required at submit.
	if tk := submit(t, mm, "ancient", "0.5.0", regions("eu", 10)); tk.Status != matchmaker.StatusUpdateRequired {
		t.Fatalf("ancient client: want update_required, got %s", tk.Status)
	}

	// The window closes (deprecated → disabled): the queued old client gets
	// update_required on the next tick, new submits too.
	if _, err := st.Pool.Exec(context.Background(),
		`update versions set state = 'disabled' where id = $1::uuid`, f.VersionID); err != nil {
		t.Fatal(err)
	}
	runOnce(t, mm)
	if g := get(t, mm, tOld.ID); g.Status != matchmaker.StatusUpdateRequired {
		t.Fatalf("after window close: want update_required, got %s", g.Status)
	}
	if tk := submit(t, mm, "old2", "1.0.9", regions("eu", 10)); tk.Status != matchmaker.StatusUpdateRequired {
		t.Fatalf("old client after window close: want update_required, got %s", tk.Status)
	}
}

// compat.overrides (ops.md §3): the migration window lets old clients play
// on new servers; override buckets do not mix with plain ones.
func TestCompatOverridesRouteOldClients(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // 1.0.0
	f.UpsertFleet(t, 2, 50)
	v2 := f.AddVersion(t, "1.1.0", "dev")
	deprecateAndActivate(t, st, v2)
	// Only the new version has capacity: without the override old clients
	// would wait for a 1.0.0 server forever.
	f.InsertServer(t, f.NodeID, v2, "ready", 20002, 0)

	compat, err := matchmaker.NewCompat([]matchmaker.Override{
		{Client: "1.0.x", Servers: []string{"1.1.x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mm := newMM(t, st, matchmaker.Config{Compat: compat})

	// Two old clients: by the override they may play on 1.1.0.
	t1 := submit(t, mm, "p1", "1.0.2", regions("eu", 10))
	t2 := submit(t, mm, "p2", "1.0.7", regions("eu", 10))
	runOnce(t, mm)
	g1, g2 := get(t, mm, t1.ID), get(t, mm, t2.ID)
	if g1.Status != matchmaker.StatusMatched || g2.Status != matchmaker.StatusMatched {
		t.Fatalf("override clients: want matched, got %s / %s", g1.Status, g2.Status)
	}
	if g1.Match.Port != 20002 || g1.Match.MatchID != g2.Match.MatchID {
		t.Fatalf("override match: %+v / %+v", g1.Match, g2.Match)
	}

	// «deprecated не получает НОВЫХ матчей, если compat клиента покрывается
	// active»: the override makes the ACTIVE 1.1.0 compatible with the old
	// clients, so they land there even when deprecated 1.0.0 capacity exists
	// (the matchmaker takes the first compatible candidate, active first).
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	f.InsertServer(t, f.NodeID, v2, "ready", 20003, 0)
	t3 := submit(t, mm, "p3", "1.0.2", regions("eu", 10))
	t4 := submit(t, mm, "p4", "1.0.7", regions("eu", 10))
	runOnce(t, mm)
	g3, g4 := get(t, mm, t3.ID), get(t, mm, t4.ID)
	if g3.Status != matchmaker.StatusMatched || g3.Match.Port != 20003 || g4.Match.Port != 20003 {
		t.Fatalf("override must prefer the active version: %+v / %+v", g3.Match, g4.Match)
	}
}

// Clients whose override sets differ do not share a bucket: a 1.0.2 client
// under an exact-patch override must not group with a 1.0.7 one.
func TestCompatOverrideSplitsBuckets(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)

	compat, err := matchmaker.NewCompat([]matchmaker.Override{
		{Client: "1.0.2", Servers: []string{"2.0.x"}}, // exact patch
	})
	if err != nil {
		t.Fatal(err)
	}
	mm := newMM(t, st, matchmaker.Config{Compat: compat})

	t1 := submit(t, mm, "p1", "1.0.2", regions("eu", 10))
	t2 := submit(t, mm, "p2", "1.0.7", regions("eu", 10))
	runOnce(t, mm)
	g1, g2 := get(t, mm, t1.ID), get(t, mm, t2.ID)
	if g1.Status == matchmaker.StatusMatched || g2.Status == matchmaker.StatusMatched {
		t.Fatalf("split buckets must not form a match: %s / %s", g1.Status, g2.Status)
	}

	// Same override set → same bucket → they match.
	t3 := submit(t, mm, "p3", "1.0.7", regions("eu", 10))
	runOnce(t, mm)
	g2, g3 := get(t, mm, t2.ID), get(t, mm, t3.ID)
	if g2.Status != matchmaker.StatusMatched || g3.Status != matchmaker.StatusMatched ||
		g2.Match.MatchID != g3.Match.MatchID {
		t.Fatalf("same-bucket clients must match: %s / %s", g2.Status, g3.Status)
	}
}

// --- environments v1 §3 (env resolution / candidate isolation / anti-dup) ---

// seedProdEnv adds a prod node (moved from the default dev), a prod version, a
// prod fleet and one ready prod server in the fixture's region — the second env
// half of a two-env stand.
func seedProdEnv(t *testing.T, st *store.Store, f *testdb.Fixture, semver string, port int32) (nodeID, versionID, serverID string) {
	t.Helper()
	ctx := context.Background()
	nodeID = f.AddNode(t, "node-prod-"+semver, "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, nodeID, "prod"); err != nil {
		t.Fatalf("move node to prod: %v", err)
	}
	versionID = f.AddVersion(t, semver, "prod")
	buffer, maxServers := int32(2), int32(50)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: f.Project, Env: "prod", Region: f.Region, ActiveVersion: &versionID,
		BufferReady: &buffer, MaxServers: &maxServers,
	}); err != nil {
		t.Fatalf("prod fleet: %v", err)
	}
	serverID = f.InsertServer(t, nodeID, versionID, "ready", port, 0)
	return nodeID, versionID, serverID
}

// A dev ticket lands on the dev fleet's server and a prod ticket on the prod
// one: candidate sets are scoped to the ticket's env, and allocation claims a
// server of that env only (never the other env's ready server in the same
// region).
func TestEnvCandidateIsolation(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()
	if _, err := st.SetProjectMatchSize(ctx, "game", 1); err != nil { // single-player matches keep the test compact
		t.Fatal(err)
	}
	f.UpsertFleet(t, 2, 50)
	devSrv := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	// ОДИНАКОВЫЙ semver в обоих env (unique (project, env, semver) это разрешает,
	// T5-m3): компат-гейт больше НЕ разводит тикеты по версии — единственное, что
	// удерживает dev-тикет от prod-сервера, это env-скоупинг кандидатов. С разными
	// semver (было dev 1.0.0 / prod 2.0.0) регрессия скоупинга маскировалась бы
	// компатом, и тест проходил бы вхолостую.
	_, prodV, prodSrv := seedProdEnv(t, st, f, "1.0.0", 20002)

	mm := newMM(t, st, matchmaker.Config{})
	tDev, err := mm.Submit(ctx, matchmaker.SubmitParams{
		Env: "dev", PlayerID: "d1", ClientVersion: "1.0.0", Regions: regions("eu", 10),
	})
	if err != nil {
		t.Fatalf("dev submit: %v", err)
	}
	tProd, err := mm.Submit(ctx, matchmaker.SubmitParams{
		Env: "prod", PlayerID: "p1", ClientVersion: "1.0.0", Regions: regions("eu", 10),
	})
	if err != nil {
		t.Fatalf("prod submit: %v", err)
	}
	runOnce(t, mm)

	gDev, gProd := get(t, mm, tDev.ID), get(t, mm, tProd.ID)
	if gDev.Status != matchmaker.StatusMatched || gDev.Match.Port != 20001 {
		t.Fatalf("dev ticket must land on the dev server 20001: %s %+v", gDev.Status, gDev.Match)
	}
	if gProd.Status != matchmaker.StatusMatched || gProd.Match.Port != 20002 {
		t.Fatalf("prod ticket must land on the prod server 20002: %s %+v", gProd.Status, gProd.Match)
	}
	assertServerVersion(t, st, devSrv, f.VersionID)
	assertServerVersion(t, st, prodSrv, prodV)
}

// Anti-dup is keyed by (project, env, player): the same player may hold one
// ticket per env at once, and a re-submit cancels only the same-env predecessor
// (environments v1 §3, M6).
func TestAntiDupIsPerEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50) // dev fleet → dev tickets are compatible and stay queued
	ctx := context.Background()
	mm := newMM(t, st, matchmaker.Config{})

	tDev, err := mm.Submit(ctx, matchmaker.SubmitParams{
		Env: "dev", PlayerID: "p1", ClientVersion: "1.0.0", Regions: regions("eu", 10),
	})
	if err != nil {
		t.Fatalf("dev submit: %v", err)
	}
	tProd, err := mm.Submit(ctx, matchmaker.SubmitParams{
		Env: "prod", PlayerID: "p1", ClientVersion: "1.0.0", Regions: regions("eu", 10),
	})
	if err != nil {
		t.Fatalf("prod submit: %v", err)
	}
	if g := get(t, mm, tDev.ID); g.Status != matchmaker.StatusQueued {
		t.Fatalf("dev ticket: want queued, got %s", g.Status)
	}
	if g := get(t, mm, tProd.ID); g.Status != matchmaker.StatusQueued {
		t.Fatalf("prod ticket of the same player must coexist: got %s", g.Status)
	}

	// Re-submit in dev cancels only the dev predecessor; prod survives.
	tDev2, err := mm.Submit(ctx, matchmaker.SubmitParams{
		Env: "dev", PlayerID: "p1", ClientVersion: "1.0.0", Regions: regions("eu", 10),
	})
	if err != nil {
		t.Fatalf("dev re-submit: %v", err)
	}
	if g := get(t, mm, tDev.ID); g.Status != matchmaker.StatusCancelled {
		t.Fatalf("old dev ticket must cancel: got %s", g.Status)
	}
	if g := get(t, mm, tProd.ID); g.Status != matchmaker.StatusQueued {
		t.Fatalf("prod ticket must survive the dev re-submit: got %s", g.Status)
	}
	if g := get(t, mm, tDev2.ID); g.Status != matchmaker.StatusQueued {
		t.Fatalf("new dev ticket: want queued, got %s", g.Status)
	}
}

// With no explicit env and several environments carrying active nodes the sole-
// env fallback is ambiguous → the ticket is rejected (env is required).
func TestEnvRequiredWhenAmbiguous(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev node, active + fresh
	ctx := context.Background()
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil { // now two envs have active nodes
		t.Fatal(err)
	}

	mm := newMM(t, st, matchmaker.Config{})
	if _, err := mm.Submit(ctx, matchmaker.SubmitParams{
		PlayerID: "p1", ClientVersion: "1.0.0", Regions: regions("eu", 10),
	}); !errors.Is(err, matchmaker.ErrInvalid) {
		t.Fatalf("ambiguous env must be rejected as invalid, got %v", err)
	}
}

func assertServerVersion(t *testing.T, st *store.Store, serverID, versionID string) {
	t.Helper()
	sv, err := st.GetServer(context.Background(), serverID)
	if err != nil {
		t.Fatal(err)
	}
	if sv.State != "allocated" {
		t.Fatalf("server %s: want allocated, got %s", serverID, sv.State)
	}
	if sv.VersionID != versionID {
		t.Fatalf("server %s: version %s, want %s", serverID, sv.VersionID, versionID)
	}
}

// --- pure unit tests (no database) ---

func TestCompatDefaultAndOverrides(t *testing.T) {
	plain := &matchmaker.Compat{} // default MAJOR.MINOR rule only
	if !plain.Compatible("1.4.2", "1.4.9") || plain.Compatible("1.4.2", "1.5.0") {
		t.Fatal("default rule broken")
	}

	c, err := matchmaker.NewCompat([]matchmaker.Override{
		{Client: "1.4.x", Servers: []string{"1.4.x", "1.5.x"}},
		{Client: "2.0.1", Servers: []string{"2.1.0"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		client, server string
		want           bool
	}{
		{"1.4.2", "1.4.9", true},  // default rule
		{"1.4.2", "1.5.3", true},  // override window
		{"1.4.2", "1.6.0", false}, // outside the override
		{"1.3.0", "1.5.0", false}, // client not in override
		{"2.0.1", "2.1.0", true},  // exact-patch override
		{"2.0.2", "2.1.0", false}, // different patch
		{"2.0.1", "2.1.1", false}, // server pattern is exact too
		{"1.4.2-rc1", "1.5.0+b7", true}, // suffixes ignored
		{"bogus", "1.4.2", false},
		{"1.4.2", "bogus", false},
	}
	for _, tc := range cases {
		if got := c.Compatible(tc.client, tc.server); got != tc.want {
			t.Fatalf("Compatible(%q, %q) = %v, want %v", tc.client, tc.server, got, tc.want)
		}
	}

	// Buckets: override membership is part of the bucket key.
	b1, err := c.BucketOf("1.4.2")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := c.BucketOf("1.4.9")
	b3, _ := c.BucketOf("1.3.0")
	b4, _ := plain.BucketOf("1.4.2")
	if b1 != b2 {
		t.Fatalf("same override set must share a bucket: %q vs %q", b1, b2)
	}
	if b1 == b3 {
		t.Fatalf("different major.minor must split buckets: %q", b1)
	}
	if b1 == b4 {
		t.Fatalf("override membership must be part of the bucket: %q", b1)
	}

	// Validation errors.
	for _, bad := range [][]matchmaker.Override{
		{{Client: "x.4", Servers: []string{"1.4"}}},
		{{Client: "1.4", Servers: nil}},
		{{Client: "1.4", Servers: []string{"1.x.2"}}},
		{{Client: "", Servers: []string{"1.4"}}},
	} {
		if _, err := matchmaker.NewCompat(bad); err == nil {
			t.Fatalf("NewCompat(%+v) must fail", bad)
		}
	}
}

func TestMajorMinor(t *testing.T) {
	ok := map[string]string{
		"1.0.0": "1.0", "1.4.2": "1.4", "v2.10.3": "2.10",
		"1.4": "1.4", "1.4-rc1": "1.4", "10.0.1-beta+build5": "10.0",
	}
	for in, want := range ok {
		got, err := matchmaker.MajorMinor(in)
		if err != nil || got != want {
			t.Fatalf("MajorMinor(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "1", "x.y.z", "1.x", "-1.0"} {
		if got, err := matchmaker.MajorMinor(in); err == nil {
			t.Fatalf("MajorMinor(%q) = %q, want error", in, got)
		}
	}
}

func TestJoinTokenVerify(t *testing.T) {
	secret := []byte("s3cret")
	exp := time.Now().Add(time.Minute)
	tok := matchmaker.GenerateJoinToken(secret, "m-1", "p-1", exp)

	m, p, err := matchmaker.VerifyJoinToken(secret, tok, time.Now())
	if err != nil || m != "m-1" || p != "p-1" {
		t.Fatalf("roundtrip: %s %s %v", m, p, err)
	}
	if _, _, err := matchmaker.VerifyJoinToken([]byte("other"), tok, time.Now()); err == nil {
		t.Fatal("wrong secret must fail")
	}
	if _, _, err := matchmaker.VerifyJoinToken(secret, tok+"x", time.Now()); err == nil {
		t.Fatal("tampered token must fail")
	}
	if _, _, err := matchmaker.VerifyJoinToken(secret, tok, exp.Add(time.Second)); err == nil {
		t.Fatal("expired token must fail")
	}
	if _, _, err := matchmaker.VerifyJoinToken(secret, strings.ReplaceAll(tok, ".", "_"), time.Now()); err == nil {
		t.Fatal("malformed token must fail")
	}
}
