package reconcile_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/ufna/birdman/master/internal/reconcile"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// RemoveImage dispatch (environments v1 §6б): on a disabled transition, the
// cleaner sends RemoveImage(image_ref) to every not-dead node of the version's
// (project, env) — unless the exact ref is still used by a non-disabled version
// of that (project, env), in which case the command is withheld.

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// removeImagesTo maps nodeID → image_ref over the RemoveImage commands recorded.
func removeImagesTo(cmds []fakeCmd) map[string]string {
	out := map[string]string{}
	for _, c := range cmds {
		if r := c.Msg.GetRemoveImage(); r != nil {
			out[c.NodeID] = r.GetImageRef()
		}
	}
	return out
}

func disableRaw(t *testing.T, st *store.Store, versionID string) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`update versions set state='disabled' where id=$1::uuid`, versionID); err != nil {
		t.Fatalf("disable version: %v", err)
	}
}

func disabledRef(t *testing.T, st *store.Store, versionID string) store.DisabledVersion {
	t.Helper()
	v, err := st.GetVersion(context.Background(), versionID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	return store.DisabledVersion{
		VersionID: v.ID, ProjectID: v.ProjectID, Env: v.Env, ImageRef: v.ImageRef, Semver: v.Semver,
	}
}

func TestCleanupImagesDispatchToEnvNodes(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // node1 in dev
	node2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	// A prod node — must not be targeted.
	pn, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "game", Region: "eu", Hostname: "node-prod", PublicIP: "203.0.113.12", CapacitySlots: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeEnv(ctx, pn.ID, "prod"); err != nil {
		t.Fatal(err)
	}

	// A disabled dev version with a unique ref (no non-disabled version holds it).
	vid := f.AddVersion(t, "2.0.0", "dev")
	disableRaw(t, st, vid)
	dv := disabledRef(t, st, vid)

	sender := &fakeSender{}
	cleaner := reconcile.NewImageCleaner(st, sender, quietLog())
	if err := cleaner.CleanupImages(ctx, []store.DisabledVersion{dv}); err != nil {
		t.Fatal(err)
	}
	got := removeImagesTo(sender.take())
	if len(got) != 2 || got[f.NodeID] != dv.ImageRef || got[node2] != dv.ImageRef {
		t.Fatalf("want RemoveImage of %s to both dev nodes, got %v", dv.ImageRef, got)
	}
	if _, ok := got[pn.ID]; ok {
		t.Fatal("prod node must not be targeted (env-scoped)")
	}
}

func TestCleanupImagesSharedRefGuardAndDeadNode(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // node1 in dev

	// Two dev versions share the exact same ref; one is disabled, one registered.
	sharedRef := "ghcr.io/example/game-server:shared"
	vKeep, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "game", Semver: "3.0.0", ImageRef: sharedRef, Env: "dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	vDis, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "game", Semver: "3.0.1", ImageRef: sharedRef, Env: "dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	disableRaw(t, st, vDis.ID)
	dv := disabledRef(t, st, vDis.ID)

	sender := &fakeSender{}
	cleaner := reconcile.NewImageCleaner(st, sender, quietLog())

	// vKeep still registered with the ref → RemoveImage withheld.
	if err := cleaner.CleanupImages(ctx, []store.DisabledVersion{dv}); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender.take()); len(got) != 0 {
		t.Fatalf("shared ref must withhold RemoveImage, got %v", got)
	}

	// Disable vKeep too → ref no longer in use → dispatch; but a dead node is skipped.
	disableRaw(t, st, vKeep.ID)
	node2 := f.AddNode(t, "n2", "203.0.113.20", 10)
	if _, err := st.Pool.Exec(ctx, `update nodes set state='dead' where id=$1::uuid`, f.NodeID); err != nil {
		t.Fatal(err)
	}
	if err := cleaner.CleanupImages(ctx, []store.DisabledVersion{dv}); err != nil {
		t.Fatal(err)
	}
	got := removeImagesTo(sender.take())
	if len(got) != 1 || got[node2] != sharedRef {
		t.Fatalf("want RemoveImage only to the live node2, got %v", got)
	}
	if _, ok := got[f.NodeID]; ok {
		t.Fatal("dead node must be skipped")
	}
}

// TestCleanupImagesDedupsRefWithinBatch (y2): N disabled versions of one
// (project, env) sharing the exact image_ref collapse to ONE RemoveImage per node
// — the env's target nodes and the ref are identical, so repeat sends are noise.
func TestCleanupImagesDedupsRefWithinBatch(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10) // node1 in dev, the sole target

	sharedRef := "ghcr.io/example/game-server:dupe"
	var batch []store.DisabledVersion
	for _, semver := range []string{"4.0.0", "4.0.1", "4.0.2"} {
		v, err := st.CreateVersion(ctx, store.CreateVersionParams{
			Project: "game", Semver: semver, ImageRef: sharedRef, Env: "dev",
		})
		if err != nil {
			t.Fatal(err)
		}
		disableRaw(t, st, v.ID)
		batch = append(batch, disabledRef(t, st, v.ID))
	}

	sender := &fakeSender{}
	cleaner := reconcile.NewImageCleaner(st, sender, quietLog())
	if err := cleaner.CleanupImages(ctx, batch); err != nil {
		t.Fatal(err)
	}
	// removeImagesTo keys by node → can't count repeats; count raw commands.
	sends := 0
	for _, c := range sender.take() {
		if c.Msg.GetRemoveImage() != nil {
			sends++
		}
	}
	if sends != 1 {
		t.Fatalf("3 disabled versions sharing a ref → 1 RemoveImage to the sole dev node, got %d", sends)
	}
}

// TestRunOnceRetentionDispatchesRemoveImage wires the whole retention subtick:
// a single RunOnce disables a registered version beyond keep (>1h) and dispatches
// RemoveImage for its ref to the env's node.
func TestRunOnceRetentionDispatchesRemoveImage(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // node1 in dev, dev version 1.0.0 recent
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "game", Name: "stg", RetentionKeep: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Move node1 into stg so it is the RemoveImage target there.
	if _, err := st.SetNodeEnv(ctx, f.NodeID, "stg"); err != nil {
		t.Fatal(err)
	}
	vOld := f.AddVersion(t, "0.1.0", "stg")
	vNew := f.AddVersion(t, "0.2.0", "stg")
	if _, err := st.Pool.Exec(ctx,
		`update versions set created_at = now() - interval '3 hours' where id=$1::uuid`, vOld); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`update versions set created_at = now() - interval '2 hours' where id=$1::uuid`, vNew); err != nil {
		t.Fatal(err)
	}

	r, sender := newReconciler(st)
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := versionState(t, st, vOld); got != "disabled" {
		t.Fatalf("vOld beyond keep >1h: want disabled, got %s", got)
	}
	if got := versionState(t, st, vNew); got != "registered" {
		t.Fatalf("vNew within keep: want registered, got %s", got)
	}
	if n, _ := st.CountEvents(ctx, store.EventVersionRetired); n != 1 {
		t.Fatalf("want 1 version_retired event, got %d", n)
	}
	got := removeImagesTo(sender.take())
	if got[f.NodeID] != "ghcr.io/example/game-server:0.1.0" {
		t.Fatalf("want RemoveImage for vOld ref to node1, got %v", got)
	}
}
