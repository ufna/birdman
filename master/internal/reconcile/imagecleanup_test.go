package reconcile_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/ufna/birdman/master/internal/reconcile"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
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

// --- Маркер по ОТЧЁТАМ агента (ImageReport, §6б) ---
//
// Раньше sweep штамповал versions.image_cleanup_at по факту Send: протокол не нёс
// результата RemoveImage (Ack подтверждает лишь ПОЛУЧЕНИЕ команды). Значит образ,
// который агент не смог снять — занят дренящимся контейнером, ошибка рантайма —
// молча терялся: версия выпадала из выборки, повторить было некому, образ доживал
// до watermark-GC. Теперь агент отвечает ImageReport{removed|absent|busy|error}, и
// маркер ставится ТОЛЬКО когда КАЖДАЯ целевая нода подтвердила, что образа у неё
// нет.

// newSweeper wires the loop exactly as main.go does: ОДИН ImageCleaner, который и
// рассылает RemoveImage в sweep'е, и принимает отчёты агентов (в проде их приносит
// agentlink.Service через ImageSink; здесь — тест).
func newSweeper(st *store.Store) (*reconcile.Reconciler, *fakeSender, *reconcile.ImageCleaner) {
	sender := &fakeSender{}
	log := quietLog()
	c := reconcile.NewImageCleaner(st, sender, log)
	return reconcile.New(st, sender, log).WithImageCleaner(c), sender, c
}

// report feeds one agent ImageReport into the cleaner — ровно то, что делает
// recv-петля сессии (agentlink.Service → ImageSink.HandleImageReport).
func report(c *reconcile.ImageCleaner, nodeID, ref, status string) {
	c.HandleImageReport(nodeID, &agentlinkv1.ImageReport{
		CmdId: "cmd-" + nodeID, ImageRef: ref, Status: status,
	})
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

// TestImageCleanupSweepConvergesAfterDrain — регрессия дефекта Фазы D (стенд):
// RemoveImage уходил В МОМЕНТ перехода версии в disabled, но ровно тогда её
// серверы ещё дренятся (реконсайлер только что выгнал их из окна, grace 30с) —
// агент видел образ занятым живым контейнером и скипал команду, а повторить её
// было некому: образ оставался на ноде до watermark-GC. Сходящийся sweep в 60с-
// субтике чинит это: пока жив хоть один сервер версии — молчим; как только их не
// осталось — RemoveImage уходит нодам окружения. Маркер (и с ним «больше не
// повторяем») появляется НЕ в момент отправки, а когда нода ОТЧИТАЛАСЬ.
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
	r1, sender1, _ := newSweeper(st)
	if err := r1.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender1.take()); len(got) != 0 {
		t.Fatalf("живой сервер версии держит образ — RemoveImage слать рано, got %v", got)
	}

	// Дренаж закончился, контейнера больше нет — здесь прежний код молчал НАВСЕГДА.
	reapServer(t, st, srv)

	// Пасс 2 (свежий реконсайлер = ещё не отработавший 60с-субтик): sweep догоняет.
	r2, sender2, cleaner2 := newSweeper(st)
	if err := r2.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender2.take()); got[f.NodeID] != ref {
		t.Fatalf("контейнеров версии не осталось — sweep обязан снять образ с ноды env, got %v", got)
	}
	if imageCleanupMarked(t, st, vid) {
		t.Fatal("команда только отправлена — маркер до отчёта агента ставить нельзя (раньше ставили вслепую)")
	}

	// Агент доложил: образа на ноде больше нет → маркер.
	report(cleaner2, f.NodeID, ref, "removed")
	if !imageCleanupMarked(t, st, vid) {
		t.Fatal("нода отчиталась removed — версия обязана быть помечена")
	}

	// Пасс 3: маркер стоит → командой каждые 60с не спамим.
	r3, sender3, _ := newSweeper(st)
	if err := r3.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender3.take()); len(got) != 0 {
		t.Fatalf("sweep обязан сработать ровно раз (маркер image_cleanup_at), got %v", got)
	}
}

