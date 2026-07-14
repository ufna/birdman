package httpapi_test

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
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
		_, key, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: scope, Scopes: []string{scope}})
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

	// Delete unused → 204 (пустое окружение, confirm не нужен); delete dev (нода на
	// нём) → 409 предусловия «сначала переведите ноды».
	if code, _ := admin.do("DELETE", "/v1/environments/game/staging", nil); code != 204 {
		t.Fatalf("delete unused env: want 204, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/environments/game/dev", nil); code != 409 {
		t.Fatalf("delete env with nodes: want 409, got %d", code)
	}

	// Node move (admin): a fresh empty node moves dev→prod.
	n2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	code, body = admin.do("PATCH", "/v1/nodes/"+n2, map[string]any{"env": "prod"})
	if code != 200 || body["node"].(map[string]any)["env"] != "prod" {
		t.Fatalf("move node: %d %v", code, body)
	}
	// Non-uuid id → 400; unknown env → 400; unknown node → 404. Несуществующий env
	// в ТЕЛЕ запроса — плохой ввод (единый ErrBadEnv → 400 «no such environment
	// <project>/<env>», v3), а не отсутствующий ресурс; 404 остаётся за самой нодой.
	if code, _ := admin.do("PATCH", "/v1/nodes/not-a-uuid", map[string]any{"env": "prod"}); code != 400 {
		t.Fatalf("bad node id: want 400, got %d", code)
	}
	code, body = admin.do("PATCH", "/v1/nodes/"+n2, map[string]any{"env": "ghost"})
	if code != 400 {
		t.Fatalf("move to missing env: want 400, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); d != "no such environment game/ghost" {
		t.Fatalf("400 detail: want «no such environment game/ghost», got %q", d)
	}
	if code, _ := admin.do("PATCH", "/v1/nodes/"+uuid.NewString(), map[string]any{"env": "prod"}); code != 404 {
		t.Fatalf("move unknown node: want 404, got %d", code)
	}
}

// TestNodeCreateEnv (w2): POST /v1/nodes принимает необязательное поле env.
// Ключевой сценарий — удалённый оператором dev: регистрация ноды без env целится
// в дефолтный dev, и раньше ensureProject молча ВОСКРЕШАЛ бы его (сев шёл при
// каждом касании проекта). Теперь сев только при вставке проекта, а такая
// регистрация — честный 400 «no such environment»; чтобы завести ноду, оператор
// называет живое окружение явно.
func TestNodeCreateEnv(t *testing.T) {
	st := testdb.New(t)
	ctx := t.Context()
	ts := envAPIServer(t, st)

	_, key, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "admin", Scopes: []string{httpapi.ScopeAdmin},
	})
	if err != nil {
		t.Fatal(err)
	}
	admin := &client{t: t, base: ts.URL, key: key}

	// Новый проект (dev+prod засеяны при вставке).
	if code, body := admin.do("PUT", "/v1/projects/newgame", map[string]any{"match_size": 4}); code != 200 {
		t.Fatalf("create project: %d %v", code, body)
	}
	// Нода сразу в prod — env в теле.
	code, body := admin.do("POST", "/v1/nodes", map[string]any{
		"project": "newgame", "region": "eu", "hostname": "n-prod",
		"public_ip": "203.0.113.21", "capacity_slots": 4, "env": "prod",
	})
	if code != 201 || body["node"].(map[string]any)["env"] != "prod" {
		t.Fatalf("create node in prod: %d %v", code, body)
	}
	// dev пуст (нода ушла в prod) → удаляется.
	if code, _ := admin.do("DELETE", "/v1/environments/newgame/dev", nil); code != 204 {
		t.Fatalf("delete dev: want 204, got %d", code)
	}

	// Регистрация без env целится в удалённый dev → 400 (не 500 и не воскрешение).
	code, body = admin.do("POST", "/v1/nodes", map[string]any{
		"project": "newgame", "region": "eu", "hostname": "n2",
		"public_ip": "203.0.113.22", "capacity_slots": 4,
	})
	if code != 400 {
		t.Fatalf("node without env into a deleted dev: want 400, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); d != "no such environment newgame/dev" {
		t.Fatalf("400 detail: want «no such environment newgame/dev», got %q", d)
	}
	// Несуществующий env явно → тот же 400.
	if code, _ := admin.do("POST", "/v1/nodes", map[string]any{
		"project": "newgame", "region": "eu", "hostname": "n3",
		"public_ip": "203.0.113.23", "capacity_slots": 4, "env": "ghost",
	}); code != 400 {
		t.Fatalf("node with an unknown env: want 400, got %d", code)
	}
	// И главное: dev не воскрес ни от одного из этих касаний проекта.
	code, body = admin.do("GET", "/v1/environments?project=newgame", nil)
	envs, _ := body["environments"].([]any)
	if code != 200 || len(envs) != 1 {
		t.Fatalf("после удаления dev у проекта обязан остаться только prod: %d %v", code, body)
	}
	if envs[0].(map[string]any)["name"] != "prod" {
		t.Fatalf("dev воскрес: %v", body)
	}
}

