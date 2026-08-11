package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"

	"net/http/httptest"
)

// Интеграционные проверки сужения (tracker #994) против ЖИВЫХ VictoriaLogs и
// VictoriaMetrics. Включаются переменными окружения — без них skip, поэтому
// рецепт CI (.github/workflows/master.yml) не меняется:
//
//	BIRDMAN_TEST_VL_URL=http://127.0.0.1:9428 BIRDMAN_TEST_VM_URL=http://127.0.0.1:8428 \
//	  go test -race -count=1 -run LiveUpstream ./internal/httpapi/
//
// ЗАЧЕМ ОНИ ЕСТЬ. Сужение сделано штатными ручками апстрима
// (`extra_stream_filters` у VL, `extra_label` у VM), а не разбором LogsQL/PromQL
// на master. Это осознанный размен: парсер чужой грамматики на границе доступа —
// своя пачка дыр, зато энфорсмент уезжает туда, где тест master'а его НЕ
// доказывает. Тест против фейкового апстрима (TestQueryProxiesNarrowToBoundScope)
// доказывает только ЧТО ушло; что апстрим это чтит — доказывается здесь и только
// здесь. Версии, на которых проверено, зафиксированы в
// infra/roles/birdman_monitoring_dev/defaults/main.yml (VL v1.51.0, VM v1.102.1).
const (
	liveProjectMine    = "alpha"
	liveProjectForeign = "beta"
	liveEnv            = "dev"
)

func liveUpstreams(t *testing.T) (vlURL, vmURL string) {
	t.Helper()
	vlURL, vmURL = os.Getenv("BIRDMAN_TEST_VL_URL"), os.Getenv("BIRDMAN_TEST_VM_URL")
	if vlURL == "" || vmURL == "" {
		t.Skip("BIRDMAN_TEST_VL_URL/BIRDMAN_TEST_VM_URL не заданы — живые VL/VM не подняты")
	}
	return strings.TrimRight(vlURL, "/"), strings.TrimRight(vmURL, "/")
}

// TestLiveUpstreamLogsNarrowing: привязанный ключ получает СВОИ строки и не
// получает чужих НИ ПРИ КАКОМ запросе — включая явный чужой server_id, пайпы и
// пайп union, которым можно было бы попытаться подклеить второй запрос.
func TestLiveUpstreamLogsNarrowing(t *testing.T) {
	vlURL, vmURL := liveUpstreams(t)
	tag := fmt.Sprintf("live-%d", time.Now().UnixNano()) // метка прогона: индекс VL общий
	seedVictoriaLogs(t, vlURL, tag)

	ts, boundSecret, globalSecret := liveServer(t, vmURL, vlURL)
	bound := &client{t: t, base: ts.URL, key: boundSecret}
	global := &client{t: t, base: ts.URL, key: globalSecret}

	mine := "mine " + tag
	foreign := "foreign " + tag
	unlabelled := "unlabelled " + tag
	otherEnv := "otherenv " + tag

	queries := []struct {
		name  string
		logs  string
		limit string
	}{
		{"всё подряд", "*", "100"},
		{"панельный пайп", "* | sort by (_time) desc", "100"},
		{"явный ЧУЖОЙ server_id", `{server_id="s-foreign"} | sort by (_time) desc`, "100"},
		{"чужой project прямо в запросе", `{project="beta"}`, "100"},
		{"свой project, чужое окружение", `{project="alpha",env="prod"}`, "100"},
		{"стрим без лейблов", `{server_id="s-legacy"}`, "100"},
		{"union второго запроса", `* | union ({project="beta"})`, "100"},
		{"клиентский extra_stream_filters", "*", "100"},
	}
	// _stream_id чужого стрима — отдельный вектор обхода: он адресует стрим
	// НАПРЯМУЮ, мимо стрим-селектора запроса. Достаём его глобальным ключом
	// (атакующий мог узнать его и иначе) и пробуем прочитать привязанным.
	_, gbody := global.doRaw("GET", "/v1/logs/query?query="+url.QueryEscape(`{project="beta"}`)+"&limit=1")
	if sid := firstStreamID(string(gbody)); sid != "" {
		queries = append(queries, struct {
			name  string
			logs  string
			limit string
		}{"чужой _stream_id напрямую", "_stream_id:" + sid, "100"})
	} else {
		t.Fatal("не удалось достать _stream_id чужого стрима — проверка обхода выродилась бы в пустую")
	}

	for _, q := range queries {
		path := "/v1/logs/query?query=" + url.QueryEscape(q.logs) + "&limit=" + q.limit
		if q.name == "клиентский extra_stream_filters" {
			path += "&extra_stream_filters=" + url.QueryEscape(`{project="beta"}`)
		}
		code, body := bound.doRaw("GET", path)
		if code != 200 {
			t.Fatalf("%s: %d (%s), want 200", q.name, code, body)
		}
		if strings.Contains(string(body), foreign) {
			t.Fatalf("УТЕЧКА (%s): привязанный ключ получил строку чужого проекта:\n%s", q.name, body)
		}
		if strings.Contains(string(body), unlabelled) {
			t.Fatalf("УТЕЧКА (%s): привязанный ключ получил строку БЕЗ лейблов:\n%s", q.name, body)
		}
		// Половина пары не пускает: ключ привязан к alpha/dev, а не к alpha/*.
		if strings.Contains(string(body), otherEnv) {
			t.Fatalf("УТЕЧКА (%s): привязанный ключ получил строку СВОЕГО проекта из ЧУЖОГО окружения:\n%s", q.name, body)
		}
	}

	// Положительная половина: свои строки приходят, иначе «чужого нет» доказывало
	// бы лишь то, что выдача пуста всегда.
	code, body := bound.doRaw("GET", "/v1/logs/query?query="+url.QueryEscape("* | sort by (_time) desc")+"&limit=100")
	if code != 200 || !strings.Contains(string(body), mine) {
		t.Fatalf("привязанный ключ НЕ получил своих строк: %d %s", code, body)
	}

	// Глобальный ключ по-прежнему видит весь флот, включая нелейблованную историю.
	code, body = global.doRaw("GET", "/v1/logs/query?query="+url.QueryEscape("* | sort by (_time) desc")+"&limit=100")
	if code != 200 {
		t.Fatalf("глобальный ключ: %d %s", code, body)
	}
	for _, want := range []string{mine, foreign, unlabelled, otherEnv} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("глобальный ключ потерял %q — passthrough сужен по ошибке:\n%s", want, body)
		}
	}
}