// TestImageCleanupSweepMarksOnlyWhenEveryNodeReports: в окружении две ноды. Маркер
// — это утверждение про ВСЁ окружение («образ снят»), поэтому одного отчёта мало:
// пока молчит вторая нода, версия остаётся непомеченной. `absent` равносилен
// `removed` (образа на ноде нет — а это всё, чего мы хотели): именно эта
// эквивалентность делает повторную отправку безопасной.
func TestImageCleanupSweepMarksOnlyWhenEveryNodeReports(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)
	node2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	vid := f.AddVersion(t, "2.0.0", "dev")
	ref := "ghcr.io/example/game-server:2.0.0"
	disableRaw(t, st, vid) // серверов у версии нет → сразу кандидат sweep'а

	r, sender, cleaner := newSweeper(st)
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender.take()); len(got) != 2 || got[f.NodeID] != ref || got[node2] != ref {
		t.Fatalf("RemoveImage обязана уйти ОБЕИМ нодам окружения, got %v", got)
	}

	report(cleaner, f.NodeID, ref, "removed")
	if imageCleanupMarked(t, st, vid) {
		t.Fatal("отчиталась только одна нода из двух — маркер ставить рано (на второй образ ещё жив)")
	}

	report(cleaner, node2, ref, "absent")
	if !imageCleanupMarked(t, st, vid) {
		t.Fatal("обе ноды подтвердили отсутствие образа (removed+absent) — версия обязана быть помечена")
	}

	r2, sender2, _ := newSweeper(st)
	if err := r2.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender2.take()); len(got) != 0 {
		t.Fatalf("версия помечена — повторов быть не должно, got %v", got)
	}
}

// TestImageCleanupSweepRetriesWhenNodeCannotRemove: агент ответил busy (образ ещё
// под живым контейнером) или error (рантайм не смог) — удаления НЕ БЫЛО, значит
// маркера быть не должно: версия остаётся в выборке, и следующий 60с-субтик шлёт
// RemoveImage снова. Это и есть починенная дыра — раньше мастер в обоих случаях
// считал дело сделанным.
func TestImageCleanupSweepRetriesWhenNodeCannotRemove(t *testing.T) {
	for _, status := range []string{"busy", "error"} {
		t.Run(status, func(t *testing.T) {
			st := testdb.New(t)
			ctx := context.Background()
			f := testdb.Seed(t, st, "eu", 10) // единственная целевая нода
			vid := f.AddVersion(t, "2.0.0", "dev")
			ref := "ghcr.io/example/game-server:2.0.0"
			disableRaw(t, st, vid)

			r1, sender1, cleaner1 := newSweeper(st)
			if err := r1.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if got := removeImagesTo(sender1.take()); got[f.NodeID] != ref {
				t.Fatalf("sweep обязан отправить RemoveImage, got %v", got)
			}
			report(cleaner1, f.NodeID, ref, status)
			if imageCleanupMarked(t, st, vid) {
				t.Fatalf("нода ответила %s — удаления не было, маркер ставить нельзя", status)
			}

			// Следующий субтик: версия всё ещё в выборке → шлём снова. Теперь
			// контейнер ушёл, агент удалил образ → маркер.
			r2, sender2, cleaner2 := newSweeper(st)
			if err := r2.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if got := removeImagesTo(sender2.take()); got[f.NodeID] != ref {
				t.Fatalf("непомеченная версия обязана уехать снова следующим субтиком, got %v", got)
			}
			report(cleaner2, f.NodeID, ref, "removed")
			if !imageCleanupMarked(t, st, vid) {
				t.Fatal("повтор удался (removed) — версия обязана быть помечена")
			}
		})
	}
}