// Удаление НЕПУСТОГО окружения (запрос владельца): GET .../usage (readonly) отдаёт
// состав, DELETE требует подтверждения вводом имени и сносит содержимое каскадом.
// Предусловие «ноль нод» сильнее confirm: пока нода в окружении — только 409.
func TestEnvironmentDeleteForce(t *testing.T) {
	st := testdb.New(t)
	ctx := t.Context()
	f := testdb.Seed(t, st, "eu", 10) // dev: нода node-1 + версия 1.0.0

	ts := envAPIServer(t, st)
	mk := func(scope string) *client {
		_, key, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: scope, Scopes: []string{scope}})
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, key: key}
	}
	admin := mk(httpapi.ScopeAdmin)
	ro := mk(httpapi.ScopeReadonly)

	// Состав окружения — readonly-скоуп (панель зовёт его при открытии диалога).
	code, body := ro.do("GET", "/v1/environments/game/dev/usage", nil)
	if code != 200 {
		t.Fatalf("usage (readonly): want 200, got %d %v", code, body)
	}
	usage, _ := body["usage"].(map[string]any)
	if usage["versions"] != float64(1) || usage["nodes"] != float64(1) ||
		usage["fleets"] != float64(0) || usage["servers"] != float64(0) ||
		usage["matches"] != float64(0) || usage["api_keys"] != float64(0) {
		t.Fatalf("usage: %v", usage)
	}
	if code, _ := ro.do("GET", "/v1/environments/game/ghost/usage", nil); code != 404 {
		t.Fatalf("usage of an unknown env: want 404, got %d", code)
	}
	// Удалять readonly-ключ не может (403 раньше любых проверок тела).
	if code, _ := ro.do("DELETE", "/v1/environments/game/dev", map[string]any{"confirm": "dev"}); code != 403 {
		t.Fatalf("readonly delete: want 403, got %d", code)
	}

	// Предусловие: нода в окружении → 409 ДАЖЕ с верным confirm.
	code, body = admin.do("DELETE", "/v1/environments/game/dev", map[string]any{"confirm": "dev"})
	if code != 409 || body["error"] != "conflict" {
		t.Fatalf("delete env with a node: want 409 conflict, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); !strings.Contains(d, "node(s)") {
		t.Fatalf("409 detail must point at the nodes: %q", d)
	}

	// Переводим ноду в prod — окружение непустое (версия), но нод больше нет.
	if code, _ := admin.do("PATCH", "/v1/nodes/"+f.NodeID, map[string]any{"env": "prod"}); code != 200 {
		t.Fatalf("move node to prod: %d", code)
	}
	// Без confirm → 400; с неверным confirm → 400 (тот же detail).
	code, body = admin.do("DELETE", "/v1/environments/game/dev", nil)
	if code != 400 || body["detail"] != "confirm must equal the environment name" {
		t.Fatalf("delete used env without confirm: want 400, got %d %v", code, body)
	}
	if code, _ := admin.do("DELETE", "/v1/environments/game/dev", map[string]any{"confirm": "Dev"}); code != 400 {
		t.Fatalf("wrong confirm: want 400, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/environments/game/dev", map[string]any{"confirm": " dev"}); code != 400 {
		t.Fatalf("confirm with a leading space: want 400, got %d", code)
	}
	// Окружение на месте (ни одна из отбитых попыток ничего не удалила).
	if code, _ := ro.do("GET", "/v1/environments/game/dev/usage", nil); code != 200 {
		t.Fatalf("dev must survive a failed confirm: %d", code)
	}

	// Верный confirm → 200 с составом снесённого.
	code, body = admin.do("DELETE", "/v1/environments/game/dev", map[string]any{"confirm": "dev"})
	if code != 200 {
		t.Fatalf("delete used env with confirm: want 200, got %d %v", code, body)
	}
	deleted, _ := body["deleted"].(map[string]any)
	if deleted["name"] != "dev" || deleted["versions"] != float64(1) ||
		deleted["fleets"] != float64(0) || deleted["api_keys_revoked"] != float64(0) {
		t.Fatalf("200 body must carry what was deleted: %v", body)
	}
	// Окружения больше нет: usage → 404, повторный DELETE → 404.
	if code, _ := ro.do("GET", "/v1/environments/game/dev/usage", nil); code != 404 {
		t.Fatalf("usage after delete: want 404, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/environments/game/dev", map[string]any{"confirm": "dev"}); code != 404 {
		t.Fatalf("delete twice: want 404, got %d", code)
	}
}
