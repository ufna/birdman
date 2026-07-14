package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Version retention (environments v1 §6а, RetireVersions): env.retention_keep>0
// → registered|disabled ranked by created_at desc, registered beyond keep AND
// older than 1h → disabled (the ONLY registered→disabled path); active/
// prepulling/deprecated never ranked nor touched; event version_retired.

// backdateVersion pins versions.created_at to now()-age (retention ordering and
// the 1h guard both key off created_at).
func backdateVersion(t *testing.T, st *store.Store, id string, age time.Duration) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`update versions set created_at = now() - $2::interval where id = $1::uuid`,
		id, fmt.Sprintf("%d milliseconds", age.Milliseconds())); err != nil {
		t.Fatalf("backdate version: %v", err)
	}
}

// forceVersionState sets versions.state directly — a store-level SQL unit test of
// RetireVersions bypasses the state machine to stage active/prepulling/deprecated.
func forceVersionState(t *testing.T, st *store.Store, id, state string) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`update versions set state = $2 where id = $1::uuid`, id, state); err != nil {
		t.Fatalf("force version state: %v", err)
	}
}

func TestRetentionDisablesBeyondKeep(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // seeds game + a recent dev version 1.0.0

	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "game", Name: "stg", RetentionKeep: 2,
	}); err != nil {
		t.Fatal(err)
	}
	// Five stg versions, all older than 1h, oldest → newest.
	ids := map[string]string{}
	for _, a := range []struct {
		semver string
		age    time.Duration
	}{
		{"0.1.0", 5 * time.Hour},
		{"0.2.0", 4 * time.Hour},
		{"0.3.0", 3 * time.Hour},
		{"0.4.0", 2 * time.Hour},
		{"0.5.0", 90 * time.Minute}, // still > 1h
	} {
		id := f.AddVersion(t, a.semver, "stg")
		backdateVersion(t, st, id, a.age)
		ids[a.semver] = id
	}

	retired, err := st.RetireVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 3 {
		t.Fatalf("keep=2 of 5 → want 3 retired, got %d: %+v", len(retired), retired)
	}
	for _, semver := range []string{"0.5.0", "0.4.0"} {
		if got := envVersionState(t, st, ids[semver]); got != "registered" {
			t.Fatalf("%s within keep: want registered, got %s", semver, got)
		}
	}
	for _, semver := range []string{"0.3.0", "0.2.0", "0.1.0"} {
		if got := envVersionState(t, st, ids[semver]); got != "disabled" {
			t.Fatalf("%s beyond keep: want disabled, got %s", semver, got)
		}
	}
	// The fixture's recent dev 1.0.0 (keep=20) is a different partition — untouched.
	if got := envVersionState(t, st, f.VersionID); got != "registered" {
		t.Fatalf("dev version must be untouched, got %s", got)
	}
	// One version_retired per retired version.
	if n, _ := st.CountEvents(ctx, store.EventVersionRetired); n != 3 {
		t.Fatalf("want 3 version_retired events, got %d", n)
	}
	// Returned refs carry (project, env, image_ref) for the RemoveImage dispatch.
	for _, d := range retired {
		if d.Env != "stg" || d.ImageRef == "" || d.ProjectID == "" || d.Semver == "" {
			t.Fatalf("retired ref incomplete: %+v", d)
		}
	}
}

func TestRetentionOneHourGuard(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "game", Name: "stg", RetentionKeep: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Two recent (<1h) versions; the older one is beyond keep=1 but the 1h guard
	// (race with the auto-deploy queue) keeps it alive.
	vOld := f.AddVersion(t, "0.1.0", "stg")
	backdateVersion(t, st, vOld, 30*time.Minute)
	vNew := f.AddVersion(t, "0.2.0", "stg")
	backdateVersion(t, st, vNew, 10*time.Minute)

	retired, err := st.RetireVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 0 {
		t.Fatalf("1h guard: want 0 retired, got %d: %+v", len(retired), retired)
	}
	if got := envVersionState(t, st, vOld); got != "registered" {
		t.Fatalf("recent version beyond keep must survive the 1h guard, got %s", got)
	}

	// Age vOld past 1h → retention now disables it (still beyond keep=1).
	backdateVersion(t, st, vOld, 2*time.Hour)
	retired, err = st.RetireVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 1 || retired[0].VersionID != vOld {
		t.Fatalf("want vOld retired once past 1h, got %+v", retired)
	}
	if got := envVersionState(t, st, vOld); got != "disabled" {
		t.Fatalf("aged version beyond keep: want disabled, got %s", got)
	}
	if got := envVersionState(t, st, vNew); got != "registered" {
		t.Fatalf("newest (within keep) must survive, got %s", got)
	}
}

