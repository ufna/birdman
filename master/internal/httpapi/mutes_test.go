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
