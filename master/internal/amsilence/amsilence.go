// Package amsilence mirrors master's alert mutes into real alertmanager
// silences, best-effort (ops.md §1, tracker #245). alert_mutes is the source of
// truth; a silence is secondary and always re-derivable from its mute. Every
// mirror call swallows its errors into the log — an unreachable alertmanager
// must NEVER break a mute/unmute or crash master (self-host without a monitoring
// stack is a first-class scenario). When AM is down a mute simply keeps its v0
// annotation semantics (muted:true, no real suppression) until AM returns and
// the reconcile loop repairs the drift.
//
// Mute → silence mapping:
//   - matchers: always {alertname == m.Alertname}; when m.Region != nil also
//     {region == *m.Region}. A nil region omits the region matcher, so the
//     silence covers every region — matching the mute's own semantics.
//   - startsAt = now; endsAt = *m.ExpiresAt, or now+indefiniteHorizon for an
//     open-ended (nil) mute (alertmanager demands a finite endsAt; the reconcile
//     loop re-issues the silence long before that far horizon lapses).
//   - createdBy = createdByPrefix + m.CreatedBy. The prefix is a deliberate
//     marker: the orphan sweep deletes ONLY silences carrying it, so a silence
//     an operator created by hand in AM is never touched.
//   - comment = m.Note, or "birdman mute" when the note is empty.
package amsilence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

const (
	// createdByPrefix marks every silence master creates; the orphan sweep only
	// ever deletes silences with this prefix.
	createdByPrefix = "birdman:"

	// callTimeout bounds every single AM HTTP call — never the client's default
	// infinite timeout, so a hung AM cannot wedge a mirror call or the loop.
	callTimeout = 5 * time.Second

	// reconcileInterval is the reconcile/orphan-sweep period.
	reconcileInterval = 60 * time.Second

	// indefiniteHorizon is the endsAt handed to an open-ended mute: alertmanager
	// requires a finite endsAt, and a decade out reads as "forever". The
	// reconcile loop re-issues the silence long before it can lapse.
	indefiniteHorizon = 10 * 365 * 24 * time.Hour

	// reissueLead: an open-ended silence is re-issued only once its endsAt is
	// nearer than this. The year of slack stops the loop from re-POSTing every
	// pass as the far horizon slowly melts toward now.
	reissueLead = 365 * 24 * time.Hour

	// driftTolerance: an endsAt within this of the mute's ExpiresAt is not drift
	// worth a re-POST (RFC3339 second-truncation alone can differ by <1s).
	driftTolerance = time.Second

	// orphanGrace spares a birdman silence younger than this from the orphan
	// sweep, closing the race where the silence exists but its id has not yet
	// been written back onto the mute row.
	orphanGrace = 2 * time.Minute
)

// Mirror mirrors mute changes into alertmanager silences and reconciles drift.
// A zero baseURL disables it entirely: every method is an instant no-op and Run
// returns at once (self-host without a monitoring stack).
type Mirror struct {
	st   *store.Store
	base string // trimmed base URL; "" → disabled
	http *http.Client
	log  *slog.Logger

	// degraded latches the "alertmanager unreachable" warn so a permanently down
	// AM does not spam the log every pass. Read/written only by the reconcile
	// goroutine (Run → reconcileOnce), sequentially, so no lock is needed.
	degraded bool
}

// New builds a Mirror. baseURL "" disables mirroring (all methods no-op, Run
// returns immediately). The HTTP client carries its own per-call timeout.
func New(st *store.Store, baseURL string, log *slog.Logger) *Mirror {
	return &Mirror{
		st:   st,
		base: trimURL(baseURL),
		http: &http.Client{Timeout: callTimeout},
		log:  log,
	}
}

func (m *Mirror) enabled() bool { return m.base != "" }

// MuteUpserted mirrors a created/updated mute into a silence, best-effort. It
// sends the mute's stored silence id (so AM updates in place); on any error with
// an id it retries once WITHOUT the id (a fresh silence), since the stored id
// may point at an expired/removed silence. AM may return a NEW id either way, so
// the returned id is persisted when it differs. Every failure is logged and
// swallowed — the caller's 201/204 never depends on it.
func (m *Mirror) MuteUpserted(ctx context.Context, mute store.AlertMute) {
	if !m.enabled() {
		return
	}
	existing := deref(mute.SilenceID)
	id, err := m.postSilence(ctx, mute, existing)
	if err != nil && existing != "" {
		id, err = m.postSilence(ctx, mute, "")
	}
	if err != nil {
		m.log.Warn("silence mirror: upsert failed — mute keeps v0 semantics",
			"mute_id", mute.ID, "alertname", mute.Alertname, "err", err)
		return
	}
	m.persistID(ctx, mute, id)
}