// TestImageCleanupSweepBusyFromOneNodeBlocksMarker — ловушка наивной реализации.
// Если busy лишь «снимает ноду из набора ожидания», то removed от ОСТАЛЬНЫХ нод
// опустошит остаток набора и проштампует маркер версии, образ которой на busy-ноде
// преспокойно жив: ровно та слепота, которую чиним. Правило: busy/error сносят
// раунд ЦЕЛИКОМ. Проверяем опасный порядок — busy приходит ПЕРВЫМ, removed вторым.
func TestImageCleanupSweepBusyFromOneNodeBlocksMarker(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)
	node2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	vid := f.AddVersion(t, "2.0.0", "dev")
	ref := "ghcr.io/example/game-server:2.0.0"
	disableRaw(t, st, vid)

	r, sender, cleaner := newSweeper(st)
	if err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender.take()); len(got) != 2 {
		t.Fatalf("RemoveImage обязана уйти обеим нодам, got %v", got)
	}

	report(cleaner, node2, ref, "busy")       // на node2 образ жив
	report(cleaner, f.NodeID, ref, "removed") // на node1 снят
	if imageCleanupMarked(t, st, vid) {
		t.Fatal("на одной из нод образ остался (busy) — маркер ставить НЕЛЬЗЯ, даже если остальные отчитались removed")
	}

	// Следующий субтик повторяет обеим; теперь обе сняли → маркер.
	r2, sender2, cleaner2 := newSweeper(st)
	if err := r2.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender2.take()); len(got) != 2 {
		t.Fatalf("непомеченная версия обязана уехать снова обеим нодам, got %v", got)
	}
	report(cleaner2, f.NodeID, ref, "absent") // уже снят прошлым разом
	report(cleaner2, node2, ref, "removed")   // контейнер ушёл — снят и здесь
	if !imageCleanupMarked(t, st, vid) {
		t.Fatal("обе ноды подтвердили — версия обязана быть помечена")
	}
}

// TestImageCleanupSweepSkipsEnvWithOfflineNode (M2): sweep — ПОВТОРИТЕЛЬ, он
// приходит к непомеченной версии каждые 60с. Раньше он слал команду и офлайн-ноде:
// та лишь парковалась в in-memory очереди хаба, маркер (правильно) не ставился — и
// на следующем субтике в ту же очередь ложилась ЕЩЁ ОДНА копия. Нода, офлайн сутки,
// копила тысячи дубликатов и получала их все разом на реконнекте. Правило: нет
// живой сессии хоть у одной целевой ноды окружения — версию пропускаем ЦЕЛИКОМ, не
// отправляя НИЧЕГО и никому (терять нечего: образ с офлайн-ноды всё равно не снять).
func TestImageCleanupSweepSkipsEnvWithOfflineNode(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)
	node2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	vid := f.AddVersion(t, "2.0.0", "dev")
	ref := "ghcr.io/example/game-server:2.0.0"
	disableRaw(t, st, vid)

	// Пасс 1: node2 офлайн → не шлём НИЧЕГО, в том числе живой node1 (образ всё
	// равно останется в окружении, а очередь офлайн-ноды пухнуть не должна).
	r1, sender1, _ := newSweeper(st)
	sender1.goOffline(node2)
	if err := r1.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender1.take()); len(got) != 0 {
		t.Fatalf("нода окружения офлайн — sweep не имеет права слать RemoveImage (очередь офлайн-ноды не льём), got %v", got)
	}
	if imageCleanupMarked(t, st, vid) {
		t.Fatal("ничего не отправлено — маркера быть не может")
	}

	// Пасс 2: нода вернулась → штатная отправка обеим и маркер по отчётам.
	r2, sender2, cleaner2 := newSweeper(st)
	if err := r2.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender2.take()); len(got) != 2 || got[f.NodeID] != ref || got[node2] != ref {
		t.Fatalf("все ноды окружения онлайн — RemoveImage обязана уйти обеим, got %v", got)
	}
	report(cleaner2, f.NodeID, ref, "removed")
	report(cleaner2, node2, ref, "removed")
	if !imageCleanupMarked(t, st, vid) {
		t.Fatal("обе ноды отчитались — версия обязана быть помечена")
	}
}

