package testdb

import (
	"sync"

	"github.com/google/uuid"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// SentCmd is one command recorded by CommandRecorder.
type SentCmd struct {
	NodeID string
	Msg    *agentlinkv1.MasterMsg
}

// CommandRecorder is an in-memory agent-command transport for tests. It
// satisfies both store.CommandSender and reconcile.Sender (same structural
// interface) in place of the gRPC agentlink.Hub.
type CommandRecorder struct {
	mu   sync.Mutex
	cmds []SentCmd
}

func (c *CommandRecorder) Send(nodeID string, msg *agentlinkv1.MasterMsg) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cmds = append(c.cmds, SentCmd{NodeID: nodeID, Msg: msg})
	return uuid.NewString()
}

// Take returns the recorded commands and clears the log.
func (c *CommandRecorder) Take() []SentCmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.cmds
	c.cmds = nil
	return out
}

// Allocates returns only the AllocateServer commands recorded so far.
func (c *CommandRecorder) Allocates() []SentCmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []SentCmd
	for _, cmd := range c.cmds {
		if cmd.Msg.GetAllocate() != nil {
			out = append(out, cmd)
		}
	}
	return out
}
