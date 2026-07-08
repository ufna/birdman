package agentlink

import (
	"sync"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// LogRouter fans agent LogChunk frames (answers to TailLogs) out to the REST
// logs proxy (итерация 4, GET /v1/servers/{id}/logs). The proxy Registers a
// cmd_id before it sends TailLogs; the AgentLink read loop Dispatches every
// incoming chunk by its cmd_id. Chunks for an unknown cmd_id (client already
// gone, or a raced cancel) are dropped.
type LogRouter struct {
	mu   sync.Mutex
	subs map[string]chan *agentlinkv1.LogChunk
}

func NewLogRouter() *LogRouter {
	return &LogRouter{subs: map[string]chan *agentlinkv1.LogChunk{}}
}

// Register opens a subscription for cmdID and returns its chunk channel plus a
// cancel that removes and closes it (call it once, e.g. via defer). The buffer
// absorbs bursts; a persistently slow HTTP client drops chunks rather than
// stalling the whole node session (Dispatch is non-blocking).
func (r *LogRouter) Register(cmdID string) (<-chan *agentlinkv1.LogChunk, func()) {
	ch := make(chan *agentlinkv1.LogChunk, 256)
	r.mu.Lock()
	r.subs[cmdID] = ch
	r.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			r.mu.Lock()
			if c, ok := r.subs[cmdID]; ok {
				delete(r.subs, cmdID)
				close(c)
			}
			r.mu.Unlock()
		})
	}
	return ch, cancel
}

// Dispatch routes one chunk to its subscriber. Non-blocking and safe against a
// concurrent cancel: the whole operation holds the lock, so it never sends on
// a closed channel (cancel deletes before closing under the same lock).
func (r *LogRouter) Dispatch(cmdID string, chunk *agentlinkv1.LogChunk) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.subs[cmdID]
	if !ok {
		return
	}
	select {
	case ch <- chunk:
	default: // subscriber too slow — drop this chunk
	}
}
