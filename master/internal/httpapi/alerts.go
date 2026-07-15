package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// Alerts endpoints for the panel П2 Alerts screen (docs/specs/panel.md §3,
// ops.md §1). master proxies the vmalert stack on the box: rules and live
// firing state come from the vmalert HTTP API, history from the alert sink log
// (/var/log/birdman/alerts.log). Alert rules carry a `description` annotation
// (EN, canonical) plus an optional `description_ru`; master passes both through
// verbatim and the panel picks the text by locale, falling back to
// `description` when `description_ru` is absent (self-host operators need not
// write bilingual rules).
//
// Mutes (POST/GET/DELETE /v1/alerts/mutes) are master state, not a vmalert
// silence: a mute is an annotation master stores and reflects as muted:true on
// matching alerts in /v1/alerts/{active,history} so the panel can dim/hide them
// and an audit trail exists. It does NOT stop vmalert firing or the Discord/log
// delivery — a real silence needs the alertmanager silence API (ops.md §1 TODO).
// Mute state lives in store/alerts.go (table alert_mutes).

const alertsUpstreamTimeout = 15 * time.Second

// --- vmalert-backed reads (rules, active) ---

// vmRulesResponse is the vmalert /api/v1/rules payload (Prometheus-compatible).
type vmRulesResponse struct {
	Data struct {
		Groups []struct {
			Name  string `json:"name"`
			File  string `json:"file"`
			Rules []struct {
				Name        string            `json:"name"`
				Query       string            `json:"query"`
				Duration    float64           `json:"duration"` // "for", seconds
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"` // inactive|pending|firing
				Type        string            `json:"type"`  // alerting|recording
			} `json:"rules"`
		} `json:"groups"`
	} `json:"data"`
}

type alertRule struct {
	Name          string `json:"name"`
	Group         string `json:"group"`
	Severity      string `json:"severity"`
	Expr          string `json:"expr"`
	For           string `json:"for"`
	State         string `json:"state"`
	Description   string `json:"description"`              // EN, canonical
	DescriptionRu string `json:"description_ru,omitempty"` // optional RU; panel falls back to Description
}

