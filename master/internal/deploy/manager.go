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

// ImageCleaner dispatches RemoveImage for versions a flip just pushed to
// `disabled` (environments v1 §6б: the flip-demote of an older deprecated is the
// main dev-stream retire path). Satisfied by *reconcile.ImageCleaner; a
// structural interface keeps deploy from importing reconcile. Optional — nil in
// tests that do not assert on image cleanup.
type ImageCleaner interface {
	CleanupImages(ctx context.Context, disabled []store.DisabledVersion) error
}

// Manager orchestrates deploys. One in-flight prepull per (project, env)
// (enforced by store.BeginDeploy's env-scoped busy-check).
type Manager struct {
	st           *store.Store
	sender       Sender
	log          *slog.Logger
	timeout      time.Duration
	imageCleaner ImageCleaner // RemoveImage dispatch on flip-demote disabled (§6б); optional

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
	ImageCleaner   ImageCleaner          // RemoveImage dispatch on flip-demote (§6б), optional
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
		imageCleaner:  o.ImageCleaner,
		jobs:          map[string]*job{},
		lastAttempted: map[projectEnv]store.CreatedID{},
	}
}

// WarnNoLiveNodes marks a deploy that had NOTHING to roll onto: the version's
// (project, env) has no live node at all, so the flip happened vacuously and no
// dedic will ever appear (tracker #1071). Machine-readable on purpose — a CI that
// calls POST /v1/deploy can branch on it; before this the same call answered a
// bare `{"state":"active","pending_nodes":0}`, i.e. plain success.
const WarnNoLiveNodes = "no_live_nodes"

