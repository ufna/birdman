package amsilence

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func strptr(s string) *string { return &s }

// --- in-memory fake alertmanager (api/v2 silences subset) ---

type fakeSilence struct {
	ID        string    `json:"id"`
	Matchers  []matcher `json:"matchers"`
	StartsAt  string    `json:"startsAt"`
	EndsAt    string    `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
	State     string    `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

func (s *fakeSilence) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"id":        s.ID,
		"status":    map[string]any{"state": s.State},
		"matchers":  s.Matchers,
		"startsAt":  s.StartsAt,
		"endsAt":    s.EndsAt,
		"createdBy": s.CreatedBy,
		"comment":   s.Comment,
		"updatedAt": s.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}

type fakeAM struct {
	mu       sync.Mutex
	silences map[string]*fakeSilence
	seq      int
	lastPost silencePost // the most recent POST body (for asserting id/matchers sent)

	// failPostStatus, when nonzero, makes handlePost record the body then answer
	// this status WITHOUT creating a silence — a responding AM rejecting the POST
	// (e.g. 404 for an unknown/stale silence id). Zero value = old behavior.
	failPostStatus int
	postCount      int      // number of POSTs received (retry accounting)
	postIDs        []string // the id field of each POST body, in order
}

func newFakeAM(t *testing.T) (*fakeAM, string) {
	t.Helper()
	f := &fakeAM{silences: map[string]*fakeSilence{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeAM) newID() string {
	f.seq++
	return "sil-" + itoa(f.seq)
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

func (f *fakeAM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/silences":
		f.handlePost(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/silences":
		writeJSON(w, f.listLocked())
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/silence/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/silence/")
		if s, ok := f.silences[id]; ok {
			writeJSON(w, s)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v2/silence/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/silence/")
		if _, ok := f.silences[id]; ok {
			delete(f.silences, id)
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeAM) handlePost(w http.ResponseWriter, r *http.Request) {
	var body silencePost
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.lastPost = body
	f.postCount++
	f.postIDs = append(f.postIDs, body.ID)
	if f.failPostStatus != 0 { // responding AM rejects the POST without creating a silence
		w.WriteHeader(f.failPostStatus)
		return
	}
	id := body.ID
	// A POST with the id of a missing/expired silence mints a NEW id (models AM).
	if id == "" {
		id = f.newID()
	} else if s, ok := f.silences[id]; !ok || s.State == "expired" {
		id = f.newID()
	}
	f.silences[id] = &fakeSilence{
		ID: id, Matchers: body.Matchers, StartsAt: body.StartsAt, EndsAt: body.EndsAt,
		CreatedBy: body.CreatedBy, Comment: body.Comment, State: "active", UpdatedAt: time.Now(),
	}
	writeJSON(w, map[string]string{"silenceID": id})
}

func (f *fakeAM) listLocked() []*fakeSilence {
	out := make([]*fakeSilence, 0, len(f.silences))
	for _, s := range f.silences {
		out = append(out, s)
	}
	return out
}

// test-side helpers (lock, so they are safe against the server goroutine).

func (f *fakeAM) get(id string) (fakeSilence, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.silences[id]
	if !ok {
		return fakeSilence{}, false
	}
	return *s, true
}

func (f *fakeAM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.silences)
}

func (f *fakeAM) expire(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.silences[id]; ok {
		s.State = "expired"
	}
}

func (f *fakeAM) setUpdatedAt(id string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.silences[id]; ok {
		s.UpdatedAt = at
	}
}

// setFailPostStatus makes every subsequent POST answer status without creating a
// silence (models a responding AM rejecting the id'd POST, e.g. 404).
func (f *fakeAM) setFailPostStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPostStatus = status
}

// posts returns the number of POSTs seen and the id sent in each, in order.
func (f *fakeAM) posts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postCount, append([]string(nil), f.postIDs...)
}

// seed inserts a silence directly (orphan-sweep tests). Returns its id.
func (f *fakeAM) seed(s *fakeSilence) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.ID == "" {
		s.ID = f.newID()
	}
	if s.State == "" {
		s.State = "active"
	}
	f.silences[s.ID] = s
	return s.ID
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// muteByID fetches the (active) mute with id from the store.
func muteByID(t *testing.T, st *store.Store, id string) store.AlertMute {
	t.Helper()
	mutes, err := st.ListAlertMutes(context.Background(), false)
	if err != nil {
		t.Fatalf("list mutes: %v", err)
	}
	for _, m := range mutes {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("mute %s not found", id)
	return store.AlertMute{}
}

// --- tests ---

func TestMuteUpsertedCreatesSilence(t *testing.T) {
	st := testdb.New(t)
	fam, url := newFakeAM(t)
	mr := New(st, url, testLog())
	ctx := context.Background()

	expires := time.Now().Add(2 * time.Hour).UTC()
	mute, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
		Alertname: "NodeDown", Region: strptr("eu"), Note: "maint", ExpiresAt: &expires, CreatedBy: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	mr.MuteUpserted(ctx, mute)

	got := muteByID(t, st, mute.ID)
	if got.SilenceID == nil {
		t.Fatal("silence_id not persisted after MuteUpserted")
	}
	sil, ok := fam.get(*got.SilenceID)
	if !ok {
		t.Fatalf("silence %s not on AM", *got.SilenceID)
	}
	// Matchers: alertname + region (both present for a region-scoped mute).
	if len(sil.Matchers) != 2 || sil.Matchers[0].Name != "alertname" || sil.Matchers[0].Value != "NodeDown" ||
		sil.Matchers[1].Name != "region" || sil.Matchers[1].Value != "eu" {
		t.Fatalf("matchers: %+v", sil.Matchers)
	}
	if sil.CreatedBy != "birdman:admin" {
		t.Fatalf("createdBy = %q, want birdman:admin", sil.CreatedBy)
	}
	// endsAt == expires_at (RFC3339 second precision).
	if sil.EndsAt != expires.Format(time.RFC3339) {
		t.Fatalf("endsAt = %q, want %q", sil.EndsAt, expires.Format(time.RFC3339))
	}
}

func TestMuteUpsertedNilRegionNoRegionMatcher(t *testing.T) {
	st := testdb.New(t)
	fam, url := newFakeAM(t)
	mr := New(st, url, testLog())
	ctx := context.Background()

	mute, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
		Alertname: "NodeDown", CreatedBy: "admin", // nil region, nil expiry
	})
	if err != nil {
		t.Fatal(err)
	}
	mr.MuteUpserted(ctx, mute)

	got := muteByID(t, st, mute.ID)
	sil, ok := fam.get(*got.SilenceID)
	if !ok {
		t.Fatal("silence missing")
	}
	if len(sil.Matchers) != 1 || sil.Matchers[0].Name != "alertname" {
		t.Fatalf("nil region must yield only an alertname matcher: %+v", sil.Matchers)
	}
	// Open-ended mute → far horizon (>9 years out).
	end := parseTime(sil.EndsAt)
	if !end.After(time.Now().Add(9 * 365 * 24 * time.Hour)) {
		t.Fatalf("open-ended endsAt %v is not a far horizon", end)
	}
}

func TestMuteUpsertedReusesID(t *testing.T) {
	st := testdb.New(t)
	fam, url := newFakeAM(t)
	mr := New(st, url, testLog())
	ctx := context.Background()

	mute, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{Alertname: "NodeDown", CreatedBy: "admin"})
	mr.MuteUpserted(ctx, mute)
	first := *muteByID(t, st, mute.ID).SilenceID

	// Re-mute the same target (upsert keeps silence_id); MuteUpserted must send
	// the stored id in the POST body and keep the id in the DB.
	mute2, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{Alertname: "NodeDown", Note: "edit", CreatedBy: "admin"})
	mr.MuteUpserted(ctx, mute2)

	if fam.lastPost.ID != first {
		t.Fatalf("re-upsert POST body id = %q, want %q", fam.lastPost.ID, first)
	}
	if got := *muteByID(t, st, mute.ID).SilenceID; got != first {
		t.Fatalf("silence_id changed on in-place update: %q → %q", first, got)
	}
	if fam.count() != 1 {
		t.Fatalf("in-place update should not create a second silence, count=%d", fam.count())
	}
}