func (s *Server) handleAlertRules(w http.ResponseWriter, r *http.Request) {
	if s.vmalertURL == "" {
		writeError(w, http.StatusServiceUnavailable, "alerts_unconfigured",
			"vmalert_url is not set on this master")
		return
	}
	var vr vmRulesResponse
	if err := s.vmalertGet(r.Context(), "/api/v1/rules", &vr); err != nil {
		writeError(w, http.StatusBadGateway, "upstream", err.Error())
		return
	}
	rules := make([]alertRule, 0)
	for _, g := range vr.Data.Groups {
		for _, ru := range g.Rules {
			if ru.Type == "recording" { // alerting rules only
				continue
			}
			rules = append(rules, alertRule{
				Name:          ru.Name,
				Group:         g.Name,
				Severity:      ru.Labels["severity"],
				Expr:          ru.Query,
				For:           durationSeconds(ru.Duration),
				State:         ru.State,
				Description:   annotation(ru.Annotations),
				DescriptionRu: ru.Annotations["description_ru"],
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// vmAlertsResponse is the vmalert /api/v1/alerts payload.
type vmAlertsResponse struct {
	Data struct {
		Alerts []struct {
			State       string            `json:"state"` // pending|firing
			Name        string            `json:"name"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
			ActiveAt    time.Time         `json:"activeAt"`
			Value       string            `json:"value"`
		} `json:"alerts"`
	} `json:"data"`
}

type activeAlert struct {
	Name          string    `json:"name"`
	Severity      string    `json:"severity"`
	Region        string    `json:"region"`
	Node          string    `json:"node"`
	State         string    `json:"state"`
	ActiveAt      time.Time `json:"active_at"`
	Value         string    `json:"value"`
	Description   string    `json:"description"`              // EN, canonical
	DescriptionRu string    `json:"description_ru,omitempty"` // optional RU; panel falls back to Description
	Muted         bool      `json:"muted"`                    // an active master mute covers this alertname+region
}

func (s *Server) handleAlertsActive(w http.ResponseWriter, r *http.Request) {
	if s.vmalertURL == "" {
		writeError(w, http.StatusServiceUnavailable, "alerts_unconfigured",
			"vmalert_url is not set on this master")
		return
	}
	var va vmAlertsResponse
	if err := s.vmalertGet(r.Context(), "/api/v1/alerts", &va); err != nil {
		writeError(w, http.StatusBadGateway, "upstream", err.Error())
		return
	}
	out := make([]activeAlert, 0)
	for _, a := range va.Data.Alerts {
		if a.State != "firing" { // active = currently firing
			continue
		}
		name := a.Name
		if name == "" {
			name = a.Labels["alertname"]
		}
		out = append(out, activeAlert{
			Name:          name,
			Severity:      a.Labels["severity"],
			Region:        a.Labels["region"],
			Node:          alertNode(a.Labels),
			State:         a.State,
			ActiveAt:      a.ActiveAt,
			Value:         a.Value,
			Description:   annotation(a.Annotations),
			DescriptionRu: a.Annotations["description_ru"],
		})
	}
	mutes, err := s.st.ListAlertMutes(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for i := range out {
		out[i].Muted = anyMuteMatches(mutes, out[i].Name, out[i].Region)
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out})
}

func (s *Server) vmalertGet(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, alertsUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimURL(s.vmalertURL)+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("vmalert returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func trimURL(u string) string {
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
}

func durationSeconds(secs float64) string {
	if secs <= 0 {
		return "0s"
	}
	return time.Duration(secs * float64(time.Second)).String()
}

func annotation(a map[string]string) string {
	if d := a["description"]; d != "" {
		return d
	}
	return a["summary"]
}

func alertNode(labels map[string]string) string {
	for _, k := range []string{"node", "instance", "hostname", "host"} {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// --- alert history (log sink) ---

// alertEvent is one normalized firing/resolution from the alert log.
type alertEvent struct {
	Name          string `json:"name"`
	Severity      string `json:"severity"`
	Region        string `json:"region"`
	Node          string `json:"node"`
	StartsAt      string `json:"startsAt"`
	EndsAt        string `json:"endsAt"`
	Description   string `json:"description"`              // EN, canonical
	DescriptionRu string `json:"description_ru,omitempty"` // optional RU; panel falls back to Description
	Active        bool   `json:"active"`
	ReceivedAt    string `json:"received_at,omitempty"`
	Muted         bool   `json:"muted"` // an active master mute covers this alertname+region
}

// amAlert is one alert in the alertmanager-v2 webhook shape written to the log.
type amAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt"`
}

// webhookLine is one JSON line: a delivery carrying a batch of alerts.
type webhookLine struct {
	ReceivedAt string    `json:"received_at"`
	Alerts     []amAlert `json:"alerts"`
}

func (s *Server) handleAlertHistory(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > 1000 {
		limit = 1000
	}
	data, err := os.ReadFile(s.alertsLogPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || s.alertsLogPath == "" {
			// No sink yet is a normal state, not an error.
			writeJSON(w, http.StatusOK, map[string]any{"alerts": []alertEvent{}})
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	events := parseAlertsLog(data, time.Now(), limit)
	mutes, err := s.st.ListAlertMutes(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for i := range events {
		events[i].Muted = anyMuteMatches(mutes, events[i].Name, events[i].Region)
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": events})
}

// parseAlertsLog parses newline-delimited JSON deliveries into a normalized,
// newest-first list capped at limit. It is defensive: unparseable lines are
// skipped, and both the {received_at, alerts:[…]} batch shape and a bare
// single-alert object are accepted. Never returns nil (→ [] in JSON).
func parseAlertsLog(data []byte, now time.Time, limit int) []alertEvent {
	out := []alertEvent{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	// Alert lines can be long (many labels/annotations) — lift the token cap.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var wl webhookLine
		if err := json.Unmarshal(line, &wl); err == nil && len(wl.Alerts) > 0 {
			for _, a := range wl.Alerts {
				out = append(out, normalizeAlert(a, wl.ReceivedAt, now))
			}
			continue
		}
		// Fallback: a bare single-alert object per line.
		var a amAlert
		if err := json.Unmarshal(line, &a); err == nil && len(a.Labels) > 0 {
			out = append(out, normalizeAlert(a, wl.ReceivedAt, now))
		}
	}
	// Newest first by delivery time (received_at); a repeated-firing alert keeps
	// one startsAt but a fresh received_at each delivery, so received_at is the
	// right "latest" key. Fall back to startsAt when a bare line has no
	// received_at. Times are parsed so mixed RFC3339 forms compare correctly.
	sort.SliceStable(out, func(i, j int) bool {
		return alertSortTime(out[i]).After(alertSortTime(out[j]))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func normalizeAlert(a amAlert, receivedAt string, now time.Time) alertEvent {
	return alertEvent{
		Name:          a.Labels["alertname"],
		Severity:      a.Labels["severity"],
		Region:        a.Labels["region"],
		Node:          alertNode(a.Labels),
		StartsAt:      a.StartsAt,
		EndsAt:        a.EndsAt,
		Description:   annotation(a.Annotations),
		DescriptionRu: a.Annotations["description_ru"],
		Active:        alertActive(a.Status, a.EndsAt, now),
		ReceivedAt:    receivedAt,
	}
}

// alertActive reports whether an alert is still firing, judged by endsAt (per
// spec): empty/zero endsAt or an endsAt in the future ⇒ active; a resolved
// status or a past endsAt ⇒ inactive.
func alertActive(status, endsAt string, now time.Time) bool {
	if status == "resolved" {
		return false
	}
	if endsAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, endsAt)
	if err != nil {
		return status == "firing" // unparseable endsAt: trust only an explicit firing
	}
	if t.Year() <= 1 { // alertmanager zero time "0001-01-01T00:00:00Z" = no end
		return true
	}
	return t.After(now)
}

// alertSortTime is an alert's ordering instant: received_at (delivery), else
// startsAt. Unparseable/absent → zero time (sorts last).
func alertSortTime(a alertEvent) time.Time {
	for _, s := range []string{a.ReceivedAt, a.StartsAt} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// anyMuteMatches reports whether any of the (active) mutes covers the alert.
func anyMuteMatches(mutes []store.AlertMute, name, region string) bool {
	for _, m := range mutes {
		if m.Matches(name, region) {
			return true
		}
	}
	return false
}

// --- mute rules (master-level suppression annotations) ---
//
// Contract (docs/specs/master.md §6): POST needs admin, GET needs readonly,
// DELETE needs admin. A mute is master state; it makes matching alerts report
// muted:true (handleAlertsActive/handleAlertHistory) but does not silence
// vmalert/Discord — that is the alertmanager silence API (ops.md §1 TODO).

type createMuteRequest struct {
	Alertname string  `json:"alertname"`
	Region    *string `json:"region"`     // absent/empty → all regions
	Note      string  `json:"note"`       // optional, defaults to ""
	ExpiresAt *string `json:"expires_at"` // absent/empty → never; else RFC3339 in the future
}

// handleCreateAlertMute is POST /v1/alerts/mutes (admin). It is an idempotent
// upsert: muting an alertname+region that already has an active mute updates
// that mute's note/expires_at in place (store.UpsertAlertMute) rather than
// stacking duplicates — so a repeat POST doubles as "extend/edit" and still
// returns 201 with the resulting mute.
func (s *Server) handleCreateAlertMute(w http.ResponseWriter, r *http.Request) {
	var req createMuteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Alertname) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "alertname is required")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && strings.TrimSpace(*req.ExpiresAt) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*req.ExpiresAt))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "expires_at must be an RFC3339 timestamp")
			return
		}
		if !t.After(time.Now()) {
			writeError(w, http.StatusBadRequest, "bad_request", "expires_at must be in the future")
			return
		}
		expiresAt = &t
	}
	createdBy := ""
	if key, ok := keyFromContext(r.Context()); ok {
		createdBy = key.Name
	}
	mute, err := s.st.UpsertAlertMute(r.Context(), store.CreateAlertMuteParams{
		Alertname: req.Alertname,
		Region:    req.Region,
		Note:      req.Note,
		ExpiresAt: expiresAt,
		CreatedBy: createdBy,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Audit — payload carries no secrets (region/expires_at may be null).
	if err := s.st.InsertEvent(r.Context(), store.EventAlertMuted, store.EventRef{}, map[string]any{
		"mute_id": mute.ID, "alertname": mute.Alertname, "region": mute.Region,
		"expires_at": mute.ExpiresAt, "created_by": mute.CreatedBy,
	}); err != nil {
		s.log.Error("alert mute: create event write failed", "mute_id", mute.ID, "err", err)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"mute": mute})
}

// handleListAlertMutes is GET /v1/alerts/mutes (readonly): active mutes
// newest-first; ?all=1 also returns expired ones.
func (s *Server) handleListAlertMutes(w http.ResponseWriter, r *http.Request) {
	mutes, err := s.st.ListAlertMutes(r.Context(), r.URL.Query().Get("all") == "1")
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mutes": emptyNotNull(mutes)})
}

// handleDeleteAlertMute is DELETE /v1/alerts/mutes/{id} (admin). 204 on a real
// removal (emits alert_unmuted), 404 for an unknown/already-removed id, 400 for
// a non-uuid id. DELETE is idempotent in effect (end state: not muted); the
// 404 lets the caller tell a real unmute from a no-op and avoids a duplicate
// audit event.
func (s *Server) handleDeleteAlertMute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "mute id must be a uuid")
		return
	}
	mute, deleted, err := s.st.DeleteAlertMute(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "not_found", "no such mute")
		return
	}
	if err := s.st.InsertEvent(r.Context(), store.EventAlertUnmuted, store.EventRef{}, map[string]any{
		"mute_id": mute.ID, "alertname": mute.Alertname, "region": mute.Region,
	}); err != nil {
		s.log.Error("alert mute: delete event write failed", "mute_id", mute.ID, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
