package httpapi_test

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// deployServer wires the REST API with a live deploy manager + recorder.
func deployServer(t *testing.T, st *store.Store) (*httptest.Server, *deploy.Manager, *testdb.CommandRecorder) {
	t.Helper()
	ts, dep, rec, _ := deployServerWithMetrics(t, st)
	return ts, dep, rec
}

// deployServerWithMetrics — тот же стенд, но отдаёт ещё и реестр метрик. Нужен
// с tracker #1003: прометеевская экспозиция больше не висит на API-листенере,
// поэтому тест, читающий счётчик, обязан поднять её отдельно (metricsText) —
// того самого объекта, который держит хендлер API.
func deployServerWithMetrics(t *testing.T, st *store.Store) (*httptest.Server, *deploy.Manager, *testdb.CommandRecorder, *metrics.Metrics) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	rec := &testdb.CommandRecorder{}
	dep := deploy.New(deploy.Options{Store: st, Sender: rec, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts.Close)
	return ts, dep, rec, m
}

// POST /v1/deploy + /v1/rollback over HTTP: scopes, status codes and the full
// prepull → flip → rollback cycle (итерация 3, master.md §5–6).
func TestDeployAndRollbackEndpoints(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	v2 := f.AddVersion(t, "1.1.0", "dev")
	ts, dep, rec := deployServer(t, st)
	ctx := t.Context()

	_, deployKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ci", Scopes: []string{httpapi.ScopeDeploy}})
	if err != nil {
		t.Fatal(err)
	}
	_, roKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})
	if err != nil {
		t.Fatal(err)
	}
	ci := &client{t: t, base: ts.URL, key: deployKey}
	ro := &client{t: t, base: ts.URL, key: roKey}

	// Scope: deploy required.
	if code, _ := ro.do("POST", "/v1/deploy", map[string]any{"version_id": v2}); code != 403 {
		t.Fatalf("readonly deploy: want 403, got %d", code)
	}
	if code, _ := ro.do("POST", "/v1/rollback", map[string]any{}); code != 403 {
		t.Fatalf("readonly rollback: want 403, got %d", code)
	}

	// Validation.
	if code, _ := ci.do("POST", "/v1/deploy", map[string]any{"version_id": "nope"}); code != 400 {
		t.Fatalf("bad version_id: want 400, got %d", code)
	}
	if code, _ := ci.do("POST", "/v1/deploy",
		map[string]any{"version_id": "00000000-0000-0000-0000-000000000000"}); code != 404 {
		t.Fatalf("unknown version: want 404, got %d", code)
	}
	// Nothing deprecated yet → rollback conflicts.
	if code, body := ci.do("POST", "/v1/rollback", map[string]any{}); code != 409 {
		t.Fatalf("premature rollback: want 409, got %d %v", code, body)
	}

	// Deploy: 202 prepulling, PrePull dispatched to the node.
	code, body := ci.do("POST", "/v1/deploy", map[string]any{"version_id": v2})
	if code != 202 {
		t.Fatalf("deploy: want 202, got %d %v", code, body)
	}
	dp := body["deploy"].(map[string]any)
	if dp["state"] != "prepulling" || dp["pending_nodes"] != float64(1) {
		t.Fatalf("deploy body: %v", body)
	}
	var prepulled bool
	for _, c := range rec.Take() {
		if p := c.Msg.GetPrepull(); p != nil && c.NodeID == f.NodeID {
			prepulled = true
		}
	}
	if !prepulled {
		t.Fatal("PrePull not dispatched")
	}

	// Node reports pulled → flip; repeated deploy → 200 active.
	dep.HandlePullReport(f.NodeID, &agentlinkv1.PullReport{
		ImageRef: "ghcr.io/example/game-server:1.1.0", Status: "pulled",
	})
	code, body = ci.do("POST", "/v1/deploy", map[string]any{"version_id": v2})
	if code != 200 || body["deploy"].(map[string]any)["state"] != "active" {
		t.Fatalf("repeat deploy: want 200 active, got %d %v", code, body)
	}

	// Rollback: one call, 200, old version active again.
	code, body = ci.do("POST", "/v1/rollback", map[string]any{})
	if code != 200 {
		t.Fatalf("rollback: want 200, got %d %v", code, body)
	}
	rb := body["rollback"].(map[string]any)
	if rb["old_semver"] != "1.1.0" {
		t.Fatalf("rollback body: %v", body)
	}
	v, err := st.GetVersion(ctx, f.VersionID)
	if err != nil || v.State != "active" {
		t.Fatalf("rolled-back version: %+v %v", v, err)
	}

	// Unknown region → 404, nothing flipped.
	if code, _ := ci.do("POST", "/v1/rollback", map[string]any{"region": "mars"}); code != 404 {
		t.Fatalf("bad region rollback: want 404, got %d", code)
	}
}