func TestMuteUpsertedAMUnreachable(t *testing.T) {
	st := testdb.New(t)
	mr := New(st, "http://127.0.0.1:1", testLog()) // connection refused, instant
	ctx := context.Background()

	mute, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{Alertname: "NodeDown", CreatedBy: "admin"})
	done := make(chan struct{})
	go func() { mr.MuteUpserted(ctx, mute); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("MuteUpserted hung against an unreachable AM")
	}
	if got := muteByID(t, st, mute.ID); got.SilenceID != nil {
		t.Fatalf("silence_id should stay nil when AM is unreachable, got %v", *got.SilenceID)
	}
}

// TestMuteUpsertedUnreachableSkipsRetry: an AM that blackholes the POST (transport
// timeout) with the retry branch armed (mute carries a stored silence id) makes
// exactly ONE attempt — a second POST would fail identically and only double the
// handler's worst-case delay (callTimeout → 2×callTimeout). Runs ~one callTimeout.
func TestMuteUpsertedUnreachableSkipsRetry(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	// Blackhole AM: count each POST, then never respond — the client times out at
	// its per-call timeout (callTimeout), a transport failure, not a non-2xx. The
	// handler parks on a test-owned channel, NOT r.Context(): with an unread body
	// the server never observes the client disconnect, so the context would never
	// fire and srv.Close would wait on the handler forever. LIFO cleanup ordering
	// closes stop first, releasing the handler before srv.Close waits.
	var posts atomic.Int32
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		<-stop // released at test end, before srv.Close (LIFO cleanup)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) }) // registered after srv.Close → runs FIRST, unblocking handlers
	mr := New(st, srv.URL, testLog())

	// A stored silence id arms the retry branch (existing != "").
	mute, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{Alertname: "NodeDown", CreatedBy: "admin"})
	if err := st.SetAlertMuteSilenceID(ctx, mute.ID, strptr("sil-stale")); err != nil {
		t.Fatal(err)
	}
	mute = muteByID(t, st, mute.ID)
	if mute.SilenceID == nil {
		t.Fatal("precondition: stored silence id should arm the retry branch")
	}

	start := time.Now()
	done := make(chan struct{})
	go func() { mr.MuteUpserted(ctx, mute); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("MuteUpserted hung against a blackholed AM")
	}
	elapsed := time.Since(start)

	if n := posts.Load(); n != 1 {
		t.Fatalf("blackholed AM: want exactly 1 POST (no retry), got %d", n)
	}
	// One callTimeout (~5s) with slack; a retry would push this toward ~10s.
	if elapsed >= 8*time.Second {
		t.Fatalf("elapsed %v suggests a second attempt against a down AM (want ~one callTimeout)", elapsed)
	}
}

