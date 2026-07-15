package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/amsilence"
	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// fakeAM is a tiny in-memory alertmanager api/v2 silences stub (a simplified
// twin of the one in internal/amsilence): enough for the mirror end-to-end path.
type fakeAM struct {
	mu       sync.Mutex
	silences map[string]bool
	seq      int
	url      string
}

func newFakeAM(t *testing.T) *fakeAM {
	t.Helper()
	f := &fakeAM{silences: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeAM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/silences":
		f.seq++
		id := "sil-" + time.Now().Format("150405.000000") + "-" + itoa(f.seq)
		f.silences[id] = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"silenceID": id})
	case r.Method == http.MethodDelete && len(r.URL.Path) > len("/api/v2/silence/"):
		delete(f.silences, r.URL.Path[len("/api/v2/silence/"):])
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/silences":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]any{})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeAM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.silences)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func mirrorServer(t *testing.T, amURL string) (*store.Store, *client) {
	t.Helper()
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ctx := t.Context()
	_, adminSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})

	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).
		WithSilenceMirror(amsilence.New(st, amURL, log)))
	t.Cleanup(ts.Close)
	return st, &client{t: t, base: ts.URL, key: adminSecret}
}

// TestMuteSilenceMirrorLive: with a reachable AM, POST creates a silence and the
// mute row carries a non-null silence_id; DELETE removes the silence.
func TestMuteSilenceMirrorLive(t *testing.T) {
	fam := newFakeAM(t)
	_, admin := mirrorServer(t, fam.url)

	code, body := admin.do("POST", "/v1/alerts/mutes", map[string]any{"alertname": "NodeDown", "region": "eu"})
	if code != 201 {
		t.Fatalf("create mute: %d %v", code, body)
	}
	muteID := body["mute"].(map[string]any)["id"].(string)
	if fam.count() != 1 {
		t.Fatalf("mirror should have created 1 silence, got %d", fam.count())
	}

	// The mirror call is synchronous before the 201, so GET already sees silence_id.
	code, body = admin.do("GET", "/v1/alerts/mutes", nil)
	if code != 200 {
		t.Fatalf("list mutes: %d %v", code, body)
	}
	got := body["mutes"].([]any)[0].(map[string]any)
	if got["silence_id"] == nil {
		t.Fatalf("silence_id should be non-null after a live mirror: %v", got)
	}

	// Unmute removes the silence.
	if code, _ := admin.do("DELETE", "/v1/alerts/mutes/"+muteID, nil); code != 204 {
		t.Fatalf("delete mute: want 204, got %d", code)
	}
	if fam.count() != 0 {
		t.Fatalf("mirror should have removed the silence, got %d", fam.count())
	}
}

// TestMuteSilenceMirrorDeadAM: an unreachable AM never breaks mute/unmute — the
// same 201/204, and silence_id stays null (pure v0 semantics).
func TestMuteSilenceMirrorDeadAM(t *testing.T) {
	_, admin := mirrorServer(t, "http://127.0.0.1:1") // connection refused, instant

	code, body := admin.do("POST", "/v1/alerts/mutes", map[string]any{"alertname": "NodeDown"})
	if code != 201 {
		t.Fatalf("create mute (dead AM): want 201, got %d %v", code, body)
	}
	muteID := body["mute"].(map[string]any)["id"].(string)

	code, body = admin.do("GET", "/v1/alerts/mutes", nil)
	got := body["mutes"].([]any)[0].(map[string]any)
	if got["silence_id"] != nil {
		t.Fatalf("silence_id must stay null when AM is unreachable: %v", got)
	}
	if code, _ := admin.do("DELETE", "/v1/alerts/mutes/"+muteID, nil); code != 204 {
		t.Fatalf("delete mute (dead AM): want 204, got %d", code)
	}
}
