package httpapi_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

func opsLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestNodeDrainUndrainAPI covers POST /v1/nodes/{id}/drain and /undrain: the
// node state flips, the agent gets Drain/Undrain, and scope is enforced.
func TestNodeDrainUndrainAPI(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 4)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	rec := &testdb.CommandRecorder{}
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, rec, nil, "", "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

	_, adminKey, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	_, roKey, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})
	admin := &client{t: t, base: ts.URL, key: adminKey}
	ro := &client{t: t, base: ts.URL, key: roKey}

	// readonly cannot drain.
	if code, _ := ro.do("POST", "/v1/nodes/"+f.NodeID+"/drain", nil); code != 403 {
		t.Fatalf("ro drain: want 403, got %d", code)
	}
	// bad uuid.
	if code, _ := admin.do("POST", "/v1/nodes/not-a-uuid/drain", nil); code != 400 {
		t.Fatalf("bad uuid: want 400, got %d", code)
	}
	// unknown node.
	if code, _ := admin.do("POST", "/v1/nodes/"+uuid.NewString()+"/drain", nil); code != 404 {
		t.Fatalf("unknown node: want 404, got %d", code)
	}

	// drain.
	code, body := admin.do("POST", "/v1/nodes/"+f.NodeID+"/drain", nil)
	if code != 200 {
		t.Fatalf("drain: want 200, got %d (%v)", code, body)
	}
	node, _ := body["node"].(map[string]any)
	if node["state"] != "draining" {
		t.Fatalf("drain: node state = %v, want draining", node["state"])
	}
	if !hasCmd(rec.Take(), func(m *agentlinkv1.MasterMsg) bool { return m.GetDrain() != nil }) {
		t.Fatal("drain: agent did not receive a Drain command")
	}

	// undrain.
	code, body = admin.do("POST", "/v1/nodes/"+f.NodeID+"/undrain", nil)
	if code != 200 {
		t.Fatalf("undrain: want 200, got %d (%v)", code, body)
	}
	node, _ = body["node"].(map[string]any)
	if node["state"] != "active" {
		t.Fatalf("undrain: node state = %v, want active", node["state"])
	}
	if !hasCmd(rec.Take(), func(m *agentlinkv1.MasterMsg) bool { return m.GetUndrain() != nil }) {
		t.Fatal("undrain: agent did not receive an Undrain command")
	}
}

// TestAgentUpgradeAPI covers POST /v1/agent-upgrade: sha256 validation, node
// targeting, UpgradeAgent dispatch and the agent_upgrade event.
func TestAgentUpgradeAPI(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 4)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	rec := &testdb.CommandRecorder{}
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, rec, nil, "", "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

	_, adminKey, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	admin := &client{t: t, base: ts.URL, key: adminKey}

	sha := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // sha256("")
	// bad sha256.
	if code, _ := admin.do("POST", "/v1/agent-upgrade", map[string]any{
		"url": "http://127.0.0.1:9/x", "sha256": "short", "version": "9.9.9"}); code != 400 {
		t.Fatalf("bad sha256: want 400, got %d", code)
	}
	// targeted upgrade.
	code, body := admin.do("POST", "/v1/agent-upgrade", map[string]any{
		"url": "http://127.0.0.1:9/birdman-agent", "sha256": sha, "version": "9.9.9", "node_id": f.NodeID})
	if code != 202 {
		t.Fatalf("upgrade: want 202, got %d (%v)", code, body)
	}
	if !hasCmd(rec.Take(), func(m *agentlinkv1.MasterMsg) bool {
		u := m.GetUpgrade()
		return u != nil && u.GetVersion() == "9.9.9" && u.GetSha256() == sha
	}) {
		t.Fatal("upgrade: agent did not receive an UpgradeAgent command")
	}
	if n, err := st.CountEvents(ctx, store.EventAgentUpgrade); err != nil || n != 1 {
		t.Fatalf("want 1 agent_upgrade event, got %d err=%v", n, err)
	}
}

