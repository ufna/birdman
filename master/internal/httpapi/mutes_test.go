package httpapi_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// muteServer wires a server with a fake vmalert (one firing NodeDown/eu) and a
// two-delivery alert log (NodeDown/eu firing, CrashLoop resolved) so both the
// active and history muted flags can be exercised. Returns admin + ro clients.
func muteServer(t *testing.T) (*store.Store, *client, *client) {
	t.Helper()
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ctx := t.Context()
	_, adminSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	_, roSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})

	vm := fakeVmalert(t)
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

	admin := &client{t: t, base: ts.URL, key: adminSecret}
	ro := &client{t: t, base: ts.URL, key: roSecret}
	return st, admin, ro
}

// muted looks up an alert by name in an /alerts/{active,history} response and
// returns its muted flag.
func muted(t *testing.T, body map[string]any, name string) bool {
	t.Helper()
	for _, a := range body["alerts"].([]any) {
		am := a.(map[string]any)
		if am["name"] == name {
			return am["muted"].(bool)
		}
	}
	t.Fatalf("alert %q not found in %v", name, body["alerts"])
	return false
}

func TestAlertMutesEndpoints(t *testing.T) {
	st, admin, ro := muteServer(t)
	ctx := t.Context()
	expires := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)

	// readonly cannot mute or unmute; can list.
	if code, _ := ro.do("POST", "/v1/alerts/mutes", map[string]any{"alertname": "NodeDown"}); code != 403 {
		t.Fatalf("ro mute: want 403, got %d", code)
	}
	if code, _ := ro.do("DELETE", "/v1/alerts/mutes/"+uuid.NewString(), nil); code != 403 {
		t.Fatalf("ro unmute: want 403, got %d", code)
	}

	// admin mutes NodeDown in eu with a note and a future expiry → 201.
	code, body := admin.do("POST", "/v1/alerts/mutes", map[string]any{
		"alertname": "NodeDown", "region": "eu", "note": "planned maint", "expires_at": expires,
	})
	if code != 201 {
		t.Fatalf("create mute: %d %v", code, body)
	}
	mute := body["mute"].(map[string]any)
	muteID, _ := mute["id"].(string)
	if muteID == "" || mute["alertname"] != "NodeDown" || mute["region"] != "eu" ||
		mute["note"] != "planned maint" || mute["created_by"] != "admin" || mute["expires_at"] == nil {
		t.Fatalf("mute shape: %v", mute)
	}

	// history: NodeDown (eu) now muted, CrashLoop not.
	code, body = ro.do("GET", "/v1/alerts/history?limit=50", nil)
	if code != 200 {
		t.Fatalf("history: %d %v", code, body)
	}
	if !muted(t, body, "NodeDown") || muted(t, body, "CrashLoop") {
		t.Fatalf("history muted flags wrong: %v", body["alerts"])
	}

	// active: firing NodeDown muted.
	code, body = ro.do("GET", "/v1/alerts/active", nil)
	if code != 200 || !muted(t, body, "NodeDown") {
		t.Fatalf("active muted: %d %v", code, body)
	}

	// list mutes (readonly): exactly the one active mute.
	code, body = ro.do("GET", "/v1/alerts/mutes", nil)
	if code != 200 {
		t.Fatalf("list mutes: %d %v", code, body)
	}
	if got := body["mutes"].([]any); len(got) != 1 || got[0].(map[string]any)["id"] != muteID {
		t.Fatalf("list mutes: %v", got)
	}

	// dedup: re-muting the same (alertname, region) upserts in place (still one
	// active row) and updates the note.
	code, body = admin.do("POST", "/v1/alerts/mutes", map[string]any{
		"alertname": "NodeDown", "region": "eu", "note": "extended",
	})
	if code != 201 {
		t.Fatalf("re-mute: %d %v", code, body)
	}
	if body["mute"].(map[string]any)["id"] != muteID {
		t.Fatalf("re-mute should reuse the active row: %v", body["mute"])
	}
	code, body = ro.do("GET", "/v1/alerts/mutes", nil)
	if got := body["mutes"].([]any); len(got) != 1 || got[0].(map[string]any)["note"] != "extended" {
		t.Fatalf("dedup upsert: want 1 mute noted 'extended', got %v", got)
	}

	// validation: empty alertname, past expiry, malformed expiry.
	if code, _ := admin.do("POST", "/v1/alerts/mutes", map[string]any{"alertname": "  "}); code != 400 {
		t.Fatalf("empty alertname: want 400, got %d", code)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if code, _ := admin.do("POST", "/v1/alerts/mutes", map[string]any{"alertname": "X", "expires_at": past}); code != 400 {
		t.Fatalf("past expiry: want 400, got %d", code)
	}
	if code, _ := admin.do("POST", "/v1/alerts/mutes", map[string]any{"alertname": "X", "expires_at": "not-a-time"}); code != 400 {
		t.Fatalf("bad expiry: want 400, got %d", code)
	}

	// unmute: bad uuid → 400, unknown → 404, real → 204, repeat → 404.
	if code, _ := admin.do("DELETE", "/v1/alerts/mutes/not-a-uuid", nil); code != 400 {
		t.Fatalf("bad uuid delete: want 400, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/alerts/mutes/"+uuid.NewString(), nil); code != 404 {
		t.Fatalf("unknown delete: want 404, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/alerts/mutes/"+muteID, nil); code != 204 {
		t.Fatalf("delete: want 204, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/alerts/mutes/"+muteID, nil); code != 404 {
		t.Fatalf("repeat delete: want 404, got %d", code)
	}

	// after unmute, NodeDown is no longer muted.
	_, body = ro.do("GET", "/v1/alerts/history?limit=50", nil)
	if muted(t, body, "NodeDown") {
		t.Fatalf("NodeDown should be un-muted after delete: %v", body["alerts"])
	}

	// audit events written (create counted the upsert too).
	if n, _ := st.CountEvents(ctx, store.EventAlertMuted); n != 2 {
		t.Fatalf("alert_muted events: want 2, got %d", n)
	}
	if n, _ := st.CountEvents(ctx, store.EventAlertUnmuted); n != 1 {
		t.Fatalf("alert_unmuted events: want 1, got %d", n)
	}
}

// muteProjectServer wires a server whose vmalert reports three firing alerts —
// one of project alpha, one of beta, and a PLATFORM NodeDown with no project
// label at all — with the alert log carrying the same three, so the muted flag
// can be exercised on /active and /history alike (tracker #957).
func muteProjectServer(t *testing.T) (*client, *client) {
	t.Helper()
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ctx := t.Context()
	_, adminSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	_, roSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})

	vm := fakeVmalertWith(t, vmAlertsProjectJSON)
	logPath := filepath.Join(t.TempDir(), "alerts.log")
	logBody := `{"received_at":"2026-07-08T09:00:00Z","alerts":[{"status":"firing","labels":{"alertname":"BufferEmptyReadyProd","severity":"critical","region":"eu","project":"alpha"},"annotations":{"description":"no ready servers"},"startsAt":"2026-07-08T08:59:00Z","endsAt":"0001-01-01T00:00:00Z"}]}
{"received_at":"2026-07-08T09:10:00Z","alerts":[{"status":"firing","labels":{"alertname":"AllocationFailures","severity":"warning","region":"eu","project":"beta"},"annotations":{"description":"alloc failures"},"startsAt":"2026-07-08T09:09:00Z","endsAt":"0001-01-01T00:00:00Z"}]}
{"received_at":"2026-07-08T09:20:00Z","alerts":[{"status":"firing","labels":{"alertname":"NodeDown","severity":"critical","region":"eu","node":"n1"},"annotations":{"description":"node is unreachable"},"startsAt":"2026-07-08T09:19:00Z","endsAt":"0001-01-01T00:00:00Z"}]}
`
	if err := os.WriteFile(logPath, []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).
		WithAlertsSources(vm.URL, logPath))
	t.Cleanup(ts.Close)

	return &client{t: t, base: ts.URL, key: adminSecret}, &client{t: t, base: ts.URL, key: roSecret}
}

