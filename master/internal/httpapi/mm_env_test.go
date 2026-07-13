package httpapi_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Matchmaking / QoS / allocate environment resolution + enforcement over REST
// (docs/superpowers/specs/2026-07-13-environments-v1-design.md §3, §5). The
// matchmaker itself never learns about keys: handleCreateTicket resolves the
// (project, env) binding before Submit, QoS reads ?env&?project, allocate
// resolves env (explicit → sole-with-ready → 409) and enforces the key binding.

// twoEnvStand builds a project "game" with a live dev half (node + version +
// fleet) and a live prod half (node moved to prod + prod version + prod fleet),
// both in region eu. Returns the fixture.
func twoEnvStand(t *testing.T, st *store.Store) *testdb.Fixture {
	t.Helper()
	f := testdb.Seed(t, st, "eu", 10) // dev node (203.0.113.10), dev version 1.0.0
	f.UpsertFleet(t, 2, 50)
	ctx := t.Context()
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatalf("move node to prod: %v", err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	buffer, maxServers := int32(2), int32(50)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu", ActiveVersion: &prodV,
		BufferReady: &buffer, MaxServers: &maxServers,
	}); err != nil {
		t.Fatalf("prod fleet: %v", err)
	}
	return f
}

// ticketBodyEnv is ticketBody (mm_api_test.go) with optional project/env fields.
func ticketBodyEnv(player, version, project, env string, regs ...any) map[string]any {
	b := ticketBody(player, version, regs...)
	if project != "" {
		b["project"] = project
	}
	if env != "" {
		b["env"] = env
	}
	return b
}

// GET /v1/qos?env= scopes ping targets to one environment's nodes; without env,
// a stand whose several environments carry active nodes answers 400 env_required.
func TestQoSEnvScoping(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev node 203.0.113.10
	ctx := t.Context()
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatalf("move node to prod: %v", err)
	}
	ts, _, _ := deployServer(t, st)
	anon := &client{t: t, base: ts.URL}

	// dev: only the dev node.
	code, body := anon.do("GET", "/v1/qos?env=dev", nil)
	eps, _ := body["qos"].([]any)
	if code != 200 || len(eps) != 1 || eps[0].(map[string]any)["host"] != "203.0.113.10" {
		t.Fatalf("qos dev: %d %v", code, body)
	}
	// prod: only the prod node.
	code, body = anon.do("GET", "/v1/qos?env=prod", nil)
	eps, _ = body["qos"].([]any)
	if code != 200 || len(eps) != 1 || eps[0].(map[string]any)["host"] != "203.0.113.30" {
		t.Fatalf("qos prod: %d %v", code, body)
	}
	// no env, two envs with active nodes → 400 env_required.
	code, body = anon.do("GET", "/v1/qos", nil)
	if code != 400 || body["error"] != "env_required" {
		t.Fatalf("qos ambiguous: want 400 env_required, got %d %v", code, body)
	}
}

// A matchmaking key bound to (game, dev) defaults project AND env for the ticket
// and may act only there; an explicit field disagreeing with the binding → 403.
func TestTicketBindingEnforcement(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	mkKey := func(project, env *string) string {
		t.Helper()
		_, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
			Name: "mm", Scopes: []string{httpapi.ScopeMatchmaking}, Project: project, Env: env,
		})
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		return secret
	}
	game, dev := "game", "dev"
	boundDev := &client{t: t, base: ts.URL, key: mkKey(&game, &dev)}
	global := &client{t: t, base: ts.URL, key: mkKey(nil, nil)}

	// bound(dev) key, no fields → ticket defaults to game/dev.
	code, body := boundDev.do("POST", "/v1/matchmaking/tickets", ticketBody("p1", "1.0.0", "eu", 10))
	if code != 201 || body["status"] != "queued" {
		t.Fatalf("bound dev ticket: %d %v", code, body)
	}
	if body["project"] != "game" || body["env"] != "dev" {
		t.Fatalf("binding must default project+env, got %v", body)
	}

	// bound(dev) key with explicit env=prod → 403 with the binding text.
	code, body = boundDev.do("POST", "/v1/matchmaking/tickets", ticketBodyEnv("p1", "1.0.0", "", "prod", "eu", 10))
	if code != 403 {
		t.Fatalf("bound ticket prod env: want 403, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); !strings.Contains(d, "key is bound to game/dev") {
		t.Fatalf("403 must name the binding, got %q", d)
	}

	// bound(dev) key with explicit project=other → 403.
	if code, _ := boundDev.do("POST", "/v1/matchmaking/tickets", ticketBodyEnv("p1", "1.0.0", "other", "", "eu", 10)); code != 403 {
		t.Fatalf("bound ticket foreign project: want 403, got %d", code)
	}

	// global mm key: explicit env is honoured.
	code, body = global.do("POST", "/v1/matchmaking/tickets", ticketBodyEnv("p2", "1.0.0", "game", "dev", "eu", 10))
	if code != 201 || body["env"] != "dev" {
		t.Fatalf("global explicit env: %d %v", code, body)
	}
}