// TestAgentUpgradeEventRedactsURL: the agent fetches its binary itself over a
// plain GET, so the URL handed to it may be a *presigned* registry link whose
// query string is a bearer-equivalent credential (OCI transport, dev-stand v2).
// The command must carry it verbatim, but the event must not: `events` rows live
// forever and are readable by any readonly key through the panel.
func TestAgentUpgradeEventRedactsURL(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 4)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	rec := &testdb.CommandRecorder{}
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, rec, nil, "", "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

	_, adminKey, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	admin := &client{t: t, base: ts.URL, key: adminKey}

	sha := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	signed := "https://pkg-containers.example.com/ghcr1/blobs/sha256:" + sha +
		"?se=2026-08-11T09%3A00%3A00Z&sig=SUPERSECRETSIGNATURE&sp=r"

	if code, _ := admin.do("POST", "/v1/agent-upgrade", map[string]any{
		"url": signed, "sha256": sha, "version": "9.9.9", "node_id": f.NodeID}); code != 202 {
		t.Fatalf("upgrade: want 202")
	}

	// The agent still gets the working, signed URL — redaction must not break it.
	if !hasCmd(rec.Take(), func(m *agentlinkv1.MasterMsg) bool {
		u := m.GetUpgrade()
		return u != nil && u.GetUrl() == signed
	}) {
		t.Fatal("agent must receive the URL verbatim, signature included")
	}

	events, err := st.ListEvents(ctx, 10, "")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var payloadURL string
	var found bool
	for _, e := range events {
		if e.Kind != store.EventAgentUpgrade {
			continue
		}
		found = true
		if v, ok := e.Payload["url"].(string); ok {
			payloadURL = v
		}
	}
	if !found {
		t.Fatal("no agent_upgrade event")
	}
	if strings.Contains(payloadURL, "SUPERSECRETSIGNATURE") || strings.Contains(payloadURL, "?") {
		t.Fatalf("event leaked the signed URL: %q", payloadURL)
	}
	// Redacted, not dropped: the operator must still see where it pulled from.
	if !strings.Contains(payloadURL, "pkg-containers.example.com") {
		t.Fatalf("event lost the useful part of the URL: %q", payloadURL)
	}
}

// TestMetricsQueryProxy covers the read-only VictoriaMetrics proxy.
func TestMetricsQueryProxy(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})

	// Fake VictoriaMetrics upstream.
	var gotPath, gotQuery string
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(vm.Close)

	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, vm.URL, "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()
	_, roKey, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})
	ro := &client{t: t, base: ts.URL, key: roKey}

	code, body := ro.do("GET", "/v1/metrics/query?query=birdman_players_online", nil)
	if code != 200 || body["status"] != "success" {
		t.Fatalf("query proxy: %d %v", code, body)
	}
	if gotPath != "/api/v1/query" || gotQuery != "birdman_players_online" {
		t.Fatalf("upstream got path=%q query=%q", gotPath, gotQuery)
	}

	// query_range routes to the range endpoint.
	if code, _ := ro.do("GET", "/v1/metrics/query_range?query=up&start=0&end=1&step=1", nil); code != 200 {
		t.Fatalf("query_range proxy: %d", code)
	}
	if gotPath != "/api/v1/query_range" {
		t.Fatalf("query_range upstream path=%q", gotPath)
	}

	// Unconfigured master → 503.
	ts2 := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts2.Close)
	ro2 := &client{t: t, base: ts2.URL, key: roKey}
	if code, _ := ro2.do("GET", "/v1/metrics/query?query=up", nil); code != 503 {
		t.Fatalf("unconfigured proxy: want 503, got %d", code)
	}
}

