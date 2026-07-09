package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/stats"
	"github.com/ufna/birdman/master/internal/statsrollup"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// insertFinishedMatch writes a finished match on the fixture's server, with the
// interval placed `agoMinStart..agoMinEnd` minutes before now (both today UTC).
func insertFinishedMatch(t *testing.T, f *testdb.Fixture, serverID, region string, playersPeak, agoMinStart, agoMinEnd int) {
	t.Helper()
	_, err := f.St.Pool.Exec(context.Background(), `
		insert into matches (project_id, server_id, version_id, region, state, players_peak, created_at, started_at, ended_at)
		select s.project_id, s.id, s.version_id, $2, 'finished', $3,
		       now() - make_interval(mins => $4),
		       now() - make_interval(mins => $5),
		       now() - make_interval(mins => $6)
		from servers s where s.id = $1::uuid`,
		serverID, region, playersPeak, agoMinStart+1, agoMinStart, agoMinEnd)
	if err != nil {
		t.Fatalf("insert match: %v", err)
	}
}

func TestStatsEndpoints(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

	// Two finished matches earlier today, both version 1.0.0, region eu.
	srv := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 20001, 0)
	insertFinishedMatch(t, f, srv, "eu", 10, 120, 90) // 30 min, peak 10
	insertFinishedMatch(t, f, srv, "eu", 6, 60, 40)   // 20 min, peak 6

	_, roSecret, _ := st.CreateAPIKey(ctx, "ro", []string{httpapi.ScopeReadonly})
	_, mmSecret, _ := st.CreateAPIKey(ctx, "mm", []string{httpapi.ScopeMatchmaking})
	ro := &client{t: t, base: ts.URL, key: roSecret}
	mmc := &client{t: t, base: ts.URL, key: mmSecret}

	// scope: readonly may read stats; a matchmaking-only key may not.
	if code, _ := mmc.do("GET", "/v1/stats/overview", nil); code != 403 {
		t.Fatalf("matchmaking key on stats: want 403, got %d", code)
	}

	// overview.
	code, body := ro.do("GET", "/v1/stats/overview?days=7", nil)
	if code != 200 {
		t.Fatalf("overview: %d %v", code, body)
	}
	if body["timezone"] != "UTC" || body["days"].(float64) != 7 {
		t.Fatalf("overview meta: %v", body)
	}
	mpd := body["matches_per_day"].(map[string]any)
	if len(mpd["points"].([]any)) != 7 {
		t.Fatalf("want 7 day points, got %d", len(mpd["points"].([]any)))
	}
	vd := body["version_distribution"].([]any)
	if len(vd) != 1 || vd[0].(map[string]any)["version"] != "1.0.0" || vd[0].(map[string]any)["matches"].(float64) != 2 {
		t.Fatalf("version distribution: %v", vd)
	}
	ttm := body["time_to_match"].(map[string]any)
	if ttm["samples"].(float64) != 2 {
		t.Fatalf("time_to_match samples: %v", ttm)
	}
	// peak CCU: the two matches do not overlap (120..90 vs 60..40) → peak 10.
	if body["peak_ccu"].(float64) != 10 {
		t.Fatalf("peak_ccu = %v, want 10", body["peak_ccu"])
	}

	// cost.
	code, body = ro.do("GET", "/v1/stats/cost?days=7", nil)
	if code != 200 {
		t.Fatalf("cost: %d %v", code, body)
	}
	if body["slot_hours_total"].(float64) <= 0 {
		t.Fatalf("slot_hours_total should be > 0: %v", body["slot_hours_total"])
	}
	util := body["utilization"].([]any)
	var euCap float64 = -1
	for _, u := range util {
		um := u.(map[string]any)
		if um["region"] == "eu" {
			euCap = um["capacity_slots"].(float64)
		}
	}
	if euCap != 8 {
		t.Fatalf("eu capacity_slots = %v, want 8 (%v)", euCap, util)
	}

	// days validation.
	for _, bad := range []string{"0", "1000", "abc", "-1"} {
		if code, _ := ro.do("GET", "/v1/stats/overview?days="+bad, nil); code != 400 {
			t.Fatalf("days=%s: want 400, got %d", bad, code)
		}
	}
}

