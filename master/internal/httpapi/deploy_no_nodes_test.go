package httpapi_test

import (
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Пин карточки #1071: у нового проекта СВОИХ нод ноль (нода принадлежит ровно
// одному проекту — nodes.project_id, #1064/#1065), деплой активируется вхолостую,
// и раньше система об этом молчала ВЕЗДЕ: `POST /v1/deploy` отвечал
// `{"state":"active","pending_nodes":0}`, авто-деплой — `"auto_deploy":"started"`,
// а единственный след («no live fleet nodes to prepull») жил в логе мастера, куда
// оператор не ходит. Наружу это выглядело как «деплой прошёл, дедиков нет».
//
// Теперь тот же факт наблюдаем ДВАЖДЫ, и тест держит обе половины:
//   - событие `deploy_no_nodes` в /v1/events — покрывает CI-путь (авто-деплой,
//     который панели не касается) и видно в ленте панели;
//   - поле `warning: "no_live_nodes"` в ответе `POST /v1/deploy` — момент действия
//     для того, кто катит курлом; в том числе на ИДЕМПОТЕНТНОМ повторе, куда
//     недоумевающий оператор встаёт первым делом («накачу ещё раз»).
//
// Контроль (вторая половина теста) обязателен: с живой нодой ни события, ни
// предупреждения быть не должно — иначе пин зелен на чём угодно.
func TestDeployWithoutNodesIsAnnounced(t *testing.T) {
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

	// Проект khl-legends: флот есть (его заводят руками), нод нет ВОВСЕ — ровно
	// замер, с которого заведена карточка.
	code, body := admin.do("POST", "/v1/versions", map[string]any{
		"project": "khl-legends", "semver": "1.0.0",
		"image_ref": "ghcr.io/example/khl:1.0.0", "env": "dev",
	})
	if code != 201 {
		t.Fatalf("create version: %d %v", code, body)
	}
	v1 := body["version"].(map[string]any)["id"].(string)
	if code, body = admin.do("PUT", "/v1/fleets/eu", map[string]any{
		"project": "khl-legends", "env": "dev", "active_version": v1, "buffer_ready": 2,
	}); code != 200 {
		t.Fatalf("upsert fleet: %d %v", code, body)
	}

	// --- (1) авто-деплой без нод оставляет событие ---
	// Регистрация второй версии в auto_deploy-окружении: цепочка «только вперёд»
	// катит её сама, греть некого → мгновенная активация вхолостую.
	code, body = admin.do("POST", "/v1/versions", map[string]any{
		"project": "khl-legends", "semver": "1.1.0",
		"image_ref": "ghcr.io/example/khl:1.1.0", "env": "dev",
	})
	if code != 201 || body["auto_deploy"] != "started" {
		t.Fatalf("auto-deploy: want 201/started, got %d %v", code, body)
	}
	v2 := body["version"].(map[string]any)["id"].(string)

	ev := findEvent(t, st, store.EventDeployNoNodes)
	if ev.VersionID == nil || *ev.VersionID != v2 {
		t.Fatalf("deploy_no_nodes must point at the deployed version %s, got %v", v2, ev.VersionID)
	}
	if ev.Project != "khl-legends" {
		t.Fatalf("deploy_no_nodes must be scoped to the project (GET /v1/events?project=), got %q", ev.Project)
	}
	for k, want := range map[string]any{"project": "khl-legends", "env": "dev", "semver": "1.1.0"} {
		if ev.Payload[k] != want {
			t.Fatalf("deploy_no_nodes payload[%s]: want %v, got %v", k, want, ev.Payload[k])
		}
	}

	// --- (2) ручной POST /v1/deploy без нод предупреждает в ответе ---
	// Флип v2 отправил v1 в deprecated, то есть v1 снова deployable; нод по-
	// прежнему ноль.
	if code, body = admin.do("POST", "/v1/deploy", map[string]any{"version_id": v1}); code != 200 {
		t.Fatalf("redeploy v1: %d %v", code, body)
	}
	if got := deployWarning(t, body); got != deploy.WarnNoLiveNodes {
		t.Fatalf("deploy onto a project with zero nodes must warn %q, got %q", deploy.WarnNoLiveNodes, got)
	}

	// Идемпотентный повтор той же (уже активной) версии — путь «накачу ещё раз».
	if code, body = admin.do("POST", "/v1/deploy", map[string]any{"version_id": v1}); code != 200 {
		t.Fatalf("idempotent redeploy: %d %v", code, body)
	}
	if got := deployWarning(t, body); got != deploy.WarnNoLiveNodes {
		t.Fatalf("idempotent repeat must warn too, got %q", got)
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
	if code, body = admin.do("POST", "/v1/deploy", map[string]any{"version_id": v2}); code != 202 {
		t.Fatalf("deploy with a live node: want 202 prepulling, got %d %v", code, body)
	}
	if got := deployWarning(t, body); got != "" {
		t.Fatalf("a deploy with a live node must carry no warning, got %q", got)
	}
	if after := countEvents(t, st, store.EventDeployNoNodes); after != before {
		t.Fatalf("no new deploy_no_nodes expected with a live node: %d → %d", before, after)
	}
}

// deployWarning reads {"deploy":{"warning":…}} out of a response body; "" when
// the field is absent (omitempty — the normal case).
func deployWarning(t *testing.T, body map[string]any) string {
	t.Helper()
	dep, ok := body["deploy"].(map[string]any)
	if !ok {
		t.Fatalf("response has no deploy object: %v", body)
	}
	w, _ := dep["warning"].(string)
	return w
}

// findEvent returns the newest event of the given kind, failing when there is none.
func findEvent(t *testing.T, st *store.Store, kind string) store.Event {
	t.Helper()
	events, err := st.ListEvents(t.Context(), 200, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events { // newest first
		if e.Kind == kind {
			return e
		}
	}
	kinds := make([]string, 0, len(events))
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	t.Fatalf("no %s event in the feed; got %v", kind, kinds)
	return store.Event{}
}

func countEvents(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	events, err := st.ListEvents(t.Context(), 200, "")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}
