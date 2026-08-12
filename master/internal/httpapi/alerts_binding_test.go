package httpapi_test

// Арендаторская граница на АЛЕРТАХ (tracker #995, решение владельца
// «б-полностью» из #993). До неё привязанный readonly-ключ проекта
// `neighbour/dev` получал 200 на `GET /v1/alerts/active` и видел горящий алерт
// чужого проекта `game` вместе со значением и описанием: сужение по проекту там
// было, но НЕ-СКРЫВАЮЩЕЕ by design (#955), а привязку ручка не смотрела вовсе.
//
// У правила ДВЕ половины, и вторая — не «приятное дополнение», а условие, при
// котором первая вообще имеет право существовать:
//  1. алерт ЧУЖОГО проекта привязанному ключу не виден (и `?project=<чужой>` →
//     403, байт-в-байт как на листингах #993 — живой чужой неотличим от
//     выдуманного);
//  2. ПЛАТФОРМЕННЫЙ алерт (без лейбла `project`) привязанному ключу виден —
//     иначе арендатор не узнает, что лежит мастер, и гейт станет вреден;
//  3. свой алерт виден ЦЕЛИКОМ (проверяется телом, а не кодом «не 403»);
//  4. непривязанный ключ, admin и сессия панели — как раньше, не-скрывающий
//     `?project=` цел и опечатка по-прежнему 400;
//  5. `/v1/alerts/mutes` сужен тем же правилом, `/v1/alerts/rules` — намеренно
//     НЕ сужен (см. TestAlertRulesStayWholeForBoundKey).

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Три горящих алерта: по одному на каждый проект и один платформенный (без
// лейбла `project` вовсе — не с пустым значением, а именно без ключа, как их
// отдаёт vmalert для MasterDown/NodeDown). Значения и описания различны, чтобы
// «увидел свой» проверялось содержимым.
const vmAlertsTenantJSON = `{"status":"success","data":{"alerts":[
	{"state":"firing","name":"BufferEmptyReadyProd","labels":{"alertname":"BufferEmptyReadyProd","severity":"critical","region":"eu","project":"game"},"annotations":{"description":"game has no ready servers"},"activeAt":"2026-08-11T09:00:00Z","value":"0"},
	{"state":"firing","name":"AllocationFailures","labels":{"alertname":"AllocationFailures","severity":"warning","region":"eu","project":"neighbour"},"annotations":{"description":"neighbour alloc failures","description_ru":"отказы аллокации у соседа"},"activeAt":"2026-08-11T09:10:00Z","value":"7"},
	{"state":"firing","name":"MasterDown","labels":{"alertname":"MasterDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"master is down"},"activeAt":"2026-08-11T09:20:00Z","value":"1"}
]}}`

// Каталог правил фикстуры: одно ПЛАТФОРМЕННОЕ правило (NodeDown, inactive) и
// одно проектное по построению — его `expr` агрегирует по `(region, project,
// env)`, и оно ГОРИТ, причём единственный его инстанс принадлежит проекту
// `game`. Второе правило заведено затем, чтобы тест каталога не был
// тавтологией: `state` горящего правила и есть та чужая величина, которую
// решение #995 сознательно оставляет видимой привязанному ключу.
const vmRulesTenantJSON = `{"status":"success","data":{"groups":[{"name":"birdman","file":"/etc/vmalert/birdman.yml","rules":[
	{"name":"NodeDown","query":"birdman_node_heartbeat_age_seconds > 30","duration":30,"labels":{"severity":"critical"},"annotations":{"description":"node is unreachable"},"state":"inactive","type":"alerting"},
	{"name":"BufferEmptyReadyProd","query":"sum by (region, project, env) (birdman_servers{state=\"ready\"}) == 0","duration":120,"labels":{"severity":"critical"},"annotations":{"description":"no ready servers"},"state":"firing","type":"alerting"}
]}]}}`

// История зеркалит активные: те же три алерта тремя доставками.
const alertsTenantLog = `{"received_at":"2026-08-11T09:00:00Z","alerts":[{"status":"firing","labels":{"alertname":"BufferEmptyReadyProd","severity":"critical","region":"eu","project":"game"},"annotations":{"description":"game has no ready servers"},"startsAt":"2026-08-11T08:59:00Z","endsAt":""}]}
{"received_at":"2026-08-11T09:10:00Z","alerts":[{"status":"firing","labels":{"alertname":"AllocationFailures","severity":"warning","region":"eu","project":"neighbour"},"annotations":{"description":"neighbour alloc failures","description_ru":"отказы аллокации у соседа"},"startsAt":"2026-08-11T09:09:00Z","endsAt":""}]}
{"received_at":"2026-08-11T09:20:00Z","alerts":[{"status":"firing","labels":{"alertname":"MasterDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"master is down"},"startsAt":"2026-08-11T09:19:00Z","endsAt":""}]}
`

type alertsTenantFixture struct {
	base string

	nbKey     string // readonly, привязан к neighbour/dev
	gameKey   string // readonly, привязан к game/dev
	globalKey string // readonly без привязки
	adminKey  string // admin (привязать его нельзя — CreateAPIKey отвергает)
}

// newAlertsTenantFixture поднимает фикстуру на batch-форме лога — той, что
// пишет сегодняшний sink. extraLogLines дописываются в ТОТ ЖЕ файл: у
// `/var/log/birdman/alerts.log` долгая жизнь (он дописывается, а не
// пересоздаётся), поэтому «старые строки одной формы вперемешку с новыми
// другой» — это не искусственный кейс, а то, как файл и выглядит после
// перестройки канала (#244). Существующие тесты передают ноль строк и
// считают ровно те же алерты, что и раньше.
func newAlertsTenantFixture(t *testing.T, extraLogLines ...string) *alertsTenantFixture {
	t.Helper()
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ctx := t.Context()

	// Оба проекта заводятся первым касанием (ensureProject), и НОВЫЙ проект
	// сразу получает окружения dev+prod (store/nodes.go) — пара привязки должна
	// существовать, иначе CreateAPIKey откажет.
	for _, slug := range []string{"game", "neighbour"} {
		if _, err := st.SetProjectMatchSize(ctx, slug, 2); err != nil {
			t.Fatalf("project %s: %v", slug, err)
		}
	}

	logPath := filepath.Join(t.TempDir(), "alerts.log")
	logBody := alertsTenantLog
	for _, ln := range extraLogLines {
		logBody += ln + "\n"
	}
	if err := os.WriteFile(logPath, []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// Свой фейк vmalert, а не общий fakeVmalertWith: этой фикстуре нужен ещё и
	// свой каталог правил (см. vmRulesTenantJSON), а общий каталог заперт
	// ожиданиями TestAlertsEndpoints.
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/rules":
			_, _ = w.Write([]byte(vmRulesTenantJSON))
		case "/api/v1/alerts":
			_, _ = w.Write([]byte(vmAlertsTenantJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(vm.Close)
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).
		WithAlertsSources(vm.URL, logPath))
	t.Cleanup(ts.Close)

	key := func(name string, scope string, project, env *string) string {
		t.Helper()
		_, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
			Name: name, Scopes: []string{scope}, Project: project, Env: env,
		})
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		return secret
	}
	nbProject, nbEnv := "neighbour", "dev"
	gameProject, gameEnv := "game", "dev"
	f := &alertsTenantFixture{
		base:      ts.URL,
		nbKey:     key("ro-neighbour", httpapi.ScopeReadonly, &nbProject, &nbEnv),
		gameKey:   key("ro-game", httpapi.ScopeReadonly, &gameProject, &gameEnv),
		globalKey: key("ro-global", httpapi.ScopeReadonly, nil, nil),
		adminKey:  key("admin", httpapi.ScopeAdmin, nil, nil),
	}

	// Три mute'а тем же раскладом, что алерты: проектный свой, проектный чужой и
	// один БЕЗ проекта (кроет всё, включая платформенный алерт арендатора).
	admin := &client{t: t, base: ts.URL, key: f.adminKey}
	mute := func(alertname string, project *string, note string) {
		t.Helper()
		body := map[string]any{"alertname": alertname, "note": note}
		if project != nil {
			body["project"] = *project
		}
		if code, resp := admin.do("POST", "/v1/alerts/mutes", body); code != 201 {
			t.Fatalf("mute %s: %d %v", alertname, code, resp)
		}
	}
	mute("BufferEmptyReadyProd", &gameProject, "game maintenance window")
	mute("AllocationFailures", &nbProject, "neighbour maintenance window")
	mute("MasterDown", nil, "platform-wide")
	return f
}