// TestStatsEmpty: a fresh DB with no matches returns valid zero-filled series.
func TestStatsEmpty(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()
	_, roSecret, _ := st.CreateAPIKey(ctx, "ro", []string{httpapi.ScopeReadonly})
	ro := &client{t: t, base: ts.URL, key: roSecret}

	code, body := ro.do("GET", "/v1/stats/overview?days=3", nil)
	if code != 200 {
		t.Fatalf("empty overview: %d %v", code, body)
	}
	if body["peak_ccu"].(float64) != 0 || body["avg_match_duration_seconds"] != nil {
		t.Fatalf("empty overview should have zero/nil scalars: %v", body)
	}
	if len(body["version_distribution"].([]any)) != 0 {
		t.Fatalf("empty version distribution should be []")
	}
	if code, _ := ro.do("GET", "/v1/stats/cost?days=3", nil); code != 200 {
		t.Fatalf("empty cost: %d", code)
	}
}

// --- rollup-backed read-path acceptance tests («Статистика v1» T10) ---
//
// handleStatsOverview/handleStatsCost switched from scanning every started
// match in the window on every request to a split read-path: everything up
// to today-2 comes from the match_stats_daily/match_ccu_daily rollups, and
// the trailing two days (yesterday+today) are recomputed live from raw
// matches on every call. TestStatsOverviewRollupBacked/TestStatsCostRollupBacked
// prove three things:
//   - the immutable range is genuinely READ from the rollup tables (a
//     phantom rollup row with no backing raw match must still surface in the
//     response — this is what actually fails against the pre-T10 handler,
//     which never looks at match_stats_daily/match_ccu_daily at all);
//   - end to end, the rollup-backed response is identical to the on-the-fly
//     reference (stats.BuildOverview/BuildCost) over the same window — the
//     golden invariant already proven at the pure-function/store layers
//     (stats/golden_test.go, statsrollup/job_test.go) survives the HTTP
//     plumbing unchanged;
//   - the trailing two days are present even when the rollup-maintenance job
//     (internal/statsrollup) has never run.

// newStatsAPI spins up a fully-wired httpapi.Server over a fresh test DB —
// the boilerplate every test below needs — seeded with fixture "game"/eu
// (testdb.Seed) plus a readonly API key.
func newStatsAPI(t *testing.T) (ts *httptest.Server, st *store.Store, f *testdb.Fixture, roSecret string) {
	t.Helper()
	st = testdb.New(t)
	f = testdb.Seed(t, st, "eu", 20)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts = httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", log))
	t.Cleanup(ts.Close)
	_, roSecret, err := st.CreateAPIKey(t.Context(), "ro", []string{httpapi.ScopeReadonly})
	if err != nil {
		t.Fatalf("create ro api key: %v", err)
	}
	return ts, st, f, roSecret
}

// httpGetJSON GETs path with a bearer key and decodes the JSON body into out
// (a pointer), failing the test on a non-200 status or a decode error.
func httpGetJSON(t *testing.T, base, path, key string, out any) {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: want 200, got %d: %s", path, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("GET %s: decode: %v\nbody: %s", path, err, raw)
	}
}

// endAt returns a pointer to t, for the optional ended_at column.
func endAt(t time.Time) *time.Time { return &t }

// insertStatMatchAt inserts a matches row with explicit created_at/started_at
// (unlike insertFinishedMatch's "ago minutes" helper, these tests need
// matches pinned to specific UTC-day offsets from "today" regardless of how
// long the test takes to run, and a nonzero created_at→started_at gap for a
// meaningful time-to-match sample).
func insertStatMatchAt(t *testing.T, st *store.Store, serverID, region string, playersPeak int, createdAt, startedAt time.Time, endedAt *time.Time) {
	t.Helper()
	state := "finished"
	if endedAt == nil {
		state = "running"
	}
	_, err := st.Pool.Exec(context.Background(), `
		insert into matches (project_id, server_id, version_id, region, state, players_peak, created_at, started_at, ended_at)
		select s.project_id, s.id, s.version_id, $2, $3, $4, $5, $6, $7
		from servers s where s.id = $1::uuid`,
		serverID, region, state, playersPeak, createdAt, startedAt, endedAt)
	if err != nil {
		t.Fatalf("insert stat match: %v", err)
	}
}