// MuteDeleted removes a mute's mirrored silence, best-effort. A nil silence id
// (never mirrored) or a disabled mirror is a no-op; a delete error is logged and
// left for the reconcile orphan sweep to clean up.
func (m *Mirror) MuteDeleted(ctx context.Context, mute store.AlertMute) {
	if !m.enabled() || mute.SilenceID == nil {
		return
	}
	if err := m.deleteSilence(ctx, *mute.SilenceID); err != nil {
		m.log.Warn("silence mirror: delete failed — reconcile sweep will retry",
			"mute_id", mute.ID, "silence_id", *mute.SilenceID, "err", err)
	}
}

// Run reconciles mutes → silences immediately, then every reconcileInterval,
// until ctx is done. Disabled mirror returns at once.
func (m *Mirror) Run(ctx context.Context) {
	if !m.enabled() {
		m.log.Info("silence mirroring disabled (no alertmanager_url) — mutes keep v0 semantics")
		return
	}
	m.reconcileOnce(ctx)
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce migrates v0 mutes (silence_id NULL), re-issues lost/expired
// silences, repairs endsAt drift, and sweeps orphan silences left behind by a
// delete that hit a down AM. The FIRST unreachable-AM error latches the degraded
// warn and aborts the pass (every later call would fail too); a completed pass
// clears it. Per-mute errors against a live AM are warned once per pass.
func (m *Mirror) reconcileOnce(ctx context.Context) {
	mutes, err := m.st.ListAlertMutes(ctx, false)
	if err != nil {
		m.log.Warn("silence mirror: reconcile list mutes failed", "err", err)
		return
	}
	for _, mute := range mutes {
		if err := m.reconcileMute(ctx, mute); err != nil {
			if isUnreachable(err) {
				m.enterDegraded(err)
				return
			}
			m.log.Warn("silence mirror: reconcile mute failed",
				"mute_id", mute.ID, "alertname", mute.Alertname, "err", err)
		}
	}
	if err := m.orphanSweep(ctx); err != nil {
		if isUnreachable(err) {
			m.enterDegraded(err)
			return
		}
		m.log.Warn("silence mirror: orphan sweep failed", "err", err)
	}
	m.leaveDegraded()
}

// reconcileMute ensures one active mute is backed by a live, up-to-date silence.
func (m *Mirror) reconcileMute(ctx context.Context, mute store.AlertMute) error {
	if mute.SilenceID == nil { // never mirrored (v0 mute or AM was down) → create
		return m.createAndStore(ctx, mute)
	}
	view, ok, err := m.getSilence(ctx, *mute.SilenceID)
	if err != nil {
		return err
	}
	if !ok || view.State != "active" { // lost or expired/pending → re-create fresh
		return m.createAndStore(ctx, mute)
	}
	if m.driftsEndsAt(mute, view) {
		return m.reissueInPlace(ctx, mute) // POST with id → update in place
	}
	return nil
}

// driftsEndsAt reports whether the silence's endsAt no longer matches the mute.
// For a bounded mute: |endsAt − ExpiresAt| > driftTolerance. For an open-ended
// mute: endsAt has melted to within reissueLead of now (else the far horizon is
// still "forever" and re-POSTing every pass would be churn).
func (m *Mirror) driftsEndsAt(mute store.AlertMute, view silenceView) bool {
	if mute.ExpiresAt != nil {
		d := view.EndsAt.Sub(*mute.ExpiresAt)
		if d < 0 {
			d = -d
		}
		return d > driftTolerance
	}
	return view.EndsAt.Before(time.Now().Add(reissueLead))
}

// orphanSweep deletes birdman silences with no backing mute (a delete that hit a
// down AM leaves the silence alive). Mutes are re-read AFTER listing AM, so a
// mute POSTed concurrently — its silence already in the AM listing but its id not
// yet in an earlier snapshot — is seen here and spared. Only active,
// birdman-prefixed silences older than orphanGrace with no matching mute are
// deleted; operator silences (no prefix) are never touched.
func (m *Mirror) orphanSweep(ctx context.Context) error {
	silences, err := m.listSilences(ctx)
	if err != nil {
		return err
	}
	mutes, err := m.st.ListAlertMutes(ctx, false)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(mutes))
	for _, mute := range mutes {
		if mute.SilenceID != nil {
			known[*mute.SilenceID] = true
		}
	}
	now := time.Now()
	for _, s := range silences {
		switch {
		case s.State != "active":
		case !strings.HasPrefix(s.CreatedBy, createdByPrefix): // operator-created
		case now.Sub(s.UpdatedAt) < orphanGrace: // too fresh — id may not be stored yet
		case known[s.ID]:
		default:
			if err := m.deleteSilence(ctx, s.ID); err != nil {
				if isUnreachable(err) {
					return err
				}
				m.log.Warn("silence mirror: orphan delete failed", "silence_id", s.ID, "err", err)
				continue
			}
			m.log.Info("silence mirror: removed orphan silence", "silence_id", s.ID)
		}
	}
	return nil
}

