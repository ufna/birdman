package httpapi_test

import (
	"strings"
	"testing"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// POST /v1/promote (environments v1 §4): идемпотентный промоут версии в другой
// env + обычный deploy-пайплайн; enforcement по (project источника, to_env).
// Плюс w8: явный несуществующий env в rollback → 400 (через GetEnvironment).

// promoteStand: проект game с живой dev-половиной (нода+версия из Seed) и
// prod-нодой + prod-флотом (без active-версии) в регионе eu — целью промоута.
func promoteStand(t *testing.T, st *store.Store) (*testdb.Fixture, string) {
	t.Helper()
	f := testdb.Seed(t, st, "eu", 10) // dev node 203.0.113.10, dev version 1.0.0
	ctx := t.Context()
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatalf("move node to prod: %v", err)
	}
	buffer, maxServers := int32(2), int32(50)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu",
		BufferReady: &buffer, MaxServers: &maxServers,
	}); err != nil {
		t.Fatalf("prod fleet: %v", err)
	}
	return f, prodNode
}

// Глобальный deploy-ключ: промоут dev 1.0.0 → prod запускает обычный deploy —
// PrePull летит на prod-ноду (fake sender ловит), отчёт pulled активирует
// prod-версию. Повтор после активации → 409 (уже active).
func TestPromoteEndpoint(t *testing.T) {
	st := testdb.New(t)
	f, prodNode := promoteStand(t, st)
	ts, dep, rec := deployServer(t, st)
	ctx := t.Context()

	_, deployKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ci", Scopes: []string{httpapi.ScopeDeploy}})
	if err != nil {
		t.Fatal(err)
	}
	ci := &client{t: t, base: ts.URL, key: deployKey}

	// Промоут → 202 prepulling, prod-версия создана с provenance promoted_from.
	code, body := ci.do("POST", "/v1/promote", map[string]any{"version_id": f.VersionID, "to_env": "prod"})
	if code != 202 {
		t.Fatalf("promote: want 202, got %d %v", code, body)
	}
	ver, _ := body["version"].(map[string]any)
	if ver == nil || ver["env"] != "prod" || ver["semver"] != "1.0.0" || ver["promoted_from"] != f.VersionID {
		t.Fatalf("promoted version body: %v", body["version"])
	}
	dp, _ := body["deploy"].(map[string]any)
	if dp == nil || dp["state"] != "prepulling" {
		t.Fatalf("deploy state: want prepulling, got %v", body["deploy"])
	}
	// PrePull ушёл именно на prod-ноду.
	var prepulled bool
	for _, c := range rec.Take() {
		if p := c.Msg.GetPrepull(); p != nil && c.NodeID == prodNode {
			prepulled = true
		}
	}
	if !prepulled {
		t.Fatal("PrePull not dispatched to the prod node")
	}

	// Prod-нода отчиталась pulled → активация prod-версии.
	prodV := ver["id"].(string)
	dep.HandlePullReport(prodNode, &agentlinkv1.PullReport{
		ImageRef: "ghcr.io/example/game-server:1.0.0", Status: "pulled",
	})
	if v, err := st.GetVersion(ctx, prodV); err != nil || v.State != "active" {
		t.Fatalf("prod version must be active after the pulled report: %+v %v", v, err)
	}

	// Повторный промоут после активации → 409 (prod-версия уже active).
	if code, body := ci.do("POST", "/v1/promote", map[string]any{"version_id": f.VersionID, "to_env": "prod"}); code != 409 {
		t.Fatalf("re-promote after activation: want 409, got %d %v", code, body)
	}
}

