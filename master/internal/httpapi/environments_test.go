package httpapi_test

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func envAPIServer(t *testing.T, st *store.Store) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts.Close)
	return ts
}

// Environments API (docs/superpowers/specs/2026-07-13-environments-v1-design.md
// §2): scopes, guardrails, delete-used 409, node-move PATCH.
func TestEnvironmentsAPI(t *testing.T) {
	st := testdb.New(t)
	ctx := t.Context()
	f := testdb.Seed(t, st, "eu", 10) // project game (dev+prod seeded), one dev node + version

	ts := envAPIServer(t, st)
	mk := func(scope string) *client {
		_, key, err := st.CreateAPIKey(ctx, scope, []string{scope})
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, key: key}
	}
	admin := mk(httpapi.ScopeAdmin)
	ro := mk(httpapi.ScopeReadonly)

	// List (readonly), explicit project.
	code, body := ro.do("GET", "/v1/environments?project=game", nil)
	if code != 200 || len(body["environments"].([]any)) != 2 {
		t.Fatalf("list envs: %d %v", code, body)
	}
	// List with no project resolves the sole project.
	if code, body := ro.do("GET", "/v1/environments", nil); code != 200 || len(body["environments"].([]any)) != 2 {
		t.Fatalf("list envs sole-project: %d %v", code, body)
	}

	// Create (admin).
	code, body = admin.do("POST", "/v1/environments", map[string]any{
		"project": "game", "name": "staging", "auto_deploy": true, "retention_keep": 5,
	})
	if code != 201 || body["environment"].(map[string]any)["name"] != "staging" {
		t.Fatalf("create env: %d %v", code, body)
	}
	// Readonly key cannot create.
	if code, _ := ro.do("POST", "/v1/environments", map[string]any{"project": "game", "name": "x"}); code != 403 {
		t.Fatalf("readonly create: want 403, got %d", code)
	}
	// Guardrail: production + auto_deploy → 400.
	if code, _ := admin.do("POST", "/v1/environments", map[string]any{
		"project": "game", "name": "bad", "production": true, "auto_deploy": true,
	}); code != 400 {
		t.Fatalf("production+auto_deploy: want 400, got %d", code)
	}
	// Reserved name → 400.
	if code, _ := admin.do("POST", "/v1/environments", map[string]any{"project": "game", "name": "all"}); code != 400 {
		t.Fatalf("reserved name: want 400, got %d", code)
	}
	// Duplicate → 409.
	if code, _ := admin.do("POST", "/v1/environments", map[string]any{"project": "game", "name": "staging"}); code != 409 {
		t.Fatalf("duplicate env: want 409, got %d", code)
	}

	// Patch (admin): disable auto_deploy on staging.
	if code, _ := admin.do("PATCH", "/v1/environments/game/staging", map[string]any{"auto_deploy": false}); code != 200 {
		t.Fatalf("patch env: want 200, got %d", code)
	}
	// Patch guardrail: enabling auto_deploy on prod (production) → 400.
	if code, _ := admin.do("PATCH", "/v1/environments/game/prod", map[string]any{"auto_deploy": true}); code != 400 {
		t.Fatalf("patch auto_deploy on prod: want 400, got %d", code)
	}
	// Patch unknown → 404.
	if code, _ := admin.do("PATCH", "/v1/environments/game/ghost", map[string]any{"retention_keep": 1}); code != 404 {
		t.Fatalf("patch unknown env: want 404, got %d", code)
	}

	// Delete unused → 204; delete used (dev has node+version) → 409.
	if code, _ := admin.do("DELETE", "/v1/environments/game/staging", nil); code != 204 {
		t.Fatalf("delete unused env: want 204, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/environments/game/dev", nil); code != 409 {
		t.Fatalf("delete used env: want 409, got %d", code)
	}

	// Node move (admin): a fresh empty node moves dev→prod.
	n2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	code, body = admin.do("PATCH", "/v1/nodes/"+n2, map[string]any{"env": "prod"})
	if code != 200 || body["node"].(map[string]any)["env"] != "prod" {
		t.Fatalf("move node: %d %v", code, body)
	}
	// Non-uuid id → 400; unknown env → 404; unknown node → 404.
	if code, _ := admin.do("PATCH", "/v1/nodes/not-a-uuid", map[string]any{"env": "prod"}); code != 400 {
		t.Fatalf("bad node id: want 400, got %d", code)
	}
	if code, _ := admin.do("PATCH", "/v1/nodes/"+n2, map[string]any{"env": "ghost"}); code != 404 {
		t.Fatalf("move to missing env: want 404, got %d", code)
	}
	if code, _ := admin.do("PATCH", "/v1/nodes/"+uuid.NewString(), map[string]any{"env": "prod"}); code != 404 {
		t.Fatalf("move unknown node: want 404, got %d", code)
	}
}
