package matchmaker_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestGameModesIsolateQueuesAndSurviveRestart(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.SetProjectMatchSize(t.Context(), "game", 2); err != nil {
		t.Fatal(err)
	}
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20002, 0)
	rec := &testdb.CommandRecorder{}
	st.SetCommandSender(rec)
	mm := newMM(t, st, matchmaker.Config{})
	ids := map[string][]string{}
	for _, mode := range []string{"vr", "dev-walk"} {
		p := partyParams(mode+"-first", 1)
		p.GameMode = mode
		tk, err := mm.Submit(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		ids[mode] = append(ids[mode], tk.ID)
		p.GameMode = "another-mode"
		if _, err = mm.Submit(t.Context(), p); !errors.Is(err, matchmaker.ErrRequestConflict) {
			t.Fatalf("changed mode retry: %v", err)
		}
	}
	runOnce(t, mm)
	for mode, tickets := range ids {
		got := get(t, mm, tickets[0])
		if got.Status != matchmaker.StatusQueued || got.GameMode != mode {
			t.Fatalf("mixed modes: %s %s", got.Status, got.GameMode)
		}
	}
	mm = newMM(t, st, matchmaker.Config{})
	if err := mm.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	for mode := range ids {
		p := partyParams(mode+"-second", 1)
		p.GameMode = mode
		tk, err := mm.Submit(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		ids[mode] = append(ids[mode], tk.ID)
	}
	runOnce(t, mm)
	seen := map[string]string{}
	for mode, tickets := range ids {
		for _, id := range tickets {
			got := get(t, mm, id)
			if got.Status != matchmaker.StatusMatched || got.Match.GameMode != mode || got.GameMode != mode {
				t.Fatalf("mode lost in assignment: %s %s", got.Status, got.GameMode)
			}
			if previous := seen[got.Match.MatchID]; previous != "" && previous != mode {
				t.Fatal("different modes share a match")
			}
			seen[got.Match.MatchID] = mode
		}
	}
	if len(seen) != 2 || len(rec.Allocates()) != 2 {
		t.Fatal("expected two isolated matches")
	}
	for _, cmd := range rec.Allocates() {
		a := cmd.Msg.GetAllocate()
		if a.GetMetadata()["game_mode"] != seen[a.GetMatchId()] {
			t.Fatal("server allocation lost game_mode")
		}
	}
}

func TestPartialStartPolicyIsScopedByGameMode(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.SetProjectMatchSize(t.Context(), "game", 4); err != nil {
		t.Fatal(err)
	}
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	st.SetCommandSender(&testdb.CommandRecorder{})
	mm := newMM(t, st, matchmaker.Config{Policies: []matchmaker.QueuePolicy{{Project: "game", Env: "dev", GameMode: "dev-walk", MinimumSize: 1, PartialWait: time.Nanosecond}}})
	ids := map[string]string{}
	for _, mode := range []string{"vr", "dev-walk"} {
		p := partyParams(mode, 1)
		p.GameMode = mode
		tk, err := mm.Submit(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		ids[mode] = tk.ID
	}
	runOnce(t, mm)
	if get(t, mm, ids["vr"]).Status != matchmaker.StatusQueued || get(t, mm, ids["dev-walk"]).Status != matchmaker.StatusMatched {
		t.Fatal("mode policy affected another mode")
	}
	p := partyParams("invalid", 1)
	for _, mode := range []string{"../../bad", "UPPER", "with space"} {
		p.GameMode = mode
		if _, err := mm.Submit(t.Context(), p); !errors.Is(err, matchmaker.ErrInvalid) {
			t.Fatalf("invalid mode accepted: %v", err)
		}
	}
}
