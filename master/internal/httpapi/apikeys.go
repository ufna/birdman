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
	writeJSON(w, http.StatusOK, map[string]any{"apikeys": emptyNotNull(keys)})
}

type createAPIKeyRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
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

	key, secret, err := s.st.CreateAPIKey(r.Context(), req.Name, scopes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Audit event — no secret in the payload.
	if err := s.st.InsertEvent(r.Context(), store.EventAPIKeyCreated, store.EventRef{},
		map[string]any{"key_id": key.ID, "name": key.Name, "scopes": key.Scopes}); err != nil {
		s.log.Error("apikey: create event write failed", "key_id", key.ID, "err", err)
	}
	// The secret is shown exactly once; only its bcrypt hash is persisted.
	writeJSON(w, http.StatusCreated, map[string]any{"key": key, "secret": secret})
}

func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "api key id must be a uuid")
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
			map[string]any{"key_id": key.ID, "name": key.Name}); err != nil {
			s.log.Error("apikey: revoke event write failed", "key_id", key.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key})
}