// Status is the POST /v1/deploy outcome.
type Status struct {
	Version store.Version `json:"version"`
	// State after the call: prepulling | active.
	State string `json:"state"`
	// Nodes still being warmed (0 when the flip already happened).
	PendingNodes int `json:"pending_nodes"`
	// Warning is a non-fatal caveat about THIS deploy — today only
	// WarnNoLiveNodes. Empty (and absent from the JSON) when there is none.
	Warning string `json:"warning,omitempty"`
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
		// Идемпотентный повтор — ровно тот путь, на который встаёт недоумевающий
		// оператор («деплой прошёл, дедиков нет — накачу ещё раз»), поэтому
		// предупреждение обязано быть и здесь (tracker #1071). Событие НЕ пишем:
		// нового деплоя не случилось, и повторы засорили бы ленту.
		return Status{Version: res.Version, State: "active", Warning: m.noNodesWarning(ctx, res.Version)}, nil
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
		// Тот же факт, но НАБЛЮДАЕМЫЙ (tracker #1071). Пишем ДО активации, чтобы
		// в ленте (id desc) причина стояла под своим deploy_activated, а не над
		// ним; подробности — в recordNoNodes.
		m.recordNoNodes(ctx, v)
		if err := m.activate(ctx, v, time.Now()); err != nil {
			return Status{}, err
		}
		v.State = "active"
		return Status{Version: v, State: "active", Warning: WarnNoLiveNodes}, nil
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

// noNodesWarning reports WarnNoLiveNodes when the version's (project, env) has
// no live node to run on, and "" otherwise. A read-only probe over the same
// PrePullTargets query startJob uses, so both answers mean the same thing. A
// query error yields "" — a missing warning is better than a wrong one.
func (m *Manager) noNodesWarning(ctx context.Context, v store.Version) string {
	return m.NoNodesWarning(ctx, v.ProjectID, v.Env, v.ID)
}

// NoNodesWarning is the same probe addressed by (project, env) instead of by a
// version — the shape a caller holding a fleet_configs row has (PUT /v1/fleets,
// tracker #1088). versionID is used for the log line only; "" is fine.
//
// It is exported ON PURPOSE, and that is the whole point of #1088: the zero-
// capacity signal must not live inside ONE funnel. Its first home was startJob,
// which the manual deploy, promote, auto-deploy, Resume and SweepOrphans all
// reach — but startJob was never the STRUCTURAL sole writer of the active
// version, only today's most travelled one, and two paths already went around
// it (Rollback and the fleet override). Any NEW path that repoints
// fleet_configs.active_version is expected to call this.
//
// Обязанность держится тестом ЧАСТИЧНО, и знать границу важнее, чем верить в
// автоматику: active_version_paths_test.go краснеет на новом SQL-писателе в
// пакете store и на новом вызывающем ActivateVersion внутри пакета deploy, но
// НЕ видит ещё одну HTTP-ручку, зовущую уже зарегистрированный
// store.UpsertFleet, и не проверяет, что пробу кто-то реально позвал (это
// работа поведенческих пинов). Разбор границы — в шапке того файла.
func (m *Manager) NoNodesWarning(ctx context.Context, projectID, env, versionID string) string {
	targets, err := m.st.PrePullTargets(ctx, projectID, env)
	if err != nil {
		m.log.Error("deploy: no-nodes probe failed", "version_id", versionID, "err", err)
		return ""
	}
	if len(targets) == 0 {
		return WarnNoLiveNodes
	}
	return ""
}

// recordNoNodes writes the deploy_no_nodes event for a flip that had nothing to
// roll onto. Best-effort BY DESIGN: a failed event write must not fail the
// operation — it is already permitted by the store, and the same fact also
// reaches the caller as the Warning field.
//
// WHEN to call it relative to the flip is the caller's choice, and the two
// callers choose differently ON PURPOSE. The feed is read id-desc, so writing
// BEFORE puts the cause under its own deploy_activated — that is what startJob
// does, and it is safe there because BeginDeploy has already permitted the
// flip. Rollback writes AFTER, because the flip itself can still refuse; the
// reasoning is spelled out at the call site.
func (m *Manager) recordNoNodes(ctx context.Context, v store.Version) {
	vid := v.ID
	if err := m.st.InsertEvent(ctx, store.EventDeployNoNodes, store.EventRef{VersionID: &vid},
		map[string]any{"project": v.Project, "env": v.Env, "semver": v.Semver, "image_ref": v.ImageRef}); err != nil {
		m.log.Error("deploy: no-nodes event write failed", "version_id", v.ID, "err", err)
	}
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

// SweepOrphans re-arms any version stuck `prepulling` in the DB with no
// in-memory job — the orphan left when PrePullTargets errored AFTER BeginDeploy
// (manager.go package comment; environments v1 §4). Resume fixes these at
// startup; the reconcile loop calls this each pass (W2-реестр) to catch one that
// appears mid-run (e.g. a transient PrePullTargets error on the auto-deploy
// path). Idempotent: a version that already has a job is skipped, so a re-armed
// prepull is never double-fanned-out.
func (m *Manager) SweepOrphans(ctx context.Context) error {
	versions, err := m.st.PrepullingVersions(ctx)
	if err != nil {
		return err
	}
	for _, v := range versions {
		m.mu.Lock()
		_, hasJob := m.jobs[v.ID]
		m.mu.Unlock()
		if hasJob {
			continue
		}
		// TOCTOU: активация (HandlePullReport → activate) могла завершиться МЕЖДУ
		// выборкой PrepullingVersions и этой map-проверкой — тогда джобы нет по
		// легитимной причине (деплой завершён), а не потому, что она осиротела.
		// Ре-чек состояния из БД (дёшево — точечный GetVersion) отсекает лишний
		// re-arm/fan-out prepull'а по уже-неактуальной строке.
		fresh, err := m.st.GetVersion(ctx, v.ID)
		if err != nil {
			m.log.Error("deploy: orphan sweep re-check failed", "version_id", v.ID, "err", err)
			continue
		}
		if fresh.State != "prepulling" {
			continue
		}
		m.log.Warn("deploy: sweeping orphan prepulling version (no in-memory job) — re-arming",
			"version_id", fresh.ID, "semver", fresh.Semver, "env", fresh.Env)
		if _, err := m.startJob(ctx, fresh); err != nil {
			m.log.Error("deploy: orphan sweep re-arm failed", "version_id", fresh.ID, "err", err)
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
	// Флип-демоут старших deprecated → disabled: снимаем их образы с нод env
	// (environments v1 §6б, главный путь dev-потока).
	m.cleanupDisabledImages(ctx, res.Disabled)
	m.onDeployFinished(v.Project, v.Env)
	return nil
}

// cleanupDisabledImages dispatches RemoveImage for versions a flip pushed to
// `disabled` (environments v1 §6б). Best-effort: a nil cleaner (tests) or a
// dispatch error never fails the deploy — the agent EnsureImage self-heal and the
// watermark GC are the backstops.
func (m *Manager) cleanupDisabledImages(ctx context.Context, disabled []store.DisabledVersion) {
	if m.imageCleaner == nil || len(disabled) == 0 {
		return
	}
	if err := m.imageCleaner.CleanupImages(ctx, disabled); err != nil {
		m.log.Error("deploy: RemoveImage dispatch (flip-demote) failed", "err", err)
	}
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

// RollbackStatus is the Rollback outcome: the store's flip result plus the same
// non-fatal caveat POST /v1/deploy carries (tracker #1088). ActivateResult is
// embedded, so res.Version / res.PrevSemver read exactly as before.
type RollbackStatus struct {
	store.ActivateResult
	// Warning — today only WarnNoLiveNodes. Empty when there is none.
	Warning string
}

// Rollback flips an environment's deprecated version back to active
// (POST /v1/rollback): images are already on the nodes — no prepull, the
// whole operation is one transaction. regions empty → all of that env's fleets;
// otherwise only those regions' fleet_configs are repointed. env-скоуп
// (environments v1 §3): откат живёт строго внутри (project, env).
//
// Отсутствие прогрева — ровно причина, по которой откат не проходит через
// startJob и потому МОЛЧАЛ при нуле живых нод (tracker #1088): переключение
// fleet_configs.active_version случалось так же вхолостую, ответ был те же
// 200 `{"rollback":{…}}`, а оператор попадает сюда как раз в панике — «дедиков
// нет, откачу». Состояние достижимо честно: единственную ноду ревокнули
// (POST /v1/nodes/{id}/revoke → dead) или она замолчала. Теперь след тот же,
// что у деплоя, и обе половины намеренно совпадают с #1071: событие
// deploy_no_nodes в ленте (CI/панель) и поле warning в ответе (curl).
func (m *Manager) Rollback(ctx context.Context, project, env string, regions []string) (RollbackStatus, error) {
	target, err := m.st.RollbackTarget(ctx, project, env)
	if err != nil {
		return RollbackStatus{}, err
	}
	// Проба — до флипа (она read-only и от него не зависит: ноды считаются по
	// (project, env), а откат живёт внутри той же пары и её не меняет).
	warning := m.NoNodesWarning(ctx, target.ProjectID, target.Env, target.ID)
	res, err := m.st.ActivateVersion(ctx, target.ID, "deprecated", store.EventDeployRolledBack, regions)
	if err != nil {
		return RollbackStatus{}, err
	}
	m.log.Info("deploy: rolled back",
		"version_id", res.Version.ID, "semver", res.Version.Semver,
		"demoted_semver", res.PrevSemver, "regions", res.Regions)
	if warning != "" {
		m.log.Warn("deploy: rolled back onto a fleet with no live nodes",
			"version_id", res.Version.ID, "semver", res.Version.Semver, "project", project, "env", env)
		// ПОРЯДОК ЗДЕСЬ ОБРАТНЫЙ startJob'у, и это осознанно. Там событие
		// пишется ДО активации ради ленты (она читается id-desc, причина должна
		// лечь ПОД своё deploy_activated), и это безопасно: BeginDeploy уже
		// разрешил флип. Здесь же отказать может ровно ActivateVersion —
		// неизвестный регион (ErrNotFound), проигранная гонка состояний
		// (ErrVersionState), — и deploy_no_nodes, записанный до неё, был бы
		// событием об откате, которого НЕ БЫЛО. Врать о случившемся хуже, чем
		// поставить причину строкой выше следствия: факт наблюдаем в обоих
		// случаях, соседней строкой.
		m.recordNoNodes(ctx, res.Version)
	}
	// Откат тоже флип: старшие deprecated вне окна → disabled, снимаем их образы.
	m.cleanupDisabledImages(ctx, res.Disabled)
	return RollbackStatus{ActivateResult: res, Warning: warning}, nil
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
		// Нет такого env (ErrBadEnv) — штатный no-op, не повод шуметь в лог;
		// всё остальное — реальный сбой стора.
		if !errors.Is(err, store.ErrBadEnv) {
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
