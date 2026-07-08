package httpapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// mmServer wires a real matchmaker (fast tick) behind the REST API.
func mmServer(t *testing.T, st *store.Store, cfg matchmaker.Config) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	if cfg.Tick == 0 {
		cfg.Tick = 20 * time.Millisecond
	}
	mm := matchmaker.New(st, m, cfg, log)
	go mm.Run(t.Context())
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, log))
	t.Cleanup(ts.Close)
	return ts
}

func mmKey(t *testing.T, st *store.Store) string {
	t.Helper()
	_, key, err := st.CreateAPIKey(t.Context(), "client", []string{httpapi.ScopeMatchmaking})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func ticketBody(player, version string, regs ...any) map[string]any {
	regions := []map[string]any{}
	for i := 0; i+1 < len(regs); i += 2 {
		regions = append(regions, map[string]any{"region": regs[i], "rtt_ms": regs[i+1]})
	}
	return map[string]any{"player_id": player, "client_version": version, "regions": regions}
}

// getJSON is a goroutine-safe GET (no t.Fatal off the test goroutine).
func getJSON(base, path, key string) (int, map[string]any, error) {
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("bad json: %s", raw)
		}
	}
	return resp.StatusCode, out, nil
}

// End-to-end matchmaking flow over REST: create, long-poll (the response must
// arrive as the match forms, well before `wait` expires), cancel, errors.
func TestMatchmakingREST(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	ts := mmServer(t, st, matchmaker.Config{})

	mmc := &client{t: t, base: ts.URL, key: mmKey(t, st)}
	anon := &client{t: t, base: ts.URL}

	// Auth: anon and wrong-scope keys are rejected.
	if code, _ := anon.do("POST", "/v1/matchmaking/tickets", ticketBody("p1", "1.0.0", "eu", 10)); code != 401 {
		t.Fatalf("anon ticket: want 401, got %d", code)
	}
	_, roKey, err := st.CreateAPIKey(t.Context(), "ro", []string{httpapi.ScopeReadonly})
	if err != nil {
		t.Fatal(err)
	}
	ro := &client{t: t, base: ts.URL, key: roKey}
	if code, _ := ro.do("POST", "/v1/matchmaking/tickets", ticketBody("p1", "1.0.0", "eu", 10)); code != 403 {
		t.Fatalf("readonly ticket: want 403, got %d", code)
	}
	// The matchmaking key must not see fleet internals.
	if code, _ := mmc.do("GET", "/v1/nodes", nil); code != 403 {
		t.Fatalf("matchmaking key on /v1/nodes: want 403, got %d", code)
	}

	// Validation.
	if code, _ := mmc.do("POST", "/v1/matchmaking/tickets",
		map[string]any{"player_id": "p1", "client_version": "1.0.0", "regions": []any{}}); code != 400 {
		t.Fatalf("no regions: want 400, got %d", code)
	}
	if code, _ := mmc.do("POST", "/v1/matchmaking/tickets",
		ticketBody("p1", "not-a-version", "eu", 10)); code != 400 {
		t.Fatalf("bad version: want 400, got %d", code)
	}

	// Create the first ticket.
	code, body := mmc.do("POST", "/v1/matchmaking/tickets", ticketBody("p1", "1.0.0", "eu", 10))
	if code != 201 || body["status"] != "queued" {
		t.Fatalf("create ticket: %d %v", code, body)
	}
	id1 := body["ticket_id"].(string)

	// Long-poll in flight; the second player arrives 150ms later. The poll
	// must return matched almost immediately after — not after `wait`.
	type pollResult struct {
		code    int
		body    map[string]any
		elapsed time.Duration
		err     error
	}
	ch := make(chan pollResult, 1)
	started := time.Now()
	go func() {
		code, body, err := getJSON(ts.URL, "/v1/matchmaking/tickets/"+id1+"?wait=20s", mmc.key)
		ch <- pollResult{code, body, time.Since(started), err}
	}()
	time.Sleep(150 * time.Millisecond)
	if code, body = mmc.do("POST", "/v1/matchmaking/tickets", ticketBody("p2", "1.0.0", "eu", 40)); code != 201 {
		t.Fatalf("second ticket: %d %v", code, body)
	}
	id2 := body["ticket_id"].(string)

	r := <-ch
	if r.err != nil || r.code != 200 {
		t.Fatalf("long-poll: %d %v %v", r.code, r.body, r.err)
	}
	if r.elapsed >= 10*time.Second {
		t.Fatalf("long-poll did not wake on match: took %s", r.elapsed)
	}
	if r.body["status"] != "matched" {
		t.Fatalf("long-poll status: %v", r.body)
	}
	match := r.body["match"].(map[string]any)
	if match["host"] != "203.0.113.10" || int(match["port"].(float64)) != 20001 {
		t.Fatalf("bad match endpoint: %v", match)
	}
	if _, err := uuid.Parse(match["match_id"].(string)); err != nil {
		t.Fatalf("bad match_id: %v", match)
	}

	// Both tickets point at the same match.
	code, body = mmc.do("GET", "/v1/matchmaking/tickets/"+id2, nil)
	if code != 200 || body["status"] != "matched" {
		t.Fatalf("second ticket state: %d %v", code, body)
	}
	if body["match"].(map[string]any)["match_id"] != match["match_id"] {
		t.Fatalf("players landed in different matches")
	}

	// Cancel flow.
	code, body = mmc.do("POST", "/v1/matchmaking/tickets", ticketBody("p3", "1.0.0", "eu", 10))
	if code != 201 {
		t.Fatalf("third ticket: %d %v", code, body)
	}
	id3 := body["ticket_id"].(string)
	if code, body = mmc.do("DELETE", "/v1/matchmaking/tickets/"+id3, nil); code != 200 || body["status"] != "cancelled" {
		t.Fatalf("cancel: %d %v", code, body)
	}
	if code, body = mmc.do("GET", "/v1/matchmaking/tickets/"+id3, nil); code != 200 || body["status"] != "cancelled" {
		t.Fatalf("cancelled ticket read: %d %v", code, body)
	}

	// update_required is reported straight from POST.
	code, body = mmc.do("POST", "/v1/matchmaking/tickets", ticketBody("p4", "0.1.0", "eu", 10))
	if code != 201 || body["status"] != "update_required" {
		t.Fatalf("old client: %d %v", code, body)
	}

	// Unknown ticket and bad wait.
	if code, _ = mmc.do("GET", "/v1/matchmaking/tickets/"+uuid.NewString(), nil); code != 404 {
		t.Fatalf("unknown ticket: want 404, got %d", code)
	}
	if code, _ = mmc.do("GET", "/v1/matchmaking/tickets/"+id2+"?wait=bogus", nil); code != 400 {
		t.Fatalf("bad wait: want 400, got %d", code)
	}
}