// TestAlertMuteProjectScoping is the heart of tracker #957: a mute carries a
// project, and the match is strict on it.
//
// The load-bearing case is the last one — a PROJECT mute must never cover a
// PLATFORM alert. Muting NodeDown "for alpha" and having master go quiet for
// everyone would be the worst kind of defect here: it looks like the mute
// works, while project beta loses a signal it never agreed to lose.
func TestAlertMuteProjectScoping(t *testing.T) {
	admin, ro := muteProjectServer(t)

	// mutedEverywhere asserts the same flag on /active and /history — the two
	// paths compute it separately, and a fix landing in only one of them would
	// leave the panel showing two different truths for one alert.
	mutedEverywhere := func(name string) (bool, bool) {
		t.Helper()
		_, act := ro.do("GET", "/v1/alerts/active", nil)
		_, hist := ro.do("GET", "/v1/alerts/history?limit=50", nil)
		return muted(t, act, name), muted(t, hist, name)
	}

	// A mute scoped to alpha covers alpha's alert only.
	code, body := admin.do("POST", "/v1/alerts/mutes", map[string]any{
		"alertname": "BufferEmptyReadyProd", "project": "alpha",
	})
	if code != 201 {
		t.Fatalf("mute alpha: %d %v", code, body)
	}
	if got := body["mute"].(map[string]any)["project"]; got != "alpha" {
		t.Fatalf("mute shape must carry the project: %v", body["mute"])
	}
	if a, h := mutedEverywhere("BufferEmptyReadyProd"); !a || !h {
		t.Fatalf("alpha's alert must be muted by alpha's mute (active=%v history=%v)", a, h)
	}

	// The same alertname muted for beta does NOT touch alpha's alert — proven
	// on a second alertname so the assertion cannot pass by accident.
	if code, _ := admin.do("POST", "/v1/alerts/mutes", map[string]any{
		"alertname": "AllocationFailures", "project": "alpha",
	}); code != 201 {
		t.Fatalf("mute AllocationFailures for alpha: %d", code)
	}
	if a, h := mutedEverywhere("AllocationFailures"); a || h {
		t.Fatalf("beta's alert must NOT be muted by an alpha mute (active=%v history=%v)", a, h)
	}

	// THE trap: a project mute must not silence the platform alert.
	if code, _ := admin.do("POST", "/v1/alerts/mutes", map[string]any{
		"alertname": "NodeDown", "project": "alpha",
	}); code != 201 {
		t.Fatalf("mute NodeDown for alpha: %d", code)
	}
	if a, h := mutedEverywhere("NodeDown"); a || h {
		t.Fatalf("a PROJECT mute must never cover the PLATFORM alert — "+
			"alpha would be silencing master for beta too (active=%v history=%v)", a, h)
	}

	// Muting it WITHOUT a project is how you silence a platform alert: visible,
	// deliberate, and still the pre-#957 meaning of a project-less mute.
	if code, _ := admin.do("POST", "/v1/alerts/mutes", map[string]any{"alertname": "NodeDown"}); code != 201 {
		t.Fatalf("mute NodeDown for all projects: %d", code)
	}
	if a, h := mutedEverywhere("NodeDown"); !a || !h {
		t.Fatalf("a project-less mute must cover the platform alert (active=%v history=%v)", a, h)
	}

	// …and that same project-less mute covers project alerts too (all projects).
	if code, _ := admin.do("POST", "/v1/alerts/mutes", map[string]any{"alertname": "AllocationFailures"}); code != 201 {
		t.Fatalf("mute AllocationFailures for all projects: %d", code)
	}
	if a, h := mutedEverywhere("AllocationFailures"); !a || !h {
		t.Fatalf("a project-less mute must cover a project alert (active=%v history=%v)", a, h)
	}

	// The five mutes are five distinct rows: the target is a triple, so alpha's
	// NodeDown mute never overwrote the project-less one (and vice versa).
	code, body = ro.do("GET", "/v1/alerts/mutes", nil)
	if code != 200 || len(body["mutes"].([]any)) != 5 {
		t.Fatalf("want 5 distinct mutes (targets differ by project), got %v", body["mutes"])
	}
	// An empty project string is the wildcard spelled sloppily, not a target of
	// its own: it upserts the project-less NodeDown mute in place.
	if code, _ := admin.do("POST", "/v1/alerts/mutes", map[string]any{
		"alertname": "NodeDown", "project": "", "note": "same target",
	}); code != 201 {
		t.Fatalf("empty project mute: %d", code)
	}
	_, body = ro.do("GET", "/v1/alerts/mutes", nil)
	if len(body["mutes"].([]any)) != 5 {
		t.Fatalf(`"project":"" must mean "all projects", not a sixth row: %v`, body["mutes"])
	}
}

