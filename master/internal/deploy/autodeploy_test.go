package deploy_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Auto-deploy «только вперёд» (environments v1 §4): регистрация версии в
// auto_deploy-env немедленно катит новейшую registered; при burst'е деплоится
// только последняя, промежуточные пропускаются (skipped-учёт); упавшая версия
// не ретраится сама; Resume при рестарте восстанавливает и in-flight, и цепочку.

// deployStartedAuto reads the {auto, skipped} annotations from the newest
// deploy_started event of a version (payload keys absent on the manual path).
func deployStartedAuto(t *testing.T, st *store.Store, versionID string) (auto bool, skipped int) {
	t.Helper()
	var autoStr, skippedStr *string
	err := st.Pool.QueryRow(context.Background(), `
		select payload->>'auto', payload->>'skipped' from events
		where kind = $1 and version_id = $2::uuid
		order by ts desc limit 1`, store.EventDeployStarted, versionID).Scan(&autoStr, &skippedStr)
	if err != nil {
		t.Fatalf("read deploy_started payload for %s: %v", versionID, err)
	}
	if autoStr != nil && *autoStr == "true" {
		auto = true
	}
	if skippedStr != nil {
		skipped, _ = strconv.Atoi(*skippedStr)
	}
	return
}

// eventCount counts a version's events of one kind — the durable trace of what
// the manager actually did with it (unlike the version state, which is a
// transient).
func eventCount(t *testing.T, st *store.Store, kind, versionID string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(),
		`select count(*)::int from events where kind = $1 and version_id = $2::uuid`,
		kind, versionID).Scan(&n); err != nil {
		t.Fatalf("count %s for %s: %v", kind, versionID, err)
	}
	return n
}

// deployStartedCount counts deploy_started events of a version — how many times
// it was put in flight (forward-only: a skipped version has 0, a failed-and-
// -not-retried version has exactly 1).
func deployStartedCount(t *testing.T, st *store.Store, versionID string) int {
	t.Helper()
	return eventCount(t, st, store.EventDeployStarted, versionID)
}

// waitEvent polls until the version has at least want events of the given kind
// (the async, timer-driven paths: prepull expire → abort → chain).
//
// Ждать движение цепочки можно ТОЛЬКО по durable-фактам (строкам events), но не
// по состоянию версии. Состояние — транзиент: между abort'ом истёкшей версии
// (AbortDeploy: prepulling → registered) и BeginDeploy следующей цели
// (deploy_started + prepulling — одна транзакция) лежит окно в несколько
// запросов, в котором НЕ prepulling вообще никто. Прежний опрос «все вышли из
// prepulling» проскакивал сквозь это окно раньше, чем цепочка сдвигалась, и
// следом падал ассерт по событию — редкий FAIL под параллельной нагрузкой
// (флейк TestAutoDeployExpireMovesChain). Событие же пишется в той же
// транзакции, что и смена состояния, — сигнал строго не слабее.
func waitEvent(t *testing.T, st *store.Store, kind, versionID string, want int) {
	t.Helper()
	const (
		timeout = 10 * time.Second
		step    = 50 * time.Millisecond
	)
	deadline := time.Now().Add(timeout)
	for {
		n := eventCount(t, st, kind, versionID)
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout (%s) waiting for %d %s event(s) of version %s, got %d",
				timeout, want, kind, versionID, n)
		}
		time.Sleep(step)
	}
}

