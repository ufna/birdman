// Package reconcile implements the fleet controller loop
// (docs/specs/master.md §2) and the node lease checker
// (docs/specs/protocol.md §1 Lease).
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// Sender dispatches a command to a node's agent (implemented by
// agentlink.Hub; an in-memory fake is used in tests). It must not block:
// commands to offline nodes are queued and replayed on reconnect.
type Sender interface {
	Send(nodeID string, msg *agentlinkv1.MasterMsg) (cmdID string)
	SessionChecker
}

// SessionChecker reports whether a node holds a LIVE agentlink session right now
// (agentlink.Hub.HasSession). Send NEVER fails: a command for an offline node is
// merely PARKED in the hub's IN-MEMORY pending queue. The image-cleanup sweep is a
// REPEATER — every 60s it re-sends RemoveImage for each version still missing its
// image_cleanup_at marker — so parking is exactly what it must not do: a node that
// is offline for a day would accumulate one duplicate per subtick and get the whole
// pile dumped on it at reconnect. The sweep therefore sends ONLY to nodes with a
// live session, and skips a version entirely while any target node of its env is
// offline (imagecleanup.go, M2). Part of Sender so the wiring is compile-time
// enforced: the only production implementation is the hub itself.
type SessionChecker interface {
	HasSession(nodeID string) bool
}

// OrphanSweeper re-arms deploy jobs stuck `prepulling` with no in-memory job
// (the PrePullTargets-errored-after-BeginDeploy orphan, environments v1 §4 /
// W2-реестр). Satisfied by *deploy.Manager (SweepOrphans); a structural interface
// keeps reconcile from importing deploy. Optional — nil in tests without a
// manager.
type OrphanSweeper interface {
	SweepOrphans(ctx context.Context) error
}

const (
	// Servers stuck in `creating` without agent progress for this long are
	// failed so the deficit is re-created (уточнено в v0, master.md §2).
	stuckCreatingTimeout = 120 * time.Second

	// Crash-loop policy (master.md §2): ≥3 failed of one (version,node)
	// within 10 min → pause creations of that pair for 15 min.
	crashLoopFails  = 3
	crashLoopWindow = 10 * time.Minute
	crashLoopPause  = 15 * time.Minute

	stopGraceSeconds = 30

	// retentionInterval throttles the version-retention subtick inside the 1 Hz
	// reconcile loop (environments v1 §6а M5: ~раз в 60с). Time-based (not a tick
	// counter) so it is robust to the loop interval and fires on the first RunOnce
	// (zero lastRetention) — a single RunOnce in a test exercises retention.
	retentionInterval = 60 * time.Second
)

type Reconciler struct {
	st      *store.Store
	sender  Sender
	log     *slog.Logger
	cleaner *ImageCleaner // RemoveImage dispatch (§6б) + the report-driven image_cleanup_at marker
	sweeper OrphanSweeper // optional: adopt orphan prepulling versions (W2-реестр)

	// (version,node) pairs already reported as crash-looping → pausedUntil,
	// to avoid re-emitting the crash_loop event every tick.
	reported map[pairKey]time.Time

	// lastRetention throttles the ~60s retention subtick (§6а M5). Written only
	// from RunOnce, which the loop serialises — no lock needed.
	lastRetention time.Time
}

type pairKey struct{ versionID, nodeID string }

func New(st *store.Store, sender Sender, log *slog.Logger) *Reconciler {
	return &Reconciler{
		st: st, sender: sender, log: log,
		cleaner:  NewImageCleaner(st, sender, log),
		reported: map[pairKey]time.Time{},
	}
}

// WithImageCleaner replaces the loop's built-in dispatcher with the SHARED one
// (main.go). It MUST be the very instance the agentlink service routes agent
// ImageReports to (agentlink.Service.WithImageSink) and the deploy manager
// flip-demotes through: the sweep's image_cleanup_at marker is now stamped by the
// cleaner when those reports come back, so a cleaner that never hears them would
// dispatch forever and mark nothing. Left at the built-in instance in tests that
// drive the loop without an agentlink service (they report straight into it).
// Returns the receiver for chaining.
func (r *Reconciler) WithImageCleaner(c *ImageCleaner) *Reconciler {
	if c != nil {
		r.cleaner = c
	}
	return r
}