// TestAlertMuteRegionScoping: a region-scoped mute only covers that region; a
// null-region mute covers every region.
func TestAlertMuteRegionScoping(t *testing.T) {
	_, admin, ro := muteServer(t)

	// A us-scoped NodeDown mute must NOT mute the eu NodeDown.
	if code, _ := admin.do("POST", "/v1/alerts/mutes", map[string]any{"alertname": "NodeDown", "region": "us"}); code != 201 {
		t.Fatalf("mute us: want 201, got %d", code)
	}
	_, body := ro.do("GET", "/v1/alerts/history?limit=50", nil)
	if muted(t, body, "NodeDown") {
		t.Fatalf("eu NodeDown must not be muted by a us-scoped mute: %v", body["alerts"])
	}

	// A null-region mute (no region field) covers eu.
	if code, _ := admin.do("POST", "/v1/alerts/mutes", map[string]any{"alertname": "NodeDown"}); code != 201 {
		t.Fatalf("mute all regions: want 201, got %d", code)
	}
	_, body = ro.do("GET", "/v1/alerts/history?limit=50", nil)
	if !muted(t, body, "NodeDown") {
		t.Fatalf("eu NodeDown must be muted by a null-region mute: %v", body["alerts"])
	}

	// ?all=1 returns the active mutes too (both us + null-region here).
	code, body := ro.do("GET", "/v1/alerts/mutes?all=1", nil)
	if code != 200 || len(body["mutes"].([]any)) != 2 {
		t.Fatalf("list all mutes: %d %v", code, body["mutes"])
	}
}