// alertsByName снимает одну ручку и раскладывает ответ по имени алерта,
// проваливая тест на не-200 или битом теле.
func alertsByName(t *testing.T, c *client, path string) map[string]map[string]any {
	t.Helper()
	code, body := c.do("GET", path, nil)
	if code != 200 {
		t.Fatalf("%s: %d %v", path, code, body)
	}
	out := map[string]map[string]any{}
	for _, raw := range body["alerts"].([]any) {
		a := raw.(map[string]any)
		out[a["name"].(string)] = a
	}
	return out
}

var alertReadPaths = []string{"/v1/alerts/active", "/v1/alerts/history"}

// Половина 1: алерт ЧУЖОГО проекта привязанному ключу не виден — ни без
// параметра (главный случай: до #995 именно так и текло), ни при явном своём
// `?project=`. Плюс симметрия вторым привязанным ключом: без неё те же проверки
// прошли бы на гейте, который сузил бы выдачу к ОДНОМУ захардкоженному проекту.
func TestAlertsHideForeignProjectFromBoundKey(t *testing.T) {
	f := newAlertsTenantFixture(t)
	nb := &client{t: t, base: f.base, key: f.nbKey}
	game := &client{t: t, base: f.base, key: f.gameKey}

	for _, path := range alertReadPaths {
		for _, q := range []string{"", "?project=neighbour"} {
			got := alertsByName(t, nb, path+q)
			if _, leaked := got["BufferEmptyReadyProd"]; leaked {
				t.Fatalf("%s%s: алерт чужого проекта виден привязанному ключу: %v", path, q, got)
			}
			if len(got) != 2 {
				t.Fatalf("%s%s: want свой+платформенный, got %v", path, q, got)
			}
		}
		// Симметрия: другой привязанный ключ видит СВОЙ алерт и не видит
		// neighbour — то есть сужение идёт по привязке запроса, а не к
		// фиксированному проекту.
		got := alertsByName(t, game, path)
		if _, leaked := got["AllocationFailures"]; leaked {
			t.Fatalf("%s ключом game/dev: виден алерт neighbour: %v", path, got)
		}
		if _, own := got["BufferEmptyReadyProd"]; !own || len(got) != 2 {
			t.Fatalf("%s ключом game/dev: want свой+платформенный, got %v", path, got)
		}
	}
}