// TestMuteUpsertedLiveAMErrorStillRetries: a RESPONDING AM that rejects the id'd
// POST (404 for a stale id) keeps the old semantics — exactly TWO POSTs, first
// with the stored id, then a retry WITHOUT it (a fresh silence). Both fail, so
// persistID is never reached and the mute keeps its id for the reconcile loop.
func TestMuteUpsertedLiveAMErrorStillRetries(t *testing.T) {
	st := testdb.New(t)
	fam, url := newFakeAM(t)
	fam.setFailPostStatus(http.StatusNotFound) // AM v2 answers 404 for an unknown/stale id
	mr := New(st, url, testLog())
	ctx := context.Background()

	// A stored silence id arms the retry branch.
	mute, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{Alertname: "NodeDown", CreatedBy: "admin"})
	if err := st.SetAlertMuteSilenceID(ctx, mute.ID, strptr("sil-stale")); err != nil {
		t.Fatal(err)
	}
	mute = muteByID(t, st, mute.ID)

	mr.MuteUpserted(ctx, mute)

	n, ids := fam.posts()
	if n != 2 {
		t.Fatalf("live AM 404: want exactly 2 POSTs (id'd, then retry without id), got %d", n)
	}
	if ids[0] != "sil-stale" {
		t.Fatalf("first POST id = %q, want the stored id %q", ids[0], "sil-stale")
	}
	if ids[1] != "" {
		t.Fatalf("retry POST id = %q, want empty (fresh silence)", ids[1])
	}
	// Both POSTs 404'd → persistID never runs → silence_id unchanged.
	if got := muteByID(t, st, mute.ID); got.SilenceID == nil || *got.SilenceID != "sil-stale" {
		t.Fatalf("silence_id should be unchanged (%q) after two failed POSTs, got %v", "sil-stale", got.SilenceID)
	}
}