// TestImageCleanupReportCounter: birdman_image_removals_total{status} считает КАЖДЫЙ
// отчёт агента — включая те, которых никто не ждал (быстрый путь, рестарт мастера,
// дубль из at-least-once outbox'а). Флот, где удаления стабильно возвращаются busy
// или error, — это утечка диска, которая раньше была НЕВИДИМОЙ. Неизвестный статус в
// метрику не пускаем: {status} — лейбл, произвольная строка раздула бы кардинальность.
func TestImageCleanupReportCounter(t *testing.T) {
	st := testdb.New(t)
	counts := map[string]int{}
	c := reconcile.NewImageCleaner(st, &fakeSender{}, quietLog())
	c.SetRemovalCounter(func(status string) { counts[status]++ })

	for _, status := range []string{"removed", "absent", "busy", "error", "removed", "wat"} {
		report(c, "node-x", "ghcr.io/example/game-server:9.9.9", status)
	}
	want := map[string]int{"removed": 2, "absent": 1, "busy": 1, "error": 1}
	if len(counts) != len(want) {
		t.Fatalf("в метрику попал неизвестный статус (кардинальность лейбла!): %v", counts)
	}
	for status, n := range want {
		if counts[status] != n {
			t.Fatalf("status=%s: want %d, got %d (%v)", status, n, counts[status], counts)
		}
	}
}

// TestImageCleanupSweepSurvivesMasterRestartMidRound (tracker #233, факт «а»):
// окно «рестарт master между Send RemoveImage и ImageReport» закрыто ПО
// ПОСТРОЕНИЮ, а не отдельным механизмом. Ожидания живут В ПАМЯТИ: рестарт их
// теряет — но вместе с ними ничего ценного. Маркер versions.image_cleanup_at
// ставится ТОЛЬКО по отчётам ВСЕХ целевых нод, а до тех пор версия остаётся в
// выборке — значит следующий субтик просто переотправит RemoveImage, и агент
// no-op'ит уже снятый образ (absent ≡ removed). Ключевая консервативность:
// поздний/replay-отчёт, которого на рестартнутом инстансе никто не ждёт, маркер НЕ
// подделывает — иначе версия с ещё живым где-то образом «схлопнулась» бы вслепую.
func TestImageCleanupSweepSurvivesMasterRestartMidRound(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // node1 в dev
	node2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	vid := f.AddVersion(t, "2.0.0", "dev")
	ref := "ghcr.io/example/game-server:2.0.0"
	disableRaw(t, st, vid) // серверов нет → сразу кандидат sweep'а

	// Пасс 1: RemoveImage уходит ОБЕИМ нодам; отчиталась только node1.
	r1, sender1, cleaner1 := newSweeper(st)
	if err := r1.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender1.take()); len(got) != 2 || got[f.NodeID] != ref || got[node2] != ref {
		t.Fatalf("пасс 1: RemoveImage обязана уйти обеим нодам, got %v", got)
	}
	report(cleaner1, f.NodeID, ref, "removed")
	if imageCleanupMarked(t, st, vid) {
		t.Fatal("отчиталась только node1 — маркер ставить рано (ждём node2)")
	}

	// «Рестарт мастера»: свежие инстансы, in-memory ожидания cleaner1 потеряны,
	// отчёт node2 в прошлой жизни так и не доехал.
	r2, sender2, cleaner2 := newSweeper(st)

	// Поздний/replay-отчёт node2 прилетает на РЕСТАРТНУТЫЙ cleaner2 ДО переотправки:
	// его никто не ждёт → маркер НЕ ставится (по отчётам он не подделывается).
	report(cleaner2, node2, ref, "removed")
	if imageCleanupMarked(t, st, vid) {
		t.Fatal("отчёт, которого после рестарта никто не ждёт, маркер подделывать не должен")
	}

	// Версия осталась в выборке (маркера нет) → субтик переотправляет ОБЕИМ нодам:
	// окно между Send и Report закрыто по построению, отдельный механизм не нужен.
	if err := r2.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := removeImagesTo(sender2.take()); len(got) != 2 || got[f.NodeID] != ref || got[node2] != ref {
		t.Fatalf("непомеченная версия обязана переотправиться обеим нодам, got %v", got)
	}
	// Образ уже снят прошлой жизнью — агент no-op'ит и отвечает absent (≡ removed).
	report(cleaner2, f.NodeID, ref, "absent")
	report(cleaner2, node2, ref, "absent")
	if !imageCleanupMarked(t, st, vid) {
		t.Fatal("обе ноды подтвердили отсутствие образа (absent) — версия обязана быть помечена")
	}
}