// Явный ЧУЖОЙ `?project=` → 403 тем же телом, что у листингов (#993) и
// requireBinding, и БАЙТ-В-БАЙТ одинаково для живого чужого проекта и для
// выдуманного. Второе важнее первого: провалидируй мы слаг до гейта, ручка
// отвечала бы 403 на существующий проект и 400 «no such project» на
// несуществующий — то есть осталась бы оракулом существования, только на другом
// коде (урок #989). Это и есть причина, по которой правило посажено на общий
// tenantScope, а не написано в alerts.go заново.
func TestAlertsRefuseForeignProjectParamForBoundKey(t *testing.T) {
	f := newAlertsTenantFixture(t)
	nb := &client{t: t, base: f.base, key: f.nbKey}

	for _, path := range alertReadPaths {
		liveCode, live := nb.doRaw("GET", path+"?project=game")
		ghostCode, ghost := nb.doRaw("GET", path+"?project=zzz-nope")
		if liveCode != 403 || ghostCode != 403 {
			t.Fatalf("%s: живой чужой проект=%d, выдуманный=%d, want 403 у обоих\n живое=%s\n выдуманное=%s",
				path, liveCode, ghostCode, live, ghost)
		}
		if !bytes.Equal(live, ghost) {
			t.Fatalf("%s: 403 отличается телом и потому остаётся оракулом существования:\n живое=%s\n выдуманное=%s",
				path, live, ghost)
		}
		if want := `{"detail":"key is bound to neighbour/dev","error":"forbidden"}`; string(bytes.TrimSpace(live)) != want {
			t.Fatalf("%s: тело отказа разъехалось с requireBinding: got %s, want %s", path, live, want)
		}
	}
}