// TestLogsQueryProxy covers the read-only VictoriaLogs proxy: passthrough of
// query/start/end, limit default/clamp/validation, and the
// logs_unconfigured/upstream error contract the panel (Task 4) depends on.
func TestLogsQueryProxy(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})

	// Fake VictoriaLogs upstream: records path/query, answers one ndjson line.
	var calls int
	var gotPath string
	var gotQuery url.Values
	vl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotPath, gotQuery = r.URL.Path, r.URL.Query()
		w.Header().Set("Content-Type", "application/stream+json")
		_, _ = w.Write([]byte(`{"_time":"2026-07-09T10:00:00Z","_msg":"hello","server_id":"s1"}` + "\n"))
	}))
	t.Cleanup(vl.Close)

	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", vl.URL, log))
	t.Cleanup(ts.Close)
	ctx := t.Context()
	_, roKey, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})
	ro := &client{t: t, base: ts.URL, key: roKey}

	// 1) passthrough + default limit.
	code, body := ro.doRaw("GET", `/v1/logs/query?query={server_id="s1"}&start=0&end=10`)
	if code != 200 {
		t.Fatalf("passthrough: want 200, got %d (%s)", code, body)
	}
	if gotPath != "/select/logsql/query" {
		t.Fatalf("upstream path = %q, want /select/logsql/query", gotPath)
	}
	if got := gotQuery.Get("query"); got != `{server_id="s1"}` {
		t.Fatalf("upstream query = %q", got)
	}
	if gotQuery.Get("start") != "0" || gotQuery.Get("end") != "10" {
		t.Fatalf("upstream start/end = %q/%q, want 0/10", gotQuery.Get("start"), gotQuery.Get("end"))
	}
	if gotQuery.Get("limit") != "1000" {
		t.Fatalf("upstream limit = %q, want default 1000", gotQuery.Get("limit"))
	}

	// 2) clamp: limit=50000 → upstream sees limit=10000.
	if code, body := ro.doRaw("GET", "/v1/logs/query?query=x&limit=50000"); code != 200 {
		t.Fatalf("clamp: want 200, got %d (%s)", code, body)
	}
	if gotQuery.Get("limit") != "10000" {
		t.Fatalf("clamp: upstream limit = %q, want 10000", gotQuery.Get("limit"))
	}

	// 3) bad limit → 400 bad_request, upstream not called.
	before := calls
	code, body = ro.doRaw("GET", "/v1/logs/query?query=x&limit=abc")
	if code != 400 {
		t.Fatalf("bad limit: want 400, got %d (%s)", code, body)
	}
	if !strings.Contains(string(body), "bad_request") {
		t.Fatalf("bad limit: body = %s, want error bad_request", body)
	}
	if calls != before {
		t.Fatalf("bad limit: upstream was called (calls %d -> %d)", before, calls)
	}

	// 4) unconfigured master → 503 logs_unconfigured.
	ts2log := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts2log.Close)
	ro2log := &client{t: t, base: ts2log.URL, key: roKey}
	code, body = ro2log.doRaw("GET", "/v1/logs/query?query=x")
	if code != 503 {
		t.Fatalf("unconfigured: want 503, got %d (%s)", code, body)
	}
	if !strings.Contains(string(body), "logs_unconfigured") {
		t.Fatalf("unconfigured: body = %s, want logs_unconfigured", body)
	}

	// 5) dead upstream → 502 upstream.
	vl.Close()
	code, body = ro.doRaw("GET", "/v1/logs/query?query=x")
	if code != 502 {
		t.Fatalf("dead upstream: want 502, got %d (%s)", code, body)
	}
	if !strings.Contains(string(body), "upstream") {
		t.Fatalf("dead upstream: body = %s, want upstream", body)
	}
}

// logSender is a fake agent transport that answers a TailLogs command with a
// couple of LogChunks through the real LogRouter (mirroring the network delay
// so the handler's Register wins the race).
type logSender struct {
	router   *agentlink.LogRouter
	mu       sync.Mutex
	canceled map[string]bool
}