func TestRetentionSkipsLiveStates(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "game", Name: "stg", RetentionKeep: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// active/prepulling/deprecated are never ranked nor touched, whatever their age.
	stage := func(semver, state string) string {
		id := f.AddVersion(t, semver, "stg")
		backdateVersion(t, st, id, 5*time.Hour)
		forceVersionState(t, st, id, state)
		return id
	}
	vActive := stage("0.1.0", "active")
	vPre := stage("0.2.0", "prepulling")
	vDep := stage("0.3.0", "deprecated")
	// Three registered, all >1h; keep=1 → newest kept, older two disabled.
	vR1 := f.AddVersion(t, "0.4.0", "stg")
	backdateVersion(t, st, vR1, 4*time.Hour)
	vR2 := f.AddVersion(t, "0.5.0", "stg")
	backdateVersion(t, st, vR2, 3*time.Hour)
	vR3 := f.AddVersion(t, "0.6.0", "stg")
	backdateVersion(t, st, vR3, 2*time.Hour)

	retired, err := st.RetireVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 2 {
		t.Fatalf("want 2 retired (registered beyond keep), got %d: %+v", len(retired), retired)
	}
	for _, tc := range []struct{ id, want string }{
		{vActive, "active"}, {vPre, "prepulling"}, {vDep, "deprecated"},
		{vR3, "registered"}, // newest registered, within keep
		{vR2, "disabled"}, {vR1, "disabled"},
	} {
		if got := envVersionState(t, st, tc.id); got != tc.want {
			t.Fatalf("state mismatch for %s: want %s, got %s", tc.id, tc.want, got)
		}
	}
}

// TestRetentionKeepsAutoDeployTarget (W2-реестр): the newest registered version
// of an auto_deploy env — the «только вперёд» chain target — is always within the
// keep window (rank 1), so retention never disables it out from under the chain.
func TestRetentionKeepsAutoDeployTarget(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // dev is auto_deploy, keep=20 by seed
	// Shrink dev keep to 1 so retention is aggressive, then register a chain of
	// versions older than 1h; the newest registered is the auto-deploy target.
	if _, err := st.PatchEnvironment(ctx, "game", "dev", store.EnvironmentPatch{
		RetentionKeep: intPtr(1),
	}); err != nil {
		t.Fatal(err)
	}
	backdateVersion(t, st, f.VersionID, 4*time.Hour) // the seeded 1.0.0
	vMid := f.AddVersion(t, "1.1.0", "dev")
	backdateVersion(t, st, vMid, 3*time.Hour)
	target := f.AddVersion(t, "1.2.0", "dev") // newest registered = chain target
	backdateVersion(t, st, target, 2*time.Hour)

	if _, err := st.RetireVersions(ctx); err != nil {
		t.Fatal(err)
	}
	if got := envVersionState(t, st, target); got != "registered" {
		t.Fatalf("auto-deploy target must survive retention, got %s", got)
	}
	// And it is still the version NewestRegisteredAfter picks (zero marker).
	got, ok, err := st.NewestRegisteredAfter(ctx, versionProjectID(t, st, target), "dev", store.CreatedID{})
	if err != nil || !ok {
		t.Fatalf("newest-registered lookup: ok=%v err=%v", ok, err)
	}
	if got.ID != target {
		t.Fatalf("chain target changed: want %s, got %s", target, got.ID)
	}
	// The older ones beyond keep are disabled.
	if s := envVersionState(t, st, vMid); s != "disabled" {
		t.Fatalf("older registered must be retired, got %s", s)
	}
}