// w7: прямой rollback без deprecated-окна → 409. Явный существующий env=dev с
// активной версией, но БЕЗ deprecated → RollbackTarget отдаёт ErrVersionState,
// хендлер маппит в 409 conflict (не 400/500), с внятным «no deprecated version».
func TestRollbackNoDeprecatedVersion(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev version 1.0.0 (registered), dev+prod seeded
	f.UpsertFleet(t, 2, 50)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	_, deployKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ci", Scopes: []string{httpapi.ScopeDeploy}})
	if err != nil {
		t.Fatal(err)
	}
	ci := &client{t: t, base: ts.URL, key: deployKey}

	code, body := ci.do("POST", "/v1/rollback", map[string]any{"env": "dev"})
	if code != 409 {
		t.Fatalf("rollback without a deprecated window: want 409, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); !strings.Contains(d, "no deprecated version") {
		t.Fatalf("409 must explain the empty rollback window, got %q", d)
	}
}

// w13: allow-сторона привязки для отката — ключ bound(game,dev) откатывает
// dev-окно в своём env → 200 (пара к enforcement-403 в mm_env_test). Именно
// привязка пропускает откат в свой env, версия 1.0.0 снова active.
func TestRollbackBoundKeyInOwnEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev version 1.0.0
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	// dev-окно: active 1.1.0, deprecated 1.0.0.
	devActive := f.AddVersion(t, "1.1.0", "dev")
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu", ActiveVersion: &devActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `update versions set state='active' where id=$1::uuid`, devActive); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`update versions set state='deprecated', deprecated_at=now() where id=$1::uuid`, f.VersionID); err != nil {
		t.Fatal(err)
	}

	game, dev := "game", "dev"
	_, boundSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ci-dev", Scopes: []string{httpapi.ScopeDeploy}, Project: &game, Env: &dev,
	})
	if err != nil {
		t.Fatal(err)
	}
	boundDev := &client{t: t, base: ts.URL, key: boundSecret}

	code, body := boundDev.do("POST", "/v1/rollback", map[string]any{"env": "dev"})
	if code != 200 {
		t.Fatalf("bound-dev rollback in its own env: want 200, got %d %v", code, body)
	}
	if v, err := st.GetVersion(ctx, f.VersionID); err != nil || v.State != "active" {
		t.Fatalf("dev deprecated must roll back to active: %+v %v", v, err)
	}
}