func TestMuteDeleted(t *testing.T) {
	st := testdb.New(t)
	fam, url := newFakeAM(t)
	mr := New(st, url, testLog())
	ctx := context.Background()

	mute, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{Alertname: "NodeDown", CreatedBy: "admin"})
	mr.MuteUpserted(ctx, mute)
	stored := muteByID(t, st, mute.ID)
	if fam.count() != 1 {
		t.Fatalf("precondition: want 1 silence, got %d", fam.count())
	}

	// Delete removes the mirrored silence.
	mr.MuteDeleted(ctx, stored)
	if fam.count() != 0 {
		t.Fatalf("MuteDeleted did not remove the silence, count=%d", fam.count())
	}

	// nil SilenceID → no-op (no panic); 404 from AM (silence already gone) → no panic.
	mr.MuteDeleted(ctx, store.AlertMute{ID: mute.ID, Alertname: "X"})            // nil SilenceID
	mr.MuteDeleted(ctx, store.AlertMute{ID: mute.ID, SilenceID: strptr("nope")}) // 404 path
}

func TestReconcileMigratesNullSilenceID(t *testing.T) {
	st := testdb.New(t)
	fam, url := newFakeAM(t)
	mr := New(st, url, testLog())
	ctx := context.Background()

	// A v0 mute with no silence yet.
	mute, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{Alertname: "NodeDown", CreatedBy: "admin"})
	if muteByID(t, st, mute.ID).SilenceID != nil {
		t.Fatal("precondition: silence_id should be nil")
	}
	mr.reconcileOnce(ctx)

	got := muteByID(t, st, mute.ID)
	if got.SilenceID == nil {
		t.Fatal("reconcile did not create+store a silence for the v0 mute")
	}
	if _, ok := fam.get(*got.SilenceID); !ok {
		t.Fatal("reconcile silence not on AM")
	}
}

func TestReconcileReissuesExpiredSilence(t *testing.T) {
	st := testdb.New(t)
	fam, url := newFakeAM(t)
	mr := New(st, url, testLog())
	ctx := context.Background()

	mute, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{Alertname: "NodeDown", CreatedBy: "admin"})
	mr.MuteUpserted(ctx, mute)
	old := *muteByID(t, st, mute.ID).SilenceID

	// AM expires the silence out from under us.
	fam.expire(old)
	mr.reconcileOnce(ctx)

	got := *muteByID(t, st, mute.ID).SilenceID
	if got == old {
		t.Fatal("reconcile should re-issue a fresh silence for an expired one")
	}
	sil, ok := fam.get(got)
	if !ok || sil.State != "active" {
		t.Fatalf("re-issued silence should be active: %+v ok=%v", sil, ok)
	}
}