// seedStatsRollupWindow inserts a representative ~10-day spread of matches:
// several region/version combinations, a match spanning midnight UTC, a
// still-running match, and matches both inside and outside the trailing
// live window. Returns today (00:00 UTC) so callers can reason about day
// offsets.
func seedStatsRollupWindow(t *testing.T, st *store.Store, f *testdb.Fixture) time.Time {
	t.Helper()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	v110 := f.AddVersion(t, "1.1.0")
	v200 := f.AddVersion(t, "2.0.0")
	srv100 := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 21001, 0)
	srv110 := f.InsertServer(t, f.NodeID, v110, "reaped", 21002, 0)
	srv200 := f.InsertServer(t, f.NodeID, v200, "reaped", 21003, 0)

	// Deep history: inside days=30, outside days=7 (axis0 = today-6).
	insertStatMatchAt(t, st, srv100, "eu", 10,
		today.AddDate(0, 0, -9).Add(9*time.Hour+55*time.Minute),
		today.AddDate(0, 0, -9).Add(10*time.Hour),
		endAt(today.AddDate(0, 0, -9).Add(10*time.Hour+30*time.Minute)))
	insertStatMatchAt(t, st, srv100, "eu", 7,
		today.AddDate(0, 0, -8).Add(8*time.Hour+58*time.Minute),
		today.AddDate(0, 0, -8).Add(9*time.Hour),
		endAt(today.AddDate(0, 0, -8).Add(9*time.Hour+15*time.Minute)))

	// Inside days=7, outside the trailing live window (today-6..today-3).
	insertStatMatchAt(t, st, srv110, "us", 4,
		today.AddDate(0, 0, -6).Add(7*time.Hour+57*time.Minute),
		today.AddDate(0, 0, -6).Add(8*time.Hour),
		endAt(today.AddDate(0, 0, -6).Add(8*time.Hour+20*time.Minute)))
	insertStatMatchAt(t, st, srv200, "apac", 3,
		today.AddDate(0, 0, -5).Add(5*time.Hour+58*time.Minute),
		today.AddDate(0, 0, -5).Add(6*time.Hour),
		endAt(today.AddDate(0, 0, -5).Add(6*time.Hour+10*time.Minute)))
	// Spans midnight UTC: start day = today-3, slot_seconds splits today-3/today-2.
	insertStatMatchAt(t, st, srv200, "as", 8,
		today.AddDate(0, 0, -3).Add(23*time.Hour+10*time.Minute),
		today.AddDate(0, 0, -3).Add(23*time.Hour+30*time.Minute),
		endAt(today.AddDate(0, 0, -2).Add(30*time.Minute)))

	// Trailing live window (yesterday/today): a still-running match plus two
	// finished ones on "today", one of them zero-duration.
	insertStatMatchAt(t, st, srv110, "eu", 5,
		today.AddDate(0, 0, -1).Add(9*time.Hour+58*time.Minute),
		today.AddDate(0, 0, -1).Add(10*time.Hour),
		nil) // still running
	insertStatMatchAt(t, st, srv100, "eu", 6,
		today.Add(59*time.Minute), today.Add(time.Hour), endAt(today.Add(time.Hour+20*time.Minute)))
	insertStatMatchAt(t, st, srv100, "eu-degenerate", 1,
		today.Add(7*time.Hour-time.Minute), today.Add(7*time.Hour), endAt(today.Add(7*time.Hour)))

	return today
}

