// Package daemon implements `birdman-agent run` (iteration 1,
// docs/05-runtime-iterations.md): supervision of many dedicated servers under
// master command via the AgentLink stream. The containerd specifics live
// behind the Runtime interface so the manager logic is unit-testable with a
// fake runtime.
package daemon

import (
	"context"
	"syscall"
)

// Exit is the terminal status of a server container.
type Exit struct {
	Code uint32
	Err  error // exit watch failure — Code is not trustworthy
}

// Handle drives one running server container.
type Handle interface {
	// Wait returns the channel armed before task start; it delivers exactly
	// one Exit.
	Wait() <-chan Exit
	// Signal sends sig to the container init process.
	Signal(ctx context.Context, sig syscall.Signal) error
	// Kill SIGKILLs every process in the container.
	Kill(ctx context.Context) error
	// Delete removes the task and container.
	Delete(ctx context.Context) error
	// SetState records the lifecycle state (and match id) in container
	// labels — the source for map restore after an agent restart.
	SetState(ctx context.Context, state, matchID string) error
}

// StartSpec is everything needed to start one server container.
type StartSpec struct {
	ID         string
	ImageRef   string
	Port       int
	Region     string
	SocketPath string // host path of the per-server agent socket
	LogPath    string // shim-side container log file
	CPUMillis  int
	MemMB      int
	Env        map[string]string
}

// RestoredServer is one container recovered from the runtime on startup.
type RestoredServer struct {
	Handle   Handle
	ID       string
	Port     int
	ImageRef string
	State    string // recorded lifecycle state ("" for unknown)
	MatchID  string
	Running  bool
	ExitCode uint32 // when !Running
}

// Runtime abstracts containerd for the manager.
type Runtime interface {
	// Pull ensures the image is present locally (PrePull).
	Pull(ctx context.Context, imageRef string) error
	// Start ensures the image, creates and starts the container.
	Start(ctx context.Context, spec StartSpec) (Handle, error)
	// Restore lists containers owned by the agent and re-attaches to them.
	Restore(ctx context.Context) ([]RestoredServer, error)
}

// Sink receives agent→master notifications (implemented by link.Outbox).
type Sink interface {
	ServerEvent(serverID, kind, detail string)
	PullReport(cmdID, imageRef, status, detail string)
}