// Половина 2 — СМЫСЛ правила, и без неё гейт вреден: платформенный алерт (без
// лейбла `project`) привязанный ключ обязан видеть, иначе он не узнает, что
// лежит мастер. Проверяется содержимым: имя, scope, severity, описание и флаг
// muted (mute без проекта кроет и платформенный алерт — арендатор должен видеть
// не только сам алерт, но и причину, по которой он приглушён).
func TestAlertsKeepPlatformAlertForBoundKey(t *testing.T) {
	f := newAlertsTenantFixture(t)
	nb := &client{t: t, base: f.base, key: f.nbKey}

	for _, path := range alertReadPaths {
		got := alertsByName(t, nb, path)
		md, ok := got["MasterDown"]
		if !ok {
			t.Fatalf("%s: ПЛАТФОРМЕННЫЙ алерт спрятан от привязанного ключа — "+
				"арендатор не узнает, что лежит мастер: %v", path, got)
		}
		if md["scope"] != "platform" || md["project"] != "" {
			t.Fatalf("%s: платформенный алерт приехал с проектом: %v", path, md)
		}
		if md["severity"] != "critical" || md["description"] != "master is down" {
			t.Fatalf("%s: платформенный алерт приехал выпотрошенным: %v", path, md)
		}
		if md["muted"] != true {
			t.Fatalf("%s: mute без проекта кроет платформенный алерт, want muted=true: %v", path, md)
		}
		// И тот же ответ содержит СВОЙ алерт целиком — «200» тут был и до
		// карточки, поэтому сверяются поля, а не код.
		own, ok := got["AllocationFailures"]
		if !ok {
			t.Fatalf("%s: свой алерт пропал вместе с чужим — сужение съело арендатора: %v", path, got)
		}
		if own["project"] != "neighbour" || own["scope"] != "project" ||
			own["severity"] != "warning" || own["description"] != "neighbour alloc failures" ||
			own["description_ru"] != "отказы аллокации у соседа" || own["muted"] != true {
			t.Fatalf("%s: свой алерт приехал не целиком: %v", path, own)
		}
		if own["region"] != "eu" {
			t.Fatalf("%s: свой алерт без региона: %v", path, own)
		}
	}
	// /active несёт ещё и value — числовое значение горящего алерта. Оно есть
	// только там, поэтому проверяется отдельно (в истории поля нет by design).
	active := alertsByName(t, nb, "/v1/alerts/active")
	if active["AllocationFailures"]["value"] != "7" {
		t.Fatalf("/v1/alerts/active: у своего алерта потеряно значение: %v", active["AllocationFailures"])
	}
	// А в истории — время доставки и старта.
	history := alertsByName(t, nb, "/v1/alerts/history")
	own := history["AllocationFailures"]
	if own["received_at"] != "2026-08-11T09:10:00Z" || own["startsAt"] != "2026-08-11T09:09:00Z" ||
		own["active"] != true {
		t.Fatalf("/v1/alerts/history: у своего алерта потеряны времена/active: %v", own)
	}
}

