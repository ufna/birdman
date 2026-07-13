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
	"errors"
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

// Manager orchestrates deploys. One in-flight prepull per (project, env)
// (enforced by store.BeginDeploy's env-scoped busy-check).
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
	// lastAttempted is the auto-deploy «только вперёд» high-water mark per
	// (project, env): the newest version the manager has put in flight there.
	// A completion only ever rolls a version strictly newer than this, so a
	// failed build is never auto-retried and the chain never goes backwards
	// (environments v1 §4). Advanced monotonically on every successful
	// BeginDeploy (manual or auto — see recordAttempt); read/reset under mu.
	lastAttempted map[projectEnv]store.CreatedID
}

// projectEnv keys the per-environment forward-only marker.
type projectEnv struct{ project, env string }

// AutoDeployOutcome is what TryAutoDeploy did, surfaced in the POST /v1/versions
// 201 response ("auto_deploy": …) for an auto_deploy env.
type AutoDeployOutcome int

const (
	// AutoDeployNoop: the env is not auto_deploy (or is gone), or the chain has
	// nothing newer to roll — no field is added to the response.
	AutoDeployNoop AutoDeployOutcome = iota
	// AutoDeployStarted: a deploy began (prepulling, or activated immediately
	// with no live nodes) — "auto_deploy":"started".
	AutoDeployStarted
	// AutoDeployQueued: a deploy of this (project, env) is already in flight, or
	// the env has no fleet yet — the version waits for the next completion —
	// "auto_deploy":"queued".
	AutoDeployQueued
)

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
		jobs:          map[string]*job{},
		lastAttempted: map[projectEnv]store.CreatedID{},
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
	res, err := m.st.BeginDeploy(ctx, versionID, store.BeginDeployOpts{})
	if err != nil {
		return Status{}, err
	}
	// Advance the forward-only marker on any successful BeginDeploy, manual
	// included: a manually deployed-then-failed version must not be auto-retried,
	// and the auto chain must not roll back past a manual deploy (environments
	// v1 §4). recordAttempt is monotonic, so a manual redeploy of an older
	// version never drags the marker backwards.
	m.recordAttempt(res.Version)
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

