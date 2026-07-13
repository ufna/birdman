package httpapi_test

import (
	"strings"
	"testing"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// API-key (project, env) binding + deploy-surface enforcement
// (docs/superpowers/specs/2026-07-13-environments-v1-design.md §5). A NULL pair
// is a global key (the pre-env default, allowed everywhere); a set pair scopes
// the key to exactly one (project, env) on versions/deploy/rollback/fleets. A
// binding is strictly a pair and incompatible with the admin scope.

// TestAPIKeyBindingCreate is the create-validation matrix (spec §5, §10):
// bound-pair → 201 with the binding echoed; half-pair → 400; admin+binding →
// 400; unknown project/env → 400; global (no binding) still works.
func TestAPIKeyBindingCreate(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10) // project "game" with dev+prod seeded
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	_, adminSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "admin", Scopes: []string{httpapi.ScopeAdmin},
	})
	if err != nil {
		t.Fatal(err)
	}
	admin := &client{t: t, base: ts.URL, key: adminSecret}

	// Bound key (project+env pair) → 201, binding echoed in the response.
	code, body := admin.do("POST", "/v1/apikeys", map[string]any{
		"name": "ci-dev", "scopes": []string{"deploy"}, "project": "game", "env": "dev",
	})
	if code != 201 {
		t.Fatalf("bound create: want 201, got %d %v", code, body)
	}
	key := body["key"].(map[string]any)
	if key["project"] != "game" || key["env"] != "dev" {
		t.Fatalf("bound key must echo its binding: %v", key)
	}

	// project without env → 400 (not a raw CHECK error).
	if code, _ := admin.do("POST", "/v1/apikeys", map[string]any{
		"name": "x", "scopes": []string{"deploy"}, "project": "game",
	}); code != 400 {
		t.Fatalf("project without env: want 400, got %d", code)
	}
	// env without project → 400.
	if code, _ := admin.do("POST", "/v1/apikeys", map[string]any{
		"name": "x", "scopes": []string{"deploy"}, "env": "dev",
	}); code != 400 {
		t.Fatalf("env without project: want 400, got %d", code)
	}
	// admin scope + binding → 400 (I8).
	if code, _ := admin.do("POST", "/v1/apikeys", map[string]any{
		"name": "x", "scopes": []string{"admin"}, "project": "game", "env": "dev",
	}); code != 400 {
		t.Fatalf("admin+binding: want 400, got %d", code)
	}
	// non-existent env → 400.
	if code, _ := admin.do("POST", "/v1/apikeys", map[string]any{
		"name": "x", "scopes": []string{"deploy"}, "project": "game", "env": "staging",
	}); code != 400 {
		t.Fatalf("unknown env: want 400, got %d", code)
	}
	// non-existent project → 400.
	if code, _ := admin.do("POST", "/v1/apikeys", map[string]any{
		"name": "x", "scopes": []string{"deploy"}, "project": "ghost", "env": "dev",
	}); code != 400 {
		t.Fatalf("unknown project: want 400, got %d", code)
	}

	// Global key (no binding) still works and carries no binding fields.
	code, body = admin.do("POST", "/v1/apikeys", map[string]any{
		"name": "global", "scopes": []string{"deploy"},
	})
	if code != 201 {
		t.Fatalf("global create: want 201, got %d %v", code, body)
	}
	if _, bound := body["key"].(map[string]any)["project"]; bound {
		t.Fatalf("global key must not carry a binding: %v", body["key"])
	}
}

