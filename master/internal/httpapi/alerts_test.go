package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/testdb"
)

const (
	vmRulesJSON = `{"status":"success","data":{"groups":[{"name":"birdman","file":"/etc/vmalert/birdman.yml","rules":[
		{"name":"NodeDown","query":"birdman_node_heartbeat_age_seconds > 30","duration":30,"labels":{"severity":"critical"},"annotations":{"description":"нода недоступна"},"state":"inactive","type":"alerting"},
		{"name":"cpu:rate5m","query":"rate(cpu[5m])","type":"recording"}
	]}]}}`
	vmAlertsJSON = `{"status":"success","data":{"alerts":[
		{"state":"firing","name":"NodeDown","labels":{"alertname":"NodeDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"нода недоступна"},"activeAt":"2026-07-08T09:00:00Z","value":"42"},
		{"state":"pending","name":"DiskHigh","labels":{"alertname":"DiskHigh","severity":"warning"},"activeAt":"2026-07-08T10:00:00Z"}
	]}}`
)

func fakeVmalert(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/rules":
			_, _ = w.Write([]byte(vmRulesJSON))
		case "/api/v1/alerts":
			_, _ = w.Write([]byte(vmAlertsJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAlertsEndpoints(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ctx := t.Context()
	_, roSecret, _ := st.CreateAPIKey(ctx, "ro", []string{httpapi.ScopeReadonly})

	vm := fakeVmalert(t)

	// A log file with two deliveries (one firing, one resolved).
	logPath := filepath.Join(t.TempDir(), "alerts.log")
	logBody := `{"received_at":"2026-07-08T09:00:00Z","alerts":[{"status":"firing","labels":{"alertname":"NodeDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"нода недоступна"},"startsAt":"2026-07-08T08:59:00Z","endsAt":"0001-01-01T00:00:00Z"}]}
{"received_at":"2026-07-08T10:00:00Z","alerts":[{"status":"resolved","labels":{"alertname":"CrashLoop","severity":"critical","node":"n2"},"annotations":{"description":"краш-луп"},"startsAt":"2026-07-08T09:30:00Z","endsAt":"2026-07-08T09:45:00Z"}]}
`
	if err := os.WriteFile(logPath, []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).
		WithAlertsSources(vm.URL, logPath))
	t.Cleanup(ts.Close)
	ro := &client{t: t, base: ts.URL, key: roSecret}

	// rules: the recording rule is filtered out; the alerting rule is normalized.
	code, body := ro.do("GET", "/v1/alerts/rules", nil)
	if code != 200 {
		t.Fatalf("rules: %d %v", code, body)
	}
	rules := body["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("want 1 alerting rule (recording filtered), got %d", len(rules))
	}
	nd := rules[0].(map[string]any)
	if nd["name"] != "NodeDown" || nd["severity"] != "critical" || nd["for"] != "30s" ||
		nd["state"] != "inactive" || nd["description"] != "нода недоступна" {
		t.Fatalf("rule normalization: %v", nd)
	}

	// active: only firing.
	code, body = ro.do("GET", "/v1/alerts/active", nil)
	if code != 200 {
		t.Fatalf("active: %d %v", code, body)
	}
	active := body["alerts"].([]any)
	if len(active) != 1 || active[0].(map[string]any)["name"] != "NodeDown" ||
		active[0].(map[string]any)["node"] != "n1" {
		t.Fatalf("active alerts: %v", active)
	}

	// history: both deliveries, newest first, active flags per endsAt.
	code, body = ro.do("GET", "/v1/alerts/history?limit=50", nil)
	if code != 200 {
		t.Fatalf("history: %d %v", code, body)
	}
	hist := body["alerts"].([]any)
	if len(hist) != 2 {
		t.Fatalf("want 2 history entries, got %d", len(hist))
	}
	if hist[0].(map[string]any)["name"] != "CrashLoop" || hist[0].(map[string]any)["active"] != false {
		t.Fatalf("history[0] (newest, resolved): %v", hist[0])
	}
	if hist[1].(map[string]any)["name"] != "NodeDown" || hist[1].(map[string]any)["active"] != true {
		t.Fatalf("history[1] (firing): %v", hist[1])
	}
	// bad limit.
	if code, _ := ro.do("GET", "/v1/alerts/history?limit=0", nil); code != 400 {
		t.Fatalf("history bad limit: want 400, got %d", code)
	}
}

// TestAlertsUnconfigured: no vmalert URL → 503 on rules/active; a missing log
// file → history is an empty list (never 500).
func TestAlertsUnconfigured(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ctx := t.Context()
	_, roSecret, _ := st.CreateAPIKey(ctx, "ro", []string{httpapi.ScopeReadonly})

	// vmalert URL empty, log path points at a nonexistent file.
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).
		WithAlertsSources("", filepath.Join(t.TempDir(), "does-not-exist.log")))
	t.Cleanup(ts.Close)
	ro := &client{t: t, base: ts.URL, key: roSecret}

	if code, _ := ro.do("GET", "/v1/alerts/rules", nil); code != 503 {
		t.Fatalf("rules unconfigured: want 503, got %d", code)
	}
	if code, _ := ro.do("GET", "/v1/alerts/active", nil); code != 503 {
		t.Fatalf("active unconfigured: want 503, got %d", code)
	}
	code, body := ro.do("GET", "/v1/alerts/history", nil)
	if code != 200 {
		t.Fatalf("history missing file: want 200, got %d", code)
	}
	if len(body["alerts"].([]any)) != 0 {
		t.Fatalf("history missing file: want [], got %v", body["alerts"])
	}
}
