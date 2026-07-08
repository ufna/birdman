package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// SSE live feed for the panel (docs/specs/master.md §6, panel.md §1):
// GET /v1/events/stream, scope readonly+. New `events` rows are streamed as
//
//	id: <event id>
//	event: <kind>
//	data: <event json, same shape as GET /v1/events items>
//
// The cursor is the events.id sequence, polled from Postgres every
// ssePollInterval. By default the stream starts at the current head (only
// events that happen after connect); `?after_id=N` or a Last-Event-ID header
// (sent by EventSource on reconnect) resumes after N. A comment line is
// written every sseKeepAlive so proxies and clients see a live connection.
const (
	ssePollInterval = time.Second
	sseKeepAlive    = 15 * time.Second
	sseBatchLimit   = 500
)

func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "response writer does not support streaming")
		return
	}

	cursor, ok := s.sseCursor(w, r)
	if !ok {
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // no buffering in reverse proxies
	w.WriteHeader(http.StatusOK)
	// Reconnect hint for EventSource; also confirms the stream to the client.
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	fl.Flush()

	ctx := r.Context() // canceled by client disconnect and server shutdown
	poll := time.NewTicker(ssePollInterval)
	defer poll.Stop()
	keepAlive := time.NewTicker(sseKeepAlive)
	defer keepAlive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-poll.C:
			events, err := s.st.ListEventsAfter(ctx, cursor, sseBatchLimit)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Error("sse: list events failed", "err", err)
				}
				continue // transient DB error: keep the stream, retry next tick
			}
			if len(events) == 0 {
				continue
			}
			for _, e := range events {
				data, err := json.Marshal(e)
				if err != nil {
					s.log.Error("sse: marshal event failed", "id", e.ID, "err", err)
					continue
				}
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Kind, data); err != nil {
					return
				}
				cursor = e.ID
			}
			fl.Flush()
			keepAlive.Reset(sseKeepAlive)
		}
	}
}

// sseCursor resolves the starting cursor: ?after_id → Last-Event-ID →
// current feed head. Writes the error response itself on bad input.
func (s *Server) sseCursor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("after_id")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "after_id must be a non-negative integer")
			return 0, false
		}
		return id, true
	}
	id, err := s.st.MaxEventID(r.Context())
	if err != nil {
		storeError(w, err)
		return 0, false
	}
	return id, true
}
