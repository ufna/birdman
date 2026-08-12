package httpapi_test

// ПОЛЕЗНАЯ НАГРУЗКА деградировавшего платформенного алерта (tracker #1020).
//
// #995 сознательно оставил привязанному ключу видимость алертов БЕЗ лейбла
// `project` («иначе арендатор не узнает, что лежит платформа»), а #1020 уточнил
// цену: платформенным может стать и ПРОЕКТНОЕ по смыслу правило, у которого не
// сматчился join. Сегодня такое ровно одно — `TickDegraded`, и его лейблы несут
// `server_id` чужого дедика, а `summary` — ещё и величину тика.
//
// Тест закрепляет ровно то, ЧТО ИЗ ЭТОГО ДОЕЗЖАЕТ ДО КЛИЕНТА, потому что
// TestAlertsKeepPlatformAlertForBoundKey доказывает только ВИДИМОСТЬ алерта и о
// его содержимом не говорит ничего. Держатся три инварианта, и все три —
// свойства нормализации (alerts.go), а не совпадение:
//   - `server_id` наружу НЕ выходит: поля под него нет ни в activeAlert, ни в
//     alertEvent, а alertNode читает только node/instance/hostname/host;
//   - текст `summary` наружу НЕ выходит: annotation() отдаёт description, если
//     он непуст, а у TickDegraded он непуст всегда (рендерится из rules.yml.j2
//     статической строкой);
//   - `node` (hostname узла) выходит на обеих ручках, числовой `value` — только
//     на /active.
// Первые два и есть та формулировка цены в docs/specs/master.md §6 и в
// комментарии keepForBinding, которая иначе тихо разъедется с кодом: начни
// нормализация переносить summary или server_id — утечка появится, спека станет
// верной задним числом, и заметить это будет некому.

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

// Лейблы и аннотации — как их отдаёт vmalert для ВТОРОЙ ветки правила
// (`unless on (server_id) birdman_server_info`): лейбла `project` НЕТ вовсе,
// `server_id` есть, `node`/`region` приезжают со скрейп-джоба агента
// (infra/roles/birdman_monitoring_dev/tasks/scrape_jobs.yml), `instance` —
// петлевой адрес vmagent'а. description — реальный текст правила, summary —
// реальный шаблон с уже подставленными $labels.server_id и $value.
const (
	degradedServerID = "srv-foreign-7f3a"
	degradedSummary  = "TickDegraded: server " + degradedServerID + " tick=120ms"
	degradedDesc     = "game-health: server tick has been degraded for longer than the threshold (ops.md §1 TickDegraded)."

	vmAlertsDegradedJSON = `{"status":"success","data":{"alerts":[
		{"state":"firing","name":"TickDegraded","labels":{"alertname":"TickDegraded","severity":"warning","region":"eu","node":"node-7","instance":"127.0.0.1:9101","server_id":"` + degradedServerID + `"},"annotations":{"summary":"` + degradedSummary + `","description":"` + degradedDesc + `","description_ru":"game-health: тик деградировал дольше порога."},"activeAt":"2026-08-12T09:00:00Z","value":"120"}
	]}}`

	alertsDegradedLog = `{"received_at":"2026-08-12T09:00:00Z","alerts":[{"status":"firing","labels":{"alertname":"TickDegraded","severity":"warning","region":"eu","node":"node-7","instance":"127.0.0.1:9101","server_id":"` + degradedServerID + `"},"annotations":{"summary":"` + degradedSummary + `","description":"` + degradedDesc + `","description_ru":"game-health: тик деградировал дольше порога."},"startsAt":"2026-08-12T08:59:00Z","endsAt":""}]}
`
)

func TestDegradedPlatformAlertPayloadForBoundKey(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ctx := t.Context()
	if _, err := st.SetProjectMatchSize(ctx, "neighbour", 2); err != nil {
		t.Fatalf("project neighbour: %v", err)
	}
	project, env := "neighbour", "dev"
	_, nbKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-neighbour", Scopes: []string{httpapi.ScopeReadonly}, Project: &project, Env: &env,
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "alerts.log")
	if err := os.WriteFile(logPath, []byte(alertsDegradedLog), 0o644); err != nil {
		t.Fatal(err)
	}
	vm := fakeVmalertWith(t, vmAlertsDegradedJSON)
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).
		WithAlertsSources(vm.URL, logPath))
	t.Cleanup(ts.Close)
	nb := &client{t: t, base: ts.URL, key: nbKey}

	for _, path := range alertReadPaths {
		// Сырое тело, а не разобранное: проверяется ОТСУТСТВИЕ, и по разобранной
		// карте его пришлось бы доказывать перечислением известных полей — то
		// есть новое поле с чужим id тест бы не поймал.
		code, raw := nb.doRaw("GET", path)
		if code != 200 {
			t.Fatalf("%s: %d %s", path, code, raw)
		}
		body := string(raw)
		if strings.Contains(body, degradedServerID) {
			t.Fatalf("%s: id чужого дедика уехал привязанному ключу — цена в "+
				"docs/specs/master.md §6 названа по этому замеру: %s", path, body)
		}
		if strings.Contains(body, "server_id") || strings.Contains(body, "summary") {
			t.Fatalf("%s: в выдаче появилось поле server_id/summary — нормализация "+
				"стала переносить лейблы/аннотации, которых §6 не ждёт: %s", path, body)
		}
		if strings.Contains(body, "tick=120ms") {
			t.Fatalf("%s: текст summary доехал до клиента (annotation() перестал "+
				"вытеснять его description'ом): %s", path, body)
		}

		got := alertsByName(t, nb, path)
		td, ok := got["TickDegraded"]
		if !ok {
			t.Fatalf("%s: платформенный алерт спрятан от привязанного ключа — "+
				"это половина правила #995, а не деталь: %v", path, got)
		}
		if td["scope"] != "platform" || td["project"] != "" {
			t.Fatalf("%s: деградировавший алерт приехал не платформенным: %v", path, td)
		}
		// А ЧТО уезжает — тоже пин, а не пояснение: цена названа именно этими
		// полями, и молчаливое исчезновение любого из них сделало бы §6 шире
		// правды с другой стороны.
		if td["node"] != "node-7" {
			t.Fatalf("%s: hostname узла — названная цена этого канала, а его нет: %v", path, td)
		}
		if td["severity"] != "warning" || td["description"] != degradedDesc {
			t.Fatalf("%s: алерт приехал выпотрошенным: %v", path, td)
		}
	}

	// Числовое значение тика — только на /active: у alertEvent поля `value` нет
	// вовсе, и §6 обязана называть эту ручку, а не обе.
	active := alertsByName(t, nb, "/v1/alerts/active")["TickDegraded"]
	if active["value"] != "120" {
		t.Fatalf("/v1/alerts/active: величина деградации не приехала: %v", active)
	}
	history := alertsByName(t, nb, "/v1/alerts/history")["TickDegraded"]
	if _, hasValue := history["value"]; hasValue {
		t.Fatalf("/v1/alerts/history: появилось поле value — §6 обещает его только "+
			"на /active: %v", history)
	}
}