// Непривязанные ключи — как раньше, и это половина приёмки: не-скрывающий
// `?project=` (#955) сознательное решение, ломать его нельзя. Проверяются все
// три формы непривязанного доступа: readonly без привязки, admin и СЕССИЯ
// ПАНЕЛИ (она наследует ключ логина целиком — #974/#1000).
func TestAlertsUnboundKeysUnchanged(t *testing.T) {
	f := newAlertsTenantFixture(t)
	for _, k := range []struct{ name, key string }{
		{"global-readonly", f.globalKey},
		{"admin", f.adminKey},
	} {
		c := &client{t: t, base: f.base, key: k.key}
		for _, path := range alertReadPaths {
			// Без параметра — вся платформа, все три алерта.
			if all := alertsByName(t, c, path); len(all) != 3 {
				t.Fatalf("%s ключом %s: want все 3 алерта, got %v", path, k.name, all)
			}
			// `?project=game` — сужение НЕ скрывающее: чужой проектный алерт
			// уходит, ПЛАТФОРМЕННЫЙ остаётся.
			got := alertsByName(t, c, path+"?project=game")
			if _, ok := got["MasterDown"]; !ok {
				t.Fatalf("%s?project=game ключом %s: платформенный алерт обязан остаться: %v",
					path, k.name, got)
			}
			if _, ok := got["BufferEmptyReadyProd"]; !ok || len(got) != 2 {
				t.Fatalf("%s?project=game ключом %s: want game+платформенный, got %v", path, k.name, got)
			}
			// Опечатка — по-прежнему 400, а не молча суженный экран (#961).
			if code, body := c.do("GET", path+"?project=zzz-nope", nil); code != 400 {
				t.Fatalf("%s?project=zzz-nope ключом %s: want 400, got %d %v", path, k.name, code, body)
			}
		}
	}

	// Сессия панели. Логин ГЛОБАЛЬНЫМ ключом — платформа целиком, как и было.
	b := &browser{t: t, base: f.base}
	_, _, resp := b.do("POST", "/v1/session", map[string]any{"api_key": f.globalKey})
	b.cookie = sessionCookieOf(t, resp)
	for _, path := range alertReadPaths {
		code, body, _ := b.do("GET", path, nil)
		if code != 200 || len(body["alerts"].([]any)) != 3 {
			t.Fatalf("%s сессией панели (глобальный ключ): %d %v", path, code, body)
		}
	}

	// Логин ПРИВЯЗАННЫМ ключом — сессия наследует привязку, и граница действует
	// в панели тоже. Это осознанное следствие (#993/#1000), а не дефект панели,
	// поэтому оно закреплено тестом, а не оставлено на догадку.
	bb := &browser{t: t, base: f.base}
	_, _, resp = bb.do("POST", "/v1/session", map[string]any{"api_key": f.nbKey})
	bb.cookie = sessionCookieOf(t, resp)
	for _, path := range alertReadPaths {
		code, body, _ := bb.do("GET", path, nil)
		if code != 200 {
			t.Fatalf("%s сессией панели (привязанный ключ): %d %v", path, code, body)
		}
		for _, raw := range body["alerts"].([]any) {
			if p := raw.(map[string]any)["project"]; p == "game" {
				t.Fatalf("%s сессией панели (привязанный ключ): виден алерт чужого проекта: %v", path, raw)
			}
		}
		if len(body["alerts"].([]any)) != 2 {
			t.Fatalf("%s сессией панели (привязанный ключ): want свой+платформенный, got %v", path, body)
		}
	}
}

// `/v1/alerts/mutes` — «листинг БЕЗ фильтра»: `?project=` он не принимает вовсе
// и до #995 отдавал привязанному ключу mute'ы чужих проектов вместе с `note`
// (свободный текст оператора) и `created_by`. Решено сужать тем же правилом, что
// алерты: mute — аннотация НАД алертом, и его видимость обязана совпадать с
// видимостью алерта, который он гасит.
func TestAlertMutesNarrowToBindingForBoundKey(t *testing.T) {
	f := newAlertsTenantFixture(t)
	nb := &client{t: t, base: f.base, key: f.nbKey}
	game := &client{t: t, base: f.base, key: f.gameKey}
	global := &client{t: t, base: f.base, key: f.globalKey}

	muteSet := func(c *client, path string) map[string]map[string]any {
		t.Helper()
		code, body := c.do("GET", path, nil)
		if code != 200 {
			t.Fatalf("%s: %d %v", path, code, body)
		}
		out := map[string]map[string]any{}
		for _, raw := range body["mutes"].([]any) {
			m := raw.(map[string]any)
			out[m["alertname"].(string)] = m
		}
		return out
	}

	// `?all=1` тоже сужается: иначе граница обходится одним параметром.
	for _, path := range []string{"/v1/alerts/mutes", "/v1/alerts/mutes?all=1"} {
		got := muteSet(nb, path)
		if _, leaked := got["BufferEmptyReadyProd"]; leaked {
			t.Fatalf("%s: mute чужого проекта виден привязанному ключу (с note и created_by): %v", path, got)
		}
		own, ok := got["AllocationFailures"]
		if !ok {
			t.Fatalf("%s: свой mute пропал: %v", path, got)
		}
		if own["project"] != "neighbour" || own["note"] != "neighbour maintenance window" {
			t.Fatalf("%s: свой mute приехал выпотрошенным: %v", path, own)
		}
		// Mute БЕЗ проекта кроет всё, включая платформенный алерт арендатора, —
		// спрятав его, мы оставили бы `muted:true` на своём алерте без причины.
		platform, ok := got["MasterDown"]
		if !ok {
			t.Fatalf("%s: mute без проекта (кроет и платформенный алерт) спрятан: %v", path, got)
		}
		if platform["project"] != nil {
			t.Fatalf("%s: у платформенного mute появился проект: %v", path, platform)
		}
		if len(got) != 2 {
			t.Fatalf("%s: want свой + платформенный mute, got %v", path, got)
		}

		// Непривязанный ключ — все три, как и было.
		if all := muteSet(global, path); len(all) != 3 {
			t.Fatalf("%s глобальным ключом: want 3 mute'а, got %v", path, all)
		}

		// СИММЕТРИЯ вторым привязанным ключом — та же проверка, что у алертов, и
		// по той же причине: без неё всё выше одинаково прошло бы на сужении к
		// ОДНОМУ захардкоженному проекту (найдено вторым проходом: мутация
		// `keepForBinding(muteProject, "neighbour")` оставалась зелёной, хотя это
		// прямая утечка — ключ game/dev получал бы чужие mute'ы с note и
		// created_by и терял свои).
		other := muteSet(game, path)
		if _, leaked := other["AllocationFailures"]; leaked {
			t.Fatalf("%s ключом game/dev: виден mute проекта neighbour: %v", path, other)
		}
		own, ok = other["BufferEmptyReadyProd"]
		if !ok || own["project"] != "game" || own["note"] != "game maintenance window" {
			t.Fatalf("%s ключом game/dev: свой mute потерян или выпотрошен: %v", path, other)
		}
		if _, ok := other["MasterDown"]; !ok || len(other) != 2 {
			t.Fatalf("%s ключом game/dev: want свой + платформенный mute, got %v", path, other)
		}
	}
}

