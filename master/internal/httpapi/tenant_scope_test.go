package httpapi_test

// Арендаторская граница на ЧТЕНИЯХ (tracker #993, решение владельца
// «б-полностью»). До неё привязка ключа была ПОТОЧЕЧНЫМ гейтом над адресуемым
// объектом (#963/#974/#988/#989), а листинги её не энфорсили вовсе: привязанный
// readonly-ключ проекта Б читал через `?project=<чужой>` имена окружений,
// hostname и public_ip нод, image_ref версий, матчи, ленту событий и агрегаты
// проекта А — и, что важнее, получал всё то же самое БЕЗ параметра, потому что
// пустой фильтр значил «вся платформа».
//
// Поэтому тест держит ОБЕ половины, и вторая — суть решения:
//  1. явный ЧУЖОЙ `?project=` → 403;
//  2. БЕЗ параметра → только своя пара (project, env), а не платформа;
//  3. свой проект → 200 с РЕАЛЬНЫМ непустым телом (не «не 403»);
//  4. глобальный ключ, admin и сессия панели — как раньше;
//  5. `?env=` (#971) больше не оракул: несуществующее окружение чужого проекта
//     и существующее чужое дают БАЙТ-В-БАЙТ неотличимый ответ.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// tenantFixture — две пары с РАЗЛИЧИМЫМ содержимым в каждой ячейке, чтобы
// «сузилось» отличалось от «пусто» и от «всё»:
//
//	game/dev       node-1     cap 10   v1.0.0   1 сервер   1 матч
//	game/prod      game-prod  cap  5   v2.0.0   —          —
//	neighbour/dev  nb-dev     cap  7   v5.5.5   1 сервер   1 матч
//	neighbour/prod nb-prod    cap  3   v6.6.6   1 сервер   1 матч
//
// Плюс нода game/dev в РЕГИОНЕ us (cap 4): регион, где у соседа нет ничего, не
// должен всплывать в его ответах даже строкой с нулями. Плюс третье окружение
// neighbour/stage (пустое) — чтобы сужение списка окружений было видно и ВНУТРИ
// своего проекта, а не только через границу.
//
// Ёмкости в eu попарно различны и в сумме дают 25: снимок utilization в
// /v1/stats/cost показывает 7 привязанному и 25 глобальному, так что «сузилось»
// проверяется числом, а не наличием ключа.
type tenantFixture struct {
	base string

	nbKey     string // readonly, привязан к neighbour/dev
	gameKey   string // readonly, привязан к game/dev
	globalKey string // readonly без привязки
	adminKey  string // admin (привязать его нельзя — CreateAPIKey отвергает)

	nbDevServerID string
}

