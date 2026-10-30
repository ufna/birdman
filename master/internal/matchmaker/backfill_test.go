package matchmaker_test

import (
	"sync"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/testdb"
)

func backfillConfig() matchmaker.Config {
	return matchmaker.Config{Policies: []matchmaker.QueuePolicy{{Project: "game", Env: "dev", GameMode: "dev-walk", Backfill: true, MinimumSize: 1, PartialWait: time.Nanosecond}}}
}

func TestBackfillCumulativeRosterRestartAndWholeParties(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.SetProjectMatchSize(t.Context(), "game", 4); err != nil {
		t.Fatal(err)
	}
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20002, 0)
	rec := &testdb.CommandRecorder{}
	st.SetCommandSender(rec)
	mm := newMM(t, st, backfillConfig())
	submit := func(name string, n int) matchmaker.Ticket {
		p := partyParams(name, n)
		p.GameMode = "dev-walk"
		tk, err := mm.Submit(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	first := submit("first", 1)
	runOnce(t, mm)
	one := get(t, mm, first.ID)
	if one.Match == nil {
		t.Fatal("solo did not start")
	}
	// Restore a second scheduler from SQL; it must reuse the live match,
	// even with zero currently connected players (all seats are reserved).
	mm = newMM(t, st, backfillConfig())
	if err := mm.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := submit("second", 2)
	runOnce(t, mm)
	two := get(t, mm, second.ID)
	if two.Match == nil || two.Match.MatchID != one.Match.MatchID || two.Match.ServerID != one.Match.ServerID {
		t.Fatal("backfill allocated another server")
	}
	// A two-seat ticket cannot consume the single remaining seat.
	third := submit("third", 2)
	runOnce(t, mm)
	three := get(t, mm, third.ID)
	if three.Match == nil || three.Match.MatchID == one.Match.MatchID {
		t.Fatal("whole party capacity not respected")
	}
	fourth := submit("fourth", 1)
	runOnce(t, mm)
	if got := get(t, mm, fourth.ID); got.Match == nil || got.Match.MatchID != one.Match.MatchID {
		t.Fatal("oldest free battle not filled")
	}
	var seats int
	if err := st.Pool.QueryRow(t.Context(), `select reserved_size from matches where id=$1::uuid`, one.Match.MatchID).Scan(&seats); err != nil || seats != 4 {
		t.Fatalf("seats=%d %v", seats, err)
	}
	var outboxRows int
	if err := st.Pool.QueryRow(t.Context(), `select count(*) from allocation_outbox where match_id=$1::uuid`, one.Match.MatchID).Scan(&outboxRows); err != nil || outboxRows != 1 {
		t.Fatal("outbox must retain one cumulative snapshot")
	}
	last := rec.Allocates()[len(rec.Allocates())-1].Msg.GetAllocate()
	if last.GetMatchId() != one.Match.MatchID || last.GetPlayersExpected() != 4 || len(last.GetMetadata()) != 4 {
		t.Fatal("bad cumulative allocation")
	}
	for _, id := range []string{first.ID, second.ID, fourth.ID} {
		if last.GetMetadata()["ticket_"+id] == "" {
			t.Fatal("lost existing admission key")
		}
	}
	// Master restart/rearm resends the latest snapshot, not the initial one.
	if err := st.RearmAllocations(t.Context(), f.NodeID); err != nil {
		t.Fatal(err)
	}
	if err := st.DispatchAllocations(t.Context()); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, cmd := range rec.Allocates() {
		a := cmd.Msg.GetAllocate()
		if a.GetMatchId() == one.Match.MatchID && a.GetPlayersExpected() == 4 {
			found = true
		}
	}
	if !found {
		t.Fatal("latest roster not recoverable")
	}
}

func TestBackfillConcurrentSchedulersCannotOverfill(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.SetProjectMatchSize(t.Context(), "game", 4); err != nil {
		t.Fatal(err)
	}
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	st.SetCommandSender(&testdb.CommandRecorder{})
	mm := newMM(t, st, backfillConfig())
	first := partyParams("first", 1)
	first.GameMode = "dev-walk"
	if _, err := mm.Submit(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	runOnce(t, mm)
	for i := range 8 {
		p := partyParams(time.Duration(i).String(), 1)
		p.GameMode = "dev-walk"
		if _, err := mm.Submit(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			peer := newMM(t, st, backfillConfig())
			if err := peer.RunOnce(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var seats, matched int
	if err := st.Pool.QueryRow(t.Context(), `select (select reserved_size from matches limit 1),(select count(*) from matchmaking_tickets where status='matched')`).Scan(&seats, &matched); err != nil {
		t.Fatal(err)
	}
	if seats != 4 || matched != 4 {
		t.Fatalf("capacity seats=%d matched=%d", seats, matched)
	}
}

func TestBackfillRejectsDrainingAndDifferentMode(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.SetProjectMatchSize(t.Context(), "game", 4); err != nil {
		t.Fatal(err)
	}
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	st.SetCommandSender(&testdb.CommandRecorder{})
	mm := newMM(t, st, backfillConfig())
	p := partyParams("first", 1)
	p.GameMode = "dev-walk"
	first, err := mm.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, mm)
	server := get(t, mm, first.ID).Match.ServerID
	p = partyParams("other-mode", 1)
	p.GameMode = "vr"
	other, err := mm.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, mm)
	if get(t, mm, other.ID).Status != matchmaker.StatusQueued {
		t.Fatal("cross-mode backfill")
	}
	if _, err := st.Pool.Exec(t.Context(), `update servers set state='draining' where id=$1::uuid`, server); err != nil {
		t.Fatal(err)
	}
	p = partyParams("second", 1)
	p.GameMode = "dev-walk"
	second, err := mm.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, mm)
	if get(t, mm, second.ID).Status != matchmaker.StatusQueued {
		t.Fatal("draining server received new player")
	}
}

func TestBackfillNewRequestCannotReplaceReservedPlayer(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.SetProjectMatchSize(t.Context(), "game", 4); err != nil {
		t.Fatal(err)
	}
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	st.SetCommandSender(&testdb.CommandRecorder{})
	mm := newMM(t, st, backfillConfig())
	p := partyParams("reserved", 1)
	p.GameMode = "dev-walk"
	first, err := mm.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, mm)
	matchID := get(t, mm, first.ID).Match.MatchID
	// A new request, even after the caller has left, cannot overwrite the
	// original private key/reserved pawn. Only the application's reconnect
	// endpoint may reuse that seat.
	p = partyParams("reserved", 1)
	p.GameMode = "dev-walk"
	again, err := mm.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	p = partyParams("new-peer", 1)
	p.GameMode = "dev-walk"
	peer, err := mm.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, mm)
	if get(t, mm, again.ID).Status != matchmaker.StatusQueued {
		t.Fatal("new request replaced old member")
	}
	if got := get(t, mm, peer.ID); got.Match == nil || got.Match.MatchID != matchID {
		t.Fatal("overlapping ticket blocked an eligible peer")
	}
}
