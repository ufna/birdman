package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/ufna/birdman/master/internal/store"
)

type serverResp struct {
	Server store.Server `json:"server"`
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	missing := func() { writeError(w, http.StatusNotFound, "not_found", "server not found") }
	if _, err := uuid.Parse(id); err != nil {
		missing()
		return
	}
	target, err := s.st.ServerLogTarget(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		missing()
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "server state is unavailable")
		return
	}
	key, _ := keyFromContext(r.Context())
	if !keyAllowed(key, target.Project, target.Env) {
		missing()
		return
	}
	server, err := s.st.GetServer(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		missing()
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "server state is unavailable")
		return
	}
	writeJSON(w, 200, serverResp{Server: server})
}
