package httpapi_test

import (
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Пин карточки #1088: предупреждение о нулевой ёмкости выдаётся на ЛЮБОМ пути,
// меняющем активную версию, а не только в deploy.startJob.
//
// #1071 завёл сигнал в startJob, через которую идут пять путей, и это прочли
// как «воронка одна». Она была одной не структурно, а по факту на тот день:
//
//	POST /v1/deploy            → startJob   — покрыт #1071 (deploy_no_nodes_test.go)
//	POST /v1/rollback          → МИМО       — откат не греет образы, зовёт
//	                                          ActivateVersion напрямую
//	PUT  /v1/fleets/{region}   → МИМО       — прямой UPSERT fleet_configs,
//	                                          bootstrap/ops-override
//
// Оба обхода переключали активную версию вхолостую и отвечали чистым 200.
// Здесь оба закрыты и оба проверяются с КОНТРОЛЕМ (живая нода → тишина),
// иначе пин зелен на чём угодно. Структурная половина инварианта — в
// internal/deploy/active_version_paths_test.go: она ловит ТРЕТИЙ обход, тот,
// которого ещё нет.
func TestActiveVersionFlipsWithoutNodesAreAnnounced(t *testing.T) {
	st := testdb.New(t)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	_, adminKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "admin", Scopes: []string{httpapi.ScopeAdmin},
	})
	if err != nil {
		t.Fatal(err)
	}
	admin := &client{t: t, base: ts.URL, key: adminKey}

	newVersion := func(semver string) string {
		t.Helper()
		code, body := admin.do("POST", "/v1/versions", map[string]any{
			"project": "khl-legends", "semver": semver,
			"image_ref": "ghcr.io/example/khl:" + semver, "env": "dev",
		})
		if code != 201 {
			t.Fatalf("create version %s: %d %v", semver, code, body)
		}
		return body["version"].(map[string]any)["id"].(string)
	}

	v1 := newVersion("1.0.0")

	// --- (1) PUT /v1/fleets с active_version: bootstrap-override при нуле нод ---
	// Ровно тот путь, которым состояние ставит сам тест #1071 — то есть живой,
	// просто не из панели.
	code, body := admin.do("PUT", "/v1/fleets/eu", map[string]any{
		"project": "khl-legends", "env": "dev", "active_version": v1, "buffer_ready": 2,
	})
	if code != 200 {
		t.Fatalf("upsert fleet: %d %v", code, body)
	}
	if got, _ := body["warning"].(string); got != deploy.WarnNoLiveNodes {
		t.Fatalf("PUT /v1/fleets с active_version при нуле живых нод обязан предупредить %q, got %q (%v)",
			deploy.WarnNoLiveNodes, got, body)
	}

	// PUT, правящий ОДНИ буферы, активную версию не трогает — и молчит.
	if code, body = admin.do("PUT", "/v1/fleets/eu", map[string]any{
		"project": "khl-legends", "env": "dev", "buffer_ready": 3,
	}); code != 200 {
		t.Fatalf("upsert fleet (buffers only): %d %v", code, body)
	}
	if got, ok := body["warning"]; ok {
		t.Fatalf("PUT без active_version активную версию не меняет и предупреждать не должен: %v", got)
	}

	// --- (2) POST /v1/rollback при нуле нод: событие И предупреждение ---
	// Флип 1.1.0 (авто-деплой) отправляет 1.0.0 в deprecated, то есть открывает
	// окно отката; нод по-прежнему ноль.
	v2 := newVersion("1.1.0")
	beforeRollback := countEvents(t, st, store.EventDeployNoNodes)
	if code, body = admin.do("POST", "/v1/rollback", map[string]any{
		"project": "khl-legends", "env": "dev",
	}); code != 200 {
		t.Fatalf("rollback: %d %v", code, body)
	}
	rb, ok := body["rollback"].(map[string]any)
	if !ok {
		t.Fatalf("rollback response has no rollback object: %v", body)
	}
	if got := rb["version"].(map[string]any)["id"]; got != v1 {
		t.Fatalf("rollback target: want %s, got %v", v1, got)
	}
	if got, _ := rb["warning"].(string); got != deploy.WarnNoLiveNodes {
		t.Fatalf("откат на проект с нулём живых нод обязан предупредить %q, got %q (%v)",
			deploy.WarnNoLiveNodes, got, rb)
	}
	if after := countEvents(t, st, store.EventDeployNoNodes); after != beforeRollback+1 {
		t.Fatalf("откат без нод обязан оставить РОВНО одно новое deploy_no_nodes: %d → %d",
			beforeRollback, after)
	}
	ev := findEvent(t, st, store.EventDeployNoNodes)
	if ev.VersionID == nil || *ev.VersionID != v1 {
		t.Fatalf("deploy_no_nodes отката обязан указывать на версию, НА которую откатились (%s), got %v",
			v1, ev.VersionID)
	}
	if ev.Project != "khl-legends" {
		t.Fatalf("deploy_no_nodes обязан быть проектным (GET /v1/events?project=), got %q", ev.Project)
	}
	for k, want := range map[string]any{"project": "khl-legends", "env": "dev", "semver": "1.0.0"} {
		if ev.Payload[k] != want {
			t.Fatalf("deploy_no_nodes payload[%s]: want %v, got %v", k, want, ev.Payload[k])
		}
	}

	// --- (3) контроль: живая нода — ни предупреждения, ни события ---
	node, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "khl-legends", Region: "eu", Hostname: "khl-1",
		PublicIP: "203.0.113.20", CapacitySlots: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`update nodes set state = 'active', last_heartbeat_at = now() where id = $1::uuid`, node.ID); err != nil {
		t.Fatal(err)
	}
	before := countEvents(t, st, store.EventDeployNoNodes)

	if code, body = admin.do("PUT", "/v1/fleets/eu", map[string]any{
		"project": "khl-legends", "env": "dev", "active_version": v1,
	}); code != 200 {
		t.Fatalf("upsert fleet with a live node: %d %v", code, body)
	}
	if got, ok := body["warning"]; ok {
		t.Fatalf("флот с живой нодой предупреждать не должен, got %v", got)
	}

	// Откат обратно на 1.1.0 (она deprecated после шага 2) — теперь есть куда.
	if code, body = admin.do("POST", "/v1/rollback", map[string]any{
		"project": "khl-legends", "env": "dev",
	}); code != 200 {
		t.Fatalf("rollback with a live node: %d %v", code, body)
	}
	rb, _ = body["rollback"].(map[string]any)
	if got := rb["version"].(map[string]any)["id"]; got != v2 {
		t.Fatalf("control rollback target: want %s, got %v", v2, got)
	}
	if got, ok := rb["warning"]; ok {
		t.Fatalf("откат с живой нодой предупреждать не должен, got %v", got)
	}
	if after := countEvents(t, st, store.EventDeployNoNodes); after != before {
		t.Fatalf("новых deploy_no_nodes при живой ноде быть не должно: %d → %d", before, after)
	}
}