// Burst of three fast registrations in an auto_deploy env: only #1 prepulls
// (the busy-check serialises the rest → queued); on completion the chain jumps
// straight to #3 (newest registered), skipping #2 — deploy_started carries
// {auto:true, skipped:1}. #2 is never deployed («только вперёд»).
func TestAutoDeployBurstForwardOnly(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // node-1, dev auto_deploy, version 1.0.0
	f.UpsertFleet(t, 2, 50)
	m, rec, _ := newManager(t, st, time.Minute)
	ctx := context.Background()

	v1 := f.AddVersion(t, "1.1.0", "dev")
	if out := m.TryAutoDeploy(ctx, "game", "dev"); out != deploy.AutoDeployStarted {
		t.Fatalf("registering v1 must start a deploy, got %v", out)
	}
	v2 := f.AddVersion(t, "1.2.0", "dev")
	if out := m.TryAutoDeploy(ctx, "game", "dev"); out != deploy.AutoDeployQueued {
		t.Fatalf("v2 must queue behind the in-flight v1, got %v", out)
	}
	v3 := f.AddVersion(t, "1.3.0", "dev")
	if out := m.TryAutoDeploy(ctx, "game", "dev"); out != deploy.AutoDeployQueued {
		t.Fatalf("v3 must queue behind the in-flight v1, got %v", out)
	}

	if got := versionState(t, st, v1); got != "prepulling" {
		t.Fatalf("v1 must be prepulling, got %s", got)
	}
	if got := versionState(t, st, v2); got != "registered" {
		t.Fatalf("v2 must be registered, got %s", got)
	}
	if got := versionState(t, st, v3); got != "registered" {
		t.Fatalf("v3 must be registered, got %s", got)
	}
	rec.Take() // drop v1's prepull fan-out

	// v1 completes → chain deploys the NEWEST registered (v3), skipping v2.
	report(m, f.NodeID, mustVersion(t, st, v1).ImageRef, "pulled")
	if got := versionState(t, st, v1); got != "active" {
		t.Fatalf("v1 must be active after all nodes pulled, got %s", got)
	}
	if got := versionState(t, st, v3); got != "prepulling" {
		t.Fatalf("v3 must auto-deploy after v1, got %s", got)
	}
	if got := versionState(t, st, v2); got != "registered" {
		t.Fatalf("v2 must be skipped (still registered), got %s", got)
	}
	if pp := prepullsTo(rec); pp[f.NodeID] != mustVersion(t, st, v3).ImageRef {
		t.Fatalf("v3 prepull fan-out expected, got %+v", pp)
	}
	if auto, skipped := deployStartedAuto(t, st, v3); !auto || skipped != 1 {
		t.Fatalf("v3 deploy_started: auto=%v skipped=%d, want auto=true skipped=1", auto, skipped)
	}

	// v3 completes → active; v2 is never deployed.
	report(m, f.NodeID, mustVersion(t, st, v3).ImageRef, "pulled")
	if got := versionState(t, st, v3); got != "active" {
		t.Fatalf("v3 must be active, got %s", got)
	}
	if got := versionState(t, st, v2); got != "registered" {
		t.Fatalf("v2 must remain skipped, got %s", got)
	}
	if n := deployStartedCount(t, st, v2); n != 0 {
		t.Fatalf("v2 must never be deployed, got %d deploy_started", n)
	}
}

// A failed pull report aborts v1 and the chain moves forward to v3; the failed
// v1 is NOT auto-retried (its marker is already past it).
func TestAutoDeployAbortSkipsFailedVersion(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	m, _, _ := newManager(t, st, time.Minute)
	ctx := context.Background()

	v1 := f.AddVersion(t, "1.1.0", "dev")
	m.TryAutoDeploy(ctx, "game", "dev")
	v2 := f.AddVersion(t, "1.2.0", "dev")
	m.TryAutoDeploy(ctx, "game", "dev")
	v3 := f.AddVersion(t, "1.3.0", "dev")
	m.TryAutoDeploy(ctx, "game", "dev")

	// v1 pull fails → abort → chain jumps to v3 (skips v2), v1 back to registered.
	report(m, f.NodeID, mustVersion(t, st, v1).ImageRef, "failed")
	if got := versionState(t, st, v1); got != "registered" {
		t.Fatalf("failed v1 must be back to registered, got %s", got)
	}
	if got := versionState(t, st, v3); got != "prepulling" {
		t.Fatalf("v3 must auto-deploy after the abort, got %s", got)
	}
	if got := versionState(t, st, v2); got != "registered" {
		t.Fatalf("v2 must be skipped, got %s", got)
	}
	if _, skipped := deployStartedAuto(t, st, v3); skipped != 1 {
		t.Fatalf("v3 skipped=%d, want 1", skipped)
	}

	// v3 completes → active; v1 (failed) is never auto-retried.
	report(m, f.NodeID, mustVersion(t, st, v3).ImageRef, "pulled")
	if got := versionState(t, st, v1); got != "registered" {
		t.Fatalf("failed v1 must not be auto-retried, got %s", got)
	}
	if n := deployStartedCount(t, st, v1); n != 1 {
		t.Fatalf("v1 must be attempted exactly once, got %d deploy_started", n)
	}
}

