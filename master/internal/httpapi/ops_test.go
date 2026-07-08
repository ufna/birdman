package httpapi_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, rec, nil, "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

	_, adminKey, _ := st.CreateAPIKey(ctx, "admin", []string{httpapi.ScopeAdmin})
	_, roKey, _ := st.CreateAPIKey(ctx, "ro", []string{httpapi.ScopeReadonly})
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
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, rec, nil, "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

	_, adminKey, _ := st.CreateAPIKey(ctx, "admin", []string{httpapi.ScopeAdmin})
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

	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, vm.URL, log))
	t.Cleanup(ts.Close)
	ctx := t.Context()
	_, roKey, _ := st.CreateAPIKey(ctx, "ro", []string{httpapi.ScopeReadonly})
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
	ts2 := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", log))
	t.Cleanup(ts2.Close)
	ro2 := &client{t: t, base: ts2.URL, key: roKey}
	if code, _ := ro2.do("GET", "/v1/metrics/query?query=up", nil); code != 503 {
		t.Fatalf("unconfigured proxy: want 503, got %d", code)
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
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, sender, router, "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()
	_, roKey, _ := st.CreateAPIKey(ctx, "ro", []string{httpapi.ScopeReadonly})

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

func hasCmd(cmds []testdb.SentCmd, pred func(*agentlinkv1.MasterMsg) bool) bool {
	for _, c := range cmds {
		if pred(c.Msg) {
			return true
		}
	}
	return false
}