// TestRetentionIdempotent (y3): a second RetireVersions pass over an already-
// retired env is a no-op — versions disabled by the first pass stay disabled (the
// update targets r.state='registered' only), nothing new is retired, and no
// duplicate version_retired events pile up on every reconcile subtick.
func TestRetentionIdempotent(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "game", Name: "stg", RetentionKeep: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Three registered, all >1h; keep=1 → two retired on the first pass.
	for _, a := range []struct {
		semver string
		age    time.Duration
	}{{"0.1.0", 4 * time.Hour}, {"0.2.0", 3 * time.Hour}, {"0.3.0", 2 * time.Hour}} {
		id := f.AddVersion(t, a.semver, "stg")
		backdateVersion(t, st, id, a.age)
	}

	first, err := st.RetireVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("first pass: want 2 retired, got %d: %+v", len(first), first)
	}
	n1, _ := st.CountEvents(ctx, store.EventVersionRetired)

	// Second pass over the same state: nothing new, no duplicate events.
	second, err := st.RetireVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("second pass must be a no-op, got %d retired: %+v", len(second), second)
	}
	n2, _ := st.CountEvents(ctx, store.EventVersionRetired)
	if n1 != 2 || n2 != 2 {
		t.Fatalf("version_retired must not double on re-run: first=%d, after=%d (want 2, 2)", n1, n2)
	}
}

// TestRetentionPreexistingDisabledFillKeep (y3): retention ranks registered AND
// disabled versions together (one partition, created_at desc), so a pre-existing
// disabled version occupies a keep-slot. With keep=2 and the two NEWEST versions
// already disabled, every registered version below them is beyond keep and gets
// retired — a naive «count only registered» ranking would have spared two.
func TestRetentionPreexistingDisabledFillKeep(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "game", Name: "stg", RetentionKeep: 2,
	}); err != nil {
		t.Fatal(err)
	}
	// Three older registered versions...
	reg := map[string]string{}
	for _, a := range []struct {
		semver string
		age    time.Duration
	}{{"0.1.0", 5 * time.Hour}, {"0.2.0", 4 * time.Hour}, {"0.3.0", 3 * time.Hour}} {
		id := f.AddVersion(t, a.semver, "stg")
		backdateVersion(t, st, id, a.age)
		reg[a.semver] = id
	}
	// ...and two NEWEST versions already disabled — they consume both keep slots.
	for _, a := range []struct {
		semver string
		age    time.Duration
	}{{"0.4.0", 2 * time.Hour}, {"0.5.0", 90 * time.Minute}} {
		id := f.AddVersion(t, a.semver, "stg")
		backdateVersion(t, st, id, a.age)
		forceVersionState(t, st, id, "disabled")
	}

	retired, err := st.RetireVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// keep=2 fully consumed by the two disabled → all three registered retired.
	if len(retired) != 3 {
		t.Fatalf("pre-existing disabled must occupy keep-slots → 3 registered retired, got %d: %+v", len(retired), retired)
	}
	for semver, id := range reg {
		if got := envVersionState(t, st, id); got != "disabled" {
			t.Fatalf("%s must be retired (disabled fills keep), got %s", semver, got)
		}
	}
}

