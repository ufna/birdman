package httpapi

import (
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func tp(s string) *time.Time { t := ts(s); return &t }

// point looks up a stacked series day by date.
func stackPoint(t *testing.T, s stackedSeries, date string) stackedPoint {
	t.Helper()
	for _, p := range s.Points {
		if p.Date == date {
			return p
		}
	}
	t.Fatalf("no point %s in series %+v", date, s.Points)
	return stackedPoint{}
}

func simplePointVal(t *testing.T, s simpleSeries, date string) float64 {
	t.Helper()
	for _, p := range s.Points {
		if p.Date == date {
			return p.Value
		}
	}
	t.Fatalf("no point %s in series %+v", date, s.Points)
	return 0
}

// TestBuildOverview checks every overview aggregate on a small hand-checkable
// dataset: two overlapping eu matches on 07-08 and one us match on 07-07.
func TestBuildOverview(t *testing.T) {
	now := ts("2026-07-08T12:00:00Z")
	days := 3
	axis := dayAxisUTC(now, days) // 07-06, 07-07, 07-08
	if got := dayKey(axis[0]); got != "2026-07-06" {
		t.Fatalf("axis[0] = %s, want 2026-07-06", got)
	}
	// Durations are round (start+30m, +30m, +20m) so avg = 1600s exactly;
	// created→started gaps are 5s, 3s, 10s (the time-to-match samples).
	matches := []store.StatMatch{
		{Region: "eu", Semver: "1.0.0", PlayersPeak: 10,
			CreatedAt: ts("2026-07-08T10:00:00Z"), StartedAt: ts("2026-07-08T10:00:05Z"), EndedAt: tp("2026-07-08T10:30:05Z")},
		{Region: "eu", Semver: "1.0.0", PlayersPeak: 6,
			CreatedAt: ts("2026-07-08T10:10:00Z"), StartedAt: ts("2026-07-08T10:10:03Z"), EndedAt: tp("2026-07-08T10:40:03Z")},
		{Region: "us", Semver: "1.1.0", PlayersPeak: 4,
			CreatedAt: ts("2026-07-07T09:00:00Z"), StartedAt: ts("2026-07-07T09:00:10Z"), EndedAt: tp("2026-07-07T09:20:10Z")},
	}
	ov := buildOverview(matches, axis, days, now)

	if ov.Days != 3 || ov.Timezone != "UTC" {
		t.Fatalf("meta: %+v", ov)
	}
	// matches/day stacked by region.
	if got := stackPoint(t, ov.MatchesPerDay, "2026-07-08"); got.Total != 2 || got.Values["eu"] != 2 {
		t.Fatalf("matches 07-08: %+v", got)
	}
	if got := stackPoint(t, ov.MatchesPerDay, "2026-07-07"); got.Total != 1 || got.Values["us"] != 1 || got.Values["eu"] != 0 {
		t.Fatalf("matches 07-07: %+v", got)
	}
	if got := stackPoint(t, ov.MatchesPerDay, "2026-07-06"); got.Total != 0 {
		t.Fatalf("matches 07-06 should be zero-filled: %+v", got)
	}
	// players/day = sum players_peak.
	if got := stackPoint(t, ov.PlayersPerDay, "2026-07-08"); got.Values["eu"] != 16 {
		t.Fatalf("players 07-08: %+v", got)
	}
	// peak CCU: 07-08 has 10 and 6 overlapping → 16.
	if ov.PeakCCU != 16 {
		t.Fatalf("peak ccu = %d, want 16", ov.PeakCCU)
	}
	if v := simplePointVal(t, ov.PeakCCUPerDay, "2026-07-08"); v != 16 {
		t.Fatalf("ccu 07-08 = %v, want 16", v)
	}
	if v := simplePointVal(t, ov.PeakCCUPerDay, "2026-07-07"); v != 4 {
		t.Fatalf("ccu 07-07 = %v, want 4", v)
	}
	// avg duration overall = (1800+1800+1200)/3 = 1600.
	if ov.AvgMatchDurationSeconds == nil || *ov.AvgMatchDurationSeconds != 1600 {
		t.Fatalf("avg duration = %v, want 1600", ov.AvgMatchDurationSeconds)
	}
	// version distribution: 1.0.0 (2) before 1.1.0 (1).
	if len(ov.VersionDistribution) != 2 || ov.VersionDistribution[0].Version != "1.0.0" ||
		ov.VersionDistribution[0].Matches != 2 {
		t.Fatalf("version dist: %+v", ov.VersionDistribution)
	}
	// time-to-match: deltas 5,3,10 → sorted 3,5,10 → p50=5, p95=10.
	if ov.TimeToMatch.Samples != 3 || ov.TimeToMatch.P50Seconds == nil ||
		*ov.TimeToMatch.P50Seconds != 5 || *ov.TimeToMatch.P95Seconds != 10 {
		t.Fatalf("ttm: %+v", ov.TimeToMatch)
	}
}

// TestBuildOverviewEmpty: no matches → zero-filled series, nil scalars, no panic.
func TestBuildOverviewEmpty(t *testing.T) {
	now := ts("2026-07-08T12:00:00Z")
	ov := buildOverview(nil, dayAxisUTC(now, 7), 7, now)
	if len(ov.MatchesPerDay.Points) != 7 {
		t.Fatalf("want 7 points, got %d", len(ov.MatchesPerDay.Points))
	}
	for _, p := range ov.MatchesPerDay.Points {
		if p.Total != 0 {
			t.Fatalf("non-zero point in empty overview: %+v", p)
		}
	}
	if ov.PeakCCU != 0 || ov.AvgMatchDurationSeconds != nil || ov.TimeToMatch.Samples != 0 {
		t.Fatalf("empty overview scalars: %+v", ov)
	}
	if ov.TimeToMatch.P50Seconds != nil {
		t.Fatalf("p50 should be nil with no samples")
	}
}

// TestPeakCCURunningMatch: a running match (EndedAt nil) is clamped to now.
func TestPeakCCURunningMatch(t *testing.T) {
	now := ts("2026-07-08T12:00:00Z")
	axis := dayAxisUTC(now, 1)
	matches := []store.StatMatch{
		{Region: "eu", Semver: "1.0.0", PlayersPeak: 7,
			CreatedAt: ts("2026-07-08T11:00:00Z"), StartedAt: ts("2026-07-08T11:00:00Z"), EndedAt: nil},
	}
	_, peak := peakCCUPerDay(matches, axis, now)
	if peak != 7 {
		t.Fatalf("running-match peak = %d, want 7", peak)
	}
}

// TestBuildCostDaySplit: a match spanning midnight splits slot-hours across
// both UTC days (20 min each → 0.33h each).
func TestBuildCostDaySplit(t *testing.T) {
	now := ts("2026-07-08T12:00:00Z")
	axis := dayAxisUTC(now, 3)
	matches := []store.StatMatch{
		{Region: "eu", Semver: "1.0.0", PlayersPeak: 2,
			CreatedAt: ts("2026-07-07T23:39:00Z"), StartedAt: ts("2026-07-07T23:40:00Z"), EndedAt: tp("2026-07-08T00:20:00Z")},
	}
	cost := buildCost(matches, nil, axis, 3, now)
	d7 := stackPoint(t, cost.SlotHoursPerDayByRegion, "2026-07-07")
	d8 := stackPoint(t, cost.SlotHoursPerDayByRegion, "2026-07-08")
	if d7.Values["eu"] != 0.33 || d8.Values["eu"] != 0.33 {
		t.Fatalf("day split: 07-07=%v 07-08=%v, want 0.33 each", d7.Values["eu"], d8.Values["eu"])
	}
	// total ≈ 40 min = 0.67h.
	if cost.SlotHoursTotal < 0.66 || cost.SlotHoursTotal > 0.67 {
		t.Fatalf("slot-hours total = %v, want ~0.67", cost.SlotHoursTotal)
	}
	if len(cost.Utilization) != 0 {
		t.Fatalf("utilization should be [] when none: %+v", cost.Utilization)
	}
}

func TestPercentileAndOverlap(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if p := percentile(s, 50); p != 5 {
		t.Fatalf("p50 = %v, want 5", p)
	}
	if p := percentile(s, 95); p != 10 {
		t.Fatalf("p95 = %v, want 10", p)
	}
	if p := percentile(nil, 50); p != 0 {
		t.Fatalf("p50 empty = %v, want 0", p)
	}
	// overlap of [10:00,10:30) with a full day = 1800s.
	got := overlapSeconds(ts("2026-07-08T10:00:00Z"), ts("2026-07-08T10:30:00Z"),
		ts("2026-07-08T00:00:00Z"), ts("2026-07-09T00:00:00Z"))
	if got != 1800 {
		t.Fatalf("overlap = %v, want 1800", got)
	}
	// disjoint → 0.
	if got := overlapSeconds(ts("2026-07-08T10:00:00Z"), ts("2026-07-08T11:00:00Z"),
		ts("2026-07-09T00:00:00Z"), ts("2026-07-10T00:00:00Z")); got != 0 {
		t.Fatalf("disjoint overlap = %v, want 0", got)
	}
}