// WithOrphanSweeper wires the deploy manager's orphan sweep into the loop
// (W2-реестр): a version stuck `prepulling` with no in-memory job is re-armed
// each pass. Returns the receiver for chaining in main.go. Left nil in tests
// that construct no deploy manager.
func (r *Reconciler) WithOrphanSweeper(s OrphanSweeper) *Reconciler {
	r.sweeper = s
	return r
}

// Run ticks RunOnce every interval until ctx is done.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
				r.log.Error("reconcile: pass failed", "err", err)
			}
		}
	}
}

// RunOnce performs a single reconcile pass over all fleets.
func (r *Reconciler) RunOnce(ctx context.Context) error {
	// Close expired multi-version windows first (итерация 3, master.md §5):
	// deprecated versions past reap_ttl_min go disabled, so this very pass reaps
	// their buffers and drains their live matches below. Every disabled transition
	// dispatches RemoveImage to the env's nodes (environments v1 §6б).
	disabled, err := r.st.DisableExpiredDeprecated(ctx)
	if err != nil {
		return err
	}
	if len(disabled) > 0 {
		retired := make([]store.DisabledVersion, 0, len(disabled))
		for _, v := range disabled {
			r.log.Info("reconcile: deprecated version disabled by reap_ttl",
				"version_id", v.ID, "semver", v.Semver)
			retired = append(retired, store.DisabledVersion{
				VersionID: v.ID, ProjectID: v.ProjectID, Env: v.Env,
				ImageRef: v.ImageRef, Semver: v.Semver,
			})
		}
		if err := r.cleaner.CleanupImages(ctx, retired); err != nil {
			r.log.Error("reconcile: RemoveImage dispatch (reap_ttl) failed", "err", err)
		}
	}

	// Version retention subtick (~60s, environments v1 §6а): registered versions
	// beyond retention_keep and older than 1h go disabled (the only registered→
	// disabled path); their images are removed from the env's nodes too.
	if time.Since(r.lastRetention) >= retentionInterval {
		r.lastRetention = time.Now()
		if retired, err := r.st.RetireVersions(ctx); err != nil {
			r.log.Error("reconcile: version retention failed", "err", err)
		} else if len(retired) > 0 {
			for _, d := range retired {
				r.log.Info("reconcile: version retired by retention",
					"version_id", d.VersionID, "semver", d.Semver, "env", d.Env)
			}
			if err := r.cleaner.CleanupImages(ctx, retired); err != nil {
				r.log.Error("reconcile: RemoveImage dispatch (retention) failed", "err", err)
			}
		}
		r.sweepImageCleanup(ctx)
	}

	// Adopt any orphan prepulling deploy (W2-реестр): a version stuck `prepulling`
	// with no in-memory job (transient PrePullTargets error after BeginDeploy).
	// Resume fixes these at startup; this catches one that appears mid-run.
	if r.sweeper != nil {
		if err := r.sweeper.SweepOrphans(ctx); err != nil {
			r.log.Error("reconcile: orphan prepull sweep failed", "err", err)
		}
	}

	if n, err := r.st.FailStuckCreating(ctx, stuckCreatingTimeout); err != nil {
		return err
	} else if n > 0 {
		r.log.Warn("reconcile: failed stuck creating servers", "count", n)
	}

	fleets, err := r.st.ListFleetConfigs(ctx)
	if err != nil {
		return err
	}
	for _, f := range fleets {
		if f.ActiveVersion == nil {
			continue
		}
		if err := r.reconcileFleet(ctx, f); err != nil {
			r.log.Error("reconcile: fleet pass failed",
				"project", f.Project, "env", f.Env, "region", f.Region, "err", err)
		}
	}
	return nil
}

