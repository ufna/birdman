package httpapi_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Единое поведение `?project=` (tracker #961). До этой карточки API отвечал на
// один и тот же параметр двумя способами: `/v1/stats/*` (W3) валидировал слаг и
// давал 400, листинги W2 и алерты #955 — нет. Правило теперь одно: `?project=`
// на аутентифицированном чтении ВАЛИДИРУЕТСЯ по БД, опечатка = 400
// bad_request «no such project <slug>» (docs/specs/master.md §6).
//
// Тест держит обе стороны правила — и «опечатка ловится», и «валидный проект
// работает как раньше» — плюс три исключения, чтобы их не «починили» случайно.

// projectFilterAPI поднимает сервер со всем, что нужно семи ручкам сразу:
// проект game (фикстура), второй проект arena, лог алертов и живой vmalert.
func projectFilterAPI(t *testing.T) (base, roKey, adminKey string) {
	t.Helper()
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ctx := t.Context()

	testdb.Seed(t, st, "eu", 10) // проект game + нода + версия
	if _, err := st.SetProjectMatchSize(ctx, "arena", 4); err != nil {
		t.Fatalf("second project: %v", err)
	}
	_, ro, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})
	_, admin, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})

	logPath := filepath.Join(t.TempDir(), "alerts.log")
	logBody := `{"received_at":"2026-07-08T09:00:00Z","alerts":[{"status":"firing","labels":{"alertname":"NodeDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"node is unreachable"},"startsAt":"2026-07-08T08:59:00Z","endsAt":""}]}
`
	if err := os.WriteFile(logPath, []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}
	vm := fakeVmalertWith(t, vmAlertsProjectJSON)

	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).
		WithAlertsSources(vm.URL, logPath))
	t.Cleanup(ts.Close)
	return ts.URL, ro, admin
}

// projectFilterPaths — все аутентифицированные чтения, сужаемые по проекту.
// Список обязан расти вместе с API: добавил `?project=` — добавь строку сюда.
var projectFilterPaths = []string{
	"/v1/nodes",
	"/v1/servers",
	"/v1/versions",
	"/v1/matches",
	"/v1/environments",
	"/v1/stats/overview",
	"/v1/stats/cost",
	"/v1/alerts/active",
	"/v1/alerts/history",
}

// Опечатка в проекте — 400 с одним и тем же телом на КАЖДОЙ ручке. Отдельно
// важно для алертов: там пустой экран — желанное состояние, поэтому «алертов
// нет» и «я опечатался» раньше выглядели одинаково и оба радовали.
func TestProjectFilterTypoIs400Everywhere(t *testing.T) {
	base, roKey, _ := projectFilterAPI(t)
	ro := &client{t: t, base: base, key: roKey}

	for _, path := range projectFilterPaths {
		code, body := ro.do("GET", path+"?project=zzz-nope", nil)
		if code != 400 {
			t.Fatalf("%s?project=zzz-nope: want 400, got %d (%v)", path, code, body)
		}
		if body["error"] != "bad_request" || body["detail"] != "no such project zzz-nope" {
			t.Fatalf("%s: тело ошибки должно быть одинаковым на всех ручках, got %v", path, body)
		}
	}
}

// Валидный проект и отсутствие фильтра работают ровно как раньше: 400 добавлен
// только для несуществующего слага, поведение остальных запросов не тронуто.
func TestProjectFilterValidProjectUnchanged(t *testing.T) {
	base, roKey, _ := projectFilterAPI(t)
	ro := &client{t: t, base: base, key: roKey}

	for _, path := range projectFilterPaths {
		if code, body := ro.do("GET", path+"?project=game", nil); code != 200 {
			t.Fatalf("%s?project=game: want 200, got %d (%v)", path, code, body)
		}
		if path == "/v1/environments" {
			// Единственная ручка, где пустой `?project=` не значит «вся
			// платформа»: окружения живут ВНУТРИ проекта, поэтому пустой фильтр
			// резолвится в единственный проект, а при двух — 400 «project is
			// required» (environments v1, поведение до этой карточки).
			continue
		}
		if code, body := ro.do("GET", path, nil); code != 200 {
			t.Fatalf("%s без фильтра: want 200, got %d (%v)", path, code, body)
		}
	}

	// Не-скрывающий контракт алертов (#955) валидацией НЕ сломан: у arena своих
	// алертов нет, но платформенный NodeDown (без лейбла project) виден всё так
	// же — 400 ловит опечатку, а не заменяет собой «пусто, потому что тихо».
	for _, path := range []string{"/v1/alerts/active", "/v1/alerts/history"} {
		code, body := ro.do("GET", path+"?project=arena", nil)
		if code != 200 {
			t.Fatalf("%s?project=arena: want 200, got %d (%v)", path, code, body)
		}
		alerts := body["alerts"].([]any)
		if len(alerts) != 1 || alerts[0].(map[string]any)["name"] != "NodeDown" {
			t.Fatalf("%s?project=arena: платформенный алерт обязан остаться видимым, got %v", path, alerts)
		}
	}
}

