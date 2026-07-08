package link

import (
	"context"
	"sync"
	"time"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// outboxCap bounds the queue; on overflow the oldest message is dropped
// (heartbeats converge state anyway, events are best effort in v0).
const outboxCap = 1024

// Log chunks live in their own bounded channel with backpressure: unlike
// events they are large, non-durable (a dead session kills the master-side
// stream anyway) and must never crowd events out of the outbox.
const (
	logChanCap     = 64
	logPushTimeout = 10 * time.Second
)

// Outbox queues agent→master messages (ServerEvent, PullReport) across
// sessions: messages survive reconnects and are drained in order once a
// stream is available. Delivery is at-least-once: a message is removed only
// after a successful stream write, so a session dying mid-drain re-sends it.
type Outbox struct {
	logf func(format string, args ...any)

	mu     sync.Mutex
	queue  []*agentlinkv1.AgentMsg
	notify chan struct{}

	logCh chan *agentlinkv1.AgentMsg
}

// NewOutbox creates an empty outbox. logf may be nil.
func NewOutbox(logf func(string, ...any)) *Outbox {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Outbox{
		logf:   logf,
		notify: make(chan struct{}, 1),
		logCh:  make(chan *agentlinkv1.AgentMsg, logChanCap),
	}
}

// ServerEvent queues a ServerEvent{server_id, kind, detail}.
func (o *Outbox) ServerEvent(serverID, kind, detail string) {
	o.push(&agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Event{
		Event: &agentlinkv1.ServerEvent{ServerId: serverID, Kind: kind, Detail: detail},
	}})
}

// PullReport queues a PullReport for a PrePull command.
func (o *Outbox) PullReport(cmdID, imageRef, status, detail string) {
	o.push(&agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Pull{
		Pull: &agentlinkv1.PullReport{CmdId: cmdID, ImageRef: imageRef, Status: status, Detail: detail},
	}})
}

// LogChunk queues one TailLogs answer chunk, waiting up to logPushTimeout for
// channel space. false → the queue stayed full (dead/slow session): the tail
// should cancel rather than pile up more chunks (daemon.Sink contract).
func (o *Outbox) LogChunk(ctx context.Context, cmdID, serverID string, data []byte, eof bool) bool {
	m := &agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Log{
		Log: &agentlinkv1.LogChunk{CmdId: cmdID, ServerId: serverID, Data: data, Eof: eof},
	}}
	t := time.NewTimer(logPushTimeout)
	defer t.Stop()
	select {
	case o.logCh <- m:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		o.logf("[link] log chunk queue full for %s — dropping tail", logPushTimeout)
		return false
	}
}

// logChunks exposes the log chunk channel to the session drain loop.
func (o *Outbox) logChunks() <-chan *agentlinkv1.AgentMsg { return o.logCh }

func (o *Outbox) push(m *agentlinkv1.AgentMsg) {
	o.mu.Lock()
	if len(o.queue) >= outboxCap {
		o.queue = o.queue[1:]
		o.logf("[link] outbox full, dropping oldest message")
	}
	o.queue = append(o.queue, m)
	o.mu.Unlock()
	select {
	case o.notify <- struct{}{}:
	default:
	}
}

// wait returns a channel that fires when the outbox has (new) content.
func (o *Outbox) wait() <-chan struct{} { return o.notify }

// peek returns the head message without removing it (nil when empty).
func (o *Outbox) peek() *agentlinkv1.AgentMsg {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.queue) == 0 {
		return nil
	}
	return o.queue[0]
}

// pop removes m if it is still the head (it was successfully sent).
func (o *Outbox) pop(m *agentlinkv1.AgentMsg) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.queue) > 0 && o.queue[0] == m {
		o.queue = o.queue[1:]
	}
}

// Len reports the number of queued messages (tests/metrics).
func (o *Outbox) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.queue)
}
