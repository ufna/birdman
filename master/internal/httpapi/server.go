// Package httpapi is the public REST API of birdman-master
// (docs/specs/master.md §6, v0 subset). The panel and CLI are plain clients
// of this API — no private side doors (ADR-9).
//
// TODO(v0): deploy/rollback, node drain, logs proxy — later iterations (3+).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/panelui"
	"github.com/ufna/birdman/master/internal/store"
)

// mmRateLimit: 5 rps per player_id on matchmaking endpoints
// (docs/specs/protocol.md §3).
const mmRateLimit = 5

type Server struct {
	st      *store.Store
	m       *metrics.Metrics
	mm      *matchmaker.Matchmaker
	mmLimit *rateLimiter
	auth    *authenticator
	log     *slog.Logger
	mux     *http.ServeMux
}

func New(st *store.Store, m *metrics.Metrics, mm *matchmaker.Matchmaker, log *slog.Logger) *Server {
	s := &Server{
		st: st, m: m, mm: mm,
		mmLimit: newRateLimiter(mmRateLimit, mmRateLimit),
		auth:    newAuthenticator(st), log: log, mux: http.NewServeMux(),
	}

	s.mux.HandleFunc("GET /healthz", s.handleHealthz) // no auth by design
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))

	s.mux.HandleFunc("POST /v1/nodes", s.requireScope(ScopeAdmin, s.handleCreateNode))
	s.mux.HandleFunc("GET /v1/nodes", s.requireScope(ScopeReadonly, s.handleListNodes))
	s.mux.HandleFunc("GET /v1/servers", s.requireScope(ScopeReadonly, s.handleListServers))
	s.mux.HandleFunc("POST /v1/versions", s.requireScope(ScopeDeploy, s.handleCreateVersion))
	s.mux.HandleFunc("GET /v1/versions", s.requireScope(ScopeReadonly, s.handleListVersions))
	s.mux.HandleFunc("PUT /v1/fleets/{region}", s.requireScope(ScopeAdmin, s.handleUpsertFleet))
	s.mux.HandleFunc("PUT /v1/projects/{slug}", s.requireScope(ScopeAdmin, s.handleUpsertProject))
	s.mux.HandleFunc("GET /v1/events", s.requireScope(ScopeReadonly, s.handleListEvents))
	s.mux.HandleFunc("GET /v1/events/stream", s.requireScope(ScopeReadonly, s.handleEventsStream))
	s.mux.HandleFunc("GET /v1/matches", s.requireScope(ScopeReadonly, s.handleListMatches))
	s.mux.HandleFunc("GET /v1/matches/{id}", s.requireScope(ScopeReadonly, s.handleGetMatch))
	s.mux.HandleFunc("POST /v1/allocate", s.requireScope(ScopeAllocate, s.handleAllocate))

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