// Resume recovers deploy state after a master restart. Call once at startup
// (before serving), in three ordered phases (environments v1 §4):
//
//	A. rebuild the auto-deploy forward-only marker for every auto_deploy env
//	   (newest version with a deploy_started event — so an aborted attempt that
//	   is back to `registered` is still remembered, follow-up v5) — BEFORE
//	   re-arming, so a re-armed prepull that activates immediately (no live nodes)
//	   already sees the marker and cannot pick a version older than the one just
//	   resumed;
//	B. re-arm prepull jobs left in flight — this also recovers a version stuck
//	   `prepulling` with no in-memory job (PrePullTargets errored post-BeginDeploy):
//	   it gets a fresh fan-out + timeout;
//	C. kick the chain per auto_deploy env — start the newest registered tail
//	   where nothing is in flight; where a prepull was re-armed the busy-check
//	   makes TryAutoDeploy wait for that deploy's completion.
func (m *Manager) Resume(ctx context.Context) error {
	autoEnvs, err := m.st.AutoDeployEnvironments(ctx)
	if err != nil {
		return err
	}
	for _, e := range autoEnvs {
		// Маркер восстанавливаем по СОБЫТИЯМ deploy_started, а не по состоянию:
		// abort возвращает упавшую версию в registered, и признак «не registered»
		// её забывал — Resume ре-атаковал бы её по разу на рестарт (follow-up v5).
		mk, ok, err := m.st.NewestAttemptedMarker(ctx, e.ProjectID, e.Name)
		if err != nil {
			return err
		}
		if ok {
			m.mu.Lock()
			m.lastAttempted[projectEnv{e.Project, e.Name}] = mk
			m.mu.Unlock()
		}
	}

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

	for _, e := range autoEnvs {
		m.TryAutoDeploy(ctx, e.Project, e.Name)
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
	// abort'ятся по таймауту. Нода принадлежит одному env → матч уникален.
	// w9 (task-2): условие pending[nodeID] в выборке само отсеивает дубликат —
	// повторный отчёт уже снятой с pending ноды не находит job (j==nil) и штатно
	// игнорируется ниже, поэтому отдельной ветки «duplicate report» больше нет.
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
		m.abort(j.version, "node "+nodeID+" pull failed: "+r.GetDetail())
		return
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

// activate performs the atomic flip (step 3) and records the prepull time. It
// is a deploy completion point (all nodes pulled, or no live nodes to warm), so
// it re-triggers the forward-only auto-deploy chain afterwards.
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
	m.onDeployFinished(v.Project, v.Env)
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
	m.abort(j.version, "prepull timeout")
}

// abort rolls a prepulling version back to registered (timeout or a failed pull
// report). It is a deploy completion point, so it re-triggers the forward-only
// chain — the failed version itself is behind the marker and never re-picked.
func (m *Manager) abort(v store.Version, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.st.AbortDeploy(ctx, v.ID, reason); err != nil {
		m.log.Error("deploy: abort failed", "version_id", v.ID, "err", err)
	}
	m.onDeployFinished(v.Project, v.Env)
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

// --- Авто-деплой «только вперёд» (environments v1 §4) ---

// TryAutoDeploy rolls the newest `registered` version of an auto_deploy env that
// is strictly newer than the (project, env) last_attempted marker, serialised by
// BeginDeploy's env-scoped busy-check. It is called after every registration
// (the POST /v1/versions handler) and from every deploy completion
// (onDeployFinished), which together drive the chain forward.
//
// A no-op on a non-auto_deploy (or missing) env — production never auto-deploys.
// On a busy env (ErrDeployInProgress) it returns quietly (queued): that
// in-flight deploy's completion re-runs the chain. The marker advances under the
// busy-success (recordAttempt), so the just-attempted version — failed or not —
// is never re-picked, and intermediate builds are skipped («только вперёд»).
func (m *Manager) TryAutoDeploy(ctx context.Context, project, env string) AutoDeployOutcome {
	e, err := m.st.GetEnvironment(ctx, project, env)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			m.log.Error("auto-deploy: get environment failed", "project", project, "env", env, "err", err)
		}
		return AutoDeployNoop
	}
	if !e.AutoDeploy {
		return AutoDeployNoop
	}

	pe := projectEnv{project, env}
	m.mu.Lock()
	marker := m.lastAttempted[pe]
	m.mu.Unlock()

	target, ok, err := m.st.NewestRegisteredAfter(ctx, e.ProjectID, env, marker)
	if err != nil {
		m.log.Error("auto-deploy: newest-registered lookup failed", "project", project, "env", env, "err", err)
		return AutoDeployNoop
	}
	if !ok {
		return AutoDeployNoop // ничего новее отметки — цепочка исчерпана
	}
	skipped, err := m.st.CountRegisteredBetween(ctx, e.ProjectID, env, marker,
		store.CreatedID{CreatedAt: target.CreatedAt, ID: target.ID})
	if err != nil {
		m.log.Error("auto-deploy: skipped count failed", "project", project, "env", env, "err", err)
		return AutoDeployNoop
	}

	res, err := m.st.BeginDeploy(ctx, target.ID, store.BeginDeployOpts{Auto: true, Skipped: skipped})
	switch {
	case errors.Is(err, store.ErrDeployInProgress):
		return AutoDeployQueued // busy — завершение in-flight деплоя перезапустит цепочку
	case errors.Is(err, store.ErrNoFleet):
		return AutoDeployQueued // катить некуда, ждём флот; отметку НЕ двигаем
	case err != nil:
		m.log.Error("auto-deploy: begin deploy failed", "version_id", target.ID, "err", err)
		return AutoDeployNoop
	}

	// Отметку двигаем ПОД busy-успехом и ДО fan-out'а: гоночное завершение обязано
	// увидеть сдвинутый last_attempted, иначе перевыберет ту же цель.
	m.recordAttempt(res.Version)

	switch {
	case res.AlreadyActive:
		return AutoDeployStarted
	case res.AlreadyPrepulling:
		// Версию уже катит конкурентный путь. Если это осиротевший prepulling без
		// job'а (гонка с рестартом) — усыновляем свежим fan-out'ом; иначе no-op.
		m.mu.Lock()
		_, hasJob := m.jobs[res.Version.ID]
		m.mu.Unlock()
		if !hasJob {
			if _, err := m.startJob(ctx, res.Version); err != nil {
				m.log.Error("auto-deploy: adopt orphan prepulling failed", "version_id", res.Version.ID, "err", err)
			}
		}
		return AutoDeployStarted
	default:
		if _, err := m.startJob(ctx, res.Version); err != nil {
			m.log.Error("auto-deploy: start job failed", "version_id", res.Version.ID, "err", err)
			return AutoDeployNoop
		}
		return AutoDeployStarted
	}
}

// onDeployFinished re-triggers the forward-only chain after any deploy
// completion (activate / abort — all four completion points of §4). Its own
// short context: the caller may be a completion running under a tight ctx
// (HandlePullReport) or a timer goroutine (expire).
func (m *Manager) onDeployFinished(project, env string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m.TryAutoDeploy(ctx, project, env)
}

// recordAttempt advances the (project, env) forward-only marker to v — but only
// forwards (monotonic high-water mark), so an out-of-order or older deploy never
// drags it back. Called after every successful BeginDeploy (manual and auto).
func (m *Manager) recordAttempt(v store.Version) {
	if v.ID == "" {
		return
	}
	pe := projectEnv{v.Project, v.Env}
	m.mu.Lock()
	if newerThan(v.CreatedAt, v.ID, m.lastAttempted[pe]) {
		m.lastAttempted[pe] = store.CreatedID{CreatedAt: v.CreatedAt, ID: v.ID}
	}
	m.mu.Unlock()
}

// newerThan reports whether (created, id) sorts strictly after marker mk by
// (created_at, then id) — the same order the store's forward-only queries use.
func newerThan(created time.Time, id string, mk store.CreatedID) bool {
	if created.After(mk.CreatedAt) {
		return true
	}
	if created.Equal(mk.CreatedAt) {
		return id > mk.ID
	}
	return false
}
