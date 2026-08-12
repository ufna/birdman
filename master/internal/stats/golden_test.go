package stats

import (
	"reflect"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

func gts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func gtp(s string) *time.Time { t := gts(s); return &t }

// sampleMatches is a representative window for the golden test: several
// regions/versions, a match spanning midnight UTC (started_at 07-06 23:30 →
// ended_at 07-07 00:30), and a still-running match (EndedAt nil, clamped to
// now).
func sampleMatches(now time.Time) []store.StatMatch {
	return []store.StatMatch{
		// Two overlapping eu/1.0.0 matches today → non-trivial peak CCU sweep.
		{Region: "eu", Semver: "1.0.0", PlayersPeak: 10,
			CreatedAt: gts("2026-07-08T09:55:00Z"), StartedAt: gts("2026-07-08T10:00:00Z"), EndedAt: gtp("2026-07-08T10:30:00Z")},
		{Region: "eu", Semver: "1.0.0", PlayersPeak: 6,
			CreatedAt: gts("2026-07-08T10:05:00Z"), StartedAt: gts("2026-07-08T10:10:00Z"), EndedAt: gtp("2026-07-08T10:40:00Z")},
		// A different region/version on a different day.
		{Region: "us", Semver: "1.1.0", PlayersPeak: 4,
			CreatedAt: gts("2026-07-07T08:57:00Z"), StartedAt: gts("2026-07-07T09:00:00Z"), EndedAt: gtp("2026-07-07T09:20:00Z")},
		// Spans midnight UTC: slot_seconds must split across 07-06/07-07, but
		// matches/players/duration attribute wholly to the start day (07-06).
		{Region: "as", Semver: "2.0.0", PlayersPeak: 8,
			CreatedAt: gts("2026-07-06T23:20:00Z"), StartedAt: gts("2026-07-06T23:30:00Z"), EndedAt: gtp("2026-07-07T00:30:00Z")},
		// Still running (EndedAt nil): excluded from duration, clamped to now
		// for peak-CCU/slot-seconds.
		{Region: "eu", Semver: "1.1.0", PlayersPeak: 5,
			CreatedAt: gts("2026-07-08T09:58:00Z"), StartedAt: gts("2026-07-08T10:00:00Z"), EndedAt: nil},
		// A fourth region/version, no overlap with anything, to broaden the
		// version/region mix.
		{Region: "apac", Semver: "2.0.0", PlayersPeak: 3,
			CreatedAt: gts("2026-07-05T05:58:00Z"), StartedAt: gts("2026-07-05T06:00:00Z"), EndedAt: gtp("2026-07-05T06:10:00Z")},
		// Zero-duration match (started_at == ended_at) as the SOLE match of
		// its region/semver: contributes matches/players but zero
		// slot_seconds, so it must not phantom-register its region/version as
		// a cost stack key (BuildCost's overlapSeconds<=0 guard skips it
		// entirely; the dims path must match).
		{Region: "eu-degenerate", Semver: "9.9.9", PlayersPeak: 1,
			CreatedAt: gts("2026-07-08T07:00:00Z"), StartedAt: gts("2026-07-08T07:00:00Z"), EndedAt: gtp("2026-07-08T07:00:00Z")},
	}
}

// TestRollupPathEqualsOnTheFly is the golden test: the dimensional rollup
// path (AggregateDaily → BuildOverviewFromDaily/BuildCostFromDaily) must
// reproduce the on-the-fly path (BuildOverview/BuildCost) byte-for-byte.
// This is what lets later tasks switch the read-path to precomputed rollups
// without changing the wire contract.
func TestRollupPathEqualsOnTheFly(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	axis := DayAxisUTC(now, 7)
	m := sampleMatches(now)
	util := []store.RegionUtil{
		{Region: "eu", CapacitySlots: 10, Allocated: 2, Ready: 1, Draining: 0},
		{Region: "us", CapacitySlots: 4, Allocated: 1, Ready: 0, Draining: 1},
	}

	dims, peak := AggregateDaily(m, axis, now)

	gotOv := BuildOverviewFromDaily(dims, peak, m, axis, 7, now)
	wantOv := BuildOverview(m, axis, 7, now)
	if !reflect.DeepEqual(gotOv, wantOv) {
		t.Fatalf("overview rollup path diverged:\n got=%+v\nwant=%+v", gotOv, wantOv)
	}

	// Оба пути получают ОДИН И ТОТ ЖЕ фильтр — иначе разошлись бы подписи
	// (utilization_note зависит от него с tracker #1009), и golden поймал бы
	// это как расхождение путей, хотя расхождения нет.
	gotCost := BuildCostFromDaily(dims, util, store.RegionUtilFilter{}, axis, 7, now)
	wantCost := BuildCost(m, util, store.RegionUtilFilter{}, axis, 7, now)
	if !reflect.DeepEqual(gotCost, wantCost) {
		t.Fatalf("cost rollup path diverged:\n got=%+v\nwant=%+v", gotCost, wantCost)
	}
}