// createAndStore POSTs a fresh silence (no id) and stores the assigned id.
func (m *Mirror) createAndStore(ctx context.Context, mute store.AlertMute) error {
	id, err := m.postSilence(ctx, mute, "")
	if err != nil {
		return err
	}
	m.persistID(ctx, mute, id)
	return nil
}

// reissueInPlace POSTs with the stored id (AM updates it in place) and stores the
// returned id, which AM may re-mint.
func (m *Mirror) reissueInPlace(ctx context.Context, mute store.AlertMute) error {
	id, err := m.postSilence(ctx, mute, deref(mute.SilenceID))
	if err != nil {
		return err
	}
	m.persistID(ctx, mute, id)
	return nil
}

// persistID stamps a newly assigned silence id on the mute when it changed. A
// store error is logged, not returned — the reconcile loop will re-stamp.
func (m *Mirror) persistID(ctx context.Context, mute store.AlertMute, id string) {
	if id == "" || id == deref(mute.SilenceID) {
		return
	}
	if err := m.st.SetAlertMuteSilenceID(ctx, mute.ID, &id); err != nil {
		m.log.Warn("silence mirror: persist silence id failed", "mute_id", mute.ID, "err", err)
	}
}

func (m *Mirror) enterDegraded(err error) {
	if !m.degraded {
		m.log.Warn("alertmanager unreachable — mutes keep v0 semantics until it returns", "err", err)
		m.degraded = true
	}
}

func (m *Mirror) leaveDegraded() {
	if m.degraded {
		m.log.Info("alertmanager reachable again — silences reconciled")
		m.degraded = false
	}
}

// --- alertmanager api/v2 wire types + calls ---

type matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

type silencePost struct {
	ID        string    `json:"id,omitempty"`
	Matchers  []matcher `json:"matchers"`
	StartsAt  string    `json:"startsAt"`
	EndsAt    string    `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
}

type postResp struct {
	SilenceID string `json:"silenceID"`
}

// apiSilence is the GET shape (single or in the array). silenceView is the
// reconcile loop's parsed projection.
type apiSilence struct {
	ID     string `json:"id"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
	EndsAt    string `json:"endsAt"`
	UpdatedAt string `json:"updatedAt"`
	CreatedBy string `json:"createdBy"`
}

type silenceView struct {
	ID        string
	State     string // active|pending|expired
	EndsAt    time.Time
	UpdatedAt time.Time
	CreatedBy string
}

func (s apiSilence) view() silenceView {
	return silenceView{
		ID:        s.ID,
		State:     s.Status.State,
		EndsAt:    parseTime(s.EndsAt),
		UpdatedAt: parseTime(s.UpdatedAt),
		CreatedBy: s.CreatedBy,
	}
}

