package httpapi_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

// TestQueryProxiesRefuseBoundKeys (#990): сырые query-проксии (LogsQL → VL,
// PromQL → VM) закрыты для ПРИВЯЗАННОГО ключа. #988 закрыл привязкой live-tail
// /v1/servers/{id}/logs, но те же байты игрового вывода дедика чужого проекта
// доставались тем же readonly-ключом через /v1/logs/query: запрос едет в
// апстрим как есть, а стримы VL лейблованы server_id/node/region — сузить их по
// проекту нечем. Проверяются обе стороны: привязанный не получает НИЧЕГО (403 и
// апстрим даже не вызван), глобальный и admin получают ровно те же байты, что и
// раньше. Отдельно — cookie-сессия привязанного ключа: POST /v1/session не
// требует admin, поэтому «сессия панели = admin» — ложная посылка, и гейт обязан
// ловить сессию так же, как Bearer.
func TestQueryProxiesRefuseBoundKeys(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})

	// Фейковые апстримы: считают вызовы и отдают опознаваемое тело — «положительная
	// половина» обязана увидеть ИМЕННО эти байты, иначе 200 ничего не доказывает.
	const logLine = `{"_time":"2026-08-11T10:00:00Z","_msg":"secret dedik output","server_id":"s1"}`
	const vmBody = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	var vlCalls, vmCalls int
	vl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vlCalls++
		w.Header().Set("Content-Type", "application/stream+json")
		_, _ = w.Write([]byte(logLine + "\n"))
	}))
	t.Cleanup(vl.Close)
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vmCalls++
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

	logsPath := `/v1/logs/query?query={server_id="s1"}&start=0&end=10`
	proxies := []struct {
		name string
		path string
		want string // байты, которые обязан увидеть непривязанный ключ
	}{
		{"logs", logsPath, logLine},
		{"metrics", "/v1/metrics/query?query=up", vmBody},
		{"metrics_range", "/v1/metrics/query_range?query=up&start=0&end=10&step=15", vmBody},
	}

	// 1) Привязанный ключ: 403 и апстрим НЕ вызван (утечки нет даже на уровне
	//    «сходили и выбросили»).
	for _, p := range proxies {
		beforeVL, beforeVM := vlCalls, vmCalls
		code, body := bound.doRaw("GET", p.path)
		if code != 403 {
			t.Fatalf("%s: привязанный ключ получил %d (%s), want 403", p.name, code, body)
		}
		if !strings.Contains(string(body), "key is bound to neighbour/dev") {
			t.Fatalf("%s: тело 403 = %s, want «key is bound to neighbour/dev»", p.name, body)
		}
		if vlCalls != beforeVL || vmCalls != beforeVM {
			t.Fatalf("%s: апстрим вызван на запрещённом запросе (vl %d→%d, vm %d→%d)",
				p.name, beforeVL, vlCalls, beforeVM, vmCalls)
		}
	}

	// 2) Глобальный и admin-ключ: 200 и РОВНО те байты, что отдал апстрим.
	//    admin здесь не второй случай привязки, а регресс-страховка по скоупу:
	//    привязка с admin несовместима, так что обе ноги идут одной веткой
	//    key.Project == nil.
	for _, p := range proxies {
		for _, c := range []struct {
			who string
			cl  *client
		}{{"global", global}, {"admin", admin}} {
			code, body := c.cl.doRaw("GET", p.path)
			if code != 200 {
				t.Fatalf("%s/%s: %d (%s), want 200 — гейт бьёт по своим", p.name, c.who, code, body)
			}
			if !strings.Contains(string(body), p.want) {
				t.Fatalf("%s/%s: тело = %q, want содержащее %q (иначе 200 ничего не доказывает)",
					p.name, c.who, body, p.want)
			}
		}
	}

	// 3) Порядок гейтов: разрешение РАНЬШЕ состояния. Кривой limit — всё равно
	//    403, а не 400; ненастроенный master — всё равно 403, а не 503. Иначе
	//    ответ 403-ей ручки рассказывал бы, подключены ли VL/VM.
	if code, body := bound.doRaw("GET", "/v1/logs/query?query=x&limit=abc"); code != 403 {
		t.Fatalf("кривой limit у привязанного ключа: %d (%s), want 403 (разрешение раньше валидации)", code, body)
	}
	tsOff := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(tsOff.Close)
	boundOff := &client{t: t, base: tsOff.URL, key: boundSecret}
	for _, p := range proxies {
		if code, body := boundOff.doRaw("GET", p.path); code != 403 {
			t.Fatalf("%s на ненастроенном master: %d (%s), want 403 (не 503 — состояние проксии не для чужих)",
				p.name, code, body)
		}
	}

	// 4) Cookie-сессия ПРИВЯЗАННОГО ключа — тот же 403. POST /v1/session не
	//    требует admin (session.go: requireScope на нём нет), сессия наследует
	//    ключ целиком вместе с привязкой; 401 здесь означал бы, что кука не
	//    доехала и проверка выродилась.
	b := &browser{t: t, base: ts.URL}
	code, _, resp := b.do("POST", "/v1/session", map[string]any{"api_key": boundSecret})
	if code != 200 {
		t.Fatalf("логин привязанным ключом: %d, want 200 (сессию заводит любой ключ)", code)
	}
	bb := &browser{t: t, base: ts.URL, cookie: sessionCookieOf(t, resp)}
	if code, body, _ := bb.do("GET", "/v1/session", nil); code != 200 {
		t.Fatalf("кука не аутентифицирует: %d %v", code, body)
	}
	beforeVL := vlCalls
	code, sbody, _ := bb.do("GET", logsPath, nil)
	if code != 403 {
		t.Fatalf("сессия привязанного ключа получила логи: %d %v, want 403", code, sbody)
	}
	// Тело сверяем так же, как в Bearer-ноге, и не ради красоты: без него ветка
	// остаётся зелёной, когда 403 прилетел ПО ДРУГОЙ ПРИЧИНЕ (скажем, CSRF-гейт
	// по недосмотру распространили на GET), а привязка в сессии при этом
	// потеряна. Проверено мутацией: «сессия теряет привязку» + «CSRF на GET»
	// вместе давали зелёный тест на дырявом коде.
	if detail, _ := sbody["detail"].(string); !strings.Contains(detail, "key is bound to neighbour/dev") {
		t.Fatalf("сессия: detail = %q, want «key is bound to neighbour/dev» (403 должен быть ОТ ГЕЙТА привязки)", detail)
	}
	if vlCalls != beforeVL {
		t.Fatalf("сессия привязанного ключа дошла до апстрима (vl %d→%d)", beforeVL, vlCalls)
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