// TestStatsOverviewRollupBacked is Task 10's acceptance test for
// /v1/stats/overview (see the file-level comment above).
func TestStatsOverviewRollupBacked(t *testing.T) {
	t.Run("immutable_range_read_from_rollup_table", func(t *testing.T) {
		ts, st, _, roSecret := newStatsAPI(t)
		ctx := t.Context()
		now := time.Now().UTC()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		phantomDay := today.AddDate(0, 0, -20) // deep in the immutable range; no backing raw match at all

		if err := st.UpsertRollupDay(ctx, phantomDay, []store.RollupDim{
			{Day: phantomDay, Region: "zzz-phantom", Semver: "0.0.0-phantom",
				Matches: 999, PlayersPeakSum: 500, DurSumSeconds: 12345, DurCount: 3, SlotSeconds: 54321},
		}, 777); err != nil {
			t.Fatalf("seed phantom rollup day: %v", err)
		}

		var got stats.OverviewResponse
		httpGetJSON(t, ts.URL, "/v1/stats/overview?days=30", roSecret, &got)

		foundVersion := false
		for _, vd := range got.VersionDistribution {
			if vd.Version == "0.0.0-phantom" {
				foundVersion = true
				if vd.Matches != 999 {
					t.Fatalf("phantom version matches = %d, want 999", vd.Matches)
				}
			}
		}
		if !foundVersion {
			t.Fatalf("phantom rollup-only day (no backing raw match) missing from version_distribution — "+
				"handler must read match_stats_daily for the immutable range: %+v", got.VersionDistribution)
		}
		if got.PeakCCU != 777 {
			t.Fatalf("peak_ccu = %v, want 777 (from the phantom match_ccu_daily row) — "+
				"handler must read match_ccu_daily for the immutable range", got.PeakCCU)
		}
		pdKey := phantomDay.Format("2006-01-02")
		foundPeakDay := false
		for _, p := range got.PeakCCUPerDay.Points {
			if p.Date == pdKey {
				foundPeakDay = true
				if p.Value != 777 {
					t.Fatalf("peak_ccu_per_day[%s] = %v, want 777", pdKey, p.Value)
				}
			}
		}
		if !foundPeakDay {
			t.Fatalf("phantom day %s missing from peak_ccu_per_day: %+v", pdKey, got.PeakCCUPerDay.Points)
		}
		if got.AvgMatchDurationSeconds == nil || *got.AvgMatchDurationSeconds != 4115 {
			t.Fatalf("avg_match_duration_seconds = %v, want 4115 (12345/3, from the phantom rollup row)", got.AvgMatchDurationSeconds)
		}
	})

	t.Run("equals_on_the_fly_path", func(t *testing.T) {
		ts, st, f, roSecret := newStatsAPI(t)
		ctx := t.Context()
		seedStatsRollupWindow(t, st, f)

		job := statsrollup.New(st, time.Hour, opsLog())
		if err := job.Backfill(ctx); err != nil {
			t.Fatalf("backfill: %v", err)
		}

		for _, days := range []int{30, 7} {
			var got stats.OverviewResponse
			httpGetJSON(t, ts.URL, fmt.Sprintf("/v1/stats/overview?days=%d", days), roSecret, &got)

			now := time.Now().UTC()
			axis := stats.DayAxisUTC(now, days)
			refMatches, err := st.StatMatches(ctx, axis[0])
			if err != nil {
				t.Fatalf("reference StatMatches: %v", err)
			}
			want := stats.BuildOverview(refMatches, axis, days, now)
			got.GeneratedAt = want.GeneratedAt // two separate time.Now() calls; not a data field
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("days=%d: rollup-backed overview != on-the-fly reference\n got=%+v\nwant=%+v", days, got, want)
			}
		}
	})

	t.Run("live_tail_present_without_job", func(t *testing.T) {
		ts, st, f, roSecret := newStatsAPI(t)
		ctx := t.Context()
		srv := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 21101, 0)

		now := time.Now().UTC()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		// Trailing-window matches only; the rollup-maintenance job
		// (internal/statsrollup) never runs in this sub-case — match_stats_daily/
		// match_ccu_daily stay completely empty.
		insertStatMatchAt(t, st, srv, "eu", 9,
			today.Add(2*time.Hour-2*time.Minute), today.Add(2*time.Hour), endAt(today.Add(2*time.Hour+15*time.Minute)))
		insertStatMatchAt(t, st, srv, "eu", 4,
			today.AddDate(0, 0, -1).Add(3*time.Hour-time.Minute), today.AddDate(0, 0, -1).Add(3*time.Hour),
			endAt(today.AddDate(0, 0, -1).Add(3*time.Hour+10*time.Minute)))

		var got stats.OverviewResponse
		httpGetJSON(t, ts.URL, "/v1/stats/overview?days=7", roSecret, &got)

		if got.PeakCCU == 0 {
			t.Fatalf("expected nonzero peak_ccu from trailing-window matches with the rollup job never run: %+v", got)
		}
		todayKey := today.Format("2006-01-02")
		foundToday := false
		for _, p := range got.MatchesPerDay.Points {
			if p.Date == todayKey && p.Total > 0 {
				foundToday = true
			}
		}
		if !foundToday {
			t.Fatalf("today's match missing from matches_per_day with the rollup job never run: %+v", got.MatchesPerDay.Points)
		}

		// Sanity: the rollup tables really are untouched in this sub-case.
		dims, err := st.RollupDims(ctx, today.AddDate(0, 0, -6), today)
		if err != nil {
			t.Fatalf("rollup dims: %v", err)
		}
		if len(dims) != 0 {
			t.Fatalf("rollup table should be empty in this sub-case (job never ran): %+v", dims)
		}
	})
}