// TestImageRefInUseGuard covers the shared-ref guard (environments v1 §6б): a ref
// is «in use» only while a NON-disabled version of the SAME (project, env) names
// it; a different env sharing the exact ref string does not count (env-scoped).
func TestImageRefInUseGuard(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)

	v1 := f.VersionID
	ref := "ghcr.io/example/game-server:1.0.0" // the fixture's ref
	projectID := versionProjectID(t, st, v1)

	// A second dev version with the SAME ref (a rebuild) → ref in use.
	v2, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "game", Semver: "1.0.0-rebuild", ImageRef: ref, Env: "dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	if inUse, _ := st.ImageRefInUse(ctx, projectID, "dev", ref); !inUse {
		t.Fatal("ref on two registered versions must be in use")
	}
	// Disable v2 → v1 still registered with the ref → still in use.
	forceVersionState(t, st, v2.ID, "disabled")
	if inUse, _ := st.ImageRefInUse(ctx, projectID, "dev", ref); !inUse {
		t.Fatal("ref still on a registered version must be in use")
	}
	// Disable v1 too → no non-disabled version holds it → NOT in use.
	forceVersionState(t, st, v1, "disabled")
	if inUse, _ := st.ImageRefInUse(ctx, projectID, "dev", ref); inUse {
		t.Fatal("ref only on disabled versions must NOT be in use")
	}
	// A prod version sharing the exact ref does NOT keep the dev ref in use.
	if _, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "game", Semver: "1.0.0", ImageRef: ref, Env: "prod",
	}); err != nil {
		t.Fatal(err)
	}
	if inUse, _ := st.ImageRefInUse(ctx, projectID, "dev", ref); inUse {
		t.Fatal("a prod version sharing the ref must NOT keep the dev ref in use (env-scoped)")
	}
	if inUse, _ := st.ImageRefInUse(ctx, projectID, "prod", ref); !inUse {
		t.Fatal("the prod ref itself IS in use")
	}
}

// TestEnvNodeIDs covers the RemoveImage dispatch target list: not-dead nodes of
// (project, env) only — offline included (queued), dead skipped, other env excluded.
func TestEnvNodeIDs(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // node1 in dev
	projectID := versionProjectID(t, st, f.VersionID)
	node2 := f.AddNode(t, "n2", "203.0.113.30", 10) // dev
	// A prod node.
	pn, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "game", Region: "eu", Hostname: "np", PublicIP: "203.0.113.31", CapacitySlots: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeEnv(ctx, pn.ID, "prod"); err != nil {
		t.Fatal(err)
	}
	// A dead dev node — must be excluded.
	dead := f.AddNode(t, "nd", "203.0.113.32", 10)
	if _, err := st.Pool.Exec(ctx, `update nodes set state='dead' where id=$1::uuid`, dead); err != nil {
		t.Fatal(err)
	}

	ids, err := st.EnvNodeIDs(ctx, projectID, "dev")
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	if !set[f.NodeID] || !set[node2] {
		t.Fatalf("want both live dev nodes, got %v", ids)
	}
	if set[pn.ID] {
		t.Fatal("prod node must be excluded (env-scoped)")
	}
	if set[dead] {
		t.Fatal("dead node must be excluded")
	}
	if len(ids) != 2 {
		t.Fatalf("want exactly 2 dev target nodes, got %d: %v", len(ids), ids)
	}
}

// Сходящийся sweep снятия образов (Фаза D, дефект стенда): RemoveImage уходил В
// МОМЕНТ перехода версии в disabled — но ровно тогда её серверы ещё дренятся,
// агент видит образ занятым живым контейнером и скипает команду, повторить её
// некому. VersionsPendingImageCleanup отдаёт disabled-версии, у которых не
// осталось живых серверов, ref не держит не-disabled версия того же (project,
// env) и sweep ещё не отправлял команду; MarkImageCleanupSent ставит маркер.

// pendingCleanup — выборка sweep'а как map version_id → DisabledVersion.
func pendingCleanup(t *testing.T, st *store.Store) map[string]store.DisabledVersion {
	t.Helper()
	pending, err := st.VersionsPendingImageCleanup(context.Background())
	if err != nil {
		t.Fatalf("pending image cleanup: %v", err)
	}
	out := map[string]store.DisabledVersion{}
	for _, d := range pending {
		out[d.VersionID] = d
	}
	return out
}

// reapServer flips a server to `reaped` — the container is gone from the node,
// so it no longer pins the image.
func reapServer(t *testing.T, st *store.Store, serverID string) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`update servers set state='reaped', updated_at=now() where id=$1::uuid`, serverID); err != nil {
		t.Fatalf("reap server: %v", err)
	}
}

