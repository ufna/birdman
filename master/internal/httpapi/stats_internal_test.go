package httpapi

import (
	"testing"

	"github.com/ufna/birdman/master/internal/stats"
)

// TestStatsLiveAxis covers statsDims' split point: which axis days the
// rollup-backed read-path recomputes live (yesterday+today) versus serves
// from the match_stats_daily/match_ccu_daily rollups. days=1 is the tricky
// edge case: axis0 ("today") is already after liveStart ("yesterday"), so the
// live axis must still resolve to just [today] — not [yesterday,today] —
// or the read-path would inject a day the caller never asked for.
func TestStatsLiveAxis(t *testing.T) {
	now := ts("2026-07-08T12:00:00Z")
	yesterday := ts("2026-07-07T00:00:00Z")

	cases := []struct {
		name string
		days int
		want []string
	}{
		{"days=1: axis0 is today, after liveStart", 1, []string{"2026-07-08"}},
		{"days=2: axis exactly matches the live window", 2, []string{"2026-07-07", "2026-07-08"}},
		{"days=7: only the trailing two days are live", 7, []string{"2026-07-07", "2026-07-08"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			axis := stats.DayAxisUTC(now, c.days)
			got := statsLiveAxis(axis, yesterday)
			if len(got) != len(c.want) {
				t.Fatalf("got %d days %v, want %d days %v", len(got), got, len(c.want), c.want)
			}
			for i, d := range got {
				if dk := d.UTC().Format("2006-01-02"); dk != c.want[i] {
					t.Fatalf("day %d = %s, want %s (full: %v)", i, dk, c.want[i], got)
				}
			}
		})
	}
}

// TestFilterDimsFrom covers the day-filter statsDims applies to the live
// recompute's output: AggregateDaily attributes matches/players/duration to
// a match's true start day even when that's before the live window (e.g. a
// match that started five days ago and is still running, seen while
// recomputing just yesterday+today) — filterDimsFrom drops that stray dim so
// it neither leaks a day outside the axis nor double-counts a day already
// served from the immutable rollup range.
func TestFilterDimsFrom(t *testing.T) {
	dims := []stats.DailyDim{
		{Day: ts("2026-07-01T00:00:00Z"), Region: "stale-start-day", Matches: 1},
		{Day: ts("2026-07-07T00:00:00Z"), Region: "yesterday", Matches: 2},
		{Day: ts("2026-07-08T00:00:00Z"), Region: "today", Matches: 3},
	}
	got := filterDimsFrom(dims, ts("2026-07-07T00:00:00Z"))
	if len(got) != 2 {
		t.Fatalf("want 2 dims (drop the stale pre-liveStart one), got %d: %+v", len(got), got)
	}
	for _, d := range got {
		if d.Region == "stale-start-day" {
			t.Fatalf("stale pre-liveStart dim should have been dropped: %+v", d)
		}
	}
}
