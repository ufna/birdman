package reconcile

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// Statuses an agent reports for one RemoveImage (proto ImageReport.status, §6б).
const (
	imageRemoved = "removed" // deleted from the node's image store
	imageAbsent  = "absent"  // nothing to delete: already gone (replay, or watermark GC won)
	imageBusy    = "busy"    // the ref still backs a live container — nothing was deleted
	imageError   = "error"   // the removal failed; detail carries the text
)

// markTimeout bounds the image_cleanup_at write done from the report path. That
// path runs on the agentlink recv-loop goroutine, so the ctx must NOT be the
// session's: a session dying right after delivering the last report must not
// cancel the marker it just earned. The write is a single tiny UPDATE.
const markTimeout = 10 * time.Second

// ImageCleaner dispatches RemoveImage to a node's agents when a version's image
// must be retired from the fleet — on ANY transition to `disabled` (environments
// v1 §6б): the flip-demote of an older deprecated in ActivateVersion (the main
// dev-stream path), the reap-TTL DisableExpiredDeprecated, and the retention
// subtick RetireVersions. It is shared by the reconcile loop (TTL + retention +
// sweep) and the deploy manager (flip), which both feed it their disabled
// versions — AND it is the agentlink service's ImageSink, because it is also the
// thing that hears the agents' answers (HandleImageReport).
//
// Shared-ref guard (§6б): RemoveImage is WITHHELD when the exact same image_ref
// still belongs to a non-disabled version of that (project, env) — a live
// containerd ref is refcounted and must not be deleted (a promote that reused the
// ref, or two versions built from one image). The comparison is the exact ref
// string (tag ≠ digest; the CI rule is «register one canonical ref», §6б M9).
//
// # Два пути отправки
//
// БЫСТРЫЙ (CleanupImages, fire-and-forget) — в момент перехода в disabled. Он НЕ
// самодостаточен и никогда им не был: переход случается ровно тогда, когда серверы
// версии ещё дренятся, агент видит образ занятым живым контейнером и ничего не
// удаляет. Маркера этот путь не ставит и отчётов не ждёт — он лишь ускоряет
// типичный случай (серверов у версии уже не осталось).
//
// СХОДЯЩИЙСЯ (SweepImages, ~60с субтик реконсайлера) — единственный, кто ставит
// маркер versions.image_cleanup_at («догоняющая команда отработала, версию из
// выборки убираем»). Раньше он штамповал маркер ВСЛЕПУЮ, по факту Send: протокол
// не нёс результата RemoveImage (Ack подтверждает только ПОЛУЧЕНИЕ команды),
// поэтому пропущенное удаление (busy/error) молча терялось навсегда — образ
// доживал до watermark-GC. Теперь агент шлёт ImageReport{status}, и маркер
// ставится ПО ОТЧЁТАМ: только когда КАЖДАЯ целевая нода окружения ответила
// removed|absent (образа на ней больше нет). busy|error оставляют версию без
// маркера → она остаётся в выборке → следующий субтик шлёт снова (контейнеры к
// тому времени уйдут; повтор идемпотентен — агент no-op'ит отсутствующий образ).
//
// # Сходимость
//
// Ожидания живут В ПАМЯТИ (awaitNodes/awaitRef) — и это не дыра, а осознанный
// выбор: рестарт мастера теряет ожидания, но вместе с ними не теряется ничего
// ценного. Маркер-то не проставлен → версия остаётся в VersionsPendingImageCleanup
// → следующий субтик просто отправит RemoveImage заново. Сходимость по построению:
// маркер появляется ТОЛЬКО после подтверждённого удаления, всё остальное — повтор.
type ImageCleaner struct {
	st     *store.Store
	sender Sender
	log    *slog.Logger

	// removals counts every agent ImageReport by status
	// (birdman_image_removals_total{status}). nil until main.go wires it.
	removals func(status string)

	mu sync.Mutex
	// awaitNodes: versionID → целевые ноды, чей вердикт ещё не пришёл. Опустел
	// набор → образ снят со всего окружения → маркер. busy|error сносят ЗАПИСЬ
	// ЦЕЛИКОМ, а не одну ноду: иначе removed от остальных нод опустошил бы остаток
	// набора и проштамповал бы маркер версии, образ которой на busy-ноде жив —
	// ровно та слепота, которую мы и чиним.
	awaitNodes map[string]map[string]struct{}
	// awaitRef: (нода, image_ref) → версии, ждущие вердикта ЭТОЙ ноды.
	//
	// Ключ — не cmd_id, хотя ImageReport его несёт (в логах он есть, для трассы).
	// Отчёт — это факт о МИРЕ («образа ref на ноде n больше нет»), а не о команде:
	// им одинаково валидно закрывается ожидание по этому (node, ref), кем бы команда
	// ни была послана — быстрым путём, прошлым субтиком или этим. Нода принадлежит
	// ровно одному (project, env), так что (node, ref) однозначен. Практический
	// выигрыш: повторная отправка ПЕРЕЗАПИСЫВАЕТ запись вместо того, чтобы плодить
	// новую на каждый субтик (карта не растёт для версии, которая почему-то не
	// сходится), а дубль отчёта (at-least-once outbox агента) не находит записи и
	// штатно игнорируется.
	awaitRef map[waitKey][]string
}

