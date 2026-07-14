package reconcile

import (
	"context"
	"log/slog"

	"github.com/ufna/birdman/master/internal/store"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// ImageCleaner dispatches RemoveImage to a node's agents when a version's image
// must be retired from the fleet — on ANY transition to `disabled` (environments
// v1 §6б): the flip-demote of an older deprecated in ActivateVersion (the main
// dev-stream path), the reap-TTL DisableExpiredDeprecated, and the retention
// subtick RetireVersions. It is shared by the reconcile loop (TTL + retention)
// and the deploy manager (flip), which both feed it their disabled versions.
//
// Shared-ref guard (§6б): RemoveImage is WITHHELD when the exact same image_ref
// still belongs to a non-disabled version of that (project, env) — a live
// containerd ref is refcounted and must not be deleted (a promote that reused the
// ref, or two versions built from one image). The comparison is the exact ref
// string (tag ≠ digest; the CI rule is «register one canonical ref», §6б M9).
//
// Delivery is at-least-once via the agentlink hub: offline nodes get the command
// replayed on reconnect; a missed RemoveImage self-heals (agent EnsureImage
// re-pulls on StartServer) and the watermark GC is the ultimate backstop — so
// this dispatcher is best-effort and never blocks a disabled transition.
//
// Гонка disabled×drain (Фаза D): отправка В МОМЕНТ перехода — быстрый путь, и он
// НЕ самодостаточен. Переход в disabled случается ровно тогда, когда серверы этой
// версии ещё дренятся: агент видит образ занятым живым контейнером и скипает
// команду, а повторить её некому. Поэтому четвёртый источник батчей —
// сходящийся sweep реконсайлера (Reconciler.sweepImageCleanup, ~60с субтик):
// VersionsPendingImageCleanup отдаёт disabled-версии, чьи контейнеры уже ушли, и
// они получают ровно одну догоняющую RemoveImage (маркер image_cleanup_at).
type ImageCleaner struct {
	st     *store.Store
	sender Sender
	log    *slog.Logger
}

// NewImageCleaner builds the dispatcher over a command Sender (agentlink.Hub in
// production, a recorder in tests) and the store.
func NewImageCleaner(st *store.Store, sender Sender, log *slog.Logger) *ImageCleaner {
	return &ImageCleaner{st: st, sender: sender, log: log}
}

// CleanupImages sends RemoveImage for each disabled version whose image_ref is no
// longer used by a non-disabled version of its (project, env), to every not-dead
// node of that (project, env). Best-effort per version: a store error aborts and
// is returned to the caller (which logs it); a withheld ref (shared) is skipped
// quietly. An empty image_ref is skipped defensively.
func (c *ImageCleaner) CleanupImages(ctx context.Context, disabled []store.DisabledVersion) error {
	// Дедуп внутри батча по (project, env, image_ref): при флип-демоуте/ретеншне
	// в disabled может уйти сразу N версий одного (project, env) с общим ref —
	// ноды окружения и сам ref у них совпадают, значит достаточно ОДНОЙ отправки
	// RemoveImage на ноду. Ключ включает env: один ref в разных окружениях — это
	// разные ноды (§6б), их не схлопываем.
	sent := map[string]bool{}
	for _, d := range disabled {
		if d.ImageRef == "" {
			continue
		}
		key := d.ProjectID + "\x00" + d.Env + "\x00" + d.ImageRef
		if sent[key] {
			continue
		}
		sent[key] = true
		inUse, err := c.st.ImageRefInUse(ctx, d.ProjectID, d.Env, d.ImageRef)
		if err != nil {
			return err
		}
		if inUse {
			// Ref числится у не-disabled версии того же (project, env) — контент под
			// живой ссылкой, RemoveImage не шлём (§6б).
			c.log.Info("imagecleanup: image_ref still in use by a non-disabled version — RemoveImage withheld",
				"version_id", d.VersionID, "semver", d.Semver, "env", d.Env, "image_ref", d.ImageRef)
			continue
		}
		nodeIDs, err := c.st.EnvNodeIDs(ctx, d.ProjectID, d.Env)
		if err != nil {
			return err
		}
		for _, nodeID := range nodeIDs {
			cmdID := c.sender.Send(nodeID, &agentlinkv1.MasterMsg{
				Msg: &agentlinkv1.MasterMsg_RemoveImage{RemoveImage: &agentlinkv1.RemoveImage{
					ImageRef: d.ImageRef,
				}},
			})
			c.log.Info("imagecleanup: RemoveImage sent",
				"node_id", nodeID, "env", d.Env, "semver", d.Semver, "image_ref", d.ImageRef, "cmd_id", cmdID)
		}
	}
	return nil
}
