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
// validated in the store (clean 400s), backed by DB CHECKs.
//
// Удаление окружения («удаление непустого окружения»): GET .../usage отдаёт состав
// (панель показывает его в диалоге), DELETE сносит окружение ВМЕСТЕ с содержимым —
// но только при нуле нод (иначе 409) и с подтверждением вводом имени в теле
// {"confirm":"<name>"} (иначе 400). Пустое окружение удаляется как раньше: 204 без
// тела и без confirm.

type createEnvironmentRequest struct {
	Project       string `json:"project"`
	Name          string `json:"name"`
	Production    bool   `json:"production"`
	AutoDeploy    bool   `json:"auto_deploy"`
	RetentionKeep int    `json:"retention_keep"`
}

// deleteEnvironmentRequest — тело DELETE. Необязательное: у пустого окружения его
// нет вовсе (совместимость), у непустого confirm обязан ТОЧНО совпасть с именем.
type deleteEnvironmentRequest struct {
	Confirm string `json:"confirm"`
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

// handleEnvironmentUsage is GET /v1/environments/{project}/{name}/usage (readonly):
// сколько версий/флотов/нод/серверов/матчей/живых ключей держит окружение. Панель
// зовёт его при открытии диалога удаления: показать состав и заблокировать
// удаление, пока в окружении есть ноды. 404 для неизвестного окружения.
func (s *Server) handleEnvironmentUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.st.EnvironmentUsage(r.Context(), r.PathValue("project"), r.PathValue("name"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage": usage})
}

// handleDeleteEnvironment is DELETE /v1/environments/{project}/{name} (admin).
// Тело необязательное: {"confirm":"<name>"}.
//
//   - 404 — неизвестное окружение;
//   - 409 — в окружении есть ноды (предусловие; НЕЗАВИСИМО от confirm: сначала
//     переведите их в другое окружение, PATCH /v1/nodes/{id});
//   - 204 — окружение пустое (никогда не использовалось): удалено, confirm не нужен;
//   - 400 — окружение НЕПУСТОЕ, а confirm отсутствует/не равен имени ТОЧНО;
//   - 200 {"deleted": {...}} — непустое окружение снесено каскадом (состав в теле).
func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	var req deleteEnvironmentRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	res, err := s.st.DeleteEnvironment(r.Context(), r.PathValue("project"), r.PathValue("name"), req.Confirm)
	if errors.Is(err, store.ErrConfirmRequired) {
		writeError(w, http.StatusBadRequest, "bad_request", "confirm must equal the environment name")
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	// Отозванные ключи обязаны умереть сразу: authenticator кэширует проверенные
	// ключи (и держит сессии панели) — гасим кэш, как это делает revoke-хендлер.
	for _, id := range res.RevokedKeyIDs {
		s.auth.invalidateKey(id)
	}
	if res.WasEmpty {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": res})
}

// handleSetNodeEnv is PATCH /v1/nodes/{id} {env} (admin): move a node to another
// environment. 200 with the updated node; 400 for a non-uuid id, a missing env or
// an unknown env (ErrBadEnv — окружение здесь ССЫЛКА в теле, опечатка в ней это
// плохой ввод, v3); 404 for an unknown node (адресуемый ресурс); 409 when the node
// is dead or carries live servers (drain it first). Emits node_env_changed.
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