// TestAPIKeyBindingEnforcement is the deploy-surface enforcement matrix (spec
// §5, §10): a key bound to (game, dev) may act only in game/dev; a global key
// and an admin key are unrestricted.
func TestAPIKeyBindingEnforcement(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev node + dev version 1.0.0; envs dev+prod
	prodV := f.AddVersion(t, "2.0.0", "prod")
	f.UpsertFleet(t, 2, 50) // dev fleet, active_version 1.0.0
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	// Isolate key-binding enforcement from the W2 dev auto-deploy: with
	// auto_deploy on (the dev seed default), registering a dev version below would
	// occupy the dev deploy slot and turn the positive manual-deploy control into
	// a 409. Auto-deploy has its own matrix (deploy/autodeploy_test.go).
	autoOff := false
	if _, err := st.PatchEnvironment(ctx, "game", "dev", store.EnvironmentPatch{AutoDeploy: &autoOff}); err != nil {
		t.Fatalf("disable dev auto_deploy: %v", err)
	}

	mkKey := func(name string, scopes []string, project, env *string) string {
		t.Helper()
		_, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
			Name: name, Scopes: scopes, Project: project, Env: env,
		})
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		return secret
	}
	game, dev := "game", "dev"
	boundDev := &client{t: t, base: ts.URL, key: mkKey("ci-dev", []string{httpapi.ScopeDeploy}, &game, &dev)}
	global := &client{t: t, base: ts.URL, key: mkKey("ci-global", []string{httpapi.ScopeDeploy}, nil, nil)}
	admin := &client{t: t, base: ts.URL, key: mkKey("admin", []string{httpapi.ScopeAdmin}, nil, nil)}

	// --- bound(game,dev) deploy key: refused outside its own (project, env) ---

	// versions in its own env → 201; project defaults from the binding (no field).
	if code, body := boundDev.do("POST", "/v1/versions", map[string]any{
		"semver": "1.2.0", "image_ref": "ghcr.io/example/game:1.2.0", "env": "dev",
	}); code != 201 {
		t.Fatalf("bound dev version: want 201, got %d %v", code, body)
	}
	// versions in prod → 403 with the exact binding text.
	code, body := boundDev.do("POST", "/v1/versions", map[string]any{
		"semver": "9.9.9", "image_ref": "ghcr.io/example/game:9.9.9", "env": "prod",
	})
	if code != 403 {
		t.Fatalf("bound prod version: want 403, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); !strings.Contains(d, "key is bound to game/dev") {
		t.Fatalf("403 must name the binding, got %q", d)
	}
	// deploy of a prod version → 403 (target = version's own env, loaded first).
	if code, _ := boundDev.do("POST", "/v1/deploy", map[string]any{"version_id": prodV}); code != 403 {
		t.Fatalf("bound deploy prod: want 403, got %d", code)
	}
	// rollback with env=prod → 403 (enforced after the env resolve).
	if code, _ := boundDev.do("POST", "/v1/rollback", map[string]any{"env": "prod"}); code != 403 {
		t.Fatalf("bound rollback prod: want 403, got %d", code)
	}
	// fleets in prod → 403. The route is admin-scoped, so a deploy-scoped bound
	// key is refused at the scope gate; binding on the fleet surface is enforced
	// too (defense in depth) — see TestAPIKeyBindingFleetGuard.
	if code, _ := boundDev.do("PUT", "/v1/fleets/eu", map[string]any{
		"project": "game", "env": "prod",
	}); code != 403 {
		t.Fatalf("bound fleet prod: want 403, got %d", code)
	}
	// positive: deploy in its own (dev) env → 2xx.
	if code, body := boundDev.do("POST", "/v1/deploy", map[string]any{"version_id": f.VersionID}); code/100 != 2 {
		t.Fatalf("bound deploy dev: want 2xx, got %d %v", code, body)
	}

	// --- global deploy key: unrestricted across envs (a bound-to-dev key would
	// 403 the exact same request) ---
	if code, body := global.do("POST", "/v1/versions", map[string]any{
		"project": "game", "semver": "3.0.0", "image_ref": "ghcr.io/example/game:3.0.0", "env": "prod",
	}); code != 201 {
		t.Fatalf("global prod version: want 201, got %d %v", code, body)
	}

	// --- admin key: unrestricted, including the admin-scoped fleet route ---
	if code, body := admin.do("PUT", "/v1/fleets/eu", map[string]any{
		"project": "game", "env": "prod", "active_version": prodV,
	}); code != 200 {
		t.Fatalf("admin prod fleet: want 200, got %d %v", code, body)
	}
}

// TestAPIKeyBindingFleetGuard covers the binding guard inside handleUpsertFleet
// directly. PUT /v1/fleets is admin-scoped and an admin key can never be bound
// via the API (create rejects admin+binding), so the guard is defense in depth:
// the only way a binding reaches this handler is a row bound out-of-band. We
// craft exactly that (admin key, bound to game/dev via SQL — the DB permits it,
// only the API forbids it) and prove the handler refuses a prod target with the
// binding text while allowing its own env.
func TestAPIKeyBindingFleetGuard(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10) // project game with dev+prod
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	k, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "admin", Scopes: []string{httpapi.ScopeAdmin},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bind the admin key to game/dev out-of-band (before its first use, so the
	// auth cache never sees the unbound shape). The DB CHECK only enforces
	// pair-parity, not admin-incompatibility, so this row is valid.
	if _, err := st.Pool.Exec(ctx, `
		update api_keys
		set project_id = (select id from projects where slug = 'game'), env = 'dev'
		where id = $1::uuid`, k.ID); err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, base: ts.URL, key: secret}

	// Foreign env (prod) → 403 with the binding text (handler guard, not scope).
	code, body := c.do("PUT", "/v1/fleets/eu", map[string]any{"project": "game", "env": "prod"})
	if code != 403 {
		t.Fatalf("bound-admin fleet prod: want 403, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); !strings.Contains(d, "key is bound to game/dev") {
		t.Fatalf("403 must name the binding, got %q", d)
	}
	// Its own env (dev) → 200 (the guard is exact, not a blanket refusal).
	if code, body := c.do("PUT", "/v1/fleets/eu", map[string]any{"project": "game", "env": "dev"}); code != 200 {
		t.Fatalf("bound-admin fleet dev: want 200, got %d %v", code, body)
	}
}