func imageCleanupAt(t *testing.T, st *store.Store, versionID string) time.Time {
	t.Helper()
	var ts *time.Time
	if err := st.Pool.QueryRow(context.Background(),
		`select image_cleanup_at from versions where id = $1::uuid`, versionID).Scan(&ts); err != nil {
		t.Fatalf("read image_cleanup_at: %v", err)
	}
	if ts == nil {
		t.Fatalf("image_cleanup_at не проставлен для %s", versionID)
	}
	return *ts
}

// TestPendingImageCleanupWaitsForLiveServers (а): пока у disabled-версии жив хоть
// один сервер (creating/ready/allocated/draining), его контейнер держит ref на
// ноде — версии в выборке нет; как только сервера не стало (reaped/failed —
// контейнера уже нет), версия попадает в выборку и sweep снимает образ.
func TestPendingImageCleanupWaitsForLiveServers(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)

	for _, state := range []string{"creating", "ready", "allocated", "draining"} {
		semver := "2.0.0-" + state
		vid := f.AddVersion(t, semver, "dev")
		forceVersionState(t, st, vid, "disabled")
		srv := f.InsertServerOn(t, f.NodeID, vid, state)

		if _, ok := pendingCleanup(t, st)[vid]; ok {
			t.Fatalf("сервер в %s держит образ живым контейнером — версия не должна попадать в sweep", state)
		}
		reapServer(t, st, srv) // дренаж закончился, контейнер снят
		got, ok := pendingCleanup(t, st)[vid]
		if !ok {
			t.Fatalf("серверов версии не осталось (%s → reaped) — версия обязана попасть в sweep", state)
		}
		// Выборка несёт всё, что нужно диспатчеру RemoveImage: (project, env) → ноды, ref → что снять.
		if got.Env != "dev" || got.ImageRef != "ghcr.io/example/game-server:"+semver ||
			got.ProjectID == "" || got.Semver != semver {
			t.Fatalf("выборка неполна для диспатча RemoveImage: %+v", got)
		}
		// Маркер — чтобы версия не тянулась в следующие итерации таблицы.
		if err := st.MarkImageCleanupSent(ctx, []string{vid}); err != nil {
			t.Fatal(err)
		}
	}

	// failed-сервер контейнера не держит — версия в выборке сразу.
	vFailed := f.AddVersion(t, "2.1.0", "dev")
	forceVersionState(t, st, vFailed, "disabled")
	f.MarkFailed(t, f.InsertServerOn(t, f.NodeID, vFailed, "creating"), 0)
	if _, ok := pendingCleanup(t, st)[vFailed]; !ok {
		t.Fatal("failed-сервер образ не держит — версия обязана быть в выборке")
	}
	// registered-версия (фикстурная 1.0.0) — не disabled, снимать нечего.
	if _, ok := pendingCleanup(t, st)[f.VersionID]; ok {
		t.Fatal("не-disabled версия в выборке sweep'а делать нечего")
	}
}

// TestPendingImageCleanupSharedRefGuard (б): ref, который держит НЕ-disabled версия
// того же (project, env), из выборки исключён — контент под живой ссылкой (та же
// семантика, что ImageRefInUse, §6б). Держатель ушёл в disabled → обе версии в
// выборке (в одну команду их схлопнет дедуп CleanupImages). Версия другого env с
// тем же ref не мешает: ноды env-скоупны.
func TestPendingImageCleanupSharedRefGuard(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10)

	ref := "ghcr.io/example/game-server:shared"
	vKeep, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "game", Semver: "3.0.0", ImageRef: ref, Env: "dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	vDis, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "game", Semver: "3.0.1", ImageRef: ref, Env: "dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	forceVersionState(t, st, vDis.ID, "disabled")

	if _, ok := pendingCleanup(t, st)[vDis.ID]; ok {
		t.Fatal("ref держит registered-версия того же env — sweep обязан молчать (shared-ref guard)")
	}
	// Прод-версия с тем же ref живёт на других (prod) нодах — dev-выборке не помеха.
	if _, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "game", Semver: "3.0.1", ImageRef: ref, Env: "prod",
	}); err != nil {
		t.Fatal(err)
	}
	forceVersionState(t, st, vKeep.ID, "disabled")

	pending := pendingCleanup(t, st)
	if _, ok := pending[vDis.ID]; !ok {
		t.Fatal("не-disabled держателей ref в dev не осталось — версия обязана попасть в sweep")
	}
	if _, ok := pending[vKeep.ID]; !ok {
		t.Fatal("вторая disabled-версия того же ref тоже в выборке (дедуп ref'ов — в CleanupImages)")
	}
}

