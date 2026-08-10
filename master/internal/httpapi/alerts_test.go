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
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

const (
	vmRulesJSON = `{"status":"success","data":{"groups":[{"name":"birdman","file":"/etc/vmalert/birdman.yml","rules":[
		{"name":"NodeDown","query":"birdman_node_heartbeat_age_seconds > 30","duration":30,"labels":{"severity":"critical"},"annotations":{"description":"node is unreachable","description_ru":"нода недоступна"},"state":"inactive","type":"alerting"},
		{"name":"cpu:rate5m","query":"rate(cpu[5m])","type":"recording"}
	]}]}}`
	vmAlertsJSON = `{"status":"success","data":{"alerts":[
		{"state":"firing","name":"NodeDown","labels":{"alertname":"NodeDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"node is unreachable","description_ru":"нода недоступна"},"activeAt":"2026-07-08T09:00:00Z","value":"42"},
		{"state":"pending","name":"DiskHigh","labels":{"alertname":"DiskHigh","severity":"warning"},"activeAt":"2026-07-08T10:00:00Z"}
	]}}`
	// Project scoping (tracker #955): one alert of alpha, one of beta, one
	// platform alert with no project label at all.
	vmAlertsProjectJSON = `{"status":"success","data":{"alerts":[
		{"state":"firing","name":"BufferEmptyReadyProd","labels":{"alertname":"BufferEmptyReadyProd","severity":"critical","region":"eu","project":"alpha"},"annotations":{"description":"no ready servers"},"activeAt":"2026-07-08T09:00:00Z","value":"0"},
		{"state":"firing","name":"AllocationFailures","labels":{"alertname":"AllocationFailures","severity":"warning","region":"eu","project":"beta"},"annotations":{"description":"alloc failures"},"activeAt":"2026-07-08T09:10:00Z","value":"3"},
		{"state":"firing","name":"NodeDown","labels":{"alertname":"NodeDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"node is unreachable"},"activeAt":"2026-07-08T09:20:00Z","value":"42"}
	]}}`
)

func fakeVmalert(t *testing.T) *httptest.Server {
	return fakeVmalertWith(t, vmAlertsJSON)
}

