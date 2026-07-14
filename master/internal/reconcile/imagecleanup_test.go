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

// reapServer flips a server to `reaped` — the agent has removed the container,
// so nothing pins the image on the node any more.
func reapServer(t *testing.T, st *store.Store, serverID string) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`update servers set state='reaped', updated_at=now() where id=$1::uuid`, serverID); err != nil {
		t.Fatalf("reap server: %v", err)
	}
}

// TestImageCleanupSweepConvergesAfterDrain — регрессия дефекта Фазы D (стенд):
// RemoveImage уходил В МОМЕНТ перехода версии в disabled, но ровно тогда её
// серверы ещё дренятся (реконсайлер только что выгнал их из окна, grace 30с) —
// агент видел образ занятым живым контейнером и скипал команду, а повторить её
// было некому: образ оставался на ноде до watermark-GC. Сходящийся sweep в 60с-
// субтике чинит это: пока жив хоть один сервер версии — молчим; как только их не
// осталось — RemoveImage уходит нодам окружения, ровно один раз (image_cleanup_at).
func TestImageCleanupSweepConvergesAfterDrain(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // node1 в dev; фикстурная 1.0.0 живёт своей жизнью
	vid := f.AddVersion(t, "2.0.0", "dev")
	ref := "ghcr.io/example/game-server:2.0.0"
	// Версия ушла в disabled (флип/TTL), но её контейнер ещё дренится — та самая гонка.
	srv := f.InsertServerOn(t, f.NodeID, vid, "draining")
	disableRaw(t, st, vid)

	// Пасс 1: контейнер жив → RemoveImage слать бессмысленно (агент скипнет).
	r1, sender1 := newReconciler(st)
	if err := r1.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender1.take()); len(got) != 0 {
		t.Fatalf("живой сервер версии держит образ — RemoveImage слать рано, got %v", got)
	}

	// Дренаж закончился, контейнера больше нет — здесь прежний код молчал НАВСЕГДА.
	reapServer(t, st, srv)

	// Пасс 2 (свежий реконсайлер = ещё не отработавший 60с-субтик): sweep догоняет.
	r2, sender2 := newReconciler(st)
	if err := r2.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender2.take()); got[f.NodeID] != ref {
		t.Fatalf("контейнеров версии не осталось — sweep обязан снять образ с ноды env, got %v", got)
	}

	// Пасс 3: маркер проставлен → командой каждые 60с не спамим.
	r3, sender3 := newReconciler(st)
	if err := r3.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender3.take()); len(got) != 0 {
		t.Fatalf("sweep обязан сработать ровно раз (маркер image_cleanup_at), got %v", got)
	}
}

// imageCleanupMarked сообщает, проставлен ли маркер image_cleanup_at у версии.
func imageCleanupMarked(t *testing.T, st *store.Store, versionID string) bool {
	t.Helper()
	var marked bool
	if err := st.Pool.QueryRow(context.Background(),
		`select image_cleanup_at is not null from versions where id = $1::uuid`, versionID).Scan(&marked); err != nil {
		t.Fatalf("read image_cleanup_at: %v", err)
	}
	return marked
}

// TestImageCleanupSweepMarkerNeedsLiveSessions (I-1): маркер image_cleanup_at
// означает «догоняющая RemoveImage отправлена, больше не повторяем» — и потому
// НЕ ИМЕЕТ ПРАВА появляться, когда команда всего лишь припаркована в in-memory
// очереди хаба. Send офлайн-ноде не падает, но очередь живёт в памяти мастера:
// рестарт (или просто нода, которая уже не вернётся тем же процессом) — и команда
// исчезла, а версия из выборки уже выпала → образ дожил бы до watermark-GC.
// Правило: маркер ставим, только если В МОМЕНТ ОТПРАВКИ у КАЖДОЙ целевой ноды env
// была живая сессия. Иначе следующий 60с-субтик шлёт снова (дубль идемпотентен —
// агент no-op'ит отсутствующий образ).
func TestImageCleanupSweepMarkerNeedsLiveSessions(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // node1 в dev — единственная целевая нода
	vid := f.AddVersion(t, "2.0.0", "dev")
	ref := "ghcr.io/example/game-server:2.0.0"
	disableRaw(t, st, vid) // серверов у версии нет → сразу кандидат sweep'а

	// Пасс 1: нода ОФЛАЙН. Команда всё равно уходит (ляжет в pending хаба), но
	// маркер ставить нельзя.
	r1, sender1 := newReconciler(st)
	sender1.goOffline(f.NodeID)
	if err := r1.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender1.take()); got[f.NodeID] != ref {
		t.Fatalf("RemoveImage обязана уйти и офлайн-ноде (очередь хаба доставит на реконнекте), got %v", got)
	}
	if imageCleanupMarked(t, st, vid) {
		t.Fatal("у целевой ноды не было живой сессии — маркер image_cleanup_at ставить нельзя: команда живёт только в памяти хаба")
	}

	// Пасс 2: нода снова онлайн. Версия без маркера осталась в выборке — sweep шлёт
	// повторно и ТЕПЕРЬ штампует маркер.
	r2, sender2 := newReconciler(st)
	if err := r2.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender2.take()); got[f.NodeID] != ref {
		t.Fatalf("версия без маркера обязана уехать снова следующим субтиком, got %v", got)
	}
	if !imageCleanupMarked(t, st, vid) {
		t.Fatal("все целевые ноды живы — маркер обязан быть проставлен")
	}

	// Пасс 3: маркер стоит → повторов больше нет.
	r3, sender3 := newReconciler(st)
	if err := r3.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender3.take()); len(got) != 0 {
		t.Fatalf("маркер проставлен — RemoveImage не спамим каждый субтик, got %v", got)
	}
}

// TestImageCleanupSweepMarkerPartialFleet (I-1, продолжение): в окружении две ноды,
// живая сессия только у одной. Команда уходит обеим, но версия НЕ считается
// доставленной — маркера нет, следующий проход повторит. Дубль безопасен: агент
// живой ноды no-op'ит уже удалённый образ.
func TestImageCleanupSweepMarkerPartialFleet(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)
	node2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	vid := f.AddVersion(t, "2.0.0", "dev")
	disableRaw(t, st, vid)

	r, sender := newReconciler(st)
	sender.goOffline(node2) // node1 онлайн, node2 — нет
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender.take()); len(got) != 2 {
		t.Fatalf("RemoveImage обязана уйти ОБЕИМ нодам окружения, got %v", got)
	}
	if imageCleanupMarked(t, st, vid) {
		t.Fatal("хоть одна целевая нода без живой сессии — маркер ставить нельзя")
	}
}
