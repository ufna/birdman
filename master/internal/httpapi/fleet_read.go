package httpapi

import (
	"net/http"

	"github.com/ufna/birdman/master/internal/store"
)

type fleetsResp struct {
	Fleets []store.FleetConfig `json:"fleets"`
}

// handleListFleets exposes desired state so operators can reconcile settings
// without rewriting them and generating an audit event on every run.
func (s *Server) handleListFleets(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.tenantScope(w, r, true)
	if !ok {
		return
	}
	fleets, err := s.st.ListFleetConfigs(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	visible := make([]store.FleetConfig, 0, len(fleets))
	for _, fleet := range fleets {
		if (project == "" || fleet.Project == project) && (env == "" || fleet.Env == env) {
			visible = append(visible, fleet)
		}
	}
	writeJSON(w, http.StatusOK, fleetsResp{Fleets: visible})
}