// POST /v1/rollback env-resolve (env v1 §3, I3): env обязателен, когда у проекта
// >1 env с deprecated-окном (иначе sole-fallback); явный env скоупит откат.
func TestRollbackEnvResolve(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev node, dev version 1.0.0
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	setState := func(id, state string) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx,
			`update versions set state=$2, deprecated_at = case when $2='deprecated' then now() else deprecated_at end where id = $1::uuid`,
			id, state); err != nil {
			t.Fatal(err)
		}
	}

	// dev: active 1.1.0, deprecated 1.0.0 — полное окно отката в dev.
	devActive := f.AddVersion(t, "1.1.0", "dev")
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu", ActiveVersion: &devActive,
	}); err != nil {
		t.Fatal(err)
	}
	setState(devActive, "active")
	setState(f.VersionID, "deprecated")

	// prod: нода + active 2.1.0 + deprecated 2.0.0 — окно отката в prod.
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatal(err)
	}
	prodDep := f.AddVersion(t, "2.0.0", "prod")
	prodActive := f.AddVersion(t, "2.1.0", "prod")
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu", ActiveVersion: &prodActive,
	}); err != nil {
		t.Fatal(err)
	}
	setState(prodActive, "active")
	setState(prodDep, "deprecated")

	_, deployKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ci", Scopes: []string{httpapi.ScopeDeploy}})
	if err != nil {
		t.Fatal(err)
	}
	ci := &client{t: t, base: ts.URL, key: deployKey}

	// Оба env имеют deprecated-окно, env не задан → 409 env_required.
	code, body := ci.do("POST", "/v1/rollback", map[string]any{})
	if code != 409 {
		t.Fatalf("ambiguous rollback: want 409, got %d %v", code, body)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "env is required") {
		t.Fatalf("409 detail must name the ambiguity, got %q", detail)
	}

	// Явный env=prod → откат ИМЕННО prod (2.0.0 → active), dev не тронут.
	code, body = ci.do("POST", "/v1/rollback", map[string]any{"env": "prod"})
	if code != 200 {
		t.Fatalf("explicit-env rollback: want 200, got %d %v", code, body)
	}
	if v, err := st.GetVersion(ctx, prodDep); err != nil || v.State != "active" {
		t.Fatalf("prod deprecated must be rolled back to active: %+v %v", v, err)
	}
	if v, err := st.GetVersion(ctx, prodActive); err != nil || v.State != "deprecated" {
		t.Fatalf("prod active must be demoted: %+v %v", v, err)
	}
	// dev нетронут: 1.1.0 всё ещё active, 1.0.0 всё ещё deprecated.
	if v, err := st.GetVersion(ctx, devActive); err != nil || v.State != "active" {
		t.Fatalf("dev active must survive prod rollback: %+v %v", v, err)
	}
	if v, err := st.GetVersion(ctx, f.VersionID); err != nil || v.State != "deprecated" {
		t.Fatalf("dev deprecated must survive prod rollback: %+v %v", v, err)
	}

	// Откат прод НЕ закрыл прод-окно (свап active↔deprecated: теперь deprecated
	// стал 2.1.0). Закроем прод-окно явно (disable) — окно останется только в dev.
	setState(prodActive, "disabled")

	// Теперь deprecated-окно только в dev → env не задан → sole-fallback, 200.
	code, body = ci.do("POST", "/v1/rollback", map[string]any{})
	if code != 200 {
		t.Fatalf("sole-env rollback: want 200, got %d %v", code, body)
	}
	if v, err := st.GetVersion(ctx, f.VersionID); err != nil || v.State != "active" {
		t.Fatalf("dev deprecated must roll back under sole-fallback: %+v %v", v, err)
	}
}

// w14: HTTP-шов авто-деплоя (handlers.go:122-128). POST /v1/versions в
// auto_deploy-env (dev из сида) с флотом и живой нодой немедленно гонит цепочку
// «только вперёд»: ответ 201 несёт "auto_deploy":"started" И fake sender получил
// PrePull. Вторая регистрация, пока первый деплой ещё in-flight (prepulling,
// никто не отчитался pulled), второй старт НЕ запускает — 201 c
// "auto_deploy":"queued". Пинит связку «вызов TryAutoDeploy + switch по исходу»:
// без этого блока поле auto_deploy пропадает и PrePull не уходит, а прежняя
// suite остаётся зелёной.
func TestCreateVersionAutoDeployOverHTTP(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev auto_deploy, живая нода, версия 1.0.0
	f.UpsertFleet(t, 2, 50)           // флот dev/eu — цель прогрева PrePull
	ts, _, rec := deployServer(t, st)
	ctx := t.Context()

	_, deployKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ci", Scopes: []string{httpapi.ScopeDeploy}})
	if err != nil {
		t.Fatal(err)
	}
	ci := &client{t: t, base: ts.URL, key: deployKey}

	// Первая регистрация в dev: цепочка стартует немедленно → 201 + "started".
	code, body := ci.do("POST", "/v1/versions", map[string]any{
		"project": "game", "semver": "1.1.0",
		"image_ref": "ghcr.io/example/game-server:1.1.0", "env": "dev",
	})
	if code != 201 || body["auto_deploy"] != "started" {
		t.Fatalf("register in auto_deploy env: want 201 auto_deploy=started, got %d %v", code, body)
	}
	// Тот же синхронный вызов уже отправил PrePull живой ноде флота.
	var prepulled bool
	for _, c := range rec.Take() {
		if p := c.Msg.GetPrepull(); p != nil && c.NodeID == f.NodeID {
			prepulled = true
		}
	}
	if !prepulled {
		t.Fatal("auto-deploy must dispatch a PrePull to the live fleet node")
	}

	// Вторая регистрация, пока деплой 1.1.0 ещё in-flight (prepulling, отчёта
	// pulled не было): второй старт занят busy-проверкой → 201 + "queued".
	code, body = ci.do("POST", "/v1/versions", map[string]any{
		"project": "game", "semver": "1.2.0",
		"image_ref": "ghcr.io/example/game-server:1.2.0", "env": "dev",
	})
	if code != 201 || body["auto_deploy"] != "queued" {
		t.Fatalf("register while a deploy is in flight: want 201 auto_deploy=queued, got %d %v", code, body)
	}
}
