package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ufna/birdman/master/internal/stats"
)

// Statistics/Cost-view endpoints for the panel П2 screens
// (docs/specs/panel.md §3, master.md §6). v0: aggregates are computed
// on-the-fly from matches/servers/nodes — no materialized rollups yet (fine at
// our volume; a rollup job comes later). The pure aggregation (series shapes,
// day bucketing, sweep-line CCU, percentiles) lives in internal/stats — no
// DB/HTTP dependencies there, so it's unit-testable and shareable with the
// future rollup job. This file is only the HTTP surface: parse ?days=, fetch
// the raw rows, hand them to the stats package, write JSON.

const (
	statsDefaultDays = 7
	statsMaxDays     = 90
)

// --- handlers ---

func (s *Server) handleStatsOverview(w http.ResponseWriter, r *http.Request) {
	days, ok := statsDays(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	axis := stats.DayAxisUTC(now, days)
	matches, err := s.st.StatMatches(r.Context(), axis[0])
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats.BuildOverview(matches, axis, days, now))
}

func (s *Server) handleStatsCost(w http.ResponseWriter, r *http.Request) {
	days, ok := statsDays(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	axis := stats.DayAxisUTC(now, days)
	matches, err := s.st.StatMatches(r.Context(), axis[0])
	if err != nil {
		storeError(w, err)
		return
	}
	util, err := s.st.RegionUtilization(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats.BuildCost(matches, util, axis, days, now))
}

// statsDays parses ?days=N (default 7, 1..90). Writes the 400 itself.
func statsDays(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("days")
	if raw == "" {
		return statsDefaultDays, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > statsMaxDays {
		writeError(w, http.StatusBadRequest, "bad_request",
			"days must be an integer in 1.."+strconv.Itoa(statsMaxDays))
		return 0, false
	}
	return n, true
}