// 5 rps per player_id (protocol.md §3): the 6th instant request is rejected.
func TestMatchmakingRateLimit(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	ts := mmServer(t, st, matchmaker.Config{})
	mmc := &client{t: t, base: ts.URL, key: mmKey(t, st)}

	// Warm the bcrypt auth cache so the burst below is actually instant
	// (an unknown ticket id is not attributed to any player bucket).
	if code, _ := mmc.do("GET", "/v1/matchmaking/tickets/"+uuid.NewString(), nil); code != 404 {
		t.Fatalf("warm-up: want 404, got %d", code)
	}

	var ok, limited int
	for range 8 {
		code, body := mmc.do("POST", "/v1/matchmaking/tickets", ticketBody("spam", "1.0.0", "eu", 10))
		switch code {
		case 201:
			ok++
		case 429:
			if body["error"] != "rate_limited" {
				t.Fatalf("429 body: %v", body)
			}
			limited++
		default:
			t.Fatalf("unexpected code %d %v", code, body)
		}
	}
	if ok < 5 || limited < 1 {
		t.Fatalf("rate limit: %d ok / %d limited (want >=5 / >=1)", ok, limited)
	}

	// Another player is unaffected.
	if code, _ := mmc.do("POST", "/v1/matchmaking/tickets", ticketBody("calm", "1.0.0", "eu", 10)); code != 201 {
		t.Fatalf("other player throttled: %d", code)
	}
}