// Три исключения из правила, каждое по своей причине (см. projectFilter в
// server.go). Тест фиксирует именно ГРАНИЦУ: без него следующий проход
// «доведём консистентность до конца» снял бы их молча.
func TestProjectFilterExceptions(t *testing.T) {
	base, roKey, adminKey := projectFilterAPI(t)
	ro := &client{t: t, base: base, key: roKey}
	admin := &client{t: t, base: base, key: adminKey}

	// 1. /v1/qos — ручка ПУБЛИЧНАЯ: 400 «no such project» отдал бы любому игроку
	// оракул слагов. Неизвестный проект = пустой список эндпоинтов.
	anon := &client{t: t, base: base}
	code, body := anon.do("GET", "/v1/qos?project=zzz-nope&env=dev", nil)
	if code != 200 {
		t.Fatalf("публичный /v1/qos не валидирует проект: want 200, got %d (%v)", code, body)
	}
	if got := body["qos"].([]any); len(got) != 0 {
		t.Fatalf("/v1/qos неизвестного проекта: want [], got %v", got)
	}

	// 2. /v1/alerts/rules — каталог конфигурации, фильтр не принимает вовсе.
	if code, body := ro.do("GET", "/v1/alerts/rules?project=zzz-nope", nil); code != 200 {
		t.Fatalf("/v1/alerts/rules должен игнорировать ?project=: %d %v", code, body)
	}

	// 3. project в теле POST /v1/alerts/mutes — матчер хранимого правила, а не
	// фильтр над данными: рядом такой же невалидируемый region, панель берёт
	// значение из ЛЕЙБЛА алерта, а промах виден строкой в списке mute'ов.
	zzz := "zzz-nope"
	code, body = admin.do("POST", "/v1/alerts/mutes", map[string]any{
		"alertname": "NodeDown", "project": &zzz,
	})
	if code != 201 {
		t.Fatalf("mute с неизвестным проектом должен создаваться (201), got %d (%v)", code, body)
	}
	if mute := body["mute"].(map[string]any); mute["project"] != "zzz-nope" {
		t.Fatalf("mute сохранил не тот проект: %v", mute)
	}
}

// Опечатка ловится ДО апстрима и ДО чтения лога: на мастере без vmalert и без
// лог-сина ответ иначе неотличим от «всё спокойно» (пустая история — 200 [],
// а не ошибка), то есть ровно тот случай, ради которого карточка и заведена.
func TestProjectFilterBeforeAlertsSources(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ctx := t.Context()
	if _, err := st.SetProjectMatchSize(ctx, "game", 2); err != nil {
		t.Fatalf("project: %v", err)
	}
	_, roSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})

	// vmalert не настроен, лог-файла нет вовсе.
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).
		WithAlertsSources("", filepath.Join(t.TempDir(), "absent.log")))
	t.Cleanup(ts.Close)
	ro := &client{t: t, base: ts.URL, key: roSecret}

	code, body := ro.do("GET", "/v1/alerts/history?project=zzz-nope", nil)
	if code != 400 || !strings.Contains(body["detail"].(string), "zzz-nope") {
		t.Fatalf("history без лога: опечатка обязана давать 400, got %d (%v)", code, body)
	}
	if code, body := ro.do("GET", "/v1/alerts/history?project=game", nil); code != 200 {
		t.Fatalf("history без лога для валидного проекта: want 200 (пустой список), got %d (%v)", code, body)
	}
	// А вот «на этом мастере алертов нет вовсе» — факт более фундаментальный,
	// чем плохой ввод: 503 alerts_unconfigured остаётся первым.
	if code, _ := ro.do("GET", "/v1/alerts/active?project=zzz-nope", nil); code != 503 {
		t.Fatalf("active без vmalert: want 503 (alerts_unconfigured), got %d", code)
	}
}