// TestStatsCostRollupBacked mirrors TestStatsOverviewRollupBacked for
// /v1/stats/cost.
func TestStatsCostRollupBacked(t *testing.T) {
	t.Run("immutable_range_read_from_rollup_table", func(t *testing.T) {
		ts, st, _, roSecret := newStatsAPI(t)
		ctx := t.Context()
		now := time.Now().UTC()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		phantomDay := today.AddDate(0, 0, -20)

		if err := st.UpsertRollupDay(ctx, phantomDay, []store.RollupDim{
			{Day: phantomDay, Region: "zzz-phantom-cost", Semver: "0.0.0-phantom", SlotSeconds: 36000}, // 10h exactly
		}, 0); err != nil {
			t.Fatalf("seed phantom rollup day: %v", err)
		}

		var got stats.CostResponse
		httpGetJSON(t, ts.URL, "/v1/stats/cost?days=30", roSecret, &got)

		if got.SlotHoursTotal != 10 {
			t.Fatalf("slot_hours_total = %v, want 10 (from the phantom rollup-only day) — "+
				"handler must read match_stats_daily for the immutable range", got.SlotHoursTotal)
		}
		foundRegion := false
		for _, p := range got.SlotHoursPerDayByRegion.Points {
			if p.Values["zzz-phantom-cost"] == 10 {
				foundRegion = true
			}
		}
		if !foundRegion {
			t.Fatalf("phantom region missing from slot_hours_per_day_by_region: %+v", got.SlotHoursPerDayByRegion.Points)
		}
	})

	t.Run("equals_on_the_fly_path", func(t *testing.T) {
		ts, st, f, roSecret := newStatsAPI(t)
		ctx := t.Context()
		seedStatsRollupWindow(t, st, f)

		job := statsrollup.New(st, time.Hour, opsLog())
		if err := job.Backfill(ctx); err != nil {
			t.Fatalf("backfill: %v", err)
		}

		for _, days := range []int{30, 7} {
			var got stats.CostResponse
			httpGetJSON(t, ts.URL, fmt.Sprintf("/v1/stats/cost?days=%d", days), roSecret, &got)

			now := time.Now().UTC()
			axis := stats.DayAxisUTC(now, days)
			refMatches, err := st.StatMatches(ctx, axis[0])
			if err != nil {
				t.Fatalf("reference StatMatches: %v", err)
			}
			util, err := st.RegionUtilization(ctx)
			if err != nil {
				t.Fatalf("reference RegionUtilization: %v", err)
			}
			want := stats.BuildCost(refMatches, util, axis, days, now)
			got.GeneratedAt = want.GeneratedAt
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("days=%d: rollup-backed cost != on-the-fly reference\n got=%+v\nwant=%+v", days, got, want)
			}
		}
	})
}

