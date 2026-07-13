package httpapi_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// seedMatch allocates a server row and records a match over it, pinning
// created_at so list ordering is deterministic.
func seedMatch(t *testing.T, st *store.Store, f *testdb.Fixture, region, state string,
	port int32, age time.Duration) string {
	t.Helper()
	ctx := context.Background()
	serverID := f.InsertServer(t, f.NodeID, f.VersionID, "allocated", port, age)
	matchID := uuid.NewString()
	if err := st.RecordMatch(ctx, matchID, f.Project, region, serverID, f.VersionID, f.Env); err != nil {
		t.Fatal(err)
	}
	_, err := st.Pool.Exec(ctx, `
		update matches set state = $2, created_at = now() - $3::interval,
		       started_at = case when $2 in ('running','finished') then now() - $3::interval end
		where id = $1::uuid`,
		matchID, state, fmt.Sprintf("%d milliseconds", age.Milliseconds()))
	if err != nil {
		t.Fatal(err)
	}
	return matchID
}

// TestMatchesAPI: GET /v1/matches list with filters/pagination and
// GET /v1/matches/{id} (docs/specs/master.md §6).
func TestMatchesAPI(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ts := apiServer(t, st)
	ctx := t.Context()

	m1 := seedMatch(t, st, f, "eu", "pending", 20001, 3*time.Minute)
	m2 := seedMatch(t, st, f, "eu", "running", 20002, 2*time.Minute)
	m3 := seedMatch(t, st, f, "us-east", "finished", 20003, time.Minute)
	// Live players on the running match's server.
	if _, err := st.Pool.Exec(ctx, `
		update servers set players = 3
		where id = (select server_id from matches where id = $1::uuid)`, m2); err != nil {
		t.Fatal(err)
	}

	ro := &client{t: t, base: ts.URL, key: scopedKey(t, st, "ro", httpapi.ScopeReadonly)}

	// Auth: anon 401; matchmaking scope must not read matches.
	if code, _ := (&client{t: t, base: ts.URL}).do("GET", "/v1/matches", nil); code != 401 {
		t.Fatal("anon matches: want 401")
	}
	mmc := &client{t: t, base: ts.URL, key: scopedKey(t, st, "mm", httpapi.ScopeMatchmaking)}
	if code, _ := mmc.do("GET", "/v1/matches", nil); code != 403 {
		t.Fatal("matchmaking-scope matches: want 403")
	}

	// Full list, newest first, with the joined read model.
	code, body := ro.do("GET", "/v1/matches", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	matches := body["matches"].([]any)
	if len(matches) != 3 {
		t.Fatalf("list size: %v", matches)
	}
	first := matches[0].(map[string]any)
	if first["id"] != m3 || matches[2].(map[string]any)["id"] != m1 {
		t.Fatalf("order: want newest first, got %v", matches)
	}
	if first["semver"] != "1.0.0" || first["host"] != "203.0.113.10" ||
		first["project"] != "game" || int(first["port"].(float64)) != 20003 {
		t.Fatalf("joined fields: %v", first)
	}

	// Filters.
	if _, body = ro.do("GET", "/v1/matches?state=running", nil); len(body["matches"].([]any)) != 1 {
		t.Fatalf("state filter: %v", body)
	}
	running := body["matches"].([]any)[0].(map[string]any)
	if running["id"] != m2 || int(running["server_players"].(float64)) != 3 ||
		running["started_at"] == nil {
		t.Fatalf("running match: %v", running)
	}
	if _, body = ro.do("GET", "/v1/matches?region=us-east", nil); len(body["matches"].([]any)) != 1 {
		t.Fatalf("region filter: %v", body)
	}
	if _, body = ro.do("GET", "/v1/matches?state=finished&region=eu", nil); len(body["matches"].([]any)) != 0 {
		t.Fatalf("combined filter: %v", body)
	}
	if code, _ = ro.do("GET", "/v1/matches?state=bogus", nil); code != 400 {
		t.Fatalf("bad state: want 400, got %d", code)
	}

	// Pagination.
	if _, body = ro.do("GET", "/v1/matches?limit=1", nil); body["matches"].([]any)[0].(map[string]any)["id"] != m3 {
		t.Fatalf("limit: %v", body)
	}
	if _, body = ro.do("GET", "/v1/matches?limit=1&offset=1", nil); body["matches"].([]any)[0].(map[string]any)["id"] != m2 {
		t.Fatalf("offset: %v", body)
	}
	if code, _ = ro.do("GET", "/v1/matches?limit=x", nil); code != 400 {
		t.Fatalf("bad limit: want 400, got %d", code)
	}

	// Single match.
	code, body = ro.do("GET", "/v1/matches/"+m2, nil)
	if code != 200 || body["match"].(map[string]any)["state"] != "running" {
		t.Fatalf("get match: %d %v", code, body)
	}
	if code, _ = ro.do("GET", "/v1/matches/"+uuid.NewString(), nil); code != 404 {
		t.Fatalf("unknown match: want 404, got %d", code)
	}
	if code, _ = ro.do("GET", "/v1/matches/not-a-uuid", nil); code != 400 {
		t.Fatalf("bad match id: want 400, got %d", code)
	}
}

// TestPanelMountedOnRoot: the API binary serves the panel (placeholder when
// no build is embedded) at `/`, and unknown /v1 paths stay JSON.
func TestPanelMountedOnRoot(t *testing.T) {
	st := testdb.New(t)
	ts := apiServer(t, st)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET /: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(raw), "birdman") {
		t.Fatalf("GET / body: %.120s", raw)
	}

	code, body := (&client{t: t, base: ts.URL}).do("GET", "/v1/definitely-not-an-endpoint", nil)
	if code != 404 || body["error"] != "not_found" {
		t.Fatalf("unknown /v1 path: %d %v", code, body)
	}
}
