package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/stats"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func rts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func rtp(s string) *time.Time { t := rts(s); return &t }

func dk(t time.Time) string { return t.UTC().Format("2006-01-02") }

// insertRollupMatch inserts a matches row with explicit absolute
// started_at/ended_at (unlike httpapi's "ago minutes" helper, the rollup
// tests need exact control of which UTC day a match falls on).
func insertRollupMatch(t *testing.T, st *store.Store, serverID, region string, playersPeak int, startedAt time.Time, endedAt *time.Time) {
	t.Helper()
	state := "finished"
	if endedAt == nil {
		state = "running"
	}
	_, err := st.Pool.Exec(context.Background(), `
		insert into matches (project_id, server_id, version_id, region, state, players_peak, created_at, started_at, ended_at)
		select s.project_id, s.id, s.version_id, $2, $3, $4, $5, $5, $6
		from servers s where s.id = $1::uuid`,
		serverID, region, state, playersPeak, startedAt, endedAt)
	if err != nil {
		t.Fatalf("insert match: %v", err)
	}
}

// TestRollupStore covers the store-level CRUD surface for the rollup tables
// (match_stats_daily/match_ccu_daily): recompute+persist two days via
// AggregateDaily -> UpsertRollupDay, read them back via RollupDims/
// RollupPeakCCU, verify re-upserting a day doesn't duplicate rows, verify an
// empty day still marks itself processed (RollupPeakCCU reports it present,
// via the map's ok-return, with peak 0 -- match_ccu_daily's row is always
// inserted), and verify StatMatchesOverlapping's window semantics (incl. a
// still-running match that started before the window).
func TestRollupStore(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()
	srv := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 20001, 0)

	day1 := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	now := day2.Add(15 * time.Hour) // well after both matches below finish

	// day1: one eu match, 10:00-10:30 (30 min), peak 10.
	insertRollupMatch(t, st, srv, "eu", 10, rts("2026-07-07T10:00:00Z"), rtp("2026-07-07T10:30:00Z"))
	// day2: one us match, 09:00-09:20 (20 min), peak 4.
	insertRollupMatch(t, st, srv, "us", 4, rts("2026-07-08T09:00:00Z"), rtp("2026-07-08T09:20:00Z"))

	// Recompute + persist day1.
	m1, err := st.StatMatchesOverlapping(ctx, day1, day2)
	if err != nil {
		t.Fatalf("overlap day1: %v", err)
	}
	if len(m1) != 1 {
		t.Fatalf("day1 overlap: want 1 match, got %d: %+v", len(m1), m1)
	}
	dims1, peak1 := stats.AggregateDaily(m1, []time.Time{day1}, now)
	if err := st.UpsertRollupDay(ctx, day1, dims1, peak1[dk(day1)]); err != nil {
		t.Fatalf("upsert day1: %v", err)
	}

	// Recompute + persist day2.
	m2, err := st.StatMatchesOverlapping(ctx, day2, day2.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("overlap day2: %v", err)
	}
	if len(m2) != 1 {
		t.Fatalf("day2 overlap: want 1 match, got %d: %+v", len(m2), m2)
	}
	dims2, peak2 := stats.AggregateDaily(m2, []time.Time{day2}, now)
	if err := st.UpsertRollupDay(ctx, day2, dims2, peak2[dk(day2)]); err != nil {
		t.Fatalf("upsert day2: %v", err)
	}

	// RollupDims returns exactly what was written.
	got, err := st.RollupDims(ctx, day1, day2)
	if err != nil {
		t.Fatalf("rollup dims: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 dim rows, got %d: %+v", len(got), got)
	}
	var gotEU, gotUS bool
	for _, d := range got {
		switch d.Region {
		case "eu":
			gotEU = true
			if !d.Day.Equal(day1) || d.Semver != "1.0.0" || d.Matches != 1 ||
				d.PlayersPeakSum != 10 || d.DurSumSeconds != 1800 || d.DurCount != 1 || d.SlotSeconds != 1800 {
				t.Fatalf("eu dim: %+v", d)
			}
		case "us":
			gotUS = true
			if !d.Day.Equal(day2) || d.Matches != 1 || d.PlayersPeakSum != 4 ||
				d.DurSumSeconds != 1200 || d.DurCount != 1 || d.SlotSeconds != 1200 {
				t.Fatalf("us dim: %+v", d)
			}
		default:
			t.Fatalf("unexpected region in dims: %+v", d)
		}
	}
	if !gotEU || !gotUS {
		t.Fatalf("missing expected regions: %+v", got)
	}

	peaks, err := st.RollupPeakCCU(ctx, day1, day2)
	if err != nil {
		t.Fatalf("rollup peak ccu: %v", err)
	}
	// Exact-value checks below also double as presence checks: a day absent
	// from the map indexes as the int zero value, which would fail these
	// (non-zero) equality checks just as surely as a wrong value would.
	if peaks[dk(day1)] != 10 || peaks[dk(day2)] != 4 {
		t.Fatalf("peaks: %+v", peaks)
	}

	// Re-upsert day1 with the same data must not duplicate rows (delete+insert).
	if err := st.UpsertRollupDay(ctx, day1, dims1, peak1[dk(day1)]); err != nil {
		t.Fatalf("re-upsert day1: %v", err)
	}
	got2, err := st.RollupDims(ctx, day1, day2)
	if err != nil {
		t.Fatalf("rollup dims after re-upsert: %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("re-upsert duplicated rows: want 2, got %d: %+v", len(got2), got2)
	}

	// An empty day (no dims, zero peak) still marks itself processed via the
	// always-inserted match_ccu_daily row -- distinguished from a day never
	// rolled up at all via the map's ok-return (plain indexing can't tell
	// "absent" from "present with the legitimate value 0" apart).
	day0 := day1.AddDate(0, 0, -1)
	if err := st.UpsertRollupDay(ctx, day0, nil, 0); err != nil {
		t.Fatalf("upsert empty day: %v", err)
	}
	peaks0, err := st.RollupPeakCCU(ctx, day0, day0)
	if err != nil {
		t.Fatalf("rollup peak ccu (empty day): %v", err)
	}
	if v, ok := peaks0[dk(day0)]; !ok || v != 0 {
		t.Fatalf("empty day should be marked processed with peak 0: ok=%v peak=%v", ok, v)
	}
	dims0, err := st.RollupDims(ctx, day0, day0)
	if err != nil {
		t.Fatalf("dims empty day: %v", err)
	}
	if len(dims0) != 0 {
		t.Fatalf("empty day should have no dims: %+v", dims0)
	}

	// StatMatchesOverlapping catches a match that started before the window
	// and is still running (EndedAt nil).
	insertRollupMatch(t, st, srv, "as", 7, rts("2026-07-06T23:00:00Z"), nil)
	overlap, err := st.StatMatchesOverlapping(ctx, day1, day2)
	if err != nil {
		t.Fatalf("overlap with running match: %v", err)
	}
	var sawRunning bool
	for _, m := range overlap {
		if m.Region == "as" && m.EndedAt == nil {
			sawRunning = true
		}
	}
	if !sawRunning {
		t.Fatalf("expected still-running match started before window in overlap: %+v", overlap)
	}
}
