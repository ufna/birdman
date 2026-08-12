// Package daemon implements `birdman-agent run` (iteration 1,
// docs/05-runtime-iterations.md): supervision of many dedicated servers under
// master command via the AgentLink stream. The containerd specifics live
// behind the Runtime interface so the manager logic is unit-testable with a
// fake runtime.
package daemon

import (
	"context"
	"syscall"

	"github.com/ufna/birdman/agent/internal/imagegc"
	"github.com/ufna/birdman/agent/internal/runtime"
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
	// Pid returns the container init pid (0 when unknown) — cgroups v2
	// usage for the metrics endpoint resolves through it (agent.md §9).
	Pid() uint32
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
	// ScopeProject/ScopeEnv — пара (project, env) владельца дедика, которая
	// становится label'ами контейнера (tracker #1008). Валидируется вызывающим
	// (Manager.serverScope) и ставится ТОЛЬКО парой: пустая половина = пары нет.
	ScopeProject string
	ScopeEnv     string
	// Lookup resolves the registry credential for the image pull, host-first
	// (registries v1, docs/superpowers/specs/2026-07-09-registries-design.md
	// §3). May be nil (always anonymous).
	Lookup runtime.CredLookup
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
	// ScopeProject/ScopeEnv — пара владельца, поднятая из label'ов контейнера
	// (tracker #1008). Пусто у дедика, запущенного до появления этих label'ов:
	// его серии остаются беспарными до перекрутки.
	ScopeProject string
	ScopeEnv     string
}

// Runtime abstracts containerd for the manager.
type Runtime interface {
	// Pull ensures the image is present locally (PrePull). lookup resolves
	// the registry credential, host-first (registries v1, §3); may be nil.
	Pull(ctx context.Context, imageRef string, lookup runtime.CredLookup) error
	// Start ensures the image, creates and starts the container.
	Start(ctx context.Context, spec StartSpec) (Handle, error)
	// Restore lists containers owned by the agent and re-attaches to them.
	Restore(ctx context.Context) ([]RestoredServer, error)

	// --- image store (RemoveImage handler + imagegc, agent.md §6) ---
	// The image-management surface the manager needs to retire a disabled
	// version's image (environments v1 §6б). ContainerdRuntime already
	// satisfies these (they are also the imagegc.Runtime surface).

	// Images lists the images present in the birdman namespace.
	Images(ctx context.Context) ([]imagegc.Image, error)
	// ImagePresent reports whether one ref is in the image store — a point Get
	// (environments v1 §6б, RemoveImage): cheaper than Images when the handler
	// only needs to know if a single ref is still there. NotFound → (false, nil).
	ImagePresent(ctx context.Context, ref string) (bool, error)
	// DeleteImage removes an image and synchronously collects its content.
	DeleteImage(ctx context.Context, ref string) error
	// UsedImageRefs returns the refs backing existing containers — an image in
	// this set is busy and must not be removed.
	UsedImageRefs(ctx context.Context) (map[string]bool, error)
}

// Sink receives agent→master notifications (implemented by link.Outbox).
type Sink interface {
	ServerEvent(serverID, kind, detail string)
	PullReport(cmdID, imageRef, status, detail string)
	// ImageReport reports the RESULT of one RemoveImage (environments v1 §6б):
	// removed|absent|busy|error, detail non-empty only for error. Exactly one
	// per handled RemoveImage — the master marks the version's image as retired
	// only once every target node reported removed|absent, and re-sends the
	// command otherwise (the plain Ack carries no result).
	ImageReport(cmdID, imageRef, status, detail string)
	// LogChunk queues one TailLogs answer chunk. Unlike events, log chunks
	// are not persistent: the call blocks briefly for queue space and
	// returns false on overflow/timeout — the tail must then cancel
	// (backpressure instead of drowning the event outbox).
	LogChunk(ctx context.Context, cmdID, serverID string, data []byte, eof bool) bool
}
