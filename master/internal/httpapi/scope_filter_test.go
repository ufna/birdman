package httpapi_test

// Батч «REST-поверхность»: #971 (?env= валидируется на листингах так же, как в
// статистике) и #974 (привязка ключа энфорсится и на чтении одного матча).
//
// Обе карточки — об одном классе: правило есть, но применяется не везде, и это
// «не везде» снаружи не отличить от нормы. Опечатка в ?env= выглядит как пустой
// список (а пустой список — желанное состояние), чужой матч отдаётся молча.

import (
	"testing"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestListingsValidateEnv (#971): опечатка в ?env= на листингах нод и версий —
// 400, а не тихий пустой список.
func TestListingsValidateEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	_, roSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro", Scopes: []string{httpapi.ScopeReadonly},
	})
	if err != nil {
		t.Fatal(err)
	}
	ro := &client{t: t, base: ts.URL, key: roSecret}

	for _, path := range []string{"/v1/nodes", "/v1/versions"} {
		if code, body := ro.do("GET", path+"?env="+f.Env, nil); code != 200 {
			t.Fatalf("%s?env=%s → %d %v, want 200", path, f.Env, code, body)
		}
		if code, body := ro.do("GET", path+"?env=dveloper", nil); code != 400 {
			t.Fatalf("%s?env=dveloper → %d %v, want 400 (опечатка не должна читаться как «пусто»)",
				path, code, body)
		}
		// Пара (project, env): имя окружения проверяется В ЭТОМ проекте.
		if code, body := ro.do("GET", path+"?project="+f.Project+"&env=dveloper", nil); code != 400 {
			t.Fatalf("%s?project=%s&env=dveloper → %d %v, want 400", path, f.Project, code, body)
		}
	}
}

// TestGetMatchEnforcesBinding (#974): привязанный readonly-ключ не читает матч
// чужого проекта, зная его uuid. Глобальный ключ (и сессия панели) — как раньше.
func TestGetMatchEnforcesBinding(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	// Матч проекта "game": сервер на его ноде + строка матча (хелпера в фикстуре
	// нет, а гонять полный матчмейкинг ради одного чтения незачем).
	serverID := f.InsertServerOn(t, f.NodeID, f.VersionID, "allocated")
	var matchID string
	if err := st.Pool.QueryRow(ctx, `
		insert into matches (id, project_id, server_id, version_id, region, env, state)
		select gen_random_uuid(), p.id, $1::uuid, $2::uuid, $3, $4, 'running'
		from projects p where p.slug = $5
		returning id::text`,
		serverID, f.VersionID, f.Region, f.Env, f.Project).Scan(&matchID); err != nil {
		t.Fatalf("insert match: %v", err)
	}

	// Соседний проект и привязанный к нему readonly-ключ.
	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatal(err)
	}
	nProject, nEnv := "neighbour", "dev"
	_, boundSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-neighbour", Scopes: []string{httpapi.ScopeReadonly}, Project: &nProject, Env: &nEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, globalSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-global", Scopes: []string{httpapi.ScopeReadonly},
	})
	if err != nil {
		t.Fatal(err)
	}

	bound := &client{t: t, base: ts.URL, key: boundSecret}
	global := &client{t: t, base: ts.URL, key: globalSecret}

	if code, body := bound.do("GET", "/v1/matches/"+matchID, nil); code != 403 {
		t.Fatalf("привязанный к чужому проекту ключ прочитал матч: %d %v, want 403", code, body)
	}
	if code, body := global.do("GET", "/v1/matches/"+matchID, nil); code != 200 {
		t.Fatalf("глобальный ключ: %d %v, want 200", code, body)
	}
}

// TestServerLogsEnforcesBinding (#988): та же асимметрия, что закрыл #974, но на
// ручке потяжелее — `GET /v1/servers/{id}/logs` отдаёт не метаданные, а игровой
// вывод дедика. Ручка readonly и адресуется по uuid, поэтому привязанный ключ
// проекта А читал логи проекта Б, зная id сервера. Глобальный ключ (и сессия
// панели, которая admin) проходит как раньше.
func TestServerLogsEnforcesBinding(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	serverID := f.InsertServerOn(t, f.NodeID, f.VersionID, "allocated")

	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatal(err)
	}
	nProject, nEnv := "neighbour", "dev"
	_, boundSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-neighbour", Scopes: []string{httpapi.ScopeReadonly}, Project: &nProject, Env: &nEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	bound := &client{t: t, base: ts.URL, key: boundSecret}

	if code, body := bound.do("GET", "/v1/servers/"+serverID+"/logs?tail=10", nil); code != 403 {
		t.Fatalf("привязанный к чужому проекту ключ получил логи дедика: %d %v, want 403", code, body)
	}

	// Ключ, привязанный к СВОЕМУ проекту, гейт пройти обязан: 403 здесь означал
	// бы, что мы сломали легитимный доступ, а не закрыли дыру.
	ownProject, ownEnv := f.Project, f.Env
	_, ownSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-own", Scopes: []string{httpapi.ScopeReadonly}, Project: &ownProject, Env: &ownEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	own := &client{t: t, base: ts.URL, key: ownSecret}
	if code, body := own.do("GET", "/v1/servers/"+serverID+"/logs?tail=10", nil); code == 403 {
		t.Fatalf("ключ своего проекта получил 403 — гейт бьёт по своим: %d %v", code, body)
	}
}