func (s *logSender) Send(_ string, msg *agentlinkv1.MasterMsg) string {
	cmdID := uuid.NewString()
	tail := msg.GetTail()
	if tail == nil {
		return cmdID
	}
	if cc := tail.GetCancelCmdId(); cc != "" {
		s.mu.Lock()
		s.canceled[cc] = true
		s.mu.Unlock()
		return cmdID
	}
	serverID := tail.GetServerId()
	go func() {
		time.Sleep(20 * time.Millisecond) // let the handler Register first
		s.router.Dispatch(cmdID, &agentlinkv1.LogChunk{CmdId: cmdID, ServerId: serverID, Data: []byte("hello from dedik\n")})
		s.router.Dispatch(cmdID, &agentlinkv1.LogChunk{CmdId: cmdID, ServerId: serverID, Data: []byte("line 2\n"), Eof: true})
	}()
	return cmdID
}

// TestServerLogsProxy covers GET /v1/servers/{id}/logs end to end through the
// LogRouter: chunks streamed by the (fake) agent reach the HTTP body, eof ends
// the stream.
func TestServerLogsProxy(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 4)
	serverID := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 26100, 0)

	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	router := agentlink.NewLogRouter()
	sender := &logSender{router: router, canceled: map[string]bool{}}
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, sender, router, "", "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()
	_, roKey, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})

	req, _ := http.NewRequest("GET", ts.URL+"/v1/servers/"+serverID+"/logs", nil)
	req.Header.Set("Authorization", "Bearer "+roKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("logs: status %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if string(raw) != "hello from dedik\nline 2\n" {
		t.Fatalf("logs body = %q", raw)
	}

	// Unknown server → 404.
	req2, _ := http.NewRequest("GET", ts.URL+"/v1/servers/"+uuid.NewString()+"/logs", nil)
	req2.Header.Set("Authorization", "Bearer "+roKey)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("unknown server logs: want 404, got %d", resp2.StatusCode)
	}
}

