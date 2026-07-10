// Package httpapi is the public REST API of birdman-master
// (docs/specs/master.md §6, v0 subset). The panel and CLI are plain clients
// of this API — no private side doors (ADR-9).
//
// Node drain/undrain, the server logs proxy, agent self-upgrade and the
// read-only metrics proxy (итерация 4) live in ops.go.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/panelui"
	"github.com/ufna/birdman/master/internal/store"
)

// mmRateLimit: 5 rps per player_id on matchmaking endpoints
// (docs/specs/protocol.md §3).
const mmRateLimit = 5

type Server struct {
	st            *store.Store
	m             *metrics.Metrics
	mm            *matchmaker.Matchmaker
	dep           *deploy.Manager
	sender        CommandSender        // agent command dispatch (agentlink.Hub)
	logs          *agentlink.LogRouter // TailLogs chunk router
	vmURL         string               // VictoriaMetrics base URL for the metrics proxy
	vlURL         string               // VictoriaLogs base URL for the logs query proxy
	vmalertURL    string               // vmalert base URL for the alerts endpoints
	alertsLogPath string               // alert sink log for GET /v1/alerts/history
	mmLimit       *rateLimiter
	auth          *authenticator
	log           *slog.Logger
	mux           *http.ServeMux

	// onRegistriesChanged fires after a successful POST/PATCH/DELETE
	// /v1/registries (registries.go). nil-safe — an unset hook is simply not
	// called. T3 wires it to broadcast a fresh SetRegistries snapshot to
	// connected agents (docs/superpowers/specs/2026-07-09-registries-design.md §2).
	onRegistriesChanged func(context.Context)
}

