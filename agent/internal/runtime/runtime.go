// Package runtime drives containerd: image pull (with GHCR auth), container
// create/start with the birdman env/mount/limits contract, exit watch and
// deletion (docs/specs/agent.md §3).
//
// Client library: github.com/containerd/containerd v1.7.x — matches the
// containerd 1.7 daemon shipped with docker 28.x on Ubuntu 24.04. The /v2
// client targets containerd 2.x daemons; we switch together with the daemon
// upgrade (rationale in agent/README.md).
package runtime

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"syscall"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/remotes/docker"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	// DefaultAddress is the containerd socket (docs/specs/agent.md).
	DefaultAddress = "/run/containerd/containerd.sock"
	// Namespace is the containerd namespace owned by birdman.
	Namespace = "birdman"
	// ContainerSocketPath is where the per-server agent socket appears
	// inside the container (protocol.md §2).
	ContainerSocketPath = "/birdman/agent.sock"

	// Container labels — the agent restores its server map from these after
	// a restart (agent.md §2; the restore itself lands in iteration 1).
	LabelServerID = "birdman/server-id"
	LabelPort     = "birdman/port"
	LabelImage    = "birdman/image"

	// serverOOMScoreAdj > agent's 0: under memory pressure the kernel kills
	// dediks before the agent (agent.md §3).
	serverOOMScoreAdj = 500
)

// Credentials authenticate pulls from a private registry. Token is read from
// registry_auth.token_file by the caller and must never be logged.
type Credentials struct {
	Username string
	Token    string
}

// Client is a containerd client bound to the birdman namespace.
type Client struct {
	c *containerd.Client
}

// Connect dials the containerd socket.
func Connect(address string) (*Client, error) {
	c, err := containerd.New(address, containerd.WithDefaultNamespace(Namespace))
	if err != nil {
		return nil, fmt.Errorf("connect containerd at %s: %w", address, err)
	}
	return &Client{c: c}, nil
}

// Close releases the containerd connection.
func (c *Client) Close() error { return c.c.Close() }

