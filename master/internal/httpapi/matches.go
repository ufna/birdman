package httpapi

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// Match listing for the panel (docs/specs/master.md §6): the matches table
// is fed by the matchmaker; state transitions pending→running→finished come
// from liba reports (later iteration).

// matchStates mirrors the matches.state check constraint (master.md §1).
var matchStates = map[string]bool{
	"pending": true, "running": true, "finished": true, "aborted": true,
}

// handleListMatches is GET /v1/matches?project=&region=&state=&limit=&offset=
// (readonly). `project` валидируется по БД (projectFilter, tracker #961) — тот
// же класс плохого ввода, что и неизвестный `state` ниже.
func (s *Server) handleListMatches(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project, ok := s.projectFilter(w, r)
	if !ok {
		return
	}
	f := store.MatchFilter{
		Project: project,
		Region:  q.Get("region"),
		State:   q.Get("state"),
	}
	if f.State != "" && !matchStates[f.State] {
		writeError(w, http.StatusBadRequest, "bad_request",
			"state must be one of pending|running|finished|aborted")
		return
	}
	if f.Limit, ok = queryInt(w, q.Get("limit"), 100); !ok {
		return
	}
	if f.Offset, ok = queryInt(w, q.Get("offset"), 0); !ok {
		return
	}
	matches, err := s.st.ListMatches(r.Context(), f)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"matches": emptyNotNull(matches)})
}

func (s *Server) handleGetMatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "match id must be a uuid")
		return
	}
	m, err := s.st.GetMatch(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"match": m})
}

// queryInt parses a non-negative integer query parameter (default when
// empty). Writes the 400 itself and returns ok=false on bad input.
func queryInt(w http.ResponseWriter, raw string, def int) (int, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "limit and offset must be non-negative integers")
		return 0, false
	}
	return n, true
}