// waitKey identifies one outstanding RemoveImage answer: node × image_ref.
type waitKey struct {
	nodeID   string
	imageRef string
}

// SweepResult is the per-pass summary the sweep logs (Reconciler.sweepImageCleanup).
type SweepResult struct {
	// Dispatched: версии, чья RemoveImage ушла живым сессиям ВСЕХ целевых нод.
	// Маркера у них ЕЩЁ НЕТ — ждём ImageReport'ов (в этом весь смысл).
	Dispatched int
	// Skipped: версии, не тронутые этим проходом — офлайн целевая нода (M2) или
	// придержанный shared-ref. Ни команды, ни маркера; повторим на следующем.
	Skipped int
	// Marked: версии, помеченные ПРЯМО СЕЙЧАС, потому что удалять физически негде —
	// в окружении нет ни одной не-dead ноды. Отчётов ждать не от кого.
	Marked int
}

// NewImageCleaner builds the dispatcher over a command Sender (agentlink.Hub in
// production, a recorder in tests) and the store.
func NewImageCleaner(st *store.Store, sender Sender, log *slog.Logger) *ImageCleaner {
	return &ImageCleaner{
		st: st, sender: sender, log: log,
		awaitNodes: map[string]map[string]struct{}{},
		awaitRef:   map[waitKey][]string{},
	}
}

// SetRemovalCounter wires birdman_image_removals_total{status} (main.go). Every
// agent ImageReport is counted by its status — removed|absent|busy|error — so a
// fleet whose removals keep coming back `busy`/`error` is visible instead of
// quietly leaking disk until the watermark GC fires.
func (c *ImageCleaner) SetRemovalCounter(f func(status string)) { c.removals = f }

// CleanupImages is the FAST path: fire-and-forget RemoveImage for each disabled
// version whose image_ref is no longer used by a non-disabled version of its
// (project, env), to every not-dead node of that (project, env) — online or not
// (an offline node's command is parked in the hub's queue and replayed on
// reconnect; this path fires ONCE per disabled transition, so that queue cannot
// grow without bound the way a 60s repeater would — см. M2 в SweepImages).
//
// Stamps NO marker and awaits NO report: at the moment of the transition the
// version's servers are typically still draining, so the honest answer is `busy`
// (the agent reports it, we count it, and the converging sweep re-sends later).
// Letting this path stamp markers would mean racing the drain for the ONE
// «догоняющая команда» a version is allowed — the sweep exists precisely because
// that race is unwinnable. Best-effort: a store error aborts and is returned to
// the caller (which logs it); a withheld (shared) ref is skipped quietly; an
// empty image_ref is skipped defensively.
func (c *ImageCleaner) CleanupImages(ctx context.Context, disabled []store.DisabledVersion) error {
	for _, g := range groupByRef(disabled) {
		nodeIDs, withheld, err := c.targets(ctx, g)
		if err != nil {
			return err
		}
		if withheld {
			continue
		}
		for _, nodeID := range nodeIDs {
			c.send(nodeID, g, "fast")
		}
	}
	return nil
}