// EnsureImage returns the image for ref: the local content store first
// (обычно образ уже прогрет PrePull'ом — agent.md §3), иначе pull из
// registry с авторизацией (creds != nil).
func (c *Client) EnsureImage(ctx context.Context, ref string, creds *Credentials) (containerd.Image, error) {
	if img, err := c.c.GetImage(ctx, ref); err == nil {
		ok, uerr := img.IsUnpacked(ctx, containerd.DefaultSnapshotter)
		if uerr == nil && ok {
			return img, nil
		}
		if uerr == nil {
			if err := img.Unpack(ctx, containerd.DefaultSnapshotter); err == nil {
				return img, nil
			}
		}
		// локальный образ есть, но непригоден — честный pull ниже
	}
	opts := []containerd.RemoteOpt{containerd.WithPullUnpack}
	if creds != nil {
		authorizer := docker.NewDockerAuthorizer(
			docker.WithAuthCreds(func(host string) (string, string, error) {
				return creds.Username, creds.Token, nil
			}))
		resolver := docker.NewResolver(docker.ResolverOptions{
			Hosts: docker.ConfigureDefaultRegistries(docker.WithAuthorizer(authorizer)),
		})
		opts = append(opts, containerd.WithResolver(resolver))
	}
	img, err := c.c.Pull(ctx, ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	return img, nil
}

// ServerSpec describes one dedicated server container.
type ServerSpec struct {
	ID       string
	Image    containerd.Image
	ImageRef string
	Port     int
	Region   string
	// SocketPath is the host path of the per-server agent socket; it is
	// bind-mounted to ContainerSocketPath inside the container.
	SocketPath string
	CPUMillis  int
	MemMB      int
	Env        map[string]string // extra env on top of the BIRDMAN_* contract
	Args       []string          // override image entrypoint (integration tests); nil = image default
}

// Server is a created (and running until proven otherwise) dedik container.
type Server struct {
	ID        string
	container containerd.Container
	task      containerd.Task
	exitCh    <-chan containerd.ExitStatus
}

// StartServer creates and starts a dedicated server container: host network,
// BIRDMAN_* env, agent socket bind mount, cgroup cpu/mem limits.
//
// Container stdout/stderr are streamed into logW by this process (v0).
// TODO(итерация 1): shim-side лог (cio.LogFile/LogURI), чтобы рестарт агента
// не прерывал поток логов живого дедика.
func (c *Client) StartServer(ctx context.Context, sp ServerSpec, logW io.Writer) (*Server, error) {
	env := []string{
		"BIRDMAN_PORT=" + strconv.Itoa(sp.Port),
		"BIRDMAN_SERVER_ID=" + sp.ID,
		"BIRDMAN_SOCKET=" + ContainerSocketPath,
		"BIRDMAN_REGION=" + sp.Region,
	}
	for _, k := range sortedKeys(sp.Env) {
		env = append(env, k+"="+sp.Env[k])
	}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(sp.Image),
		oci.WithHostNamespace(specs.NetworkNamespace), // host network (ADR-6)
		oci.WithHostHostsFile,
		oci.WithHostResolvconf,
		oci.WithEnv(env),
		oci.WithMounts([]specs.Mount{{
			Destination: ContainerSocketPath,
			Type:        "bind",
			Source:      sp.SocketPath,
			Options:     []string{"bind", "rw"},
		}}),
		oci.WithMemoryLimit(uint64(sp.MemMB) << 20),
		// cpu_millis → CFS: 1000 millis = 1 ядро = квота 100000мкс на период 100000мкс
		oci.WithCPUCFS(int64(sp.CPUMillis)*100, 100000),
		withOOMScoreAdj(serverOOMScoreAdj),
	}
	if len(sp.Args) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(sp.Args...))
	}

	labels := map[string]string{
		LabelServerID: sp.ID,
		LabelPort:     strconv.Itoa(sp.Port),
		LabelImage:    sp.ImageRef,
	}

	cont, err := c.c.NewContainer(ctx, sp.ID,
		containerd.WithNewSnapshot(sp.ID+"-snap", sp.Image),
		containerd.WithNewSpec(specOpts...),
		containerd.WithContainerLabels(labels),
	)
	if err != nil {
		return nil, fmt.Errorf("create container %s: %w", sp.ID, err)
	}

	cleanupCtx := context.WithoutCancel(ctx)
	task, err := cont.NewTask(ctx, cio.NewCreator(cio.WithStreams(nil, logW, logW)))
	if err != nil {
		_ = cont.Delete(cleanupCtx, containerd.WithSnapshotCleanup)
		return nil, fmt.Errorf("create task %s: %w", sp.ID, err)
	}
	// Wait до Start — выход не может быть пропущен. Контекст без отмены:
	// exit-watch обязан пережить отмену сигнального ctx (graceful stop),
	// иначе вместо реального кода контейнера придёт rpc "context canceled".
	exitCh, err := task.Wait(cleanupCtx)
	if err != nil {
		_, _ = task.Delete(cleanupCtx, containerd.WithProcessKill)
		_ = cont.Delete(cleanupCtx, containerd.WithSnapshotCleanup)
		return nil, fmt.Errorf("wait task %s: %w", sp.ID, err)
	}
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(cleanupCtx, containerd.WithProcessKill)
		_ = cont.Delete(cleanupCtx, containerd.WithSnapshotCleanup)
		return nil, fmt.Errorf("start task %s: %w", sp.ID, err)
	}
	return &Server{ID: sp.ID, container: cont, task: task, exitCh: exitCh}, nil
}

// Wait returns the exit channel armed before task start.
func (s *Server) Wait() <-chan containerd.ExitStatus { return s.exitCh }

// Signal sends sig to the container init process (graceful stop = SIGTERM).
func (s *Server) Signal(ctx context.Context, sig syscall.Signal) error {
	err := s.task.Kill(ctx, sig)
	if errdefs.IsNotFound(err) {
		return nil // процесс уже завершился
	}
	return err
}

// ForceKill SIGKILLs every process in the container.
func (s *Server) ForceKill(ctx context.Context) error {
	err := s.task.Kill(ctx, syscall.SIGKILL, containerd.WithKillAll)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// Delete removes the task and the container with its snapshot.
func (s *Server) Delete(ctx context.Context) error {
	var firstErr error
	if _, err := s.task.Delete(ctx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
		firstErr = fmt.Errorf("delete task %s: %w", s.ID, err)
	}
	if err := s.container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) && firstErr == nil {
		firstErr = fmt.Errorf("delete container %s: %w", s.ID, err)
	}
	return firstErr
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func withOOMScoreAdj(adj int) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		s.Process.OOMScoreAdj = &adj
		return nil
	}
}
