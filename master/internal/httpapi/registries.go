package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// Registry credential management for the panel "Admin → Registries" screen
// (docs/superpowers/specs/2026-07-09-registries-design.md §1, §4; extended by
// the Реестры v2 design with typed registries + PATCH edit). Every route
// (GET/POST/PATCH/DELETE) requires the admin scope — unlike apikeys/alerts,
// even the list read is admin-only here (secret-adjacent). The token is
// write-only: POST/PATCH accept it, but GET never returns it and neither does
// any audit event payload — the only read that carries a token is
// store.ListRegistryCreds, used solely by agentlink (T3) to build the
// SetRegistries snapshot.

type createRegistryRequest struct {
	Host     string `json:"host"`
	Type     string `json:"type"`
	Username string `json:"username"`
	Token    string `json:"token"`
	Note     string `json:"note"`
}

// patchRegistryRequest is the PATCH body (Реестры v2 §2). type/username/note
// are pointers so an absent field means "leave unchanged"; token is optional
// (absent/empty → keep the existing secret, present → rotate). Host is
// accepted but IGNORED — it is immutable (delete + re-add to change it); the
// field exists only so a client echoing the current host back is not rejected
// by decodeJSON's DisallowUnknownFields.
type patchRegistryRequest struct {
	Host     *string `json:"host"`
	Type     *string `json:"type"`
	Username *string `json:"username"`
	Token    *string `json:"token"`
	Note     *string `json:"note"`
}

// handleListRegistries is GET /v1/registries (admin): every registry, no
// tokens.
func (s *Server) handleListRegistries(w http.ResponseWriter, r *http.Request) {
	regs, err := s.st.ListRegistries(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, registriesResp{Registries: emptyNotNull(regs)})
}

// handleCreateRegistry is POST /v1/registries (admin). It is an upsert by
// (normalized) host: POSTing an existing host replaces its
// type/username/token/note in place — the only way to rotate a token via POST,
// though PatchRegistry is the partial-update door. host, type and token are
// required; username is required for ghcr/generic but forced to _json_key (and
// ignored) for gar; note is optional. All per-type validation (host shape, the
// gar service-account-JSON check, unknown-type rejection) happens inside
// UpsertRegistry → ValidateRegistry and surfaces here as a 400 with a clear
// detail. After a successful write it fires onRegistriesChanged (nil-safe) so
// a connected agent set can be refreshed (T3 wires the actual broadcast) —
// with context.WithoutCancel (task review, Fix 2): the write is already
// durable at this point, and the hook's own store read (BroadcastRegistries)
// must not abort just because the client that made this request happened to
// disconnect right after the commit — that would leave connected agents on a
// stale credential set until the next change or reconnect.
func (s *Server) handleCreateRegistry(w http.ResponseWriter, r *http.Request) {
	var req createRegistryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	reg, err := s.st.UpsertRegistry(r.Context(), req.Host, req.Type, req.Username, req.Token, req.Note)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Audit — payload carries host/type/username only, never the token.
	if err := s.st.InsertEvent(r.Context(), store.EventRegistryUpserted, store.EventRef{},
		map[string]any{"host": reg.Host, "type": reg.Type, "username": reg.Username}); err != nil {
		s.log.Error("registry: upsert event write failed", "host", reg.Host, "err", err)
	}
	if s.onRegistriesChanged != nil {
		// WithoutCancel: the broadcast must survive this request's client
		// disconnecting (see the doc comment above).
		s.onRegistriesChanged(context.WithoutCancel(r.Context()))
	}
	writeJSON(w, http.StatusCreated, registryResp{Registry: reg})
}

// handlePatchRegistry is PATCH /v1/registries/{id} (admin) — the partial edit
// (Реестры v2 §2). It updates type/username/note (absent field = unchanged),
// rotates the token when one is supplied (absent/empty = keep the existing
// secret, no re-encrypt), and IGNORES any host in the body (immutable). 400 on
// a non-uuid id or a per-type validation failure (including a type change to
// gar without a fresh service-account JSON key), 404 for an unknown id, 200
// with the updated registry (no token) otherwise. Emits registry_updated
// (payload host/type/username, never the token) and fires onRegistriesChanged
// (context.WithoutCancel — see handleCreateRegistry's doc comment).
func (s *Server) handlePatchRegistry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "registry id must be a uuid")
		return
	}
	var req patchRegistryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token := ""
	if req.Token != nil {
		token = *req.Token
	}
	reg, found, err := s.st.PatchRegistry(r.Context(), id, req.Type, req.Username, req.Note, token)
	if err != nil {
		// PatchRegistry only reaches a validation error on an existing row (it
		// loads the row first), so this is always a per-type/gar-guard 400.
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no such registry")
		return
	}
	if err := s.st.InsertEvent(r.Context(), store.EventRegistryUpdated, store.EventRef{},
		map[string]any{"host": reg.Host, "type": reg.Type, "username": reg.Username}); err != nil {
		s.log.Error("registry: update event write failed", "host", reg.Host, "err", err)
	}
	if s.onRegistriesChanged != nil {
		s.onRegistriesChanged(context.WithoutCancel(r.Context()))
	}
	writeJSON(w, http.StatusOK, registryResp{Registry: reg})
}

// handleDeleteRegistry is DELETE /v1/registries/{id} (admin). 204 on a real
// removal (emits registry_removed + fires onRegistriesChanged, via
// context.WithoutCancel — see handleCreateRegistry's doc comment, task
// review Fix 2), 404 for an unknown/already-removed id, 400 for a non-uuid
// id.
func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "registry id must be a uuid")
		return
	}
	reg, deleted, err := s.st.DeleteRegistry(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "not_found", "no such registry")
		return
	}
	if err := s.st.InsertEvent(r.Context(), store.EventRegistryRemoved, store.EventRef{},
		map[string]any{"host": reg.Host, "username": reg.Username}); err != nil {
		s.log.Error("registry: delete event write failed", "host", reg.Host, "err", err)
	}
	if s.onRegistriesChanged != nil {
		// WithoutCancel: the broadcast must survive this request's client
		// disconnecting (see handleCreateRegistry's doc comment).
		s.onRegistriesChanged(context.WithoutCancel(r.Context()))
	}
	w.WriteHeader(http.StatusNoContent)
}