func newTenantFixture(t *testing.T) *tenantFixture {
	t.Helper()
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // game/dev: node-1 (cap 10) + v1.0.0
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

	nodeIn := func(project, env, region, hostname, ip string, cap int32) string {
		t.Helper()
		n, _, err := st.CreateNode(ctx, store.CreateNodeParams{
			Project: project, Region: region, Hostname: hostname, PublicIP: ip,
			CapacitySlots: cap, Env: env,
		})
		if err != nil {
			t.Fatalf("create node %s/%s: %v", project, env, err)
		}
		f.SetHeartbeatAge(t, n.ID, 0)
		return n.ID
	}
	node := func(project, env, hostname, ip string, cap int32) string {
		t.Helper()
		return nodeIn(project, env, "eu", hostname, ip, cap)
	}
	version := func(project, env, semver string) string {
		t.Helper()
		v, err := st.CreateVersion(ctx, store.CreateVersionParams{
			Project: project, Semver: semver, Env: env,
			ImageRef: "ghcr.io/example/" + project + "-server:" + semver,
		})
		if err != nil {
			t.Fatalf("create version %s/%s: %v", project, env, err)
		}
		return v.ID
	}
	// Матч с непустым started_at — статистика считает только стартовавшие.
	match := func(project, env, serverID, versionID string) {
		t.Helper()
		_, err := st.Pool.Exec(ctx, `
			insert into matches (project_id, server_id, version_id, region, env, state,
			                     players_peak, started_at, created_at)
			select p.id, $1::uuid, $2::uuid, 'eu', $3, 'running', 5,
			       now() - interval '1 hour', now() - interval '1 hour'
			from projects p where p.slug = $4`, serverID, versionID, env, project)
		if err != nil {
			t.Fatalf("insert match %s/%s: %v", project, env, err)
		}
	}

	node("game", "prod", "game-prod-1", "203.0.113.11", 5)
	nbDevNode := node("neighbour", "dev", "nb-dev-1", "198.51.100.10", 7)
	nbProdNode := node("neighbour", "prod", "nb-prod-1", "198.51.100.11", 3)
	// Регион, в котором есть ТОЛЬКО чужая ёмкость: привязанному ключу он не
	// должен показаться даже строкой с нулями — иначе состав платформы читается
	// по именам регионов, а сузили мы только числа.
	nodeIn("game", "dev", "us", "game-us-1", "203.0.113.12", 4)

	gameProdVer := version("game", "prod", "2.0.0")
	nbDevVer := version("neighbour", "dev", "5.5.5")
	nbProdVer := version("neighbour", "prod", "6.6.6")
	_ = gameProdVer

	gameDevSrv := f.InsertServerOn(t, f.NodeID, f.VersionID, "allocated")
	nbDevSrv := f.InsertServerOn(t, nbDevNode, nbDevVer, "allocated")
	nbProdSrv := f.InsertServerOn(t, nbProdNode, nbProdVer, "allocated")

	match("game", "dev", gameDevSrv, f.VersionID)
	match("neighbour", "dev", nbDevSrv, nbDevVer)
	match("neighbour", "prod", nbProdSrv, nbProdVer)

	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "neighbour", Name: "stage", RetentionKeep: 5,
	}); err != nil {
		t.Fatalf("create neighbour/stage: %v", err)
	}

	// Платформенное событие (project_id is null): лента не скрывающая, и это
	// должно остаться верным и для привязанного ключа.
	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind, payload) values ('backup_completed', '{}'::jsonb)`); err != nil {
		t.Fatalf("insert platform event: %v", err)
	}

	key := func(name string, project, env *string) string {
		t.Helper()
		_, secret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
			Name: name, Scopes: []string{httpapi.ScopeReadonly}, Project: project, Env: env,
		})
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		return secret
	}
	nbProject, nbEnv := "neighbour", "dev"
	gameProject, gameEnv := "game", "dev"
	_, adminSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "admin", Scopes: []string{httpapi.ScopeAdmin},
	})
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}

	return &tenantFixture{
		base:          ts.URL,
		nbKey:         key("ro-neighbour", &nbProject, &nbEnv),
		gameKey:       key("ro-game", &gameProject, &gameEnv),
		globalKey:     key("ro-global", nil, nil),
		adminKey:      adminSecret,
		nbDevServerID: nbDevSrv,
	}
}

// tenantListingPaths — ВЕСЬ класс листингов/агрегатов, снятый с роутера
// (server.go), а не с карточки (она сама называет свой список неполным).
// Вне класса и вне этого диффа по границам слайса: `/v1/alerts/*` (у них свой
// фильтр — не-скрывающий для непривязанного ключа; закрыты #995, тесты в
// alerts_binding_test.go), сырые проксии `/v1/logs/query`·`/v1/metrics/query*`
// (#990 закрыл их дверью requireUnboundKey, сужение — #994). `/v1/projects`
// проверяется отдельно: он `?project=` не принимает вовсе. `GET
// /v1/events/stream` вошёл в класс карточкой #999 и ходит через тот же
// `tenantScope`, но в ЭТОТ слайс не добавлен: обход его циклами (doRaw до конца
// тела) на бесконечном стриме не вернулся бы никогда — тенантные тесты стрима
// живут в sse_test.go и сверяют то же тело отказа.
var tenantListingPaths = []string{
	"/v1/nodes",
	"/v1/servers",
	"/v1/versions",
	"/v1/matches",
	"/v1/environments",
	"/v1/events",
	"/v1/stats/overview",
	"/v1/stats/cost",
}

// Половина 1: явный ЧУЖОЙ `?project=` → 403 на КАЖДОЙ ручке класса, и отказ
// БАЙТ-В-БАЙТ одинаков для живого чужого проекта и для выдуманного. Второе
// важнее первого: провалидируй мы слаг до гейта, ручка отвечала бы 403 на
// существующий проект и 400 «no such project» на несуществующий — то есть
// осталась бы оракулом существования, только на другом коде (урок #989).
func TestListingsRefuseForeignProjectForBoundKey(t *testing.T) {
	f := newTenantFixture(t)
	bound := &client{t: t, base: f.base, key: f.nbKey}

	for _, path := range tenantListingPaths {
		liveCode, live := bound.doRaw("GET", path+"?project=game")
		ghostCode, ghost := bound.doRaw("GET", path+"?project=zzz-nope")
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

// Половина 2 — СУТЬ РЕШЕНИЯ и та, которую легко забыть: БЕЗ параметра
// привязанный ключ обязан видеть только свою пару, а не платформу. Проверяется
// СОСТАВОМ выдачи, а не кодом: «200» тут был и до карточки.
func TestListingsNarrowToBindingWithoutParam(t *testing.T) {
	f := newTenantFixture(t)
	bound := &client{t: t, base: f.base, key: f.nbKey}

	// Ноды: только nb-dev-1. Хостнеймы и public_ip чужих нод — ровно то, что
	// утекало (замер карточки), поэтому сверяем поимённо.
	_, body := bound.do("GET", "/v1/nodes", nil)
	nodes := body["nodes"].([]any)
	if len(nodes) != 1 || nodes[0].(map[string]any)["hostname"] != "nb-dev-1" {
		t.Fatalf("/v1/nodes привязанным ключом: want ровно nb-dev-1, got %v", nodes)
	}

	// Версии: только 5.5.5 (image_ref чужих сборок — вторая утечка замера).
	_, body = bound.do("GET", "/v1/versions", nil)
	versions := body["versions"].([]any)
	if len(versions) != 1 || versions[0].(map[string]any)["semver"] != "5.5.5" {
		t.Fatalf("/v1/versions привязанным ключом: want ровно 5.5.5, got %v", versions)
	}

	// Серверы: только сервер своей пары (у neighbour есть и prod-сервер —
	// сужение по env, а не только по проекту).
	_, body = bound.do("GET", "/v1/servers", nil)
	servers := body["servers"].([]any)
	if len(servers) != 1 || servers[0].(map[string]any)["id"] != f.nbDevServerID {
		t.Fatalf("/v1/servers привязанным ключом: want ровно сервер neighbour/dev, got %v", servers)
	}

	// Матчи: пар-точно, как `GET /v1/matches/{id}` с #974.
	_, body = bound.do("GET", "/v1/matches", nil)
	matches := body["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("/v1/matches привязанным ключом: want 1 матч своей пары, got %v", matches)
	}
	if mm := matches[0].(map[string]any); mm["project"] != "neighbour" || mm["env"] != "dev" {
		t.Fatalf("/v1/matches отдал матч чужой пары: %v", mm)
	}

	// Окружения: ровно своё. Их перечисление и было главным каналом имён —
	// #989 закрыл usage, а имена соседей отдавал этот листинг.
	_, body = bound.do("GET", "/v1/environments", nil)
	envs := body["environments"].([]any)
	if len(envs) != 1 || envs[0].(map[string]any)["name"] != "dev" {
		t.Fatalf("/v1/environments привязанным ключом: want ровно [dev], got %v", envs)
	}

	// Проекты: только свой. Иначе «граница на чтениях» закрывает данные, но
	// оставляет открытым состав платформы.
	_, body = bound.do("GET", "/v1/projects", nil)
	projects := body["projects"].([]any)
	if len(projects) != 1 || projects[0].(map[string]any)["slug"] != "neighbour" {
		t.Fatalf("/v1/projects привязанным ключом: want ровно [neighbour], got %v", projects)
	}

	// Лента: ни одного события проекта game; платформенные (project_id is null)
	// остаются видимыми — фильтр не скрывающий (эпик #968).
	_, body = bound.do("GET", "/v1/events?limit=1000", nil)
	events := body["events"].([]any)
	var ownSeen, platformSeen bool
	for _, raw := range events {
		e := raw.(map[string]any)
		switch e["project"] {
		case "game":
			t.Fatalf("/v1/events отдал событие чужого проекта: %v", e)
		case "neighbour":
			ownSeen = true
		case nil, "": // `project` — omitempty, у платформенного события поля нет
			platformSeen = true
		}
	}
	if !ownSeen || !platformSeen {
		t.Fatalf("/v1/events: своё событие=%v, платформенное=%v — обязаны быть оба (%v)",
			ownSeen, platformSeen, events)
	}

	// Статистика: в окне два матча neighbour (dev+prod) и один game, но
	// привязанному ключу видна только его пара — распределение версий состоит
	// ровно из его 5.5.5.
	_, body = bound.do("GET", "/v1/stats/overview", nil)
	dist := body["version_distribution"].([]any)
	if len(dist) != 1 || dist[0].(map[string]any)["version"] != "5.5.5" {
		t.Fatalf("/v1/stats/overview привязанным ключом: want ровно версию 5.5.5, got %v", dist)
	}

	// Cost: снимок ёмкости — тоже выдача, а не «служебное поле». Платформа
	// держит в eu 25 слотов (10+5+7+3), пара привязки — 7.
	//
	// Сверяются ВСЕ ЧЕТЫРЕ числа, а не только ёмкость. `store/stats.go` обещает,
	// что обе половины снимка (ёмкость нод и занятость серверов) сужаются
	// ОДИНАКОВО, и проверка одной ёмкости это обещание НЕ доказывает: с
	// предикатами, снятыми только с CTE `srv`, привязанный ключ увидел бы свои 7
	// слотов и ЧУЖИЕ 3 занятых сервера — то есть утилизацию соседей, — и такая
	// мутация осталась бы зелёной (найдено вторым проходом).
	eu := euUtil(t, bound, "")
	if eu["capacity_slots"] != float64(7) || eu["allocated"] != float64(1) ||
		eu["ready"] != float64(0) || eu["draining"] != float64(0) {
		t.Fatalf("/v1/stats/cost привязанным ключом: %v, want capacity=7 allocated=1 ready=0 draining=0 "+
			"(обе половины снимка обязаны сужаться одинаково)", eu)
	}

	// Регион, где у арендатора нет НИЧЕГО, не должен появляться даже строкой с
	// нулями: иначе состав платформы читается по именам регионов, хотя числа и
	// сужены. У game есть нода в us, у neighbour — нет.
	_, body = bound.do("GET", "/v1/stats/cost", nil)
	for _, raw := range body["utilization"].([]any) {
		if r := raw.(map[string]any)["region"]; r != "eu" {
			t.Fatalf("/v1/stats/cost привязанным ключом выдал чужой регион %v: %v", r, body["utilization"])
		}
	}

	// СИММЕТРИЯ: второй привязанный ключ, другая пара. Без неё все проверки выше
	// одинаково прошли бы и на гейте, который сузил бы выдачу к ОДНОМУ
	// захардкоженному проекту (или, скажем, к первому проекту в БД), — а это уже
	// не граница, а совпадение с фикстурой.
	game := &client{t: t, base: f.base, key: f.gameKey}
	_, body = game.do("GET", "/v1/nodes", nil)
	gameNodes := body["nodes"].([]any)
	// Две ноды пары game/dev: node-1 (eu) и game-us-1 (us). Граница — по паре, а
	// НЕ по региону: региона в привязке нет, и сужать по нему было бы сужением
	// сверх обещанного.
	if len(gameNodes) != 2 {
		t.Fatalf("/v1/nodes ключом game/dev: want 2 ноды своей пары, got %v", gameNodes)
	}
	for _, raw := range gameNodes {
		n := raw.(map[string]any)
		if n["project"] != "game" || n["env"] != "dev" {
			t.Fatalf("/v1/nodes ключом game/dev отдал ноду чужой пары: %v", n)
		}
	}
	_, body = game.do("GET", "/v1/projects", nil)
	if projects := body["projects"].([]any); len(projects) != 1 ||
		projects[0].(map[string]any)["slug"] != "game" {
		t.Fatalf("/v1/projects ключом game/dev: want ровно [game], got %v", projects)
	}
	if got := euCapacity(t, game, ""); got != 10 {
		t.Fatalf("/v1/stats/cost ключом game/dev: capacity_slots=%v, want 10", got)
	}
	// И зеркальный отказ: для него чужой уже neighbour.
	if code, gb := game.do("GET", "/v1/nodes?project=neighbour", nil); code != 403 ||
		gb["detail"] != "key is bound to game/dev" {
		t.Fatalf("ключ game/dev на ?project=neighbour: %d %v, want 403 «key is bound to game/dev»", code, gb)
	}
}

// Половина 3: свой проект названный ЯВНО работает и отдаёт РЕАЛЬНОЕ тело.
// Проверка «не 403» тут ничего не стоит (у #988 положительная половина
// оказалась вырожденной ровно так), поэтому сверяется полный ответ с тем, что
// тот же ключ получает без параметра.
func TestListingsAllowOwnProjectForBoundKey(t *testing.T) {
	f := newTenantFixture(t)
	bound := &client{t: t, base: f.base, key: f.nbKey}

	for _, path := range tenantListingPaths {
		if path == "/v1/stats/overview" || path == "/v1/stats/cost" {
			continue // generated_at меняется от запроса к запросу — ниже отдельно
		}
		implicitCode, implicit := bound.doRaw("GET", path)
		explicitCode, explicit := bound.doRaw("GET", path+"?project=neighbour")
		if implicitCode != 200 || explicitCode != 200 {
			t.Fatalf("%s своим проектом: без параметра=%d, с параметром=%d, want 200 — гейт бьёт по своим",
				path, implicitCode, explicitCode)
		}
		if !bytes.Equal(implicit, explicit) {
			t.Fatalf("%s: явный свой проект дал не то же, что пустой фильтр:\n без=%s\n с=%s",
				path, implicit, explicit)
		}
		// Непустое тело, проверенное РАЗБОРОМ, а не поиском подстроки: «200 с
		// пустым списком» доказывало бы только то, что мы не упали (у #988
		// положительная половина оказалась вырожденной именно так).
		_, body := bound.do("GET", path, nil)
		if n := singleListLen(t, path, body); n == 0 {
			t.Fatalf("%s: выдача своей пары пуста (%v) — положительная половина вырождена", path, body)
		}
		// У ЛЕНТЫ непустоты мало: она не скрывающая, поэтому одно платформенное
		// событие удовлетворяет проверке даже при НУЛЕ своих (найдено вторым
		// проходом). Требуем именно своё событие.
		if path == "/v1/events" {
			var own bool
			for _, raw := range body["events"].([]any) {
				if raw.(map[string]any)["project"] == "neighbour" {
					own = true
				}
			}
			if !own {
				t.Fatalf("/v1/events: в выдаче нет НИ ОДНОГО события своего проекта, "+
					"непустота держится на платформенных: %v", body)
			}
		}
	}

	// Свой env, названный явно, тоже проходит — и тоже с реальным телом.
	for _, path := range []string{"/v1/nodes", "/v1/versions"} {
		code, body := bound.do("GET", path+"?project=neighbour&env=dev", nil)
		if code != 200 {
			t.Fatalf("%s своей парой: %d %v, want 200", path, code, body)
		}
		if n := singleListLen(t, path, body); n != 1 {
			t.Fatalf("%s своей парой: элементов %d, want 1 (%v)", path, n, body)
		}
	}
	// Статистика своей парой: агрегат считается, а не отдаётся пустым.
	_, body := bound.do("GET", "/v1/stats/overview?project=neighbour&env=dev", nil)
	if dist := body["version_distribution"].([]any); len(dist) != 1 ||
		dist[0].(map[string]any)["version"] != "5.5.5" {
		t.Fatalf("/v1/stats/overview своей парой: want ровно версию 5.5.5, got %v", dist)
	}
	if got := euCapacity(t, bound, "?project=neighbour&env=dev"); got != 7 {
		t.Fatalf("/v1/stats/cost своей парой: capacity_slots=%v, want 7", got)
	}
}

// singleListLen достаёт длину ЕДИНСТВЕННОГО списка в ответе листинга (все они
// формы {"<ресурс>": [...]}). Падает, если списка нет вовсе, — так «пусто» не
// маскируется под «поля нет».
func singleListLen(t *testing.T, path string, body map[string]any) int {
	t.Helper()
	for _, v := range body {
		if list, isList := v.([]any); isList {
			return len(list)
		}
	}
	t.Fatalf("%s: в ответе нет списка вовсе: %v", path, body)
	return 0
}

// Половина 4: обязательная. Глобальный ключ, admin и сессия панели видят
// платформу ровно как раньше — иначе «дыра закрыта» означало бы «панель
// сломана».
func TestListingsUnchangedForUnboundKeys(t *testing.T) {
	f := newTenantFixture(t)

	for name, secret := range map[string]string{"глобальный readonly": f.globalKey, "admin": f.adminKey} {
		c := &client{t: t, base: f.base, key: secret}

		_, body := c.do("GET", "/v1/nodes", nil)
		if got := len(body["nodes"].([]any)); got != 5 {
			t.Fatalf("%s: /v1/nodes отдал %d нод, want 5 (весь флот, как до #993)", name, got)
		}
		_, body = c.do("GET", "/v1/projects", nil)
		if got := len(body["projects"].([]any)); got != 2 {
			t.Fatalf("%s: /v1/projects отдал %d проектов, want 2", name, got)
		}
		// Чужой (для него — любой) проект по-прежнему читается явным фильтром.
		_, body = c.do("GET", "/v1/nodes?project=game", nil)
		if got := len(body["nodes"].([]any)); got != 3 {
			t.Fatalf("%s: /v1/nodes?project=game отдал %d нод, want 3", name, got)
		}
		// Опечатка — прежний 400, а не 403: валидация #961 никуда не делась.
		if code, body := c.do("GET", "/v1/nodes?project=zzz-nope", nil); code != 400 ||
			body["detail"] != "no such project zzz-nope" {
			t.Fatalf("%s: опечатка в ?project= должна давать прежний 400: %d %v", name, code, body)
		}
		// И снимок ёмкости остаётся платформенным.
		if got := euCapacity(t, c, ""); got != 25 {
			t.Fatalf("%s: /v1/stats/cost capacity_slots=%v, want 25 (платформенный снимок)", name, got)
		}
		if got := euCapacity(t, c, "?project=game"); got != 25 {
			t.Fatalf("%s: ?project= сужать utilization не должен (сужает только привязка): %v", name, got)
		}
	}

	// Сессия панели: логин admin-ключом, дальше только cookie — как ходит панель.
	b := &browser{t: t, base: f.base, csrf: true}
	code, _, resp := b.do("POST", "/v1/session", map[string]any{"api_key": f.adminKey})
	if code != 200 {
		t.Fatalf("логин сессии панели: %d", code)
	}
	b.cookie = sessionCookieOf(t, resp)
	code, body, _ := b.do("GET", "/v1/nodes", nil)
	if code != 200 || len(body["nodes"].([]any)) != 5 {
		t.Fatalf("сессия панели: %d %v, want 200 и весь флот", code, body)
	}
}

// Половина 5: оракул `?env=` (#971) закрыт ТЕМ ЖЕ гейтом, а не отдельной
// заплаткой. До карточки `scopeFilter` валидировал `?env=` по паре и отвечал
// любому readonly-ключу `400 no such environment game/ghost`, то есть
// перечислял окружения ЧУЖИХ проектов; закрыв `?project=` и оставив это, мы бы
// просто переселили оракул на второй параметр.
func TestEnvParamIsNoLongerAnOracleForBoundKey(t *testing.T) {
	f := newTenantFixture(t)
	bound := &client{t: t, base: f.base, key: f.nbKey}

	for _, path := range []string{"/v1/nodes", "/v1/versions", "/v1/stats/overview", "/v1/stats/cost"} {
		// (а) чужой проект: существующее чужое окружение против выдуманного.
		realCode, real := bound.doRaw("GET", path+"?project=game&env=prod")
		ghostCode, ghost := bound.doRaw("GET", path+"?project=game&env=ghost")
		if realCode != 403 || ghostCode != 403 || !bytes.Equal(real, ghost) {
			t.Fatalf("%s: чужое окружение существующее=%d %s, выдуманное=%d %s — ответы обязаны быть неотличимы",
				path, realCode, real, ghostCode, ghost)
		}

		// (б) СВОЙ проект, чужой env: привязка — пара, поэтому перебор имён
		// закрыт и внутри своего проекта, а не только через границу.
		ownRealCode, ownReal := bound.doRaw("GET", path+"?project=neighbour&env=prod")
		ownGhostCode, ownGhost := bound.doRaw("GET", path+"?project=neighbour&env=ghost")
		if ownRealCode != 403 || ownGhostCode != 403 || !bytes.Equal(ownReal, ownGhost) {
			t.Fatalf("%s: своё-чужое окружение существующее=%d %s, выдуманное=%d %s — ответы обязаны быть неотличимы",
				path, ownRealCode, ownReal, ownGhostCode, ownGhost)
		}

		// (в) без `?project=` вовсе: `EnvironmentNameExists` (проверка имени по
		// ВСЕЙ платформе) — второй вход того же оракула, и он тоже закрыт.
		bareRealCode, bareReal := bound.doRaw("GET", path+"?env=prod")
		bareGhostCode, bareGhost := bound.doRaw("GET", path+"?env=ghost")
		if bareRealCode != 403 || bareGhostCode != 403 || !bytes.Equal(bareReal, bareGhost) {
			t.Fatalf("%s: без проекта существующее=%d %s, выдуманное=%d %s — ответы обязаны быть неотличимы",
				path, bareRealCode, bareReal, bareGhostCode, bareGhost)
		}
	}

	// Для ГЛОБАЛЬНОГО ключа `?env=` остаётся валидируемым параметром с 400 на
	// опечатку (#971): он видит платформу по построению, оракулом это для него
	// не является. Строка держит границу решения — «закрыли привязанному», а не
	// «убрали валидацию».
	global := &client{t: t, base: f.base, key: f.globalKey}
	if code, body := global.do("GET", "/v1/nodes?project=game&env=ghost", nil); code != 400 ||
		body["detail"] != "no such environment game/ghost" {
		t.Fatalf("глобальный ключ: опечатка в ?env= должна остаться 400, got %d %v", code, body)
	}
}

// Гейт сравнивает СТРОКИ, поэтому его стоит подёргать ровно за то, чем обычно
// обходят строковые сравнения. Тест не про «а вдруг Go передумает», а про то,
// что КАЖДЫЙ исход тут безопасен: либо 403, либо выдача своей пары. Обратное
// («параметр как-то хитро записан → отдали чужое») не должно быть достижимо
// ни в одном варианте, и без этой фиксации следующий рефакторинг гейта
// (например, переход на r.FormValue или ручной разбор RawQuery) мог бы
// поменять поведение молча.
func TestTenantGateResistsQueryTricks(t *testing.T) {
	f := newTenantFixture(t)
	bound := &client{t: t, base: f.base, key: f.nbKey}

	// Отказ: любое написание ЧУЖОГО слага. Регистр не нормализуется нигде —
	// значит «GAME» просто не равен привязке и отсекается (fail-closed).
	for _, q := range []string{
		"?project=game",                   // чужой как есть
		"?project=GAME",                   // другой регистр
		"?project=%67ame",                 // URL-энкодинг первой буквы
		"?project=game&project=neighbour", // чужой ПЕРВЫМ из двух
		"?project=game&env=dev",           // чужой проект со своим env
	} {
		if code, body := bound.do("GET", "/v1/nodes"+q, nil); code != 403 {
			t.Fatalf("/v1/nodes%s: %d %v, want 403", q, code, body)
		}
	}

	// Проход, но ВСЕГДА суженный: ни один из этих вариантов не должен вернуть
	// чужую ноду. Проверяем именно СОСТАВ — «200» здесь ничего не значит.
	for _, q := range []string{
		"",                                // без параметра
		"?project=",                       // пустое значение
		"?project=neighbour&project=game", // свой ПЕРВЫМ: второе значение Go не читает
		"?PROJECT=game",                   // имя параметра регистрозависимо — не читается вовсе
		"?env=dev",                        // свой env без проекта
	} {
		code, body := bound.do("GET", "/v1/nodes"+q, nil)
		if code != 200 {
			t.Fatalf("/v1/nodes%s: %d %v, want 200", q, code, body)
		}
		nodes := body["nodes"].([]any)
		if len(nodes) != 1 || nodes[0].(map[string]any)["hostname"] != "nb-dev-1" {
			t.Fatalf("/v1/nodes%s отдал не только свою ноду: %v", q, nodes)
		}
	}

	// Сравнение слага ТОЧНОЕ, а не регистронезависимое, и это видно только на
	// СВОЁМ слаге в другом регистре: для чужого 403 приходит при любом правиле.
	// Пиннинг фиксирует fail-closed — «не уверены, что это тот же тенант, значит
	// не тот». Расхождение тут было бы неприятным и в другую сторону: гейт по
	// EqualFold рядом с `p.slug = $1` в сторе — две разные идеи о равенстве.
	gameBound := &client{t: t, base: f.base, key: f.gameKey}
	if code, body := gameBound.do("GET", "/v1/nodes?project=GAME", nil); code != 403 {
		t.Fatalf("ключ game/dev на ?project=GAME: %d %v, want 403 (сравнение слага точное)", code, body)
	}
	if code, body := gameBound.do("GET", "/v1/nodes?project=game", nil); code != 200 {
		t.Fatalf("ключ game/dev на своём слаге: %d %v, want 200", code, body)
	}

	// HEAD роутится тем же паттерном «GET /v1/nodes» (net/http ServeMux), то
	// есть проходит через тот же гейт: чужой проект — 403, а не 200 с пустым
	// телом, по которому статус всё равно читается.
	req, err := http.NewRequest("HEAD", f.base+"/v1/nodes?project=game", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.nbKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("HEAD /v1/nodes?project=game привязанным ключом: %d, want 403", resp.StatusCode)
	}
}

// euUtil достаёт строку utilization региона eu из /v1/stats/cost целиком.
func euUtil(t *testing.T, c *client, query string) map[string]any {
	t.Helper()
	code, body := c.do("GET", "/v1/stats/cost"+query, nil)
	if code != 200 {
		t.Fatalf("/v1/stats/cost%s: %d %v", query, code, body)
	}
	for _, raw := range body["utilization"].([]any) {
		u := raw.(map[string]any)
		if u["region"] == "eu" {
			return u
		}
	}
	t.Fatalf("/v1/stats/cost%s: региона eu нет в utilization: %v", query, body["utilization"])
	return nil
}

// euCapacity — только ёмкость eu (для проверок, где важна одна она).
func euCapacity(t *testing.T, c *client, query string) float64 {
	t.Helper()
	return euUtil(t, c, query)["capacity_slots"].(float64)
}
