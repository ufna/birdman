package httpapi_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
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