// postSilence creates or updates a silence for the mute and returns the id AM
// assigned. existingID (may be "") is sent as the silence id so AM updates in
// place; AM MAY still return a NEW id (an expired silence, changed matchers), so
// callers must persist the returned value.
func (m *Mirror) postSilence(ctx context.Context, mute store.AlertMute, existingID string) (string, error) {
	now := time.Now()
	body, err := json.Marshal(silencePost{
		ID:        existingID,
		Matchers:  muteMatchers(mute),
		StartsAt:  now.UTC().Format(time.RFC3339),
		EndsAt:    muteEndsAt(mute, now).UTC().Format(time.RFC3339),
		CreatedBy: createdByPrefix + mute.CreatedBy,
		Comment:   muteComment(mute),
	})
	if err != nil {
		return "", err
	}
	status, data, err := m.roundtrip(ctx, http.MethodPost, "/api/v2/silences", body)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		return "", fmt.Errorf("alertmanager POST silence: status %d: %s", status, snippet(data))
	}
	var resp postResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	return resp.SilenceID, nil
}

// getSilence fetches one silence. ok=false with a nil error means the silence is
// lost (any non-200 → treat as gone; the caller re-creates). A transport error
// (AM unreachable) is returned wrapped so the reconcile loop can latch degraded.
func (m *Mirror) getSilence(ctx context.Context, id string) (silenceView, bool, error) {
	status, data, err := m.roundtrip(ctx, http.MethodGet, "/api/v2/silence/"+url.PathEscape(id), nil)
	if err != nil {
		return silenceView{}, false, err
	}
	if status/100 != 2 {
		return silenceView{}, false, nil
	}
	var s apiSilence
	if err := json.Unmarshal(data, &s); err != nil {
		return silenceView{}, false, err
	}
	return s.view(), true, nil
}

func (m *Mirror) listSilences(ctx context.Context) ([]silenceView, error) {
	status, data, err := m.roundtrip(ctx, http.MethodGet, "/api/v2/silences", nil)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("alertmanager GET silences: status %d", status)
	}
	var arr []apiSilence
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, err
	}
	out := make([]silenceView, 0, len(arr))
	for _, s := range arr {
		out = append(out, s.view())
	}
	return out, nil
}

// deleteSilence removes a silence. A 404/not-found is success (already
// expired/removed), logged at debug.
func (m *Mirror) deleteSilence(ctx context.Context, id string) error {
	status, _, err := m.roundtrip(ctx, http.MethodDelete, "/api/v2/silence/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		m.log.Debug("silence mirror: delete found no silence (already gone)", "silence_id", id)
		return nil
	}
	if status/100 != 2 {
		return fmt.Errorf("alertmanager DELETE silence %s: status %d", id, status)
	}
	return nil
}

// roundtrip performs one AM call under a per-call timeout and reads the whole
// (tiny) body before the timeout is cancelled. A transport/timeout failure is
// wrapped in unreachableErr so the reconcile loop can tell "AM down" from a
// per-mute non-2xx.
func (m *Mirror) roundtrip(ctx context.Context, method, path string, reqBody []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var rd io.Reader
	if reqBody != nil {
		rd = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return 0, nil, unreachableErr{err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, unreachableErr{err}
	}
	return resp.StatusCode, data, nil
}

// --- mapping + small helpers ---

func muteMatchers(mute store.AlertMute) []matcher {
	ms := []matcher{{Name: "alertname", Value: mute.Alertname, IsRegex: false, IsEqual: true}}
	if mute.Region != nil {
		ms = append(ms, matcher{Name: "region", Value: *mute.Region, IsRegex: false, IsEqual: true})
	}
	return ms
}

func muteEndsAt(mute store.AlertMute, now time.Time) time.Time {
	if mute.ExpiresAt != nil {
		return *mute.ExpiresAt
	}
	return now.Add(indefiniteHorizon)
}

func muteComment(mute store.AlertMute) string {
	if mute.Note != "" {
		return mute.Note
	}
	return "birdman mute"
}

// unreachableErr wraps a transport-level failure (connection refused, timeout,
// DNS): the AM is unreachable. A non-2xx from a responding AM is NOT wrapped, so
// the reconcile loop tells "AM down" (latch degraded, abort the pass) from a
// per-mute error (e.g. a 4xx).
type unreachableErr struct{ err error }

func (e unreachableErr) Error() string { return e.err.Error() }
func (e unreachableErr) Unwrap() error { return e.err }

func isUnreachable(err error) bool {
	var u unreachableErr
	return errors.As(err, &u)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max]
	}
	return s
}

// trimURL strips trailing slashes from the base URL (local copy of the helper in
// httpapi/alerts.go, kept package-local to avoid a cross-package dependency).
func trimURL(u string) string {
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
}
