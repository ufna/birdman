package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/store"
)

// Matchmaking endpoints (docs/specs/master.md §4/§6, protocol.md §3).
// Transport: long-poll `?wait=25s`; SSE stays panel-only (later iteration).

// maxWait caps GET ?wait= (spec suggests 25s; anything longer risks
// intermediary timeouts).
const maxWait = 30 * time.Second

type submitTicketRequest struct {
	Project       string                  `json:"project,omitempty"`
	Env           string                  `json:"env,omitempty"`
	PlayerID      string                  `json:"player_id"`
	ClientVersion string                  `json:"client_version"`
	Regions       []matchmaker.RegionPing `json:"regions"`
}

func (s *Server) handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	var req submitTicketRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.PlayerID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "player_id is required")
		return
	}
	if !s.mmLimit.allow(req.PlayerID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"matchmaking rate limit: 5 rps per player_id")
		return
	}
	// Binding (environments v1 §3/§5): a bound key defaults AND constrains both
	// project and env; an explicit field that disagrees with the binding is
	// refused (403) here, so the matchmaker never learns about keys. A global key
	// passes its request fields through unchanged (sole-project/sole-env resolve
	// downstream in Submit).
	project := bindProject(r, req.Project)
	env := req.Env
	if env == "" {
		if key, ok := keyFromContext(r.Context()); ok && key.Env != nil {
			env = *key.Env
		}
	}
	if !s.requireBinding(w, r, project, env) {
		return
	}
	t, err := s.mm.Submit(r.Context(), matchmaker.SubmitParams{
		Project:       project,
		Env:           env,
		PlayerID:      req.PlayerID,
		ClientVersion: req.ClientVersion,
		Regions:       req.Regions,
	})
	switch {
	case errors.Is(err, matchmaker.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case errors.Is(err, store.ErrBadEnv), errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrConflict):
		// Unknown project / unknown env (ErrBadEnv, v3) / ambiguous default project.
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleGetTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := s.mm.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "ticket "+id+" not found")
		return
	}
	if !s.mmLimit.allow(t.PlayerID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"matchmaking rate limit: 5 rps per player_id")
		return
	}

	wait := time.Duration(0)
	if raw := r.URL.Query().Get("wait"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				"wait must be a duration like 25s")
			return
		}
		wait = min(d, maxWait)
	}

	t, ok = s.mm.Wait(r.Context(), id, wait)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "ticket "+id+" not found")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleCancelTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := s.mm.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "ticket "+id+" not found")
		return
	}
	if !s.mmLimit.allow(t.PlayerID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"matchmaking rate limit: 5 rps per player_id")
		return
	}
	t, ok = s.mm.Cancel(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "ticket "+id+" not found")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleQoS lists per-region ping targets (public by design, master.md §6).
// The UDP echo responder on nodes ships with the agent in iteration 4; the
// endpoint already returns the correct host list of live nodes.
func (s *Server) handleQoS(w http.ResponseWriter, r *http.Request) {
	// Public endpoint: project and env are optional query params (environments
	// v1 §3, M8). project falls back to the sole project; env to the sole
	// environment with active nodes — ambiguous env → 400 env_required.
	//
	// This is the one read where ?project= is deliberately NOT validated against
	// the DB (tracker #961 draws the line here): the endpoint is unauthenticated,
	// so a "no such project <slug>" would hand every player a free oracle over
	// project slugs. Elsewhere the 400 tells an authorized caller nothing that
	// readonly GET /v1/projects wouldn't; here there is no such caller. An unknown
	// project simply resolves to an empty QoS list.
	project := r.URL.Query().Get("project")
	if project == "" {
		var err error
		project, err = s.st.SoleProjectSlug(r.Context())
		if err != nil {
			storeError(w, err)
			return
		}
	}
	env := r.URL.Query().Get("env")
	if env == "" {
		// Зеркалим allocate-путь (T5-m1): только ErrConflict (ноль/несколько env с
		// активными нодами) → 400 env_required; реальный сбой БД идёт в storeError,
		// а не маскируется под env_required.
		resolved, err := s.st.SoleEnvWithActiveNodes(r.Context(), project)
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusBadRequest, "env_required",
				"env is required (zero or several environments have active nodes)")
			return
		}
		if err != nil {
			storeError(w, err)
			return
		}
		env = resolved
	}
	eps, err := s.st.ListQoSEndpoints(r.Context(), project, env)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"qos": emptyNotNull(eps)})
}

// --- projects (match_size, docs/specs/master.md §4) ---

type upsertProjectRequest struct {
	MatchSize int32 `json:"match_size"`
}

// handleListProjects is GET /v1/projects (readonly) — the panel's project
// selector reads it (мультипроект W1). Readonly on purpose, unlike the admin
// PUT below: a readonly session must still be able to see WHICH project it is
// looking at, and the payload carries nothing secret (slug, match_size).
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.st.ListProjects(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": emptyNotNull(projects)})
}

func (s *Server) handleUpsertProject(w http.ResponseWriter, r *http.Request) {
	var req upsertProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := s.st.SetProjectMatchSize(r.Context(), r.PathValue("slug"), req.MatchSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": p})
}
