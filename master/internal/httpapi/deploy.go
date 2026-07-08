package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// Deploy endpoints (итерация 3, docs/specs/master.md §5–6, scope `deploy`):
//
//	POST /v1/deploy   {version_id}         → 202 prepulling | 200 active
//	POST /v1/rollback {project?, region?}  → 200 rolled back (seconds)

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
	Region  string `json:"region"`
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req rollbackRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	project := req.Project
	if project == "" {
		// v0 convenience mirroring matchmaking: sole project needs no field.
		var err error
		project, err = s.st.SoleProjectSlug(r.Context())
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "project is required: "+err.Error())
			return
		}
	}
	var regions []string
	if req.Region != "" {
		regions = []string{req.Region}
	}
	res, err := s.dep.Rollback(r.Context(), project, regions)
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
