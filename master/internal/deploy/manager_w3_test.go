package deploy_test

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// W3 гигиена (environments v1 §6): the deploy manager dispatches RemoveImage for
// versions a flip pushed to disabled (via ImageCleaner), and the reconcile loop's
// orphan sweep (SweepOrphans) re-arms a version stuck prepulling with no job.

// recordingCleaner is a fake deploy.ImageCleaner that records the disabled
// versions the manager hands it — the shared-ref guard lives in the real
// reconcile.ImageCleaner (tested there), here we assert the manager's dispatch.
type recordingCleaner struct {
	mu       sync.Mutex
	disabled []store.DisabledVersion
}

func (c *recordingCleaner) CleanupImages(_ context.Context, d []store.DisabledVersion) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disabled = append(c.disabled, d...)
	return nil
}

func (c *recordingCleaner) take() []store.DisabledVersion {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.disabled
	c.disabled = nil
	return out
}

// TestFlipDemoteDispatchesRemoveImage drives three deploys on one node: the third
// flip pushes the oldest deprecated version out of the window to disabled, and the
// manager hands exactly that version to the ImageCleaner.
func TestFlipDemoteDispatchesRemoveImage(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // node1, version 1.0.0
	f.UpsertFleet(t, 1, 50)
	ctx := context.Background()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cleaner := &recordingCleaner{}
	m := deploy.New(deploy.Options{
		Store: st, Sender: &testdb.CommandRecorder{}, Log: log,
		Timeout: time.Minute, ImageCleaner: cleaner,
	})

	deployAndPull := func(versionID, ref string) {
		t.Helper()
		if _, err := m.Deploy(ctx, versionID); err != nil {
			t.Fatalf("deploy %s: %v", versionID, err)
		}
		report(m, f.NodeID, ref, "pulled")
		if got := versionState(t, st, versionID); got != "active" {
			t.Fatalf("version %s did not activate: %s", versionID, got)
		}
	}

	// v1 active (no demotions, cleaner not called).
	deployAndPull(f.VersionID, "ghcr.io/example/game-server:1.0.0")
	// v2 active, v1 → deprecated (still in window, nothing disabled).
	v2 := f.AddVersion(t, "1.1.0", "dev")
	deployAndPull(v2, "ghcr.io/example/game-server:1.1.0")
	if got := cleaner.take(); len(got) != 0 {
		t.Fatalf("first demotion is deprecated (in-window), cleaner must be quiet, got %+v", got)
	}
	// v3 active, v2 → deprecated, v1 (was deprecated) → disabled → RemoveImage.
	v3 := f.AddVersion(t, "1.2.0", "dev")
	deployAndPull(v3, "ghcr.io/example/game-server:1.2.0")

	got := cleaner.take()
	if len(got) != 1 || got[0].VersionID != f.VersionID {
		t.Fatalf("want exactly v1 handed to the cleaner, got %+v", got)
	}
	if got[0].ImageRef != "ghcr.io/example/game-server:1.0.0" || got[0].Env != "dev" || got[0].ProjectID == "" {
		t.Fatalf("disabled ref incomplete: %+v", got[0])
	}
	if versionState(t, st, f.VersionID) != "disabled" {
		t.Fatal("v1 must be disabled by the third flip")
	}
}

// TestSweepOrphansReArmsPrepulling: a version stuck `prepulling` in the DB with no
// in-memory job (store.BeginDeploy bypassing the manager) is adopted by the sweep
// — a fresh PrePull fan-out + job; the sweep is idempotent.
func TestSweepOrphansReArmsPrepulling(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // node1 active, fresh heartbeat
	f.UpsertFleet(t, 1, 50)
	ctx := context.Background()

	v2 := f.AddVersion(t, "1.1.0", "dev")
	// Orphan it: store-level BeginDeploy leaves v2 prepulling with NO manager job.
	if _, err := st.BeginDeploy(ctx, v2, store.BeginDeployOpts{}); err != nil {
		t.Fatal(err)
	}

	m, rec, _ := newManager(t, st, time.Minute)
	if m.PendingNodes(v2) != 0 {
		t.Fatal("precondition: manager must have no job for the orphan")
	}

	if err := m.SweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if m.PendingNodes(v2) != 1 {
		t.Fatalf("want 1 pending node after adopt, got %d", m.PendingNodes(v2))
	}
	if got := prepullsTo(rec); got[f.NodeID] != "ghcr.io/example/game-server:1.1.0" {
		t.Fatalf("want PrePull of 1.1.0 to node1, got %v", got)
	}

	// Idempotent: a second sweep finds a job already → no re-fan-out.
	if err := m.SweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if got := prepullsTo(rec); len(got) != 0 {
		t.Fatalf("second sweep must be quiet, got %v", got)
	}
}
