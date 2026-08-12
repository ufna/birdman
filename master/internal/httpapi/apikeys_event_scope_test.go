package httpapi_test

import (
	"strings"
	"testing"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// События жизненного цикла ПРИВЯЗАННОГО ключа атрибутируются его проекту, а не
// платформе (tracker #1017). Замер карточки: ключ `ci-game-prod-deployer`
// (привязка `game/dev`) отзывается через `DELETE /v1/apikeys/{id}`, и его имя
// приезжает в ленту арендатора `neighbour` — потому что `apikey_revoked`
// уходил в БД с `project_id is null`.
//
// Проверяется СЛЕДСТВИЕ, а не поле в payload'е: сосед не должен видеть эти
// события НИ ЧЕРЕЗ `GET /v1/events`, НИ через выборку стрима — это два входа в
// одну таблицу (#999), и правило у них общее. Фильтр ленты при этом остаётся
// не скрывающим ПО ЗАМЫСЛУ (#955/#968/#993): чинится атрибуция на записи, а не
// лента.
func TestAPIKeyLifecycleEventsAreAttributedToProject(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10) // проект game с окружениями dev+prod
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatal(err)
	}
	_, adminKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "admin", Scopes: []string{"admin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	admin := &client{t: t, base: ts.URL, key: adminKey}

	// Имя, по которому видно утечку: оно само называет чужой проект и окружение,
	// как в замере карточки.
	const victim = "ci-game-prod-deployer"
	code, body := admin.do("POST", "/v1/apikeys", map[string]any{
		"name": victim, "scopes": []string{"deploy"}, "project": "game", "env": "dev",
	})
	if code != 201 {
		t.Fatalf("create bound key: %d %v", code, body)
	}
	created, _ := body["key"].(map[string]any)
	keyID, _ := created["id"].(string)
	if keyID == "" {
		t.Fatalf("create bound key: нет id в ответе: %v", body)
	}

	if code, b := admin.do("DELETE", "/v1/apikeys/"+keyID, nil); code != 200 {
		t.Fatalf("revoke: %d %v", code, b)
	}
	if code, b := admin.do("DELETE", "/v1/apikeys/"+keyID+"?purge=true", nil); code != 204 {
		t.Fatalf("purge: %d %v", code, b)
	}

	// 1. Атрибуция в БД: у всех трёх событий проект проставлен и он ТОТ.
	kinds := map[string]bool{
		store.EventAPIKeyCreated: false,
		store.EventAPIKeyRevoked: false,
		store.EventAPIKeyPurged:  false,
	}
	rows, err := st.Pool.Query(ctx, `
		select e.kind, coalesce(p.slug, '<null>')
		from events e left join projects p on p.id = e.project_id
		where e.kind = any($1::text[])`,
		[]string{store.EventAPIKeyCreated, store.EventAPIKeyRevoked, store.EventAPIKeyPurged})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, slug string
		if err := rows.Scan(&kind, &slug); err != nil {
			t.Fatal(err)
		}
		if slug != "game" {
			t.Fatalf("%s атрибутировано %q, want game — платформенное событие видит каждый арендатор", kind, slug)
		}
		kinds[kind] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for kind, seen := range kinds {
		if !seen {
			t.Fatalf("события %s нет в БД вовсе — проверять нечего", kind)
		}
	}

	// 2. Следствие, ради которого всё делалось: сосед не видит имени чужого
	// ключа ни на листинге, ни в выборке стрима.
	events, err := st.ListEvents(ctx, 200, "neighbour")
	if err != nil {
		t.Fatal(err)
	}
	streamed, _, err := st.ListEventsAfter(ctx, 0, 200, "neighbour")
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []struct {
		name string
		evs  []store.Event
	}{{"GET /v1/events", events}, {"SSE", streamed}} {
		for _, e := range src.evs {
			if name, _ := e.Payload["name"].(string); strings.Contains(name, victim) {
				t.Fatalf("%s: соседу приехало имя чужого ключа: kind=%s project=%q payload=%v",
					src.name, e.Kind, e.Project, e.Payload)
			}
		}
	}

	// 3. Контроль от теста-пустышки: СВОЙ арендатор эти же события видит —
	// иначе «починка» могла бы оказаться сокрытием аудита от всех.
	own, err := st.ListEvents(ctx, 200, "game")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range own {
		if name, _ := e.Payload["name"].(string); name == victim {
			seen[e.Kind] = true
		}
	}
	for _, kind := range []string{store.EventAPIKeyCreated, store.EventAPIKeyRevoked, store.EventAPIKeyPurged} {
		if !seen[kind] {
			t.Fatalf("свой арендатор не видит %s своего же ключа: %v", kind, own)
		}
	}

	// 4. Глобальный ключ (без привязки) остаётся ПЛАТФОРМЕННЫМ — привязки нет,
	// приписывать его проекту нечему и незачем.
	code, body = admin.do("POST", "/v1/apikeys", map[string]any{
		"name": "ops-global", "scopes": []string{"readonly"},
	})
	if code != 201 {
		t.Fatalf("create global key: %d %v", code, body)
	}
	globalID, _ := body["key"].(map[string]any)["id"].(string)
	if code, b := admin.do("DELETE", "/v1/apikeys/"+globalID, nil); code != 200 {
		t.Fatalf("revoke global: %d %v", code, b)
	}
	var slug string
	if err := st.Pool.QueryRow(ctx, `
		select coalesce(p.slug, '<null>') from events e
		left join projects p on p.id = e.project_id
		where e.kind = $1 and e.payload->>'name' = 'ops-global'`,
		store.EventAPIKeyRevoked).Scan(&slug); err != nil {
		t.Fatal(err)
	}
	if slug != "<null>" {
		t.Fatalf("отзыв глобального ключа атрибутирован %q — привязки у него нет", slug)
	}
}
