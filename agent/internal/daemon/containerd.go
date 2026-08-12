package daemon

import (
	"context"
	"syscall"

	"github.com/ufna/birdman/agent/internal/imagegc"
	"github.com/ufna/birdman/agent/internal/runtime"
)

// ContainerdRuntime adapts runtime.Client (containerd) to the Runtime
// interface. Registry auth is resolved by the caller-supplied CredLookup
// (registries v1, docs/superpowers/specs/2026-07-09-registries-design.md
// §3) — the manager owns the master-snapshot/legacy-file/anonymous chain,
// this type just forwards it to EnsureImage.
type ContainerdRuntime struct {
	Client *runtime.Client
}

func (r *ContainerdRuntime) Pull(ctx context.Context, imageRef string, lookup runtime.CredLookup) error {
	_, err := r.Client.EnsureImage(ctx, imageRef, lookup)
	return err
}

func (r *ContainerdRuntime) Start(ctx context.Context, spec StartSpec) (Handle, error) {
	img, err := r.Client.EnsureImage(ctx, spec.ImageRef, spec.Lookup)
	if err != nil {
		return nil, err
	}
	srv, err := r.Client.StartServer(ctx, runtime.ServerSpec{
		ID:         spec.ID,
		Image:      img,
		ImageRef:   spec.ImageRef,
		Port:       spec.Port,
		Region:     spec.Region,
		SocketPath: spec.SocketPath,
		CPUMillis:  spec.CPUMillis,
		MemMB:      spec.MemMB,
		Env:        spec.Env,
		LogPath:    spec.LogPath,

		ScopeProject: spec.ScopeProject,
		ScopeEnv:     spec.ScopeEnv,
	}, nil)
	if err != nil {
		return nil, err
	}
	return newHandle(srv), nil
}

// Images / DeleteImage / UsedImageRefs make ContainerdRuntime satisfy
// imagegc.Runtime (agent.md §6).
func (r *ContainerdRuntime) Images(ctx context.Context) ([]imagegc.Image, error) {
	infos, err := r.Client.Images(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]imagegc.Image, 0, len(infos))
	for _, i := range infos {
		out = append(out, imagegc.Image{Name: i.Name, UpdatedAt: i.UpdatedAt})
	}
	return out, nil
}

func (r *ContainerdRuntime) ImagePresent(ctx context.Context, ref string) (bool, error) {
	return r.Client.ImagePresent(ctx, ref)
}

func (r *ContainerdRuntime) DeleteImage(ctx context.Context, name string) error {
	return r.Client.DeleteImage(ctx, name)
}

func (r *ContainerdRuntime) UsedImageRefs(ctx context.Context) (map[string]bool, error) {
	return r.Client.UsedImageRefs(ctx)
}

func (r *ContainerdRuntime) Restore(ctx context.Context) ([]RestoredServer, error) {
	restored, err := r.Client.Restore(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RestoredServer, 0, len(restored))
	for _, c := range restored {
		out = append(out, RestoredServer{
			Handle:   newHandle(c.Server),
			ID:       c.ID,
			Port:     c.Port,
			ImageRef: c.ImageRef,
			State:    c.State,
			MatchID:  c.MatchID,
			Running:  c.Running,
			ExitCode: c.ExitCode,

			ScopeProject: c.ScopeProject,
			ScopeEnv:     c.ScopeEnv,
		})
	}
	return out, nil
}

// containerdHandle wraps runtime.Server, translating the containerd exit
// channel into daemon.Exit.
type containerdHandle struct {
	srv  *runtime.Server
	exit chan Exit
}

func newHandle(srv *runtime.Server) *containerdHandle {
	h := &containerdHandle{srv: srv, exit: make(chan Exit, 1)}
	if ch := srv.Wait(); ch != nil {
		go func() {
			st := <-ch
			code, _, err := st.Result()
			h.exit <- Exit{Code: code, Err: err}
		}()
	}
	return h
}

func (h *containerdHandle) Wait() <-chan Exit { return h.exit }

func (h *containerdHandle) Pid() uint32 { return h.srv.Pid() }

func (h *containerdHandle) Signal(ctx context.Context, sig syscall.Signal) error {
	return h.srv.Signal(ctx, sig)
}

func (h *containerdHandle) Kill(ctx context.Context) error { return h.srv.ForceKill(ctx) }

func (h *containerdHandle) Delete(ctx context.Context) error { return h.srv.Delete(ctx) }

func (h *containerdHandle) SetState(ctx context.Context, state, matchID string) error {
	return h.srv.SetLabels(ctx, map[string]string{
		runtime.LabelState:   state,
		runtime.LabelMatchID: matchID,
	})
}
