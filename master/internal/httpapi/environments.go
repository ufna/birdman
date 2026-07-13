package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// Environments API (docs/superpowers/specs/2026-07-13-environments-v1-design.md
// §2). Read is readonly-scoped; create/patch/delete and the node-move PATCH are
// admin. Guardrail production⇒!auto_deploy and the name shape/reserved rules are
// validated in the store (clean 400s), backed by DB CHECKs. A used environment
// is undeletable (409) — versions rows are never removed (honest I10).

type createEnvironmentRequest struct {
	Project       string `json:"project"`
	Name          string `json:"name"`
	Production    bool   `json:"production"`
	AutoDeploy    bool   `json:"auto_deploy"`
	RetentionKeep int    `json:"retention_keep"`
}

// patchEnvironmentRequest is the PATCH body — every field a pointer so an absent
// field means "leave unchanged". Name is immutable (not a field).
type patchEnvironmentRequest struct {
	Production    *bool `json:"production"`
	AutoDeploy    *bool `json:"auto_deploy"`
	RetentionKeep *int  `json:"retention_keep"`
}

type setNodeEnvRequest struct {
	Env string `json:"env"`
}

// handleListEnvironments is GET /v1/environments?project= (readonly). project is
// resolved to the sole project when omitted (single-project convention).
func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project == "" {
		p, err := s.st.SoleProjectSlug(r.Context())
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "project is required: "+err.Error())
			return
		}
		project = p
	}
	envs, err := s.st.ListEnvironments(r.Context(), project)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environments": emptyNotNull(envs)})
}

// handleCreateEnvironment is POST /v1/environments (admin). 201 on success;
// 409 for a duplicate; 400 for a guardrail/name/FK/CHECK violation (the store
// validates production⇒!auto_deploy and the name shape/reserved rules).
func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request) {
	var req createEnvironmentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	env, err := s.st.CreateEnvironment(r.Context(), store.CreateEnvironmentParams{
		Project:       req.Project,
		Name:          req.Name,
		Production:    req.Production,
		AutoDeploy:    req.AutoDeploy,
		RetentionKeep: req.RetentionKeep,
	})
	if errors.Is(err, store.ErrConflict) {
		storeError(w, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"environment": env})
}

// handlePatchEnvironment is PATCH /v1/environments/{project}/{name} (admin).
// 200 on success; 404 for an unknown env; 400 when the resulting state breaks
// the production⇒!auto_deploy guardrail (in any field order).
func (s *Server) handlePatchEnvironment(w http.ResponseWriter, r *http.Request) {
	var req patchEnvironmentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	env, err := s.st.PatchEnvironment(r.Context(), r.PathValue("project"), r.PathValue("name"),
		store.EnvironmentPatch{
			Production:    req.Production,
			AutoDeploy:    req.AutoDeploy,
			RetentionKeep: req.RetentionKeep,
		})
	if errors.Is(err, store.ErrNotFound) {
		storeError(w, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environment": env})
}

// handleDeleteEnvironment is DELETE /v1/environments/{project}/{name} (admin).
// 204 for a never-used env; 409 when it is referenced by versions/fleets/nodes/
// keys (listed in the detail); 404 for an unknown env.
func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteEnvironment(r.Context(), r.PathValue("project"), r.PathValue("name")); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetNodeEnv is PATCH /v1/nodes/{id} {env} (admin): move a node to another
// environment. 200 with the updated node; 400 for a non-uuid id or a missing
// env; 404 for an unknown node/env; 409 when the node is dead or carries live
// servers (drain it first). Emits node_env_changed.
func (s *Server) handleSetNodeEnv(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "node id must be a uuid")
		return
	}
	var req setNodeEnvRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Env == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "env is required")
		return
	}
	node, err := s.st.SetNodeEnv(r.Context(), id, req.Env)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": node})
}