// The prepull-timeout completion path advances the chain the same way as a
// failed report: v1 expires (not retried), v3 is attempted (skips v2). Both the
// wait and the asserts ride durable events — no reliance on transient states.
func TestAutoDeployExpireMovesChain(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	// Таймаут prepull'а — с запасом больше, чем регистрация трёх версий: v1 обязан
	// быть ещё в полёте, когда v2/v3 встают за ним в очередь (истеки он прямо в
	// сетапе — цепочка выкатила бы v2 и снесла бы саму предпосылку теста). Ждём мы
	// всё равно по событиям, а не по этой константе, так что запас ничего не стоит.
	m, _, _ := newManager(t, st, 500*time.Millisecond)
	ctx := context.Background()

	v1 := f.AddVersion(t, "1.1.0", "dev")
	m.TryAutoDeploy(ctx, "game", "dev")
	v2 := f.AddVersion(t, "1.2.0", "dev")
	m.TryAutoDeploy(ctx, "game", "dev")
	v3 := f.AddVersion(t, "1.3.0", "dev")
	m.TryAutoDeploy(ctx, "game", "dev")

	// v1 expires → the chain attempts v3 → v3 expires too → nothing newer is left.
	// Обе точки ждём по durable-событиям: deploy_started(v3) — цепочка сдвинулась;
	// deploy_failed(v3) — второй истёк и прошёл ещё один (уже пустой) проход
	// цепочки, то есть счётчики ниже устоялись.
	waitEvent(t, st, store.EventDeployStarted, v3, 1)
	waitEvent(t, st, store.EventDeployFailed, v3, 1)
	if n := deployStartedCount(t, st, v1); n != 1 {
		t.Fatalf("v1 attempted once (expired, not retried), got %d", n)
	}
	if n := deployStartedCount(t, st, v2); n != 0 {
		t.Fatalf("v2 skipped by forward-only, got %d deploy_started", n)
	}
	if n := deployStartedCount(t, st, v3); n != 1 {
		t.Fatalf("v3 attempted once via the expire path, got %d", n)
	}
	if _, skipped := deployStartedAuto(t, st, v3); skipped != 1 {
		t.Fatalf("v3 skipped=%d, want 1", skipped)
	}
}

// A production environment cannot carry auto_deploy (DB CHECK). A direct
// TryAutoDeploy on prod is a no-op: nothing prepulls, the version stays
// registered.
func TestAutoDeployProductionIsNoop(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	m, rec, _ := newManager(t, st, time.Minute)
	ctx := context.Background()

	vp := f.AddVersion(t, "2.0.0", "prod")
	if out := m.TryAutoDeploy(ctx, "game", "prod"); out != deploy.AutoDeployNoop {
		t.Fatalf("prod TryAutoDeploy must be a no-op, got %v", out)
	}
	if got := versionState(t, st, vp); got != "registered" {
		t.Fatalf("prod version must stay registered, got %s", got)
	}
	if pp := prepullsTo(rec); len(pp) != 0 {
		t.Fatalf("prod must not prepull anything, got %+v", pp)
	}
}

// A master restart with an active head and a registered tail: Resume
// reconstructs the forward-only marker (newest non-registered version) and
// continues the chain — the newest registered tail deploys, older ones skipped.
func TestAutoDeployResumeContinuesChain(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	ctx := context.Background()

	m1, _, _ := newManager(t, st, time.Minute)
	v1 := f.AddVersion(t, "1.1.0", "dev")
	m1.TryAutoDeploy(ctx, "game", "dev")
	report(m1, f.NodeID, mustVersion(t, st, v1).ImageRef, "pulled") // v1 active
	v2 := f.AddVersion(t, "1.2.0", "dev")                           // registered tail
	v3 := f.AddVersion(t, "1.3.0", "dev")                           // registered tail

	// "restart": a fresh manager loses the in-memory marker and jobs.
	m2, rec2, _ := newManager(t, st, time.Minute)
	if err := m2.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if got := versionState(t, st, v3); got != "prepulling" {
		t.Fatalf("resume must continue the chain to v3, got %s", got)
	}
	if got := versionState(t, st, v2); got != "registered" {
		t.Fatalf("v2 must be skipped by forward-only, got %s", got)
	}
	if pp := prepullsTo(rec2); pp[f.NodeID] != mustVersion(t, st, v3).ImageRef {
		t.Fatalf("resume must prepull v3, got %+v", pp)
	}
	report(m2, f.NodeID, mustVersion(t, st, v3).ImageRef, "pulled")
	if got := versionState(t, st, v3); got != "active" {
		t.Fatalf("resumed chain must activate v3, got %s", got)
	}
}

