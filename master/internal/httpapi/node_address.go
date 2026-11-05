package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/ufna/birdman/master/internal/store"
)

type reconcileNodeAddressRequest struct {
	Project          string `json:"project"`
	Env              string `json:"env"`
	Region           string `json:"region"`
	Hostname         string `json:"hostname"`
	ExpectedPublicIP string `json:"expected_public_ip"`
	PublicIP         string `json:"public_ip"`
}

func (s *Server) handleReconcileNodeAddress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, 400, "bad_request", "node id must be a uuid")
		return
	}
	var req reconcileNodeAddressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requireBinding(w, r, req.Project, req.Env) {
		return
	}
	node, err := s.st.ReconcileNodeAddress(r.Context(), id, store.NodeAddressChange{Project: req.Project, Env: req.Env, Region: req.Region, Hostname: req.Hostname, ExpectedPublicIP: req.ExpectedPublicIP, PublicIP: req.PublicIP})
	if errors.Is(err, store.ErrBadNodeAddress) {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, nodeResp{Node: node})
}