// SweepImages is the CONVERGING path (the reconciler's ~60s cleanup subtick) and
// the only one that stamps versions.image_cleanup_at — from the agents'
// ImageReports, not from the bare fact of a Send (см. док типа).
//
// M2 — НИЧЕГО НЕ ШЛЁМ ОФЛАЙН-НОДАМ. Sweep — это ПОВТОРИТЕЛЬ: версия без маркера
// приходит сюда каждые 60 секунд. Раньше он слал команду и офлайн-ноде: та
// парковалась в in-memory очереди хаба, маркер (правильно) не ставился — и на
// следующем субтике в ту же очередь ложилась ЕЩЁ ОДНА копия. Нода, офлайн сутки,
// накапливала тысячи дубликатов, которые на реконнекте выливались на агента разом.
// Поэтому: нет живой сессии хоть у одной целевой ноды окружения — версию в этом
// проходе пропускаем ЦЕЛИКОМ (ни команды, ни маркера). Терять нечего: пока нода вне
// сети, образ с неё всё равно не снять; вернётся — субтик отработает штатно; не
// вернётся никогда — lease-checker переведёт её в `down` (в `dead` уводит только
// РУЧНАЯ ревокация — store/models.go), а `down`-нода из EnvNodeIDs НЕ выпадает:
// значит, окружение с навсегда пропавшей машиной не чистит образы и на своих
// живых нодах, пока оператор её не ревокнёт (о пропаже кричит NodeDown-алерт;
// диск страхует watermark-GC). «Ни одной ноды в окружении» — вырожденный случай: удалять негде,
// отчётов ждать не от кого, маркер ставим сразу, иначе версия вечно крутилась бы в
// выборке.
func (c *ImageCleaner) SweepImages(ctx context.Context, disabled []store.DisabledVersion) (SweepResult, error) {
	var res SweepResult
	for _, g := range groupByRef(disabled) {
		nodeIDs, withheld, err := c.targets(ctx, g)
		if err != nil {
			return res, err
		}
		if withheld {
			res.Skipped += len(g.versionIDs)
			continue
		}
		if len(nodeIDs) == 0 {
			if err := c.st.MarkImageCleanupSent(ctx, g.versionIDs); err != nil {
				return res, err
			}
			res.Marked += len(g.versionIDs)
			continue
		}
		if offline := c.offline(nodeIDs); len(offline) > 0 {
			c.log.Info("imagecleanup: sweep — целевая нода окружения без живой сессии, версию пропускаем (в очередь офлайн-ноды не льём)",
				"env", g.env, "semver", g.semver, "image_ref", g.imageRef,
				"nodes", len(nodeIDs), "offline_nodes", len(offline), "versions", len(g.versionIDs))
			res.Skipped += len(g.versionIDs)
			continue
		}
		// Ожидание регистрируем ДО отправки: отчёт может прилететь раньше, чем Send
		// вернёт управление, и обязан найти, что закрывать.
		c.expect(g, nodeIDs)
		for _, nodeID := range nodeIDs {
			c.send(nodeID, g, "sweep")
		}
		res.Dispatched += len(g.versionIDs)
	}
	return res, nil
}

// HandleImageReport consumes one agent ImageReport (agentlink.ImageSink) — the
// RESULT of a RemoveImage, which the Ack never carried:
//
//	removed|absent → образа на этой ноде нет. Снимаем её из ожиданий версий,
//	                 которые ждали именно её; опустело ожидание — значит образ снят
//	                 со ВСЕГО окружения → MarkImageCleanupSent (одна маленькая
//	                 UPDATE, идемпотентная: `image_cleanup_at is null` в WHERE).
//	busy|error     → удаления не было. Раунд этих версий сносим целиком: маркера не
//	                 будет, версия остаётся в выборке, следующий субтик отправит
//	                 снова (к тому времени контейнеры уйдут / причина ошибки уйдёт).
//
// Отчёт, которого никто не ждёт (быстрый путь, рестарт мастера, дубль из
// at-least-once outbox агента), — не ошибка: считаем метрику и молча выходим.
func (c *ImageCleaner) HandleImageReport(nodeID string, r *agentlinkv1.ImageReport) {
	status, ref := r.GetStatus(), r.GetImageRef()
	switch status {
	case imageRemoved, imageAbsent, imageBusy, imageError:
		if c.removals != nil {
			c.removals(status)
		}
	default:
		// Чужой/битый статус в метрику не пускаем: {status} — лейбл, произвольная
		// строка от агента раздула бы кардинальность.
		c.log.Warn("imagecleanup: неизвестный статус ImageReport — игнорируем",
			"node_id", nodeID, "image_ref", ref, "status", status, "cmd_id", r.GetCmdId())
		return
	}
	switch status {
	case imageBusy:
		// Штатно в гонке с дренажом (быстрый путь) — не повод шуметь Warn'ом.
		c.log.Info("imagecleanup: образ ещё занят живым контейнером — маркер откладываем, повторим следующим субтиком",
			"node_id", nodeID, "image_ref", ref, "cmd_id", r.GetCmdId())
	case imageError:
		c.log.Warn("imagecleanup: агент не смог снять образ — маркер не ставим, повторим следующим субтиком",
			"node_id", nodeID, "image_ref", ref, "cmd_id", r.GetCmdId(), "detail", r.GetDetail())
	}

	c.mu.Lock()
	k := waitKey{nodeID: nodeID, imageRef: ref}
	versionIDs := c.awaitRef[k]
	delete(c.awaitRef, k)
	var done []string
	for _, vid := range versionIDs {
		nodes, ok := c.awaitNodes[vid]
		if !ok {
			continue // раунд уже закрыт маркером или снесён busy/error с другой ноды
		}
		if status == imageBusy || status == imageError {
			delete(c.awaitNodes, vid)
			continue
		}
		delete(nodes, nodeID)
		if len(nodes) == 0 {
			delete(c.awaitNodes, vid)
			done = append(done, vid)
		}
	}
	c.mu.Unlock()

	if len(done) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), markTimeout)
	defer cancel()
	if err := c.st.MarkImageCleanupSent(ctx, done); err != nil {
		// Маркер не встал — версия осталась в выборке, следующий субтик повторит
		// RemoveImage (агент no-op'ит уже удалённый образ). Сходимость не страдает.
		c.log.Error("imagecleanup: маркер image_cleanup_at не проставлен — повторим на следующем субтике",
			"versions", len(done), "image_ref", ref, "err", err)
		return
	}
	c.log.Info("imagecleanup: образ снят со ВСЕХ целевых нод окружения — версия помечена",
		"versions", len(done), "node_id", nodeID, "image_ref", ref, "status", status)
}

