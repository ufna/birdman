package httpapi_test

// Батч «REST-поверхность»: #971 (?env= валидируется на листингах так же, как в
// статистике) и #974 (привязка ключа энфорсится и на чтении одного матча).
//
// Обе карточки — об одном классе: правило есть, но применяется не везде, и это
// «не везде» снаружи не отличить от нормы. Опечатка в ?env= выглядит как пустой
// список (а пустой список — желанное состояние), чужой матч отдаётся молча.

import (
	"bytes"
	"reflect"
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

// TestEnvironmentUsageEnforcesBinding (#989): третья ручка того же класса, что
// #974 (чтение матча) и #988 (логи дедика). `GET /v1/environments/{project}/
// {name}/usage` — readonly и адресуется ПАРОЙ (project, name) прямо в пути,
// поэтому привязанный к своему проекту ключ читал состав чужого окружения,
// просто подставив чужой слаг (а слаги перечисляет `GET /v1/projects` того же
// скоупа).
//
// Тест держит ОБЕ половины и оракул:
//   - чужой привязанный ключ → 403 на СУЩЕСТВУЮЩЕЕ окружение;
//   - он же → БАЙТ-В-БАЙТ такой же 403 на выдуманное окружение и на выдуманный
//     проект: гейт стоит ДО резолва, поэтому ответ формируется раньше, чем о
//     существовании что-либо известно (в #988 такой порядок был невозможен —
//     там пара известна только после похода в стор по uuid). Сверяются ответы
//     целиком: одинаковый код с разными телами — тот же оракул, только на
//     строке вместо статуса;
//   - свой ключ и глобальный получают РЕАЛЬНЫЙ состав (200 + шесть счётчиков),
//     а не просто «не 403» — иначе положительная половина ничего не доказывает;
//   - свой ключ на чужом ОКРУЖЕНИИ своего проекта → 403 (привязка — пара, а не
//     проект), а на несуществующем окружении своей пары → обычный 404.
func TestEnvironmentUsageEnforcesBinding(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	// Состав окружения game/dev делаем НЕтривиальным: нода и версия приехали с
	// фикстурой, сервер добавляем здесь, живой привязанный ключ появится ниже.
	f.InsertServerOn(t, f.NodeID, f.VersionID, "allocated")

	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatal(err)
	}
	nProject, nEnv := "neighbour", "dev"
	_, neighbourSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-neighbour", Scopes: []string{httpapi.ScopeReadonly}, Project: &nProject, Env: &nEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownProject, ownEnv := f.Project, f.Env
	_, ownSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-own", Scopes: []string{httpapi.ScopeReadonly}, Project: &ownProject, Env: &ownEnv,
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
	neighbour := &client{t: t, base: ts.URL, key: neighbourSecret}
	own := &client{t: t, base: ts.URL, key: ownSecret}
	global := &client{t: t, base: ts.URL, key: globalSecret}

	usagePath := "/v1/environments/" + f.Project + "/" + f.Env + "/usage"
	ghostPath := "/v1/environments/" + f.Project + "/ghost/usage"

	if code, body := neighbour.do("GET", usagePath, nil); code != 403 {
		t.Fatalf("привязанный к чужому проекту ключ прочитал состав окружения: %d %v, want 403", code, body)
	}

	// Оракул закрыт сверкой ОТВЕТОВ ЦЕЛИКОМ, а не только кодов: одинаковый 403 с
	// разными телами («key is bound to…» против «no such environment») — тот же
	// оракул, только на строке вместо статуса, и пиннинг одного кода его
	// пропускает. Байт-в-байт, как это делает #963 для тикетов (mm_env_test.go).
	// Третий адрес — ВЫДУМАННЫЙ ПРОЕКТ: существование проектов эта ручка тоже не
	// подтверждает (хотя /v1/projects того же скоупа их и так перечисляет).
	liveCode, live := neighbour.doRaw("GET", usagePath)
	ghostCode, ghost := neighbour.doRaw("GET", ghostPath)
	noProjCode, noProj := neighbour.doRaw("GET", "/v1/environments/nosuchproject/dev/usage")
	if liveCode != 403 || ghostCode != 403 || noProjCode != 403 {
		t.Fatalf("чужой ключ: живое=%d выдуманное окружение=%d выдуманный проект=%d, want 403 везде — "+
			"расхождение кодов сделало бы ручку оракулом существования", liveCode, ghostCode, noProjCode)
	}
	if !bytes.Equal(live, ghost) || !bytes.Equal(live, noProj) {
		t.Fatalf("403 отличается телом и потому остаётся оракулом:\n живое=%s\n окружения нет=%s\n проекта нет=%s",
			live, ghost, noProj)
	}

	// Положительная половина: РЕАЛЬНОЕ тело, а не «не 403». Состав детерминирован
	// (свежая БД на тест): версия и нода из фикстуры, сервер вставлен выше,
	// живой привязанный ключ — ro-own (ro-neighbour принадлежит другой паре,
	// ro-global не привязан вовсе), флотов и матчей нет.
	want := map[string]any{
		"versions": float64(1), "fleets": float64(0), "nodes": float64(1),
		"servers": float64(1), "matches": float64(0), "api_keys": float64(1),
	}
	for name, c := range map[string]*client{"свой привязанный ключ": own, "глобальный ключ": global} {
		code, body := c.do("GET", usagePath, nil)
		if code != 200 {
			t.Fatalf("%s: %d %v, want 200 — гейт бьёт по своим", name, code, body)
		}
		if got := body["usage"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s получил не тот состав окружения: %v, want %v", name, got, want)
		}
	}

	// Привязка — ПАРА, а не проект: prod того же проекта чужой для ключа dev.
	if code, body := own.do("GET", "/v1/environments/"+f.Project+"/prod/usage", nil); code != 403 {
		t.Fatalf("ключ, привязанный к %s/%s, прочитал состав prod: %d %v, want 403",
			f.Project, f.Env, code, body)
	}
	// Из-за той же пар-точности привязанный ключ получает 403 и на выдуманное
	// окружение СВОЕГО проекта — то есть перебор имён закрыт для него полностью,
	// а не только через границу проекта.
	if code, body := own.do("GET", ghostPath, nil); code != 403 {
		t.Fatalf("привязанный ключ на несуществующем окружении своего проекта: %d %v, want 403 "+
			"(пара (%s,ghost) не равна (%s,%s))", code, body, f.Project, f.Project, f.Env)
	}
	// А 404 никуда не делся: гейт не подменил собой резолв — тот, кому пара
	// разрешена, по-прежнему отличает «нет такого окружения» от отказа.
	if code, body := global.do("GET", ghostPath, nil); code != 404 {
		t.Fatalf("глобальный ключ на несуществующем окружении: %d %v, want 404", code, body)
	}
}
