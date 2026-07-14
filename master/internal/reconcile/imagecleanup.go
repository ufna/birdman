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
// this dispatcher is best-effort and never blocks a disabled transition. That
// replay queue is IN-MEMORY, though, so «Send'ed» ≠ «will be delivered» across a
// master restart — hence CleanupImagesDelivered, which tells the sweep which
// versions may be marked as done (I-1).
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
// quietly. An empty image_ref is skipped defensively. Fire-and-forget — the
// immediate-dispatch callers (flip-demote, reap-TTL, retention) stamp no marker,
// the sweep re-sends for them later.
func (c *ImageCleaner) CleanupImages(ctx context.Context, disabled []store.DisabledVersion) error {
	_, err := c.cleanupImages(ctx, disabled)
	return err
}

// CleanupImagesDelivered is CleanupImages for the CONVERGING sweep: it additionally
// reports which versions' RemoveImage was handed to a LIVE session of EVERY target
// node of their (project, env) — the only ones the sweep may stamp image_cleanup_at
// for (I-1, reconcile.SessionChecker).
//
// Send never fails, but a command for an offline node is only PARKED in the hub's
// in-memory pending queue — a master restart wipes it. Stamping the «одна догоняющая
// команда» marker for such a version would lose the RemoveImage for good (the marker
// removes it from VersionsPendingImageCleanup), so those versions stay unmarked and
// the next subtick sends again; a duplicate is harmless (the agent no-ops a missing
// image). A version whose ref is WITHHELD (shared-ref guard) sent nothing at all and
// is likewise not reported — once the ref frees up it still gets its one catch-up
// command. A (project, env) with zero target nodes is vacuously delivered: there is
// nothing to send, so re-querying it every subtick forever would be pure noise.
//
// NOT an ack: the residual window «master restarts between Send and Ack of an ONLINE
// node» stays open (documented at reconcile.sweepImageCleanup) — closing it needs a
// persistent command queue.
func (c *ImageCleaner) CleanupImagesDelivered(ctx context.Context, disabled []store.DisabledVersion) ([]string, error) {
	return c.cleanupImages(ctx, disabled)
}

// cleanupImages is the shared dispatch: it returns the version ids whose RemoveImage
// reached a live session of every target node (see CleanupImagesDelivered).
func (c *ImageCleaner) cleanupImages(ctx context.Context, disabled []store.DisabledVersion) ([]string, error) {
	// Дедуп внутри батча по (project, env, image_ref): при флип-демоуте/ретеншне
	// в disabled может уйти сразу N версий одного (project, env) с общим ref —
	// ноды окружения и сам ref у них совпадают, значит достаточно ОДНОЙ отправки
	// RemoveImage на ноду. Ключ включает env: один ref в разных окружениях — это
	// разные ноды (§6б), их не схлопываем. delivered хранит вердикт по ключу, чтобы
	// схлопнутые версии унаследовали результат единственной отправки.
	delivered := map[string]bool{}
	var out []string
	for _, d := range disabled {
		if d.ImageRef == "" {
			continue
		}
		key := d.ProjectID + "\x00" + d.Env + "\x00" + d.ImageRef
		if _, done := delivered[key]; !done {
			live, err := c.dispatch(ctx, d)
			if err != nil {
				return nil, err
			}
			delivered[key] = live
		}
		if delivered[key] {
			out = append(out, d.VersionID)
		}
	}
	return out, nil
}

// dispatch sends one version's RemoveImage to every not-dead node of its
// (project, env) and reports whether ALL of them had a live agentlink session at
// that moment (an empty node set → true, см. CleanupImagesDelivered). A withheld
// (shared) ref sends nothing and reports false.
func (c *ImageCleaner) dispatch(ctx context.Context, d store.DisabledVersion) (bool, error) {
	inUse, err := c.st.ImageRefInUse(ctx, d.ProjectID, d.Env, d.ImageRef)
	if err != nil {
		return false, err
	}
	if inUse {
		// Ref числится у не-disabled версии того же (project, env) — контент под
		// живой ссылкой, RemoveImage не шлём (§6б).
		c.log.Info("imagecleanup: image_ref still in use by a non-disabled version — RemoveImage withheld",
			"version_id", d.VersionID, "semver", d.Semver, "env", d.Env, "image_ref", d.ImageRef)
		return false, nil
	}
	nodeIDs, err := c.st.EnvNodeIDs(ctx, d.ProjectID, d.Env)
	if err != nil {
		return false, err
	}
	allLive := true
	for _, nodeID := range nodeIDs {
		live := c.sender.HasSession(nodeID)
		if !live {
			// Нода офлайн: команда легла в in-memory pending хаба и не переживёт
			// рестарт мастера — вердикт по версии отрицательный (маркер не ставим).
			allLive = false
		}
		cmdID := c.sender.Send(nodeID, &agentlinkv1.MasterMsg{
			Msg: &agentlinkv1.MasterMsg_RemoveImage{RemoveImage: &agentlinkv1.RemoveImage{
				ImageRef: d.ImageRef,
			}},
		})
		c.log.Info("imagecleanup: RemoveImage sent",
			"node_id", nodeID, "env", d.Env, "semver", d.Semver, "image_ref", d.ImageRef,
			"cmd_id", cmdID, "session_live", live)
	}
	return allLive, nil
}