// refGroup — версии одного (project, env, image_ref): ноды окружения и сам ref у
// них совпадают, значит достаточно ОДНОЙ RemoveImage на ноду (y2). Ключ включает
// env: один ref в разных окружениях — это разные ноды (§6б), их не схлопываем.
type refGroup struct {
	projectID  string
	env        string
	imageRef   string
	semver     string // представитель группы, только для логов
	versionIDs []string
}

// groupByRef дедуплицирует батч: при флип-демоуте/ретеншне в disabled может уйти
// сразу N версий одного (project, env) с общим ref. Версия без ref пропускается
// (защитно). Порядок групп детерминирован — как во входном батче.
func groupByRef(disabled []store.DisabledVersion) []refGroup {
	idx := map[string]int{}
	var out []refGroup
	for _, d := range disabled {
		if d.ImageRef == "" {
			continue
		}
		key := d.ProjectID + "\x00" + d.Env + "\x00" + d.ImageRef
		if i, ok := idx[key]; ok {
			out[i].versionIDs = append(out[i].versionIDs, d.VersionID)
			continue
		}
		idx[key] = len(out)
		out = append(out, refGroup{
			projectID: d.ProjectID, env: d.Env, imageRef: d.ImageRef, semver: d.Semver,
			versionIDs: []string{d.VersionID},
		})
	}
	return out
}

// targets resolves a group's RemoveImage targets: every not-dead node of its
// (project, env). withheld=true means the shared-ref guard fired — the exact ref
// still backs a non-disabled version there, so nothing may be sent (§6б).
func (c *ImageCleaner) targets(ctx context.Context, g refGroup) (nodeIDs []string, withheld bool, err error) {
	inUse, err := c.st.ImageRefInUse(ctx, g.projectID, g.env, g.imageRef)
	if err != nil {
		return nil, false, err
	}
	if inUse {
		// Ref числится у не-disabled версии того же (project, env) — контент под
		// живой ссылкой, RemoveImage не шлём (§6б).
		c.log.Info("imagecleanup: image_ref still in use by a non-disabled version — RemoveImage withheld",
			"versions", len(g.versionIDs), "semver", g.semver, "env", g.env, "image_ref", g.imageRef)
		return nil, true, nil
	}
	nodeIDs, err = c.st.EnvNodeIDs(ctx, g.projectID, g.env)
	if err != nil {
		return nil, false, err
	}
	return nodeIDs, false, nil
}

// offline returns those target nodes that hold no live agentlink session right
// now (M2, см. SweepImages).
func (c *ImageCleaner) offline(nodeIDs []string) []string {
	var out []string
	for _, nodeID := range nodeIDs {
		if !c.sender.HasSession(nodeID) {
			out = append(out, nodeID)
		}
	}
	return out
}

// expect records the outstanding answers for one dispatched group: every version
// waits for every target node, and every (node, ref) knows which versions hang on
// it. A re-send OVERWRITES the previous round instead of stacking onto it.
func (c *ImageCleaner) expect(g refGroup, nodeIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, vid := range g.versionIDs {
		nodes := make(map[string]struct{}, len(nodeIDs))
		for _, nodeID := range nodeIDs {
			nodes[nodeID] = struct{}{}
		}
		c.awaitNodes[vid] = nodes
	}
	for _, nodeID := range nodeIDs {
		c.awaitRef[waitKey{nodeID: nodeID, imageRef: g.imageRef}] = append([]string(nil), g.versionIDs...)
	}
}

// send dispatches one RemoveImage and logs it. path is "fast" (immediate,
// fire-and-forget) or "sweep" (converging, awaiting the node's report).
func (c *ImageCleaner) send(nodeID string, g refGroup, path string) {
	cmdID := c.sender.Send(nodeID, &agentlinkv1.MasterMsg{
		Msg: &agentlinkv1.MasterMsg_RemoveImage{RemoveImage: &agentlinkv1.RemoveImage{
			ImageRef: g.imageRef,
		}},
	})
	c.log.Info("imagecleanup: RemoveImage sent",
		"node_id", nodeID, "env", g.env, "semver", g.semver, "image_ref", g.imageRef,
		"cmd_id", cmdID, "path", path, "versions", len(g.versionIDs))
}