func New(st *store.Store, m *metrics.Metrics, mm *matchmaker.Matchmaker, dep *deploy.Manager, sender CommandSender, logs *agentlink.LogRouter, vmURL, vlURL string, log *slog.Logger) *Server {
	s := &Server{
		st: st, m: m, mm: mm, dep: dep, sender: sender, logs: logs, vmURL: vmURL, vlURL: vlURL,
		mmLimit: newRateLimiter(mmRateLimit, mmRateLimit),
		auth:    newAuthenticator(st), log: log, mux: http.NewServeMux(),
	}

	s.mux.HandleFunc("GET /healthz", s.handleHealthz) // no auth by design
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))

	s.mux.HandleFunc("POST /v1/nodes", s.requireScope(ScopeAdmin, s.handleCreateNode))
	s.mux.HandleFunc("GET /v1/nodes", s.requireScope(ScopeReadonly, s.handleListNodes))
	s.mux.HandleFunc("POST /v1/nodes/{id}/drain", s.requireScope(ScopeAdmin, s.handleDrainNode))
	s.mux.HandleFunc("POST /v1/nodes/{id}/undrain", s.requireScope(ScopeAdmin, s.handleUndrainNode))
	// Public internal-CA cert bundle (mTLS agentlink v1, ca.go) — ansible
	// delivers it to nodes; cert-only, the CA key cannot leak (design §5).
	s.mux.HandleFunc("GET /v1/ca", s.requireScope(ScopeReadonly, s.handleGetCA))
	s.mux.HandleFunc("GET /v1/servers", s.requireScope(ScopeReadonly, s.handleListServers))
	s.mux.HandleFunc("GET /v1/servers/{id}/logs", s.requireScope(ScopeReadonly, s.handleServerLogs))
	s.mux.HandleFunc("POST /v1/agent-upgrade", s.requireScope(ScopeAdmin, s.handleAgentUpgrade))
	s.mux.HandleFunc("GET /v1/metrics/query", s.requireScope(ScopeReadonly, s.handleMetricsQuery))
	s.mux.HandleFunc("GET /v1/metrics/query_range", s.requireScope(ScopeReadonly, s.handleMetricsQueryRange))
	s.mux.HandleFunc("GET /v1/logs/query", s.requireScope(ScopeReadonly, s.handleLogsQuery))
	s.mux.HandleFunc("POST /v1/versions", s.requireScope(ScopeDeploy, s.handleCreateVersion))
	s.mux.HandleFunc("GET /v1/versions", s.requireScope(ScopeReadonly, s.handleListVersions))
	s.mux.HandleFunc("POST /v1/deploy", s.requireScope(ScopeDeploy, s.handleDeploy))
	s.mux.HandleFunc("POST /v1/rollback", s.requireScope(ScopeDeploy, s.handleRollback))
	s.mux.HandleFunc("PUT /v1/fleets/{region}", s.requireScope(ScopeAdmin, s.handleUpsertFleet))
	s.mux.HandleFunc("PUT /v1/projects/{slug}", s.requireScope(ScopeAdmin, s.handleUpsertProject))
	s.mux.HandleFunc("GET /v1/events", s.requireScope(ScopeReadonly, s.handleListEvents))
	s.mux.HandleFunc("GET /v1/events/stream", s.requireScope(ScopeReadonly, s.handleEventsStream))
	s.mux.HandleFunc("GET /v1/matches", s.requireScope(ScopeReadonly, s.handleListMatches))
	s.mux.HandleFunc("GET /v1/matches/{id}", s.requireScope(ScopeReadonly, s.handleGetMatch))
	s.mux.HandleFunc("POST /v1/allocate", s.requireScope(ScopeAllocate, s.handleAllocate))

	// API-key management (П2 Access, apikeys.go); stats aggregates (П2
	// Statistics/Cost-view, stats.go); alerts (П2 Alerts, alerts.go).
	s.mux.HandleFunc("GET /v1/apikeys", s.requireScope(ScopeAdmin, s.handleListAPIKeys))
	s.mux.HandleFunc("POST /v1/apikeys", s.requireScope(ScopeAdmin, s.handleCreateAPIKey))
	s.mux.HandleFunc("DELETE /v1/apikeys/{id}", s.requireScope(ScopeAdmin, s.handleRevokeAPIKey))
	// Private registry credentials (П4 Admin/Реестры, registries.go) — admin
	// scope on every route, including the list read (secret-adjacent).
	s.mux.HandleFunc("GET /v1/registries", s.requireScope(ScopeAdmin, s.handleListRegistries))
	s.mux.HandleFunc("POST /v1/registries", s.requireScope(ScopeAdmin, s.handleCreateRegistry))
	s.mux.HandleFunc("PATCH /v1/registries/{id}", s.requireScope(ScopeAdmin, s.handlePatchRegistry))
	s.mux.HandleFunc("DELETE /v1/registries/{id}", s.requireScope(ScopeAdmin, s.handleDeleteRegistry))
	s.mux.HandleFunc("GET /v1/stats/overview", s.requireScope(ScopeReadonly, s.handleStatsOverview))
	s.mux.HandleFunc("GET /v1/stats/cost", s.requireScope(ScopeReadonly, s.handleStatsCost))
	s.mux.HandleFunc("GET /v1/alerts/rules", s.requireScope(ScopeReadonly, s.handleAlertRules))
	s.mux.HandleFunc("GET /v1/alerts/history", s.requireScope(ScopeReadonly, s.handleAlertHistory))
	s.mux.HandleFunc("GET /v1/alerts/active", s.requireScope(ScopeReadonly, s.handleAlertsActive))
	s.mux.HandleFunc("POST /v1/alerts/mutes", s.requireScope(ScopeAdmin, s.handleCreateAlertMute))
	s.mux.HandleFunc("GET /v1/alerts/mutes", s.requireScope(ScopeReadonly, s.handleListAlertMutes))
	s.mux.HandleFunc("DELETE /v1/alerts/mutes/{id}", s.requireScope(ScopeAdmin, s.handleDeleteAlertMute))

	s.mux.HandleFunc("POST /v1/matchmaking/tickets", s.requireScope(ScopeMatchmaking, s.handleCreateTicket))
	s.mux.HandleFunc("GET /v1/matchmaking/tickets/{id}", s.requireScope(ScopeMatchmaking, s.handleGetTicket))
	s.mux.HandleFunc("DELETE /v1/matchmaking/tickets/{id}", s.requireScope(ScopeMatchmaking, s.handleCancelTicket))
	s.mux.HandleFunc("GET /v1/qos", s.handleQoS) // public by design (master.md §6)

	// Browser sessions for the panel (session.go); auth is inside the
	// handlers (login carries the key in the body, logout the cookie).
	s.mux.HandleFunc("POST /v1/session", s.handleCreateSession)
	s.mux.HandleFunc("GET /v1/session", s.handleGetSession)
	s.mux.HandleFunc("DELETE /v1/session", s.handleDeleteSession)

	// Embedded panel SPA: `/`, `/assets/*` and SPA-fallback routes
	// (panelui). Registered last — "/" catches everything unrouted.
	s.mux.Handle("/", panelui.Handler())

	return s
}

// WithAlertsSources wires the vmalert base URL and the alert-sink log path for
// the П2 alerts endpoints (config.Alerts; alerts.go). Kept a setter rather than
// a New parameter so the existing New signature — and its call sites — stay
// untouched; the alert handlers read these at request time. Returns s for
// chaining. Empty vmalert URL → the rules/active endpoints answer 503; a
// missing log file → history answers an empty list.
func (s *Server) WithAlertsSources(vmalertURL, alertsLogPath string) *Server {
	s.vmalertURL = vmalertURL
	s.alertsLogPath = alertsLogPath
	return s
}

// WithRegistriesHook wires a callback invoked after a successful registries
// change (POST/DELETE /v1/registries) — T3 uses this to broadcast a fresh
// SetRegistries snapshot to connected agents
// (docs/superpowers/specs/2026-07-09-registries-design.md §2). Kept a setter
// rather than a New parameter, like WithAlertsSources, so the existing New
// signature and its call sites stay untouched. Nil-safe: an unset hook is
// simply not called. Returns s for chaining.
func (s *Server) WithRegistriesHook(fn func(context.Context)) *Server {
	s.onRegistriesChanged = fn
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]string{"error": code, "detail": detail})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	return true
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.st.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "db": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// storeError maps store sentinel errors to HTTP responses.
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