// v5: a version that was ATTEMPTED and then aborted (prepull failed/timeout →
// back to `registered`) must NOT be re-attacked after a master restart. The bug:
// Resume rebuilt the forward-only marker from version STATE (newest non-
// registered), but an abort returns the version to `registered`, so the attempt
// was forgotten — a fresh Resume re-picked and re-deployed the same failed build,
// once per restart. The fix reconstructs the marker from deploy_started EVENTS,
// so the attempt is remembered even after the rollback to registered.
//
// Burst of two versions; the chain rolls the newest (vB); it fails (abort →
// registered); nothing newer exists → the chain stops. A fresh Manager + Resume
// must leave vB alone: exactly one deploy_started, still registered, no prepull.
func TestAutoDeployResumeDoesNotRetryFailedVersion(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev auto_deploy, version 1.0.0, node
	f.UpsertFleet(t, 2, 50)
	ctx := context.Background()

	m1, _, _ := newManager(t, st, time.Minute)
	// Burst: two near-simultaneous registrations, no TryAutoDeploy between them.
	vA := f.AddVersion(t, "1.1.0", "dev")
	vB := f.AddVersion(t, "1.2.0", "dev") // newest → the chain's target
	_ = vA

	// One chain pass rolls the newest registered = vB (1.0.0 and vA skipped).
	if out := m1.TryAutoDeploy(ctx, "game", "dev"); out != deploy.AutoDeployStarted {
		t.Fatalf("burst head must start a deploy, got %v", out)
	}
	if got := versionState(t, st, vB); got != "prepulling" {
		t.Fatalf("vB must be prepulling, got %s", got)
	}

	// vB fails → abort → back to registered; nothing newer → the chain stops.
	report(m1, f.NodeID, mustVersion(t, st, vB).ImageRef, "failed")
	if got := versionState(t, st, vB); got != "registered" {
		t.Fatalf("aborted vB must be back to registered, got %s", got)
	}
	if n := deployStartedCount(t, st, vB); n != 1 {
		t.Fatalf("vB attempted exactly once before the restart, got %d", n)
	}

	// "Restart": a fresh manager (empty memory) + Resume. The marker must rebuild
	// PAST vB from its deploy_started event, otherwise vB is re-attacked.
	m2, rec2, _ := newManager(t, st, time.Minute)
	if err := m2.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if got := versionState(t, st, vB); got != "registered" {
		t.Fatalf("vB must NOT be re-attacked after resume, got %s", got)
	}
	if n := deployStartedCount(t, st, vB); n != 1 {
		t.Fatalf("vB must stay attempted exactly once across the restart, got %d deploy_started", n)
	}
	if pp := prepullsTo(rec2); len(pp) != 0 {
		t.Fatalf("resume must not prepull anything (nothing newer than the failed head), got %+v", pp)
	}
}

// Recovery of a version left `prepulling` with no in-memory job (the master
// died right after BeginDeploy, before/at PrePullTargets): Resume re-arms it
// with a fresh prepull fan-out and it completes normally.
func TestAutoDeployResumeRecoversOrphanPrepulling(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	ctx := context.Background()

	v1 := f.AddVersion(t, "1.1.0", "dev")
	if _, err := st.Pool.Exec(ctx,
		`update versions set state = 'prepulling' where id = $1::uuid`, v1); err != nil {
		t.Fatal(err)
	}

	m, rec, _ := newManager(t, st, time.Minute)
	if err := m.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if pp := prepullsTo(rec); pp[f.NodeID] != mustVersion(t, st, v1).ImageRef {
		t.Fatalf("resume must re-arm the orphan prepull, got %+v", pp)
	}
	report(m, f.NodeID, mustVersion(t, st, v1).ImageRef, "pulled")
	if got := versionState(t, st, v1); got != "active" {
		t.Fatalf("orphan prepulling must complete via resume, got %s", got)
	}
}