func fakeVmalertWith(t *testing.T, alertsJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/rules":
			_, _ = w.Write([]byte(vmRulesJSON))
		case "/api/v1/alerts":
			_, _ = w.Write([]byte(alertsJSON))
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
	_, roSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})

	vm := fakeVmalert(t)

	// A log file with two deliveries (one firing, one resolved).
	logPath := filepath.Join(t.TempDir(), "alerts.log")
	logBody := `{"received_at":"2026-07-08T09:00:00Z","alerts":[{"status":"firing","labels":{"alertname":"NodeDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"node is unreachable","description_ru":"нода недоступна"},"startsAt":"2026-07-08T08:59:00Z","endsAt":"0001-01-01T00:00:00Z"}]}
{"received_at":"2026-07-08T10:00:00Z","alerts":[{"status":"resolved","labels":{"alertname":"CrashLoop","severity":"critical","node":"n2"},"annotations":{"description":"crash loop"},"startsAt":"2026-07-08T09:30:00Z","endsAt":"2026-07-08T09:45:00Z"}]}
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
		nd["state"] != "inactive" || nd["description"] != "node is unreachable" {
		t.Fatalf("rule normalization: %v", nd)
	}
	// Bilingual: description is EN (canonical), description_ru carries the RU text.
	if nd["description_ru"] != "нода недоступна" {
		t.Fatalf("rule description_ru not passed through: %v", nd)
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
	if a0 := active[0].(map[string]any); a0["description"] != "node is unreachable" || a0["description_ru"] != "нода недоступна" {
		t.Fatalf("active bilingual description: %v", a0)
	}
	// A rule built on a metric without a `project` label is platform-scoped.
	if a0 := active[0].(map[string]any); a0["project"] != "" || a0["scope"] != "platform" {
		t.Fatalf("NodeDown must be platform-scoped: %v", a0)
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
	// NodeDown carries a RU translation; CrashLoop has none → description_ru
	// omitted (omitempty), leaving the panel to fall back to the EN description.
	if h1 := hist[1].(map[string]any); h1["description"] != "node is unreachable" || h1["description_ru"] != "нода недоступна" {
		t.Fatalf("history bilingual description: %v", h1)
	}
	if _, ok := hist[0].(map[string]any)["description_ru"]; ok {
		t.Fatalf("history without description_ru should omit the field: %v", hist[0])
	}
	// bad limit.
	if code, _ := ro.do("GET", "/v1/alerts/history?limit=0", nil); code != 400 {
		t.Fatalf("history bad limit: want 400, got %d", code)
	}
}

// TestAlertsProjectScope covers ?project= on /v1/alerts/{active,history}
// (tracker #955). The load-bearing assertion is the SECOND one: the filter is
// non-hiding, so a platform alert (no `project` label) must survive a project
// selection — silently swallowing NodeDown/MasterDown would be worse than
// showing a neighbour's alert, and a project has no "All" mode to get it back.
//
// The three slugs are REAL projects here (tracker #961): ?project= is validated
// against the DB now, so a made-up slug tests the typo path, not the filter.
func TestAlertsProjectScope(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ctx := t.Context()
	_, roSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})
	for _, slug := range []string{"alpha", "beta", "gamma"} {
		if _, err := st.SetProjectMatchSize(ctx, slug, 2); err != nil {
			t.Fatalf("project %s: %v", slug, err)
		}
	}

	vm := fakeVmalertWith(t, vmAlertsProjectJSON)

	// History mirrors the active set: alpha, beta and one unlabelled platform alert.
	logPath := filepath.Join(t.TempDir(), "alerts.log")
	logBody := `{"received_at":"2026-07-08T09:00:00Z","alerts":[{"status":"firing","labels":{"alertname":"BufferEmptyReadyProd","severity":"critical","region":"eu","project":"alpha"},"annotations":{"description":"no ready servers"},"startsAt":"2026-07-08T08:59:00Z","endsAt":""}]}
{"received_at":"2026-07-08T09:10:00Z","alerts":[{"status":"firing","labels":{"alertname":"AllocationFailures","severity":"warning","region":"eu","project":"beta"},"annotations":{"description":"alloc failures"},"startsAt":"2026-07-08T09:09:00Z","endsAt":""}]}
{"received_at":"2026-07-08T09:20:00Z","alerts":[{"status":"firing","labels":{"alertname":"NodeDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"node is unreachable"},"startsAt":"2026-07-08T09:19:00Z","endsAt":""}]}
`
	if err := os.WriteFile(logPath, []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).
		WithAlertsSources(vm.URL, logPath))
	t.Cleanup(ts.Close)
	ro := &client{t: t, base: ts.URL, key: roSecret}

	// names collects name→alert for one endpoint call, failing the test on a
	// non-200 or a malformed body.
	names := func(path string) map[string]map[string]any {
		t.Helper()
		code, body := ro.do("GET", path, nil)
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

	for _, path := range []string{"/v1/alerts/active", "/v1/alerts/history"} {
		// 1. No ?project= → behaviour as before: everything is returned.
		all := names(path)
		if len(all) != 3 {
			t.Fatalf("%s without filter: want 3 alerts, got %d (%v)", path, len(all), all)
		}
		// scope is derived from the presence of the label — no scope: label in rules.
		if all["BufferEmptyReadyProd"]["scope"] != "project" || all["BufferEmptyReadyProd"]["project"] != "alpha" {
			t.Fatalf("%s: project alert scope/project: %v", path, all["BufferEmptyReadyProd"])
		}
		if all["NodeDown"]["scope"] != "platform" || all["NodeDown"]["project"] != "" {
			t.Fatalf("%s: platform alert scope/project: %v", path, all["NodeDown"])
		}

		// 2. ?project=alpha → narrowed, but NOT hiding.
		got := names(path + "?project=alpha")
		if _, ok := got["BufferEmptyReadyProd"]; !ok {
			t.Fatalf("%s?project=alpha: own alert missing (%v)", path, got)
		}
		if _, ok := got["NodeDown"]; !ok {
			t.Fatalf("%s?project=alpha: PLATFORM alert must stay visible (%v)", path, got)
		}
		if _, ok := got["AllocationFailures"]; ok {
			t.Fatalf("%s?project=alpha: beta's alert must be gone (%v)", path, got)
		}
		if len(got) != 2 {
			t.Fatalf("%s?project=alpha: want alpha+platform, got %v", path, got)
		}

		// 3. A project with no alerts of its own still sees the platform ones.
		none := names(path + "?project=gamma")
		if len(none) != 1 {
			t.Fatalf("%s?project=gamma: want just the platform alert, got %v", path, none)
		}
		if _, ok := none["NodeDown"]; !ok {
			t.Fatalf("%s?project=gamma: platform alert missing (%v)", path, none)
		}

		// 4. …and a project that does NOT exist is a typo, not a quiet screen
		// (tracker #961): case 3 above is exactly what a typo used to look like.
		if code, body := ro.do("GET", path+"?project=gama", nil); code != 400 {
			t.Fatalf("%s?project=gama (typo): want 400, got %d (%v)", path, code, body)
		}
	}

	// /v1/alerts/rules is the configuration catalogue, not alert instances —
	// ?project= must NOT shrink it (the project lives in the expr, not in labels).
	_, unfiltered := ro.do("GET", "/v1/alerts/rules", nil)
	_, filtered := ro.do("GET", "/v1/alerts/rules?project=alpha", nil)
	if len(filtered["rules"].([]any)) != len(unfiltered["rules"].([]any)) {
		t.Fatalf("rules must ignore ?project=: %v vs %v", filtered["rules"], unfiltered["rules"])
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
	_, roSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})

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