// TestStatsOccupancyBoundary pins the product owner's decided occupancy
// semantics (Task 10 review): a match that STARTED before the selected
// window but is still occupying a slot DURING the window (still running,
// ended_at IS NULL) counts toward that window's peak_ccu and slot_hours —
// slot-hours is "allocated dedik time" and peak_ccu is "max concurrent
// allocated-players" (docs/specs/master.md §6), not "matches that started in
// the window". This is intentionally MORE correct than the old on-the-fly
// handler (which only ever scanned matches with started_at >= since and so
// under-counted this case) — this test does not compare against that
// reference (unlike equals_on_the_fly_path above), it pins the decided
// behavior directly.
//
// It also pins the companion bug fix: such a match contributes a Matches=0
// slot-only dim for the semver it carries on every window day it overlaps
// (see AggregateDaily), which must not surface as a phantom {matches:0}
// version_distribution entry (stats.dropZeroMatchSemvers). Without that fix
// this test fails on the version_distribution assertion below.
func TestStatsOccupancyBoundary(t *testing.T) {
	ts, st, f, roSecret := newStatsAPI(t)
	ctx := t.Context()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	// Pre-window boundary match: started 8 days ago — before days=7's
	// axis0 (today-6) — and still running, so it occupies a slot throughout
	// the whole window without ever "starting" in it (version 1.0.0, f's
	// default, region "eu-boundary").
	srvBoundary := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 21301, 0)
	insertStatMatchAt(t, st, srvBoundary, "eu-boundary", 50,
		today.AddDate(0, 0, -8).Add(time.Hour), today.AddDate(0, 0, -8).Add(time.Hour+5*time.Minute), nil)

	// A normal in-window match on a distinct version, so the window has a
	// real started-match too (not otherwise empty) without masking the
	// boundary match's phantom entry above — they must not share a semver.
	v2 := f.AddVersion(t, "2.0.0")
	srvNormal := f.InsertServer(t, f.NodeID, v2, "reaped", 21302, 0)
	insertStatMatchAt(t, st, srvNormal, "eu-boundary", 3,
		today.Add(58*time.Minute), today.Add(time.Hour), endAt(today.Add(time.Hour+10*time.Minute)))

	job := statsrollup.New(st, time.Hour, opsLog())
	if err := job.Backfill(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var overview stats.OverviewResponse
	httpGetJSON(t, ts.URL, "/v1/stats/overview?days=7", roSecret, &overview)
	var cost stats.CostResponse
	httpGetJSON(t, ts.URL, "/v1/stats/cost?days=7", roSecret, &cost)

	// Occupancy IS counted: the pre-window still-running match overlaps
	// every day of the window, so it must contribute to peak_ccu and
	// slot_hours despite never starting inside it.
	if overview.PeakCCU < 50 {
		t.Fatalf("peak_ccu = %d, want >= 50 (the pre-window still-running match occupies the whole window)", overview.PeakCCU)
	}
	if cost.SlotHoursTotal <= 0 {
		t.Fatalf("slot_hours_total = %v, want > 0 (the pre-window still-running match occupies a slot across the window)", cost.SlotHoursTotal)
	}

	// Phantom bug fixed: the boundary match's semver (1.0.0) never started
	// in the window (only slot-only dims carry it there), so it must not
	// appear as a zero-match entry — nor should anything else.
	for _, vd := range overview.VersionDistribution {
		if vd.Matches == 0 {
			t.Fatalf("version_distribution has a phantom zero-match entry: %+v (full: %+v)", vd, overview.VersionDistribution)
		}
	}
}
