package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// Registry credential management for the panel "Admin → Registries" screen
// (docs/superpowers/specs/2026-07-09-registries-design.md §1, §4). All three
// routes require the admin scope — unlike apikeys/alerts, even the list read
// is admin-only here (secret-adjacent). The token is write-only: POST accepts
// it, but GET never returns it and neither does the audit event payload — the
// only read that carries a token is store.ListRegistryCreds, used solely by
// agentlink (T3) to build the SetRegistries snapshot.

type createRegistryRequest struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Token    string `json:"token"`
	Note     string `json:"note"`
}

// handleListRegistries is GET /v1/registries (admin): every registry, no
// tokens.
func (s *Server) handleListRegistries(w http.ResponseWriter, r *http.Request) {
	regs, err := s.st.ListRegistries(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"registries": emptyNotNull(regs)})
}

// handleCreateRegistry is POST /v1/registries (admin). It is an upsert by
// (normalized) host: POSTing an existing host replaces its
// username/token/note in place — the only way to rotate a token, since there
// is no "edit note only" form. host, username and token are all required;
// note is optional. Host validation (NormalizeRegistryHost, including the
// docker.io rejection) happens inside UpsertRegistry and surfaces here as a
// 400 with a clear detail. After a successful write it fires
// onRegistriesChanged (nil-safe) so a connected agent set can be refreshed
// (T3 wires the actual broadcast) — with context.WithoutCancel (task review,
// Fix 2): the write is already durable at this point, and the hook's own
// store read (BroadcastRegistries) must not abort just because the client
// that made this request happened to disconnect right after the commit —
// that would leave connected agents on a stale credential set until the next
// change or reconnect.
func (s *Server) handleCreateRegistry(w http.ResponseWriter, r *http.Request) {
	var req createRegistryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Username == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "username is required")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "token is required")
		return
	}
	reg, err := s.st.UpsertRegistry(r.Context(), req.Host, req.Username, req.Token, req.Note)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Audit — payload carries host/username only, never the token.
	if err := s.st.InsertEvent(r.Context(), store.EventRegistryUpserted, store.EventRef{},
		map[string]any{"host": reg.Host, "username": reg.Username}); err != nil {
		s.log.Error("registry: upsert event write failed", "host", reg.Host, "err", err)
	}
	if s.onRegistriesChanged != nil {
		// WithoutCancel: the broadcast must survive this request's client
		// disconnecting (see the doc comment above).
		s.onRegistriesChanged(context.WithoutCancel(r.Context()))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"registry": reg})
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