// `/v1/alerts/rules` НЕ сужается — и это решение, а не забытая ручка. Правило
// не несёт проекта СТРУКТУРНО (его `expr` разворачивается в серию на проект),
// поэтому выбор был между «отдать всем» и «закрыть привязанному вовсе», и
// выбрано отдать. Тест сверяет ответы привязанного и глобального ключа
// БАЙТ-В-БАЙТ и ЗАОДНО показывает ЦЕНУ решения: в каталоге горит правило,
// единственный инстанс которого принадлежит чужому проекту и в `/active`
// привязанному ключу не виден. Так следующий читатель видит не только «ручку не
// сужаем», но и ровно ту чужую величину, которую мы согласились отдать.
func TestAlertRulesStayWholeForBoundKey(t *testing.T) {
	f := newAlertsTenantFixture(t)
	nb := &client{t: t, base: f.base, key: f.nbKey}
	global := &client{t: t, base: f.base, key: f.globalKey}

	boundCode, bound := nb.doRaw("GET", "/v1/alerts/rules")
	globalCode, all := global.doRaw("GET", "/v1/alerts/rules")
	if boundCode != 200 || globalCode != 200 {
		t.Fatalf("rules: привязанный=%d глобальный=%d\n%s\n%s", boundCode, globalCode, bound, all)
	}
	if !bytes.Equal(bound, all) {
		t.Fatalf("каталог правил разъехался с решением #995 (сужать не по чему):\n привязанный=%s\n глобальный=%s",
			bound, all)
	}
	// ЦЕНА, названная вслух: `state` правила — платформенный агрегат, и «горит»
	// он здесь из-за инстанса проекта game, которого привязанный ключ в /active
	// не видит. Утверждать «каталог не отдаёт ни одной чужой величины» нельзя —
	// отдаёт эту; принято сознательно (спека §6, #995).
	if !bytes.Contains(bound, []byte(`"name":"BufferEmptyReadyProd","group":"birdman","severity":"critical"`)) ||
		!bytes.Contains(bound, []byte(`"state":"firing"`)) {
		t.Fatalf("каталог привязанному ключу приехал без горящего правила — тест перестал показывать цену решения: %s", bound)
	}
	if active := alertsByName(t, nb, "/v1/alerts/active"); active["BufferEmptyReadyProd"] != nil {
		t.Fatalf("инстанс чужого правила виден привязанному ключу — фикстура разъехалась с замыслом теста: %v", active)
	}
	if !bytes.Contains(bound, []byte(`"name":"NodeDown"`)) {
		t.Fatalf("каталог правил пуст — тест сверяет два пустых ответа: %s", bound)
	}
}