// TestLiveUpstreamMetricsNarrowing: то же для VictoriaMetrics. Отдельно
// проверяется, что клиентский `extra_filters[]` не доезжает: у VM несколько
// extra_filters[] складываются по ИЛИ, то есть протёкший параметр РАСШИРИЛ бы
// выдачу до чужих проектов.
func TestLiveUpstreamMetricsNarrowing(t *testing.T) {
	vlURL, vmURL := liveUpstreams(t)
	seedVictoriaMetrics(t, vmURL)

	ts, boundSecret, globalSecret := liveServer(t, vmURL, vlURL)
	bound := &client{t: t, base: ts.URL, key: boundSecret}
	global := &client{t: t, base: ts.URL, key: globalSecret}

	// Данные становятся видимыми не мгновенно (у VM есть -search.latencyOffset).
	waitFor(t, 90*time.Second, func() bool {
		_, body := global.doRaw("GET", "/v1/metrics/query?query="+url.QueryEscape(`{__name__=~"birdman_live_.*"}`))
		return strings.Contains(string(body), liveProjectForeign)
	}, "серии не появились в VM")

	promQueries := []string{
		`{__name__=~"birdman_live_.*"}`,
		`birdman_live_servers`,
		`sum by (project) (birdman_live_servers)`,
		`birdman_live_servers{project="beta"}`,
	}
	for _, q := range promQueries {
		path := "/v1/metrics/query?query=" + url.QueryEscape(q) +
			"&" + url.QueryEscape("extra_filters[]") + "=" + url.QueryEscape(`{project="beta"}`) +
			"&extra_label=" + url.QueryEscape("project=beta")
		code, body := bound.doRaw("GET", path)
		if code != 200 {
			t.Fatalf("%s: %d (%s), want 200", q, code, body)
		}
		if strings.Contains(string(body), `"`+liveProjectForeign+`"`) {
			t.Fatalf("УТЕЧКА (%s): привязанный ключ получил серию чужого проекта:\n%s", q, body)
		}
		if strings.Contains(string(body), "birdman_live_platform") {
			t.Fatalf("УТЕЧКА (%s): привязанный ключ получил серию БЕЗ пары:\n%s", q, body)
		}
		if strings.Contains(string(body), `"prod"`) {
			t.Fatalf("УТЕЧКА (%s): привязанный ключ получил серию своего проекта из ЧУЖОГО окружения:\n%s", q, body)
		}
	}

	code, body := bound.doRaw("GET", "/v1/metrics/query?query=birdman_live_servers")
	if code != 200 || !strings.Contains(string(body), `"`+liveProjectMine+`"`) {
		t.Fatalf("привязанный ключ НЕ получил своих серий: %d %s", code, body)
	}

	// query_range — вторая дверь той же комнаты.
	now := time.Now().Unix()
	rangePath := fmt.Sprintf("/v1/metrics/query_range?query=%s&start=%d&end=%d&step=60",
		url.QueryEscape("birdman_live_servers"), now-600, now)
	code, body = bound.doRaw("GET", rangePath)
	if code != 200 {
		t.Fatalf("query_range: %d %s", code, body)
	}
	if strings.Contains(string(body), `"`+liveProjectForeign+`"`) {
		t.Fatalf("УТЕЧКА (query_range): чужой проект в выдаче:\n%s", body)
	}
}

