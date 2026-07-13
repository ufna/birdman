// Package deploy implements the deploy manager (итерация 3,
// docs/specs/master.md §5): soft rollout of a new version without breaking
// live matches.
//
//	POST /v1/deploy {version_id}:
//	  1. version → prepulling; PrePull to every active node of the project's
//	     fleet regions;
//	  2. wait for PullReport `pulled` from all of them (timeout → abort +
//	     event deploy_failed; a `failed` report aborts immediately);
//	  3. atomic flip: old active → deprecated, new → active,
//	     fleet_configs.active_version → new (event deploy_activated);
//	  4-5. reconcile owns the rest: buffers of both window versions, reap.
//
//	POST /v1/rollback: the flip backwards (images are already on the nodes) —
//	seconds, no prepull.
//
// The prepull wait state is in-memory; the version row (`prepulling`) is the
// durable marker. After a master restart Resume() re-arms the jobs with a
// fresh timeout — PrePull is idempotent and cheap on warm caches.
package deploy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// PrePullTimeout aborts a deploy whose nodes did not all report `pulled`
// (master.md §5: 15 минут).
const PrePullTimeout = 15 * time.Minute

// Sender dispatches a command to a node's agent (agentlink.Hub in
// production; recorders in tests). Must not block.
type Sender interface {
	Send(nodeID string, msg *agentlinkv1.MasterMsg) (cmdID string)
}

// Manager orchestrates deploys. One in-flight prepull per project (enforced
// by store.BeginDeploy).
type Manager struct {
	st      *store.Store
	sender  Sender
	log     *slog.Logger
	timeout time.Duration

	// observePrepull records the deploy_started→all-pulled duration
	// (birdman_deploy_prepull_seconds). Optional.
	observePrepull func(seconds float64)

	mu   sync.Mutex
	jobs map[string]*job // versionID → in-flight prepull
}

type job struct {
	version store.Version
	started time.Time
	pending map[string]bool // nodeID → still awaiting `pulled`
	timer   *time.Timer
}

// Options configure the Manager.
type Options struct {
	Store          *store.Store
	Sender         Sender
	Log            *slog.Logger
	Timeout        time.Duration         // default PrePullTimeout
	ObservePrepull func(seconds float64) // metric hook, optional
}

func New(o Options) *Manager {
	if o.Timeout <= 0 {
		o.Timeout = PrePullTimeout
	}
	if o.ObservePrepull == nil {
		o.ObservePrepull = func(float64) {}
	}
	return &Manager{
		st: o.Store, sender: o.Sender, log: o.Log,
		timeout: o.Timeout, observePrepull: o.ObservePrepull,
		jobs: map[string]*job{},
	}
}

// Status is the POST /v1/deploy outcome.
type Status struct {
	Version store.Version `json:"version"`
	// State after the call: prepulling | active.
	State string `json:"state"`
	// Nodes still being warmed (0 when the flip already happened).
	PendingNodes int `json:"pending_nodes"`
}

// Deploy runs steps 1–2 of the deploy (see the package comment). It returns
// as soon as the prepull fan-out is dispatched; the flip happens when the
// last PullReport lands (HandlePullReport). Idempotent: a repeated call for
// a prepulling version reports the in-flight job (re-arming it if the master
// restarted meanwhile); for an active version it is a no-op.
func (m *Manager) Deploy(ctx context.Context, versionID string) (Status, error) {
	res, err := m.st.BeginDeploy(ctx, versionID)
	if err != nil {
		return Status{}, err
	}
	if res.AlreadyActive {
		return Status{Version: res.Version, State: "active"}, nil
	}
	if res.AlreadyPrepulling {
		m.mu.Lock()
		j, ok := m.jobs[res.Version.ID]
		var pending int
		if ok {
			pending = len(j.pending)
		}
		m.mu.Unlock()
		if !ok {
			// prepulling in the DB but no job in memory — master restarted
			// mid-deploy; resume.
			return m.startJob(ctx, res.Version)
		}
		return Status{Version: res.Version, State: "prepulling", PendingNodes: pending}, nil
	}
	return m.startJob(ctx, res.Version)
}

// startJob fans PrePull out to the fleet nodes and arms the timeout. With no
// live nodes to warm the flip happens immediately (vacuously pulled).
func (m *Manager) startJob(ctx context.Context, v store.Version) (Status, error) {
	targets, err := m.st.PrePullTargets(ctx, v.ProjectID, v.Env)
	if err != nil {
		return Status{}, err
	}
	if len(targets) == 0 {
		m.log.Warn("deploy: no live fleet nodes to prepull — activating immediately",
			"version_id", v.ID, "semver", v.Semver)
		if err := m.activate(ctx, v, time.Now()); err != nil {
			return Status{}, err
		}
		v.State = "active"
		return Status{Version: v, State: "active"}, nil
	}

	j := &job{version: v, started: time.Now(), pending: map[string]bool{}}
	for _, n := range targets {
		j.pending[n.ID] = true
	}
	vid := v.ID
	j.timer = time.AfterFunc(m.timeout, func() { m.expire(vid) })

	m.mu.Lock()
	if old, ok := m.jobs[vid]; ok { // re-armed resume race — keep one timer
		old.timer.Stop()
	}
	m.jobs[vid] = j
	m.mu.Unlock()

	for _, n := range targets {
		cmdID := m.sender.Send(n.ID, &agentlinkv1.MasterMsg{
			Msg: &agentlinkv1.MasterMsg_Prepull{Prepull: &agentlinkv1.PrePull{
				ImageRef: v.ImageRef,
			}},
		})
		m.log.Info("deploy: prepull sent",
			"version_id", v.ID, "semver", v.Semver, "node_id", n.ID, "region", n.Region, "cmd_id", cmdID)
	}
	return Status{Version: v, State: "prepulling", PendingNodes: len(targets)}, nil
}

