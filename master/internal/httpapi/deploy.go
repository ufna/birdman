package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// Deploy endpoints (итерация 3, docs/specs/master.md §5–6, scope `deploy`):
//
//	POST /v1/deploy   {version_id}               → 202 prepulling | 200 active
//	POST /v1/rollback {project?, env?, region?}  → 200 rolled back (seconds)

type deployRequest struct {
	VersionID string `json:"version_id"`
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := uuid.Parse(req.VersionID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "version_id must be a version id (uuid)")
		return
	}
	// Binding (environments v1 §5): the target is the version's own (project,
	// env). Load it first so a key bound to another env is refused (403) without
	// side effects; an unknown version stays a 404 (same as Deploy would report).
	v, err := s.st.GetVersion(r.Context(), req.VersionID)
	if err != nil {
		deployError(w, err)
		return
	}
	if !s.requireBinding(w, r, v.Project, v.Env) {
		return
	}
	st, err := s.dep.Deploy(r.Context(), req.VersionID)
	if err != nil {
		deployError(w, err)
		return
	}
	code := http.StatusOK // already active (idempotent repeat)
	if st.State == "prepulling" {
		code = http.StatusAccepted // flip happens when all nodes report pulled
	}
	writeJSON(w, code, map[string]any{"deploy": st})
}

type rollbackRequest struct {
	Project string `json:"project"`
	Env     string `json:"env,omitempty"`
	Region  string `json:"region"`
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req rollbackRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Binding (environments v1 §5): a bound key defaults its own project; else the
	// v0 sole-project convenience (mirroring matchmaking) fills it when omitted.
	project := bindProject(r, req.Project)
	if project == "" {
		var err error
		project, err = s.st.SoleProjectSlug(r.Context())
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "project is required: "+err.Error())
			return
		}
	}
	// env-резолв (environments v1 §3, I3): явный env — как есть; иначе смотрим,
	// у скольких окружений проекта есть deprecated-окно — ровно одно → откат туда
	// (sole-fallback), ноль → нечего откатывать (409), больше одного → env обязателен.
	env := req.Env
	if env == "" {
		envs, err := s.st.EnvsWithDeprecated(r.Context(), project)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		switch len(envs) {
		case 0:
			writeError(w, http.StatusConflict, "conflict",
				"project "+project+" has no deprecated version to roll back to")
			return
		case 1:
			env = envs[0]
		default:
			writeError(w, http.StatusConflict, "conflict",
				"env is required: multiple environments have a rollback window")
			return
		}
	}
	// Binding enforced on the resolved target (environments v1 §5): a key bound to
	// another env cannot roll back this (project, env).
	if !s.requireBinding(w, r, project, env) {
		return
	}
	var regions []string
	if req.Region != "" {
		regions = []string{req.Region}
	}
	res, err := s.dep.Rollback(r.Context(), project, env, regions)
	if err != nil {
		deployError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollback": map[string]any{
		"version":    res.Version,
		"regions":    res.Regions,
		"old_semver": res.PrevSemver,
	}})
}

// deployError maps deploy store sentinels onto HTTP responses.
func deployError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrVersionState),
		errors.Is(err, store.ErrDeployInProgress),
		errors.Is(err, store.ErrNoFleet):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