// TestQueryProxiesNarrowToBoundScope (tracker #994, продолжение #990): сырые
// query-проксии (LogsQL → VL, PromQL → VM) больше не закрыты для ПРИВЯЗАННОГО
// ключа — они СУЖАЮТСЯ его парой (project, env). #990 отвечал здесь глухим 403,
// потому что лейбла владельца в стримах VL не было вовсе; теперь его чеканит
// агент в пути файла лога, и запрос можно сузить, не разбирая его.
//
// Что именно доказывает этот тест: ЧТО МАСТЕР ОТПРАВИЛ В АПСТРИМ. Энфорсмент
// самого фильтра выполняет VictoriaLogs/VictoriaMetrics, и никакой тест против
// фейкового апстрима его доказать не может — это делает интеграционный
// TestLiveUpstreamLogsNarrowing/TestLiveUpstreamMetricsNarrowing (живые VL/VM, включаются
// BIRDMAN_TEST_VL_URL/BIRDMAN_TEST_VM_URL). Поэтому здесь сверяется контракт
// границы: запрос клиента доезжает ДОСЛОВНО, фильтр master'а приклеен, а
// клиентские ручки сужения (extra_stream_filters / extra_filters / extra_label)
// до апстрима не доезжают ВООБЩЕ — именно ими сужение и снималось бы.
func TestQueryProxiesNarrowToBoundScope(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})

	// Фейковые апстримы: считают вызовы, ЗАПОМИНАЮТ пришедшую query-строку и
	// отдают опознаваемое тело — «положительная половина» обязана увидеть
	// ИМЕННО эти байты, иначе 200 ничего не доказывает.
	const logLine = `{"_time":"2026-08-11T10:00:00Z","_msg":"secret dedik output","server_id":"s1"}`
	const vmBody = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	var vlCalls, vmCalls int
	var gotVL, gotVM url.Values
	vl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vlCalls++
		gotVL = r.URL.Query()
		w.Header().Set("Content-Type", "application/stream+json")
		_, _ = w.Write([]byte(logLine + "\n"))
	}))
	t.Cleanup(vl.Close)
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vmCalls++
		gotVM = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vmBody))
	}))
	t.Cleanup(vm.Close)

	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, vm.URL, vl.URL, log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

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
	_, adminSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "admin", Scopes: []string{httpapi.ScopeAdmin},
	})
	if err != nil {
		t.Fatal(err)
	}
	bound := &client{t: t, base: ts.URL, key: boundSecret}
	global := &client{t: t, base: ts.URL, key: globalSecret}
	admin := &client{t: t, base: ts.URL, key: adminSecret}

	const wantStreamFilter = `{project="neighbour",env="dev"}`
	// Запрос панели как он есть: селектор по ЧУЖОМУ дедику плюс пайп. Ровно эта
	// форма и была утечкой #990, и ровно на ней ломается «просто подставить
	// фильтр в текст запроса».
	foreignLogsQuery := `{server_id="s1"} | sort by (_time) desc`
	logsPath := "/v1/logs/query?query=" + url.QueryEscape(foreignLogsQuery) + "&start=0&end=10"

	// 1) ЛОГИ, привязанный ключ: 200 (не 403 — дверь #990 снята), апстрим вызван,
	//    запрос клиента доехал ДОСЛОВНО, фильтр master'а приклеен.
	code, body := bound.doRaw("GET", logsPath)
	if code != 200 {
		t.Fatalf("logs/bound: %d (%s), want 200 — привязанный ключ снова читает СВОИ логи", code, body)
	}
	if !strings.Contains(string(body), logLine) {
		t.Fatalf("logs/bound: тело = %q, want содержащее %q (иначе 200 ничего не доказывает)", body, logLine)
	}
	if got := gotVL.Get("query"); got != foreignLogsQuery {
		t.Fatalf("logs/bound: апстрим получил query = %q, want %q дословно (запрос не переписывается)", got, foreignLogsQuery)
	}
	if got := gotVL.Get("extra_stream_filters"); got != wantStreamFilter {
		t.Fatalf("logs/bound: extra_stream_filters = %q, want %q — БЕЗ ЭТОГО ПРИВЯЗАННЫЙ КЛЮЧ ЧИТАЕТ ЧУЖОЕ", got, wantStreamFilter)
	}
	if got := gotVL.Get("limit"); got != "1000" {
		t.Fatalf("logs/bound: limit = %q, want 1000 (клампинг остаётся)", got)
	}
	for _, k := range []string{"start", "end"} {
		if gotVL.Get(k) == "" {
			t.Fatalf("logs/bound: параметр %s не доехал до апстрима — диапазон времени потерян", k)
		}
	}

	// 2) ПОПЫТКА СНЯТЬ СУЖЕНИЕ СВОИМИ ЖЕ ПАРАМЕТРАМИ. Привязанный ключ шлёт
	//    собственные extra_stream_filters/extra_filters — до апстрима не должно
	//    доехать НИ ОДНОГО из них: master пересобирает query-строку из белого
	//    списка. Это главная мутационная ловушка: стоит начать «дописывать» в
	//    пришедшую строку вместо пересборки — тест краснеет.
	smuggle := logsPath +
		"&extra_stream_filters=" + url.QueryEscape(`{project="game"}`) +
		"&extra_filters=" + url.QueryEscape(`{project="game"}`) +
		"&extra_label=project%3Dgame"
	if code, body := bound.doRaw("GET", smuggle); code != 200 {
		t.Fatalf("logs/bound+smuggle: %d (%s), want 200", code, body)
	}
	if got := gotVL["extra_stream_filters"]; len(got) != 1 || got[0] != wantStreamFilter {
		t.Fatalf("logs/bound+smuggle: extra_stream_filters = %q, want ровно [%q] — клиентский параметр протёк", got, wantStreamFilter)
	}
	if got, ok := gotVL["extra_filters"]; ok {
		t.Fatalf("logs/bound+smuggle: клиентский extra_filters доехал до апстрима: %q", got)
	}
	if got, ok := gotVL["extra_label"]; ok {
		t.Fatalf("logs/bound+smuggle: клиентский extra_label доехал до апстрима: %q", got)
	}

	// 3) МЕТРИКИ, привязанный ключ: обе ручки (query и query_range) — одна дверь
	//    из двух была бы повторением ошибки #988/#990.
	metricProxies := []struct {
		name, path string
		wantParams []string
	}{
		{"metrics", "/v1/metrics/query?query=up&time=5", []string{"query", "time"}},
		{"metrics_range", "/v1/metrics/query_range?query=up&start=0&end=10&step=15", []string{"query", "start", "end", "step"}},
	}
	for _, p := range metricProxies {
		smug := p.path + "&extra_label=project%3Dgame&" + url.QueryEscape("extra_filters[]") + "=" + url.QueryEscape(`{project="game"}`)
		code, body := bound.doRaw("GET", smug)
		if code != 200 {
			t.Fatalf("%s/bound: %d (%s), want 200", p.name, code, body)
		}
		if !strings.Contains(string(body), vmBody) {
			t.Fatalf("%s/bound: тело = %q, want содержащее %q", p.name, body, vmBody)
		}
		want := []string{"project=neighbour", "env=dev"}
		if got := gotVM["extra_label"]; !slices.Equal(got, want) {
			t.Fatalf("%s/bound: extra_label = %q, want ровно %q — БЕЗ ЭТОГО ПРИВЯЗАННЫЙ КЛЮЧ ЧИТАЕТ ЧУЖИЕ СЕРИИ", p.name, got, want)
		}
		// extra_filters[] у VM складываются по ИЛИ — протёкший клиентский
		// параметр РАСШИРИЛ бы выдачу до чужих проектов, а не сузил.
		if got, ok := gotVM["extra_filters[]"]; ok {
			t.Fatalf("%s/bound: клиентский extra_filters[] доехал до апстрима: %q (у VM это ИЛИ — прямое расширение)", p.name, got)
		}
		if got := gotVM.Get("query"); got != "up" {
			t.Fatalf("%s/bound: query = %q, want %q дословно", p.name, got, "up")
		}
		for _, k := range p.wantParams {
			if gotVM.Get(k) == "" {
				t.Fatalf("%s/bound: параметр %s не доехал до апстрима", p.name, k)
			}
		}
	}

	// 4) Глобальный и admin-ключ: passthrough как был — те же байты и НИ ОДНОГО
	//    приклеенного фильтра (сужать глобальный ключ было бы регрессом
	//    функциональности, а не безопасностью). admin здесь не второй случай
	//    привязки, а регресс-страховка по скоупу: привязка с admin несовместима.
	for _, c := range []struct {
		who string
		cl  *client
	}{{"global", global}, {"admin", admin}} {
		code, body := c.cl.doRaw("GET", logsPath)
		if code != 200 || !strings.Contains(string(body), logLine) {
			t.Fatalf("logs/%s: %d (%s), want 200 с байтами апстрима", c.who, code, body)
		}
		if got, ok := gotVL["extra_stream_filters"]; ok {
			t.Fatalf("logs/%s: passthrough сужен фильтром %q — глобальный ключ видит весь флот", c.who, got)
		}
		code, body = c.cl.doRaw("GET", "/v1/metrics/query?query=up")
		if code != 200 || !strings.Contains(string(body), vmBody) {
			t.Fatalf("metrics/%s: %d (%s), want 200 с байтами апстрима", c.who, code, body)
		}
		if got, ok := gotVM["extra_label"]; ok {
			t.Fatalf("metrics/%s: passthrough сужен %q", c.who, got)
		}
	}

	// 5) Клампинг limit и валидация — на привязанном ключе работают как на всех.
	if code, body := bound.doRaw("GET", "/v1/logs/query?query=*&limit=99999"); code != 200 {
		t.Fatalf("logs/bound limit=99999: %d (%s), want 200", code, body)
	}
	if got := gotVL.Get("limit"); got != "10000" {
		t.Fatalf("logs/bound: limit = %q, want 10000 (клампинг сохраняется при сужении)", got)
	}
	beforeVL := vlCalls
	if code, body := bound.doRaw("GET", "/v1/logs/query?query=*&limit=abc"); code != 400 {
		t.Fatalf("logs/bound limit=abc: %d (%s), want 400", code, body)
	}
	if vlCalls != beforeVL {
		t.Fatalf("кривой limit доехал до апстрима (vl %d→%d)", beforeVL, vlCalls)
	}

	// 6) Cookie-сессия ПРИВЯЗАННОГО ключа сужается так же, как Bearer. POST
	//    /v1/session не требует admin (session.go: requireScope на нём нет),
	//    сессия наследует ключ целиком вместе с привязкой — «сессия панели =
	//    admin» ложная посылка (#990). 401 здесь означал бы, что кука не доехала
	//    и проверка выродилась.
	b := &browser{t: t, base: ts.URL}
	code2, _, resp := b.do("POST", "/v1/session", map[string]any{"api_key": boundSecret})
	if code2 != 200 {
		t.Fatalf("логин привязанным ключом: %d, want 200 (сессию заводит любой ключ)", code2)
	}
	bb := &browser{t: t, base: ts.URL, cookie: sessionCookieOf(t, resp)}
	if code2, sbody, _ := bb.do("GET", "/v1/session", nil); code2 != 200 {
		t.Fatalf("кука не аутентифицирует: %d %v", code2, sbody)
	}
	if code2, _, _ := bb.do("GET", logsPath, nil); code2 != 200 {
		t.Fatalf("сессия привязанного ключа: %d, want 200", code2)
	}
	if got := gotVL.Get("extra_stream_filters"); got != wantStreamFilter {
		t.Fatalf("сессия привязанного ключа: extra_stream_filters = %q, want %q — привязка в сессии потеряна", got, wantStreamFilter)
	}
	// Метрики через сессию — та же нога (мутация «снять сужение только с
	// proxyVictoria» иначе остаётся зелёной на cookie-пути).
	if code2, _, _ := bb.do("GET", "/v1/metrics/query?query=up", nil); code2 != 200 {
		t.Fatalf("сессия привязанного ключа, метрики: %d, want 200", code2)
	}
	if got := gotVM["extra_label"]; !slices.Equal(got, []string{"project=neighbour", "env=dev"}) {
		t.Fatalf("сессия привязанного ключа, метрики: extra_label = %q — привязка в сессии потеряна", got)
	}

	// 7) Ненастроенный master: привязанный ключ теперь ДОПУЩЕН, поэтому видит
	//    состояние проксии как все (503), а не 403. Порядок «разрешение раньше
	//    состояния» (#988) при этом сохранён — 403 остался только для
	//    непригодной к сужению пары, см. TestNarrowScope.
	tsOff := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(tsOff.Close)
	boundOff := &client{t: t, base: tsOff.URL, key: boundSecret}
	for _, p := range []string{"/v1/logs/query?query=*", "/v1/metrics/query?query=up"} {
		if code, body := boundOff.doRaw("GET", p); code != 503 {
			t.Fatalf("%s на ненастроенном master: %d (%s), want 503", p, code, body)
		}
	}
}

func hasCmd(cmds []testdb.SentCmd, pred func(*agentlinkv1.MasterMsg) bool) bool {
	for _, c := range cmds {
		if pred(c.Msg) {
			return true
		}
	}
	return false
}