// Resume re-arms prepull jobs for versions left `prepulling` by a previous
// master process. Call once at startup.
func (m *Manager) Resume(ctx context.Context) error {
	versions, err := m.st.PrepullingVersions(ctx)
	if err != nil {
		return err
	}
	for _, v := range versions {
		m.log.Info("deploy: resuming interrupted prepull", "version_id", v.ID, "semver", v.Semver)
		if _, err := m.startJob(ctx, v); err != nil {
			m.log.Error("deploy: resume failed", "version_id", v.ID, "err", err)
		}
	}
	return nil
}

// HandlePullReport consumes agent PullReports (wired into the agentlink
// service): `pulled` from the last pending node triggers the atomic flip;
// `failed` aborts the deploy; `pulling` is progress noise.
func (m *Manager) HandlePullReport(nodeID string, r *agentlinkv1.PullReport) {
	status := r.GetStatus()
	if status != "pulled" && status != "failed" {
		return
	}
	imageRef := r.GetImageRef()

	// C2 (environments v1 §3): выбираем job по (image_ref, нода ещё в pending
	// ЭТОГО job'а), а не по одному image_ref. Иначе два параллельных деплоя
	// одного ref в разных env (промоут!) съедают отчёты друг друга и ложно
	// abort'ятся по таймауту. Нода принадлежит одному env → матч уникален; отчёт
	// уже отработавшей ноды не находит job (j==nil) и штатно игнорируется.
	m.mu.Lock()
	var j *job
	for _, cand := range m.jobs {
		if cand.version.ImageRef == imageRef && cand.pending[nodeID] {
			j = cand
			break
		}
	}
	if j == nil {
		m.mu.Unlock()
		return // not ours (ad-hoc prepull, a duplicate, or a long-gone deploy)
	}
	if status == "failed" {
		delete(m.jobs, j.version.ID)
		j.timer.Stop()
		m.mu.Unlock()
		m.log.Error("deploy: node reported pull failed — aborting",
			"version_id", j.version.ID, "node_id", nodeID, "detail", r.GetDetail())
		m.abort(j.version.ID, "node "+nodeID+" pull failed: "+r.GetDetail())
		return
	}
	if !j.pending[nodeID] {
		m.mu.Unlock()
		return // duplicate report
	}
	delete(j.pending, nodeID)
	done := len(j.pending) == 0
	if done {
		delete(m.jobs, j.version.ID)
		j.timer.Stop()
	}
	remaining := len(j.pending)
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nid, vid := nodeID, j.version.ID
	if err := m.st.InsertEvent(ctx, store.EventDeployNodePulled,
		store.EventRef{NodeID: &nid, VersionID: &vid},
		map[string]any{"image_ref": imageRef, "remaining": remaining}); err != nil {
		m.log.Error("deploy: event write failed", "err", err)
	}
	m.log.Info("deploy: node pulled",
		"version_id", j.version.ID, "node_id", nodeID, "remaining", remaining)

	if done {
		if err := m.activate(ctx, j.version, j.started); err != nil {
			m.log.Error("deploy: activation failed", "version_id", j.version.ID, "err", err)
		}
	}
}

// activate performs the atomic flip (step 3) and records the prepull time.
func (m *Manager) activate(ctx context.Context, v store.Version, started time.Time) error {
	res, err := m.st.ActivateVersion(ctx, v.ID, "prepulling", store.EventDeployActivated, nil)
	if err != nil {
		return err
	}
	elapsed := time.Since(started)
	m.observePrepull(elapsed.Seconds())
	m.log.Info("deploy: activated",
		"version_id", res.Version.ID, "semver", res.Version.Semver,
		"old_semver", res.PrevSemver, "regions", res.Regions,
		"prepull_seconds", elapsed.Seconds())
	return nil
}

// expire aborts a deploy whose prepull timed out.
func (m *Manager) expire(versionID string) {
	m.mu.Lock()
	j, ok := m.jobs[versionID]
	if ok {
		delete(m.jobs, versionID)
	}
	m.mu.Unlock()
	if !ok {
		return // completed/aborted meanwhile
	}
	m.log.Error("deploy: prepull timed out — aborting",
		"version_id", versionID, "semver", j.version.Semver, "pending_nodes", len(j.pending))
	m.abort(versionID, "prepull timeout")
}

func (m *Manager) abort(versionID, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.st.AbortDeploy(ctx, versionID, reason); err != nil {
		m.log.Error("deploy: abort failed", "version_id", versionID, "err", err)
	}
}

// Rollback flips an environment's deprecated version back to active
// (POST /v1/rollback): images are already on the nodes — no prepull, the
// whole operation is one transaction. regions empty → all of that env's fleets;
// otherwise only those regions' fleet_configs are repointed. env-скоуп
// (environments v1 §3): откат живёт строго внутри (project, env).
func (m *Manager) Rollback(ctx context.Context, project, env string, regions []string) (store.ActivateResult, error) {
	target, err := m.st.RollbackTarget(ctx, project, env)
	if err != nil {
		return store.ActivateResult{}, err
	}
	res, err := m.st.ActivateVersion(ctx, target.ID, "deprecated", store.EventDeployRolledBack, regions)
	if err != nil {
		return store.ActivateResult{}, err
	}
	m.log.Info("deploy: rolled back",
		"version_id", res.Version.ID, "semver", res.Version.Semver,
		"demoted_semver", res.PrevSemver, "regions", res.Regions)
	return res, nil
}

// PendingNodes reports the in-flight prepull size for a version (tests).
func (m *Manager) PendingNodes(versionID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[versionID]; ok {
		return len(j.pending)
	}
	return 0
}