// bound(dev)-ключ не может промоутить в prod — цель промоута это ЦЕЛЕВОЙ env
// (prod), не env источника; промоут В свой env даёт 409 (same env), НЕ 403 —
// доказывает, что гейт именно по целевому env.
func TestPromoteBindingEnforcement(t *testing.T) {
	st := testdb.New(t)
	f, _ := promoteStand(t, st)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	game, dev := "game", "dev"
	_, boundSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ci-dev", Scopes: []string{httpapi.ScopeDeploy}, Project: &game, Env: &dev,
	})
	if err != nil {
		t.Fatal(err)
	}
	boundDev := &client{t: t, base: ts.URL, key: boundSecret}

	code, body := boundDev.do("POST", "/v1/promote", map[string]any{"version_id": f.VersionID, "to_env": "prod"})
	if code != 403 {
		t.Fatalf("bound-dev promote to prod: want 403, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); !strings.Contains(d, "key is bound to game/dev") {
		t.Fatalf("403 must name the binding, got %q", d)
	}

	// Промоут в свой env → 409 (same env), не 403.
	if code, body := boundDev.do("POST", "/v1/promote", map[string]any{"version_id": f.VersionID, "to_env": "dev"}); code != 409 {
		t.Fatalf("bound-dev promote to its own env: want 409 (same env), got %d %v", code, body)
	}
}

// Идемпотентность спасает после отказа деплоя: prod без флота → 409 no_fleet, но
// prod-версия уже создана (tx промоута отдельна от деплоя); повтор НЕ 409-semver,
// а снова доходит до деплоя; появился флот → 202.
func TestPromoteIdempotentAfterFailedDeploy(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // ТОЛЬКО dev-половина; prod без флота
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	_, deployKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ci", Scopes: []string{httpapi.ScopeDeploy}})
	if err != nil {
		t.Fatal(err)
	}
	ci := &client{t: t, base: ts.URL, key: deployKey}

	// Первый промоут: версия создаётся, но деплой падает — prod без флота → 409.
	if code, body := ci.do("POST", "/v1/promote", map[string]any{"version_id": f.VersionID, "to_env": "prod"}); code != 409 {
		t.Fatalf("promote without prod fleet: want 409, got %d %v", code, body)
	}
	// prod-версия всё же существует как registered.
	prodV := versionID(t, st, "game", "prod", "1.0.0")
	if v, err := st.GetVersion(ctx, prodV); err != nil || v.State != "registered" {
		t.Fatalf("prod version must exist as registered after the failed deploy: %+v %v", v, err)
	}

	// Повтор без изменений → снова доходит до деплоя (реюз), 409 no_fleet, НЕ
	// 409 semver/state-conflict.
	code, body := ci.do("POST", "/v1/promote", map[string]any{"version_id": f.VersionID, "to_env": "prod"})
	if code != 409 {
		t.Fatalf("re-promote still no fleet: want 409, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); strings.Contains(d, "already") || strings.Contains(d, "different image") {
		t.Fatalf("re-promote must NOT be a semver/state conflict, got %q", d)
	}

	// Появился prod-флот + нода → повтор стартует деплой (202).
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatal(err)
	}
	buffer, maxServers := int32(2), int32(50)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu", BufferReady: &buffer, MaxServers: &maxServers,
	}); err != nil {
		t.Fatal(err)
	}
	if code, body := ci.do("POST", "/v1/promote", map[string]any{"version_id": f.VersionID, "to_env": "prod"}); code != 202 {
		t.Fatalf("re-promote with a fresh fleet: want 202, got %d %v", code, body)
	}
}

// w8: явный несуществующий env в rollback → 400 (через GetEnvironment), а не
// вводящий в заблуждение 409 «нет deprecated-версии» (ErrVersionState-путём).
func TestRollbackUnknownEnv(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10) // project game с сидами dev+prod
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	_, deployKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ci", Scopes: []string{httpapi.ScopeDeploy}})
	if err != nil {
		t.Fatal(err)
	}
	ci := &client{t: t, base: ts.URL, key: deployKey}

	code, body := ci.do("POST", "/v1/rollback", map[string]any{"env": "ghost"})
	if code != 400 {
		t.Fatalf("rollback unknown env: want 400, got %d %v", code, body)
	}
	if d, _ := body["detail"].(string); !strings.Contains(d, "no such environment") {
		t.Fatalf("400 must say «no such environment», got %q", d)
	}
}