// firstStreamID вытаскивает _stream_id из первой ndjson-строки ответа VL.
func firstStreamID(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if v, ok := row["_stream_id"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func liveServer(t *testing.T, vmURL, vlURL string) (ts *httptest.Server, boundSecret, globalSecret string) {
	t.Helper()
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts = httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, vmURL, vlURL, log))
	t.Cleanup(ts.Close)

	ctx := t.Context()
	if _, err := st.CreateProject(ctx, liveProjectMine, 2); err != nil {
		t.Fatal(err)
	}
	p, e := liveProjectMine, liveEnv
	var err error
	if _, boundSecret, err = st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-bound", Scopes: []string{httpapi.ScopeReadonly}, Project: &p, Env: &e,
	}); err != nil {
		t.Fatal(err)
	}
	if _, globalSecret, err = st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-global", Scopes: []string{httpapi.ScopeReadonly},
	}); err != nil {
		t.Fatal(err)
	}
	return ts, boundSecret, globalSecret
}

// seedVictoriaLogs кладёт три стрима: свой, чужой и БЕЗ пары (история, которая
// была записана до разметки — она не отдаётся привязанному ключу, решение
// владельца по карточке).
func seedVictoriaLogs(t *testing.T, vlURL, tag string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	body := strings.Join([]string{
		fmt.Sprintf(`{"_time":%q,"_msg":"mine %s","server_id":"s-mine","node":"n1","region":"eu","project":%q,"env":%q}`,
			now, tag, liveProjectMine, liveEnv),
		fmt.Sprintf(`{"_time":%q,"_msg":"foreign %s","server_id":"s-foreign","node":"n1","region":"eu","project":%q,"env":%q}`,
			now, tag, liveProjectForeign, liveEnv),
		// Свой проект, но ЧУЖОЕ окружение: половина пары не должна пускать.
		fmt.Sprintf(`{"_time":%q,"_msg":"otherenv %s","server_id":"s-mine-prod","node":"n1","region":"eu","project":%q,"env":"prod"}`,
			now, tag, liveProjectMine),
		fmt.Sprintf(`{"_time":%q,"_msg":"unlabelled %s","server_id":"s-legacy","node":"n1","region":"eu"}`, now, tag),
	}, "\n")
	post(t, vlURL+"/insert/jsonline?_stream_fields=project,env,server_id,node,region&_msg_field=_msg&_time_field=_time",
		"application/stream+json", body)
	// Индексация не мгновенна.
	time.Sleep(2 * time.Second)
}

func seedVictoriaMetrics(t *testing.T, vmURL string) {
	t.Helper()
	ts := (time.Now().Add(-time.Minute).UnixNano() / 1e6)
	body := strings.Join([]string{
		fmt.Sprintf(`birdman_live_servers{project=%q,env=%q,state="ready"} 3 %d`, liveProjectMine, liveEnv, ts),
		fmt.Sprintf(`birdman_live_servers{project=%q,env=%q,state="ready"} 7 %d`, liveProjectForeign, liveEnv, ts),
		// Свой проект, ЧУЖОЕ окружение: половина пары не должна пускать.
		fmt.Sprintf(`birdman_live_servers{project=%q,env="prod",state="ready"} 5 %d`, liveProjectMine, ts),
		fmt.Sprintf(`birdman_live_platform_total 99 %d`, ts),
	}, "\n")
	post(t, vmURL+"/api/v1/import/prometheus", "text/plain", body)
}

func post(t *testing.T, target, contentType, body string) {
	t.Helper()
	resp, err := http.Post(target, contentType, bytes.NewBufferString(body+"\n"))
	if err != nil {
		t.Fatalf("seed %s: %v", target, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("seed %s: %d %s", target, resp.StatusCode, raw)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s (ждали %s)", msg, d)
}