// TestPendingImageCleanupMarkerStopsResend (в): MarkImageCleanupSent убирает версию
// из выборки навсегда — каждая disabled-версия получает максимум ОДНУ догоняющую
// RemoveImage, а не команду каждые 60с. Маркер идемпотентен, пустой батч — no-op.
func TestPendingImageCleanupMarkerStopsResend(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)

	vid := f.AddVersion(t, "4.0.0", "dev")
	forceVersionState(t, st, vid, "disabled")
	if _, ok := pendingCleanup(t, st)[vid]; !ok {
		t.Fatal("disabled-версия без живых серверов и без держателей ref обязана быть в выборке")
	}

	if err := st.MarkImageCleanupSent(ctx, []string{vid}); err != nil {
		t.Fatal(err)
	}
	if _, ok := pendingCleanup(t, st)[vid]; ok {
		t.Fatal("после маркера версия обязана уйти из выборки — sweep не спамит командами каждый субтик")
	}

	if err := st.MarkImageCleanupSent(ctx, nil); err != nil {
		t.Fatalf("пустой батч обязан быть no-op: %v", err)
	}
	first := imageCleanupAt(t, st, vid)
	if err := st.MarkImageCleanupSent(ctx, []string{vid}); err != nil {
		t.Fatal(err)
	}
	if got := imageCleanupAt(t, st, vid); !got.Equal(first) {
		t.Fatalf("повторный маркер не должен переписывать первую отметку: было %s, стало %s", first, got)
	}
}

// TestPendingImageCleanupBatchLimit (M-2): один проход sweep'а забирает не больше
// 200 кандидатов. Разовый всплеск — после миграции, на большом парке или после
// долгого простоя sweep'а — иначе раздул бы in-memory pending-очереди хаба
// (RemoveImage уходит КАЖДОЙ ноде окружения). Хвост не голодает: выборка
// упорядочена по created_at, а отправленное гасится маркером, поэтому следующий
// 60с-субтик берёт следующую порцию.
func TestPendingImageCleanupBatchLimit(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)
	projectID := versionProjectID(t, st, f.VersionID)

	// 250 disabled-версий с уникальными ref'ами, без серверов — все кандидаты.
	const total = 250
	if _, err := st.Pool.Exec(ctx, `
		insert into versions (project_id, semver, image_ref, env, state, created_at)
		select $1::uuid, '9.0.' || i, 'ghcr.io/example/game-server:9.0.' || i, 'dev', 'disabled',
		       now() - (interval '1 second' * (1000 - i))
		from generate_series(1, $2) as i`, projectID, total); err != nil {
		t.Fatalf("seed disabled versions: %v", err)
	}

	pending, err := st.VersionsPendingImageCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 200 {
		t.Fatalf("кандидатов %d — за проход обязано выйти ровно 200 (LIMIT), got %d", total, len(pending))
	}

	// Остаток доберётся следующим проходом: гасим отданное маркером.
	ids := make([]string, 0, len(pending))
	for _, d := range pending {
		ids = append(ids, d.VersionID)
	}
	if err := st.MarkImageCleanupSent(ctx, ids); err != nil {
		t.Fatal(err)
	}
	rest, err := st.VersionsPendingImageCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != total-200 {
		t.Fatalf("хвост обязан достаться следующему субтику: want %d, got %d", total-200, len(rest))
	}
}

// versionProjectID returns a version's project uuid (the id EnvNodeIDs /
// ImageRefInUse take).
func versionProjectID(t *testing.T, st *store.Store, versionID string) string {
	t.Helper()
	v, err := st.GetVersion(context.Background(), versionID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	return v.ProjectID
}