// GET /v1/qos is public and lists live nodes only.
func TestQoSEndpoint(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ts := mmServer(t, st, matchmaker.Config{})
	anon := &client{t: t, base: ts.URL}

	code, body := anon.do("GET", "/v1/qos", nil)
	if code != 200 {
		t.Fatalf("qos: %d %v", code, body)
	}
	eps := body["qos"].([]any)
	if len(eps) != 1 {
		t.Fatalf("qos endpoints: %v", body)
	}
	ep := eps[0].(map[string]any)
	if ep["region"] != "eu" || ep["host"] != "203.0.113.10" || int(ep["udp_port"].(float64)) != 19999 {
		t.Fatalf("qos endpoint: %v", ep)
	}

	// A silent node drops off the list.
	f.SetHeartbeatAge(t, f.NodeID, time.Minute)
	code, body = anon.do("GET", "/v1/qos", nil)
	if code != 200 || len(body["qos"].([]any)) != 0 {
		t.Fatalf("stale node still listed: %d %v", code, body)
	}
}

// match_size is per project (PUT /v1/projects/{slug}): with 3 the match
// forms only when the third player arrives.
func TestProjectMatchSize(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	ts := mmServer(t, st, matchmaker.Config{})

	_, adminKey, err := st.CreateAPIKey(t.Context(), "admin", []string{httpapi.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	admin := &client{t: t, base: ts.URL, key: adminKey}
	mmc := &client{t: t, base: ts.URL, key: mmKey(t, st)}

	// Scope: matchmaking key cannot change project settings.
	if code, _ := mmc.do("PUT", "/v1/projects/game", map[string]any{"match_size": 3}); code != 403 {
		t.Fatalf("matchmaking key set match_size: want 403, got %d", code)
	}
	code, body := admin.do("PUT", "/v1/projects/game", map[string]any{"match_size": 3})
	if code != 200 {
		t.Fatalf("set match_size: %d %v", code, body)
	}
	if ms := body["project"].(map[string]any)["match_size"].(float64); int(ms) != 3 {
		t.Fatalf("match_size not applied: %v", body)
	}
	if code, _ := admin.do("PUT", "/v1/projects/game", map[string]any{"match_size": 0}); code != 400 {
		t.Fatalf("match_size 0: want 400, got %d", code)
	}

	ids := make([]string, 0, 3)
	for _, p := range []string{"p1", "p2"} {
		code, body := mmc.do("POST", "/v1/matchmaking/tickets", ticketBody(p, "1.0.0", "eu", 10))
		if code != 201 {
			t.Fatalf("ticket %s: %d %v", p, code, body)
		}
		ids = append(ids, body["ticket_id"].(string))
	}
	time.Sleep(200 * time.Millisecond) // several ticks pass
	if code, body := mmc.do("GET", "/v1/matchmaking/tickets/"+ids[0], nil); code != 200 || body["status"] != "queued" {
		t.Fatalf("2 of 3 players must stay queued: %d %v", code, body)
	}

	code, body = mmc.do("POST", "/v1/matchmaking/tickets", ticketBody("p3", "1.0.0", "eu", 10))
	if code != 201 {
		t.Fatalf("third ticket: %d %v", code, body)
	}
	ids = append(ids, body["ticket_id"].(string))

	matchIDs := map[string]bool{}
	for _, id := range ids {
		code, body, err := getJSON(ts.URL, "/v1/matchmaking/tickets/"+id+"?wait=10s", mmc.key)
		if err != nil || code != 200 || body["status"] != "matched" {
			t.Fatalf("ticket %s: %d %v %v", id, code, body, err)
		}
		matchIDs[body["match"].(map[string]any)["match_id"].(string)] = true
	}
	if len(matchIDs) != 1 {
		t.Fatalf("want one shared match, got %v", matchIDs)
	}
}
