package deploy_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

type prepullObs struct {
	mu   sync.Mutex
	vals []float64
}

func (o *prepullObs) observe(s float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.vals = append(o.vals, s)
}

func (o *prepullObs) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.vals)
}

func newManager(t *testing.T, st *store.Store, timeout time.Duration) (*deploy.Manager, *testdb.CommandRecorder, *prepullObs) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	rec := &testdb.CommandRecorder{}
	obs := &prepullObs{}
	m := deploy.New(deploy.Options{
		Store: st, Sender: rec, Log: log, Timeout: timeout, ObservePrepull: obs.observe,
	})
	return m, rec, obs
}

func versionState(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	v, err := st.GetVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v.State
}

func fleetActive(t *testing.T, st *store.Store, region string) string {
	t.Helper()
	fleets, err := st.ListFleetConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fleets {
		if f.Region == region && f.ActiveVersion != nil {
			return *f.ActiveVersion
		}
	}
	return ""
}

func prepullsTo(rec *testdb.CommandRecorder) map[string]string {
	out := map[string]string{} // nodeID → image_ref
	for _, c := range rec.Take() {
		if p := c.Msg.GetPrepull(); p != nil {
			out[c.NodeID] = p.GetImageRef()
		}
	}
	return out
}

func report(m *deploy.Manager, nodeID, imageRef, status string) {
	m.HandlePullReport(nodeID, &agentlinkv1.PullReport{ImageRef: imageRef, Status: status})
}

// Full deploy: prepull fan-out to every fleet node, flip on the last pulled
// report, fleet repointed, events + metric written (master.md §5 steps 1-3).
func TestDeployPrepullThenFlip(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // version 1.0.0
	nodeB := f.AddNode(t, "node-2", "203.0.113.11", 10)
	f.UpsertFleet(t, 2, 50) // active 1.0.0
	v2 := f.AddVersion(t, "1.1.0", "dev")
	m, rec, obs := newManager(t, st, time.Minute)
	ctx := context.Background()

	stt, err := m.Deploy(ctx, v2)
	if err != nil {
		t.Fatal(err)
	}
	if stt.State != "prepulling" || stt.PendingNodes != 2 {
		t.Fatalf("deploy status: %+v", stt)
	}
	if got := versionState(t, st, v2); got != "prepulling" {
		t.Fatalf("version state after deploy: %s", got)
	}
	pp := prepullsTo(rec)
	if len(pp) != 2 || pp[f.NodeID] != "ghcr.io/example/game-server:1.1.0" || pp[nodeB] != "ghcr.io/example/game-server:1.1.0" {
		t.Fatalf("prepull fan-out: %+v", pp)
	}
	if n, _ := st.CountEvents(ctx, store.EventDeployStarted); n != 1 {
		t.Fatalf("want 1 deploy_started event, got %d", n)
	}

	// `pulling` progress is noise; first `pulled` is not enough.
	report(m, f.NodeID, "ghcr.io/example/game-server:1.1.0", "pulling")
	report(m, f.NodeID, "ghcr.io/example/game-server:1.1.0", "pulled")
	report(m, f.NodeID, "ghcr.io/example/game-server:1.1.0", "pulled") // duplicate
	if got := versionState(t, st, v2); got != "prepulling" {
		t.Fatalf("flip before all nodes pulled: %s", got)
	}
	if m.PendingNodes(v2) != 1 {
		t.Fatalf("pending nodes: %d", m.PendingNodes(v2))
	}

	// The last node reports → atomic flip.
	report(m, nodeB, "ghcr.io/example/game-server:1.1.0", "pulled")
	if got := versionState(t, st, v2); got != "active" {
		t.Fatalf("new version state: %s", got)
	}
	if got := versionState(t, st, f.VersionID); got != "deprecated" {
		t.Fatalf("old version state: %s", got)
	}
	if got := fleetActive(t, st, "eu"); got != v2 {
		t.Fatalf("fleet active_version: %s, want %s", got, v2)
	}
	old, err := st.GetVersion(ctx, f.VersionID)
	if err != nil || old.DeprecatedAt == nil {
		t.Fatalf("deprecated_at not set: %+v %v", old, err)
	}
	if n, _ := st.CountEvents(ctx, store.EventDeployActivated); n != 1 {
		t.Fatalf("want 1 deploy_activated event, got %d", n)
	}
	if n, _ := st.CountEvents(ctx, store.EventDeployNodePulled); n != 2 {
		t.Fatalf("want 2 deploy_node_pulled events, got %d", n)
	}
	if obs.count() != 1 {
		t.Fatalf("prepull metric observations: %d", obs.count())
	}

	// Idempotency: deploying the active version again is a clean no-op.
	stt, err = m.Deploy(ctx, v2)
	if err != nil || stt.State != "active" {
		t.Fatalf("repeat deploy: %+v %v", stt, err)
	}
	if n, _ := st.CountEvents(ctx, store.EventDeployStarted); n != 1 {
		t.Fatalf("repeat deploy must not re-start: %d deploy_started", n)
	}
}

