package httpapi_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ufna/birdman/master/internal/backup"
	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// fakeBackupRunner stands in for *backup.Runner over the BackupRunner interface
// so the httpapi tests exercise the run-now route without a real pg_dump: it
// counts successful calls and, when busy, returns backup.ErrBusy (→ 409).
type fakeBackupRunner struct {
	calls atomic.Int64
	busy  bool
}

func (f *fakeBackupRunner) RunNow(ctx context.Context) error {
	if f.busy {
		return backup.ErrBusy
	}
	f.calls.Add(1)
	return nil
}

// backupsServer wires a server with the backup runner and s3-test injected
// (WithBackups), plus admin/readonly clients — by the registryServer template
// (registries_test.go). s3TestErr != nil makes the fake s3-test return it (→ a
// 400 s3_test_failed); nil makes it succeed (→ 200). There is no change-hook
// here (unlike registries), so the tuple is (store, admin, readonly).
func backupsServer(t *testing.T, runner httpapi.BackupRunner, s3TestErr error) (*store.Store, *client, *client) {
	t.Helper()
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	s3Test := func(context.Context) error { return s3TestErr }
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).WithBackups(runner, s3Test))
	t.Cleanup(ts.Close)
	ctx := t.Context()
	_, adminSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	_, roSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})
	admin := &client{t: t, base: ts.URL, key: adminSecret}
	ro := &client{t: t, base: ts.URL, key: roSecret}
	return st, admin, ro
}

// TestBackupsSettingsAPI: readonly is gated off every route (admin-only,
// secret-adjacent); GET carries the seed defaults with has_s3_secret=false and
// never an s3_secret_key field; PATCH sets the secret (has_s3_secret flips true
// and the secret never leaks back into GET); a follow-up PATCH omitting the
// secret keeps it (still decryptable); an out-of-range interval is a 400.
func TestBackupsSettingsAPI(t *testing.T) {
	st, admin, ro := backupsServer(t, &fakeBackupRunner{}, nil)

	// Gate: readonly → 403 on every path.
	for _, probe := range [][2]string{
		{"GET", "/v1/backups/settings"}, {"PATCH", "/v1/backups/settings"},
		{"GET", "/v1/backups/runs"}, {"POST", "/v1/backups/run"}, {"POST", "/v1/backups/s3/test"},
	} {
		if code, _ := ro.do(probe[0], probe[1], map[string]any{}); code != 403 {
			t.Fatalf("%s %s readonly: want 403, got %d", probe[0], probe[1], code)
		}
	}

	// GET defaults.
	code, body := admin.do("GET", "/v1/backups/settings", nil)
	if code != 200 {
		t.Fatalf("get: %d %v", code, body)
	}
	s := body["settings"].(map[string]any)
	if s["interval_hours"].(float64) != 6 || s["has_s3_secret"].(bool) {
		t.Fatalf("defaults: %v", s)
	}
	if _, leaked := s["s3_secret_key"]; leaked {
		t.Fatal("settings response leaked s3_secret_key field")
	}

	// PATCH: enable S3 with a secret; response and the follow-up GET carry no secret.
	code, body = admin.do("PATCH", "/v1/backups/settings", map[string]any{
		"s3_enabled": true, "s3_endpoint": "https://s3.example.com",
		"s3_bucket": "b", "s3_access_key": "ak", "s3_secret_key": "sk-secret",
	})
	if code != 200 {
		t.Fatalf("patch: %d %v", code, body)
	}
	if !body["settings"].(map[string]any)["has_s3_secret"].(bool) {
		t.Fatal("has_s3_secret must be true after setting the secret")
	}
	code, raw := admin.doRaw("GET", "/v1/backups/settings")
	if code != 200 || strings.Contains(string(raw), "sk-secret") {
		t.Fatalf("secret leaked into GET: %s", raw)
	}

	// PATCH without s3_secret_key = keep (the secret stays decryptable).
	if code, _ := admin.do("PATCH", "/v1/backups/settings", map[string]any{"s3_region": "eu"}); code != 200 {
		t.Fatalf("keep patch: %d", code)
	}
	cfg, err := st.BackupS3Config(context.Background())
	if err != nil || cfg.SecretKey != "sk-secret" {
		t.Fatalf("keep broke secret: %+v %v", cfg, err)
	}

	// Validation: interval out of bounds → 400.
	if code, _ := admin.do("PATCH", "/v1/backups/settings", map[string]any{"interval_hours": 0}); code != 400 {
		t.Fatalf("interval 0: want 400, got %d", code)
	}
}

// TestBackupsRunNow: POST /v1/backups/run triggers the runner once (202); a
// busy runner (backup.ErrBusy) is a 409.
func TestBackupsRunNow(t *testing.T) {
	runner := &fakeBackupRunner{}
	_, admin, _ := backupsServer(t, runner, nil)

	code, body := admin.do("POST", "/v1/backups/run", nil)
	if code != 202 || runner.calls.Load() != 1 {
		t.Fatalf("run: %d %v calls=%d", code, body, runner.calls.Load())
	}
	runner.busy = true
	if code, _ := admin.do("POST", "/v1/backups/run", nil); code != 409 {
		t.Fatalf("busy run: want 409, got %d", code)
	}
}

// TestBackupsS3Test: POST /v1/backups/s3/test surfaces a failing check as a 400
// s3_test_failed and a passing one as 200.
func TestBackupsS3Test(t *testing.T) {
	_, admin, _ := backupsServer(t, &fakeBackupRunner{}, errors.New("bucket missing"))
	code, body := admin.do("POST", "/v1/backups/s3/test", nil)
	if code != 400 || body["error"] != "s3_test_failed" {
		t.Fatalf("s3 test: %d %v", code, body)
	}
	_, admin2, _ := backupsServer(t, &fakeBackupRunner{}, nil)
	if code, _ := admin2.do("POST", "/v1/backups/s3/test", nil); code != 200 {
		t.Fatalf("s3 test ok: want 200, got %d", code)
	}
}

// TestBackupsRunsList: GET /v1/backups/runs returns the history newest-first;
// an empty history is exactly {"runs":[]} — an array, never null.
func TestBackupsRunsList(t *testing.T) {
	st, admin, _ := backupsServer(t, &fakeBackupRunner{}, nil)

	// Empty history FIRST: the shape must be "runs":[] (not null) — the panel
	// (Task 5) iterates the array without a null guard, so emptyNotNull is
	// load-bearing here. Raw-substring assert pins the exact wire form.
	code, raw := admin.doRaw("GET", "/v1/backups/runs")
	if code != 200 || !strings.Contains(string(raw), `"runs":[]`) {
		t.Fatalf(`empty runs must be "runs":[] (not null): %d %s`, code, raw)
	}

	id, _ := st.InsertBackupRun(context.Background(), "manual")
	_ = st.FinishBackupRun(context.Background(), id, "ok", 42, false, "")
	code, body := admin.do("GET", "/v1/backups/runs?limit=10", nil)
	if code != 200 {
		t.Fatalf("runs: %d %v", code, body)
	}
	runs := body["runs"].([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["result"] != "ok" {
		t.Fatalf("runs body: %v", runs)
	}
}