// sweepImageCleanup is the CONVERGING half of image cleanup (Фаза D, дефект
// стенда), run in the ~60s cleanup subtick next to retention.
//
// Немедленная отправка RemoveImage при переходе версии в disabled (флип-демоут,
// reap-TTL, ретеншн) остаётся быстрым путём — но она гоняется с дренажом: в этот
// самый момент серверы версии ещё дренятся (их только что выгнали из окна, grace
// 30с), агент видит образ занятым живым контейнером и скипает команду, а повторить
// её было НЕКОМУ. Sweep берёт disabled-версии, у которых контейнеров уже не
// осталось (и ref не держит не-disabled версия того же (project, env)), и шлёт
// RemoveImage повторно — ровно один раз на версию: маркер image_cleanup_at гасит
// её в выборке, поэтому команда не спамится каждый субтик.
//
// Ошибка диспатча оставляет маркер пустым — следующий субтик повторит (доставка и
// так at-least-once, а RemoveImage идемпотентна: агент no-op'ит отсутствующий образ).
//
// МАРКЕР СТАВИТСЯ НЕ ЗДЕСЬ. Раньше sweep штамповал image_cleanup_at по факту Send —
// вслепую: протокол не нёс результата RemoveImage (Ack подтверждает лишь получение
// команды), поэтому пропущенное удаление (образ занят дренящимся контейнером,
// ошибка рантайма) терялось навсегда — версия выпадала из выборки, а образ доживал
// до watermark-GC. Теперь агент отвечает ImageReport{removed|absent|busy|error}, и
// маркер ставит ImageCleaner.HandleImageReport — только когда КАЖДАЯ целевая нода
// подтвердила, что образа у неё больше нет. Здесь мы лишь отправляем и логируем
// исход прохода (SweepResult): Dispatched — команды ушли, ждём отчётов; Skipped —
// версия пропущена (офлайн-нода M2 или shared-ref); Marked — помечена сразу (в
// окружении нет живых нод, удалять негде).
//
// Рестарт мастера теряет ожидания (они in-memory) — и это безопасно: маркера у
// версии нет, значит она осталась в выборке и следующий субтик отправит RemoveImage
// заново. Сходимость по построению: маркер = подтверждённое удаление, всё
// остальное = повтор.
func (r *Reconciler) sweepImageCleanup(ctx context.Context) {
	pending, err := r.st.VersionsPendingImageCleanup(ctx)
	if err != nil {
		r.log.Error("reconcile: image cleanup sweep query failed", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	res, err := r.cleaner.SweepImages(ctx, pending)
	if err != nil {
		r.log.Error("reconcile: RemoveImage dispatch (sweep) failed", "err", err)
		return // маркер не ставим — догоним на следующем субтике
	}
	r.log.Info("reconcile: image cleanup sweep — RemoveImage для отдренившихся версий",
		"versions", len(pending), "dispatched", res.Dispatched, "skipped", res.Skipped, "marked", res.Marked)
}

func (r *Reconciler) reconcileFleet(ctx context.Context, f store.FleetConfig) error {
	dep, err := r.st.DeprecatedWindowVersion(ctx, f.ProjectID, f.Env)
	if err != nil {
		return err
	}
	paused := map[string][]string{}
	for _, vid := range windowVersionIDs(f, dep) {
		p, err := r.pausedNodes(ctx, vid, f)
		if err != nil {
			return err
		}
		paused[vid] = p
	}
	starts, stops, drains, locked, err := r.st.PlanFleet(ctx, f, dep, paused)
	if err != nil {
		return err
	}
	if !locked {
		return nil // another master holds the fleet lock
	}
	for _, p := range starts {
		cmdID := r.sender.Send(p.NodeID, &agentlinkv1.MasterMsg{
			Msg: &agentlinkv1.MasterMsg_Start{Start: &agentlinkv1.StartServer{
				ServerId: p.ServerID,
				ImageRef: p.ImageRef,
				// BIRDMAN_ENV — игровой сервер знает своё окружение (конфиги/
				// аналитика). Заполнение существующего map-поля, ноль диффов proto.
				// BIRDMAN_PROJECT (tracker #994) — тем же каналом едет вторая
				// половина пары: агент кладёт лог дедика в путь, несущий
				// (project, env), чтобы стрим в VictoriaLogs был размечен
				// владельцем. Пара берётся из ФЛОТА, под который дедик
				// запускается, то есть чеканится в момент старта — инвариант I6:
				// перевод НОДЫ в другое окружение (PATCH /v1/nodes/{id}) историю
				// уже запущенных дедиков не переписывает.
				Env:  map[string]string{"BIRDMAN_ENV": f.Env, "BIRDMAN_PROJECT": f.Project},
				Port: 0, // agent picks from its pool
			}},
		})
		r.log.Info("reconcile: start server",
			"server_id", p.ServerID, "node_id", p.NodeID, "image_ref", p.ImageRef, "cmd_id", cmdID)
	}
	for _, p := range stops {
		cmdID := r.sender.Send(p.NodeID, &agentlinkv1.MasterMsg{
			Msg: &agentlinkv1.MasterMsg_Stop{Stop: &agentlinkv1.StopServer{
				ServerId: p.ServerID,
				GraceS:   stopGraceSeconds,
			}},
		})
		r.log.Info("reconcile: stop server (surplus/stale)",
			"server_id", p.ServerID, "node_id", p.NodeID, "cmd_id", cmdID)
	}
	for _, p := range drains {
		cmdID := r.sender.Send(p.NodeID, &agentlinkv1.MasterMsg{
			Msg: &agentlinkv1.MasterMsg_DrainServer{DrainServer: &agentlinkv1.DrainServer{
				ServerId:  p.ServerID,
				DeadlineS: p.DeadlineS,
				Reason:    p.Reason,
			}},
		})
		r.log.Info("reconcile: drain server (live match plays out)",
			"server_id", p.ServerID, "node_id", p.NodeID, "reason", p.Reason, "cmd_id", cmdID)
	}
	return nil
}

func windowVersionIDs(f store.FleetConfig, dep *store.Version) []string {
	ids := []string{*f.ActiveVersion}
	if dep != nil && dep.ID != *f.ActiveVersion {
		ids = append(ids, dep.ID)
	}
	return ids
}

// pausedNodes derives crash-looping (version,node) pairs from recent
// failures. Restart-safe: input is the servers table, not process memory.
func (r *Reconciler) pausedNodes(ctx context.Context, versionID string, f store.FleetConfig) ([]string, error) {
	failures, err := r.st.RecentFailedTimes(ctx, versionID, f.Region, crashLoopWindow+crashLoopPause)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var paused []string
	for nodeID, times := range failures {
		until, looping := crashLoopPausedUntil(times, now)
		if !looping {
			continue
		}
		paused = append(paused, nodeID)
		key := pairKey{versionID, nodeID}
		if prev, ok := r.reported[key]; !ok || now.After(prev) {
			r.reported[key] = until
			nid, vid := nodeID, versionID
			if err := r.st.InsertEvent(ctx, store.EventCrashLoop,
				store.EventRef{NodeID: &nid, VersionID: &vid},
				map[string]any{"project": f.Project, "region": f.Region,
					"paused_until": until.UTC().Format(time.RFC3339)}); err != nil {
				return nil, err
			}
			r.log.Warn("reconcile: crash loop detected, pausing (version,node) pair",
				"node_id", nodeID, "version_id", versionID, "paused_until", until)
		}
	}
	return paused, nil
}

// crashLoopPausedUntil scans failure timestamps (ascending) for any
// crashLoopFails-sized run inside crashLoopWindow; the pause lasts
// crashLoopPause from the last failure of the newest such run.
func crashLoopPausedUntil(times []time.Time, now time.Time) (time.Time, bool) {
	var until time.Time
	for i := 0; i+crashLoopFails-1 < len(times); i++ {
		last := times[i+crashLoopFails-1]
		if last.Sub(times[i]) <= crashLoopWindow {
			if u := last.Add(crashLoopPause); u.After(until) {
				until = u
			}
		}
	}
	return until, until.After(now)
}