// A repeated POST /v1/deploy while prepulling reports the in-flight job and
// does not fan out a second prepull.
func TestDeployIdempotentWhilePrepulling(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	v2 := f.AddVersion(t, "1.1.0", "dev")
	m, rec, _ := newManager(t, st, time.Minute)
	ctx := context.Background()

	if _, err := m.Deploy(ctx, v2); err != nil {
		t.Fatal(err)
	}
	first := len(prepullsTo(rec))
	stt, err := m.Deploy(ctx, v2)
	if err != nil {
		t.Fatal(err)
	}
	if stt.State != "prepulling" || stt.PendingNodes != 1 {
		t.Fatalf("repeat status: %+v", stt)
	}
	if more := len(prepullsTo(rec)); first != 1 || more != 0 {
		t.Fatalf("prepull fan-out repeated: first=%d more=%d", first, more)
	}

	// A different version of the same project cannot start meanwhile.
	v3 := f.AddVersion(t, "1.2.0", "dev")
	if _, err := m.Deploy(ctx, v3); !errors.Is(err, store.ErrDeployInProgress) {
		t.Fatalf("want ErrDeployInProgress, got %v", err)
	}
}

// Prepull timeout aborts the deploy: version back to registered +
// deploy_failed event (master.md §5 step 2).
func TestDeployTimeoutAborts(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	v2 := f.AddVersion(t, "1.1.0", "dev")
	m, _, obs := newManager(t, st, 50*time.Millisecond)
	ctx := context.Background()

	if _, err := m.Deploy(ctx, v2); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for versionState(t, st, v2) != "registered" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := versionState(t, st, v2); got != "registered" {
		t.Fatalf("timed-out deploy must abort to registered, got %s", got)
	}
	if n, _ := st.CountEvents(ctx, store.EventDeployFailed); n != 1 {
		t.Fatalf("want 1 deploy_failed event, got %d", n)
	}
	if got := fleetActive(t, st, "eu"); got != f.VersionID {
		t.Fatalf("fleet must keep the old version, got %s", got)
	}
	if obs.count() != 0 {
		t.Fatal("aborted deploy must not observe the prepull metric")
	}

	// The version can be deployed again after the abort.
	if _, err := m.Deploy(ctx, v2); err != nil {
		t.Fatalf("re-deploy after abort: %v", err)
	}
}

// A failed pull report aborts immediately.
func TestDeployPullFailedAborts(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	v2 := f.AddVersion(t, "1.1.0", "dev")
	m, _, _ := newManager(t, st, time.Minute)
	ctx := context.Background()

	if _, err := m.Deploy(ctx, v2); err != nil {
		t.Fatal(err)
	}
	report(m, f.NodeID, "ghcr.io/example/game-server:1.1.0", "failed")
	if got := versionState(t, st, v2); got != "registered" {
		t.Fatalf("failed pull must abort to registered, got %s", got)
	}
	if n, _ := st.CountEvents(ctx, store.EventDeployFailed); n != 1 {
		t.Fatalf("want 1 deploy_failed event, got %d", n)
	}
}

// Deploying a disabled version is rejected; a version of a project without
// fleets is rejected.
func TestDeployRejections(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	m, _, _ := newManager(t, st, time.Minute)
	ctx := context.Background()

	v2 := f.AddVersion(t, "1.1.0", "dev")
	if _, err := m.Deploy(ctx, v2); !errors.Is(err, store.ErrNoFleet) {
		t.Fatalf("no fleet: want ErrNoFleet, got %v", err)
	}
	f.UpsertFleet(t, 2, 50)
	if _, err := st.Pool.Exec(ctx,
		`update versions set state = 'disabled' where id = $1::uuid`, v2); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, v2); !errors.Is(err, store.ErrVersionState) {
		t.Fatalf("disabled version: want ErrVersionState, got %v", err)
	}
	if _, err := m.Deploy(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown version: want ErrNotFound, got %v", err)
	}
}

