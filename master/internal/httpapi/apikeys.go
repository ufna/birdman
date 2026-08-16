package httpapi

import (
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// API-key management for the panel П2 Access screen (docs/specs/master.md §6,
// docs/specs/panel.md §3). All three routes require the admin scope. The
// secret is returned exactly once, at creation (like the bootstrap key); the
// list and revoke reads never expose it.

// validScopes is the closed set a key may be granted (auth.go scopes).
var validScopes = []string{ScopeAdmin, ScopeDeploy, ScopeMatchmaking, ScopeAllocate, ScopeReadonly}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.st.ListAPIKeys(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiKeysResp{APIKeys: emptyNotNull(keys)})
}

type createAPIKeyRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// Optional (project, env) binding (environments v1 §5) — strictly a pair.
	// Both empty → a global key (the pre-env default). The store validates parity,
	// admin-incompatibility and existence; every error maps to 400.
	Project string `json:"project,omitempty"`
	Env     string `json:"env,omitempty"`
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req createAPIKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	if len(req.Scopes) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "at least one scope is required")
		return
	}
	for _, sc := range req.Scopes {
		if !slices.Contains(validScopes, sc) {
			writeError(w, http.StatusBadRequest, "bad_request",
				"unknown scope "+sc+" (allowed: admin, deploy, matchmaking, allocate, readonly)")
			return
		}
	}
	// Normalize: drop duplicate scopes so the stored set is clean.
	scopes := slices.Clone(req.Scopes)
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)

	// Binding is optional and strictly a pair — an empty field means "unset" (a
	// global key). The store validates parity/admin-incompatibility/existence.
	var project, env *string
	if req.Project != "" {
		project = &req.Project
	}
	if req.Env != "" {
		env = &req.Env
	}
	key, secret, err := s.st.CreateAPIKey(r.Context(), store.CreateAPIKeyParams{
		Name: req.Name, Scopes: scopes, Project: project, Env: env,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Audit event — no secret in the payload; binding recorded when present.
	payload := apiKeyEventPayload(key)
	payload["scopes"] = key.Scopes
	if err := s.st.InsertEvent(r.Context(), store.EventAPIKeyCreated, store.EventRef{}, payload); err != nil {
		s.log.Error("apikey: create event write failed", "key_id", key.ID, "err", err)
	}
	// The secret is shown exactly once; only its bcrypt hash is persisted.
	writeJSON(w, http.StatusCreated, apiKeyCreatedResp{Key: key, Secret: secret})
}

func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "api key id must be a uuid")
		return
	}
	// ?purge=true|1 hard-deletes an already-revoked key instead of the
	// default soft-revoke (registries v1 design §6): same-route-double-DELETE
	// escalation was rejected on review — an explicit, distinct query value is
	// required, so a plain retry/double-click of the revoke call (no purge
	// param, or any other value) keeps taking the byte-identical path below.
	if isTrue(r.URL.Query().Get("purge")) {
		s.purgeAPIKey(w, r, id)
		return
	}
	key, changed, err := s.st.RevokeAPIKey(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrLastAdminKey):
		writeError(w, http.StatusConflict, "last_admin_key",
			"refusing to revoke the last active admin key (self-lockout)")
		return
	case errors.Is(err, store.ErrNotFound):
		storeError(w, err)
		return
	case err != nil:
		storeError(w, err)
		return
	}
	// Only a real revoke (not an idempotent no-op on an already-revoked key)
	// invalidates auth and writes an audit event. Revoking must take effect
	// now: AuthAPIKey already refuses revoked keys at the DB, but the
	// authenticator caches verified keys (and holds panel sessions) — drop both
	// so the revoked key stops working immediately, not after the cache TTL.
	if changed {
		s.auth.invalidateKey(id)
		if err := s.st.InsertEvent(r.Context(), store.EventAPIKeyRevoked, store.EventRef{},
			apiKeyEventPayload(key)); err != nil {
			s.log.Error("apikey: revoke event write failed", "key_id", key.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, apiKeyResp{Key: key})
}

// purgeAPIKey handles DELETE /v1/apikeys/{id}?purge=true (registries v1
// design §6): hard-deletes a key row, but ONLY if it is already revoked —
// purge never revokes on its own behalf, so an active key answers 409
// not_revoked instead of being silently revoked-then-deleted. A missing row
// (unknown id, or the same id purged again) answers 404: the retry case is
// deliberately a no-op-shaped 404, not a repeated 204, so a double-click
// can't escalate into anything more destructive than the first call already
// was.
func (s *Server) purgeAPIKey(w http.ResponseWriter, r *http.Request, id string) {
	key, purged, notRevoked, err := s.st.PurgeAPIKey(r.Context(), id)
	switch {
	case err != nil:
		storeError(w, err)
		return
	case notRevoked:
		writeError(w, http.StatusConflict, "not_revoked", "key is still active; revoke it before purging")
		return
	case !purged:
		writeError(w, http.StatusNotFound, "not_found", "api key "+id+" not found")
		return
	}
	// Defense in depth, mirroring revoke: the key is already revoked and so
	// already refused by AuthAPIKey, but drop any stale cache/session entry
	// for it anyway.
	s.auth.invalidateKey(id)
	if err := s.st.InsertEvent(r.Context(), store.EventAPIKeyPurged, store.EventRef{},
		apiKeyEventPayload(key)); err != nil {
		s.log.Error("apikey: purge event write failed", "key_id", key.ID, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiKeyEventPayload — ЕДИНОЕ тело аудит-события жизненного цикла ключа
// (`apikey_created`, `apikey_revoked`, `apikey_purged`). Секрета в нём нет
// никогда; привязка кладётся, КОГДА ОНА ЕСТЬ, и это не украшение, а
// АТРИБУЦИЯ (tracker #1017).
//
// `store.insertEvent` выводит `project_id` из ссылок события, а у событий без
// ссылок — из слага `project` в payload'е (эпик #968 шаг 2). У ключа ссылки
// нет вовсе (колонки `api_key_id` в `events` не существует), поэтому слаг в
// payload'е — ЕДИНСТВЕННЫЙ источник атрибуции: нет поля → `project_id is
// null` → событие платформенное → его видит КАЖДЫЙ арендатор, потому что
// фильтр ленты не скрывающий ПО ЗАМЫСЛУ (#955/#968/#993). Так имя чужого
// ключа (`ci-game-prod-deployer` — операционная строка, часто называющая
// проект и окружение прямо в себе) приезжало соседу и через `GET /v1/events`,
// и через SSE.
//
// Поэтому payload у всех трёх видов собирается ЗДЕСЬ, а не по месту: три
// вызывающих расходились ровно так, как расходятся руками собранные мапы —
// `created` привязку клал, `revoked` не клал, `purged` не клал даже `key_id`.
// Каскадные отзывы в сторе (`projects.go`, `environments.go`) кладут `project`
// давно; расхождение было ВНУТРИ одного вида события.
func apiKeyEventPayload(key store.APIKey) map[string]any {
	payload := map[string]any{"key_id": key.ID, "name": key.Name}
	if key.Project != nil {
		payload["project"] = *key.Project
	}
	if key.Env != nil {
		payload["env"] = *key.Env
	}
	return payload
}