func TestReconcileFixesEndsAtDrift(t *testing.T) {
	st := testdb.New(t)
	fam, url := newFakeAM(t)
	mr := New(st, url, testLog())
	ctx := context.Background()

	exp1 := time.Now().Add(1 * time.Hour).UTC()
	mute, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
		Alertname: "NodeDown", ExpiresAt: &exp1, CreatedBy: "admin",
	})
	mr.MuteUpserted(ctx, mute)
	sid := *muteByID(t, st, mute.ID).SilenceID

	// Move the mute's expiry far away (in-place upsert keeps silence_id).
	exp2 := time.Now().Add(6 * time.Hour).UTC()
	if _, err := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{
		Alertname: "NodeDown", ExpiresAt: &exp2, CreatedBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	mr.reconcileOnce(ctx)

	sil, ok := fam.get(sid)
	if !ok {
		// in-place POST may re-mint id; re-read from the mute.
		sil, ok = fam.get(*muteByID(t, st, mute.ID).SilenceID)
	}
	if !ok {
		t.Fatal("silence missing after drift reconcile")
	}
	if sil.EndsAt != exp2.Format(time.RFC3339) {
		t.Fatalf("endsAt after drift fix = %q, want %q", sil.EndsAt, exp2.Format(time.RFC3339))
	}
}

func TestReconcileOrphanSweep(t *testing.T) {
	st := testdb.New(t)
	fam, url := newFakeAM(t)
	mr := New(st, url, testLog())
	ctx := context.Background()

	stale := time.Now().Add(-10 * time.Minute)

	// (a) birdman orphan, old → deleted.
	orphan := fam.seed(&fakeSilence{CreatedBy: "birdman:admin", UpdatedAt: stale,
		Matchers: []matcher{{Name: "alertname", Value: "Ghost", IsEqual: true}}})
	// (b) operator silence, old, no mute → kept (no birdman prefix).
	operator := fam.seed(&fakeSilence{CreatedBy: "ops@corp", UpdatedAt: stale,
		Matchers: []matcher{{Name: "alertname", Value: "Manual", IsEqual: true}}})
	// (c) birdman orphan but fresh (within grace) → kept.
	fresh := fam.seed(&fakeSilence{CreatedBy: "birdman:admin", UpdatedAt: time.Now(),
		Matchers: []matcher{{Name: "alertname", Value: "Fresh", IsEqual: true}}})

	mr.reconcileOnce(ctx)

	if _, ok := fam.get(orphan); ok {
		t.Fatal("old birdman orphan should have been swept")
	}
	if _, ok := fam.get(operator); !ok {
		t.Fatal("operator silence must never be swept")
	}
	if _, ok := fam.get(fresh); !ok {
		t.Fatal("fresh birdman silence (within grace) must not be swept")
	}
}

func TestReconcileOrphanKeepsBackedSilence(t *testing.T) {
	st := testdb.New(t)
	fam, url := newFakeAM(t)
	mr := New(st, url, testLog())
	ctx := context.Background()

	mute, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{Alertname: "NodeDown", CreatedBy: "admin"})
	mr.MuteUpserted(ctx, mute)
	sid := *muteByID(t, st, mute.ID).SilenceID
	// Age it past the grace window so only "backed by a mute" keeps it alive.
	fam.setUpdatedAt(sid, time.Now().Add(-10*time.Minute))

	mr.reconcileOnce(ctx)

	if _, ok := fam.get(sid); !ok {
		t.Fatal("a silence backed by an active mute must not be swept")
	}
}

func TestDisabledMirrorIsNoOp(t *testing.T) {
	st := testdb.New(t)
	mr := New(st, "", testLog())
	ctx := context.Background()

	mute, _ := st.UpsertAlertMute(ctx, store.CreateAlertMuteParams{Alertname: "NodeDown", CreatedBy: "admin"})
	mr.MuteUpserted(ctx, mute)
	mr.MuteDeleted(ctx, mute)

	// Run returns immediately (no ticker) even with a live, never-cancelled ctx.
	done := make(chan struct{})
	go func() { mr.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run on a disabled mirror must return immediately")
	}
	if muteByID(t, st, mute.ID).SilenceID != nil {
		t.Fatal("disabled mirror must not touch silence_id")
	}
}