// Three-version chain: activating v3 pushes the older deprecated v1 to
// disabled — exactly one deprecated version per project (уточнено в v0).
func TestDeployChainDisablesOlderDeprecated(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	m, _, _ := newManager(t, st, time.Minute)
	ctx := context.Background()

	deployAll := func(vid string) {
		t.Helper()
		if _, err := m.Deploy(ctx, vid); err != nil {
			t.Fatal(err)
		}
		report(m, f.NodeID, mustVersion(t, st, vid).ImageRef, "pulled")
		if got := versionState(t, st, vid); got != "active" {
			t.Fatalf("version %s: want active, got %s", vid, got)
		}
	}

	v2 := f.AddVersion(t, "1.1.0", "dev")
	deployAll(v2) // 1.0.0 → deprecated
	v3 := f.AddVersion(t, "1.2.0", "dev")
	deployAll(v3) // 1.1.0 → deprecated, 1.0.0 → disabled

	if got := versionState(t, st, f.VersionID); got != "disabled" {
		t.Fatalf("oldest version: want disabled, got %s", got)
	}
	if got := versionState(t, st, v2); got != "deprecated" {
		t.Fatalf("middle version: want deprecated, got %s", got)
	}
	if n, _ := st.CountEvents(ctx, store.EventVersionDisabled); n != 1 {
		t.Fatalf("want 1 version_disabled event, got %d", n)
	}
}

// Rollback: deprecated ↔ active flip back in one transaction, fleets
// repointed, no prepull needed (master.md §5).
func TestRollback(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	v2 := f.AddVersion(t, "1.1.0", "dev")
	m, rec, _ := newManager(t, st, time.Minute)
	ctx := context.Background()

	// Nothing deprecated yet → rollback refused.
	if _, err := m.Rollback(ctx, "game", "dev", nil); !errors.Is(err, store.ErrVersionState) {
		t.Fatalf("premature rollback: want ErrVersionState, got %v", err)
	}

	if _, err := m.Deploy(ctx, v2); err != nil {
		t.Fatal(err)
	}
	report(m, f.NodeID, "ghcr.io/example/game-server:1.1.0", "pulled")
	rec.Take() // drop the prepull

	res, err := m.Rollback(ctx, "game", "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Version.ID != f.VersionID || res.PrevSemver != "1.1.0" {
		t.Fatalf("rollback result: %+v", res)
	}
	if got := versionState(t, st, f.VersionID); got != "active" {
		t.Fatalf("rolled-back version: want active, got %s", got)
	}
	if got := versionState(t, st, v2); got != "deprecated" {
		t.Fatalf("demoted version: want deprecated, got %s", got)
	}
	if got := fleetActive(t, st, "eu"); got != f.VersionID {
		t.Fatalf("fleet active_version after rollback: %s", got)
	}
	if cmds := rec.Take(); len(cmds) != 0 {
		t.Fatalf("rollback must not send agent commands, got %d", len(cmds))
	}
	if n, _ := st.CountEvents(ctx, store.EventDeployRolledBack); n != 1 {
		t.Fatalf("want 1 deploy_rolled_back event, got %d", n)
	}

	// Unknown region is rejected without flipping anything.
	if _, err := m.Rollback(ctx, "game", "dev", []string{"mars"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bad region rollback: want ErrNotFound, got %v", err)
	}
	if got := versionState(t, st, f.VersionID); got != "active" {
		t.Fatalf("failed rollback must not flip states, got %s", got)
	}
}

// A master restart mid-prepull: Resume() re-arms the job and the deploy
// completes on the (re-)reports.
func TestResumeAfterRestart(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	v2 := f.AddVersion(t, "1.1.0", "dev")
	ctx := context.Background()

	m1, _, _ := newManager(t, st, time.Minute)
	if _, err := m1.Deploy(ctx, v2); err != nil {
		t.Fatal(err)
	}
	// "restart": a fresh manager with no in-memory job.
	m2, rec2, _ := newManager(t, st, time.Minute)
	if err := m2.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if pp := prepullsTo(rec2); len(pp) != 1 || pp[f.NodeID] == "" {
		t.Fatalf("resume must re-send prepull, got %+v", pp)
	}
	report(m2, f.NodeID, "ghcr.io/example/game-server:1.1.0", "pulled")
	if got := versionState(t, st, v2); got != "active" {
		t.Fatalf("resumed deploy must complete, got %s", got)
	}
}

// Deploy with no live fleet nodes flips immediately (nothing to warm).
func TestDeployNoNodesActivatesImmediately(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.SetHeartbeatAge(t, f.NodeID, time.Hour) // node long dead
	v2 := f.AddVersion(t, "1.1.0", "dev")
	m, _, _ := newManager(t, st, time.Minute)

	stt, err := m.Deploy(context.Background(), v2)
	if err != nil {
		t.Fatal(err)
	}
	if stt.State != "active" {
		t.Fatalf("want immediate activation, got %+v", stt)
	}
	if got := versionState(t, st, v2); got != "active" {
		t.Fatalf("version state: %s", got)
	}
}

func mustVersion(t *testing.T, st *store.Store, id string) store.Version {
	t.Helper()
	v, err := st.GetVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