// /v1/allocate resolves env (explicit → sole-with-ready → 409) and enforces the
// key binding on the resolved env.
func TestAllocateEnvResolution(t *testing.T) {
	st := testdb.New(t)
	f := twoEnvStand(t, st)
	prodNodeIP := "203.0.113.30"
	// One ready server per env in region eu.
	devSrv := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	prodNode := prodNodeID(t, st, prodNodeIP)
	prodV := versionID(t, st, "game", "prod", "2.0.0")
	prodSrv := f.InsertServer(t, prodNode, prodV, "ready", 20002, 0)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	mkKey := func(project, env *string) string {
		t.Helper()
		_, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
			Name: "alloc", Scopes: []string{httpapi.ScopeAllocate}, Project: project, Env: env,
		})
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		return secret
	}
	game, dev := "game", "dev"
	globalAlloc := &client{t: t, base: ts.URL, key: mkKey(nil, nil)}
	boundDev := &client{t: t, base: ts.URL, key: mkKey(&game, &dev)}

	// explicit env=dev → claims the dev server.
	code, body := globalAlloc.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "env": "dev", "region": "eu", "match_id": uuid.NewString(),
	})
	if code != 200 || body["server_id"] != devSrv {
		t.Fatalf("allocate env=dev: want dev server, got %d %v", code, body)
	}
	// explicit env=prod → claims the prod server.
	code, body = globalAlloc.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "env": "prod", "region": "eu", "match_id": uuid.NewString(),
	})
	if code != 200 || body["server_id"] != prodSrv {
		t.Fatalf("allocate env=prod: want prod server, got %d %v", code, body)
	}

	// no env, both envs have ready servers → 409 env_required.
	devSrv2 := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20003, 0)
	f.InsertServer(t, prodNode, prodV, "ready", 20004, 0)
	code, body = globalAlloc.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "region": "eu", "match_id": uuid.NewString(),
	})
	if code != 409 || body["error"] != "env_required" {
		t.Fatalf("allocate no env: want 409 env_required, got %d %v", code, body)
	}

	// bound(dev) key on env=prod → 403.
	code, body = boundDev.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "env": "prod", "region": "eu", "match_id": uuid.NewString(),
	})
	if code != 403 {
		t.Fatalf("bound-dev allocate prod: want 403, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); !strings.Contains(d, "key is bound to game/dev") {
		t.Fatalf("403 must name the binding, got %q", d)
	}
	// bound(dev) key on env=dev → claims the dev server (its own env).
	code, body = boundDev.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "env": "dev", "region": "eu", "match_id": uuid.NewString(),
	})
	if code != 200 || body["server_id"] != devSrv2 {
		t.Fatalf("bound-dev allocate dev: want dev server, got %d %v", code, body)
	}
}

// A bound(game,dev) allocate key without an env field resolves env from its
// binding (§3: explicit → key binding → sole → 409), so it succeeds even when
// BOTH dev and prod carry ready servers — where the sole-with-ready fallback
// alone would answer 409 env_required (W-I2).
func TestAllocateBoundKeyDefaultsEnv(t *testing.T) {
	st := testdb.New(t)
	f := twoEnvStand(t, st)
	devSrv := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	prodNode := prodNodeID(t, st, "203.0.113.30")
	prodV := versionID(t, st, "game", "prod", "2.0.0")
	f.InsertServer(t, prodNode, prodV, "ready", 20002, 0)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	game, dev := "game", "dev"
	_, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "alloc", Scopes: []string{httpapi.ScopeAllocate}, Project: &game, Env: &dev,
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	boundDev := &client{t: t, base: ts.URL, key: secret}

	// No env field, ready servers in BOTH envs: the key-binding step resolves env
	// to dev before the sole-with-ready fallback would 409.
	code, body := boundDev.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "region": "eu", "match_id": uuid.NewString(),
	})
	if code != 200 || body["server_id"] != devSrv {
		t.Fatalf("bound-dev allocate without env: want 200 dev server, got %d %v", code, body)
	}
}

// A bound-CI key POSTing a version without env gets «env is required» (400),
// not the binding 403 — empty-env validation runs before the binding guard,
// parity with handleUpsertFleet (§5, w10).
func TestCreateVersionEnvRequiredBeforeBinding(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10) // project game with seeded dev+prod
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	game, dev := "game", "dev"
	_, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ci", Scopes: []string{httpapi.ScopeDeploy}, Project: &game, Env: &dev,
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ci := &client{t: t, base: ts.URL, key: secret}

	code, body := ci.do("POST", "/v1/versions", map[string]any{
		"semver": "1.2.0", "image_ref": "ghcr.io/example/game:1.2.0",
	})
	if code != 400 {
		t.Fatalf("version without env: want 400 «env is required», got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); !strings.Contains(d, "env is required") {
		t.Fatalf("want «env is required», got %q", d)
	}
}

// prodNodeID returns the id of the node with the given public IP.
func prodNodeID(t *testing.T, st *store.Store, ip string) string {
	t.Helper()
	var id string
	if err := st.Pool.QueryRow(t.Context(),
		`select id::text from nodes where host(public_ip) = $1`, ip).Scan(&id); err != nil {
		t.Fatalf("prod node id: %v", err)
	}
	return id
}

// versionID returns the id of a (project, env, semver) version.
func versionID(t *testing.T, st *store.Store, project, env, semver string) string {
	t.Helper()
	var id string
	if err := st.Pool.QueryRow(t.Context(), `
		select v.id::text from versions v join projects p on p.id = v.project_id
		where p.slug = $1 and v.env = $2 and v.semver = $3`, project, env, semver).Scan(&id); err != nil {
		t.Fatalf("version id: %v", err)
	}
	return id
}
