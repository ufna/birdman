package deploy_test

import (
	"context"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestPullReportSameRefTwoEnvs — C2 (env v1 §3): два деплой-джоба на ОДИН
// image_ref (dev- и prod-версии с общим ref — сценарий промоута) не должны
// съедать отчёты друг друга. PullReport матчится по (image_ref, node ∈ pending
// ЭТОГО job'а), а не по одному ref: отчёт ноды из pending prod-джоба закрывает
// именно prod-джоб, dev-джоб остаётся pending. Заодно фиксирует busy-per-env:
// prod-деплой не сериализуется за in-flight dev-деплоем того же проекта.
func TestPullReportSameRefTwoEnvs(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // project game, dev node A, dev version 1.0.0
	ctx := context.Background()

	// dev-флот + активная 1.0.0 под ним (bootstrap): штатный флип dev-джоба.
	f.UpsertFleet(t, 2, 50) // dev fleet eu, active = 1.0.0

	// prod: нода того же региона eu + флот. active_version prod-флота не нужен —
	// PrePull целит по региону флота, не по его версии.
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10) // входит как dev
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatalf("move node to prod: %v", err)
	}
	buffer, maxServers := int32(2), int32(50)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu",
		BufferReady: &buffer, MaxServers: &maxServers,
	}); err != nil {
		t.Fatalf("prod fleet: %v", err)
	}

	// Общий image_ref: dev/1.1.0 и prod/1.1.0 → оба "…game-server:1.1.0".
	devV := f.AddVersion(t, "1.1.0", "dev")
	prodV := f.AddVersion(t, "1.1.0", "prod")
	ref := "ghcr.io/example/game-server:1.1.0"

	m, _, _ := newManager(t, st, time.Minute)

	// Два деплоя одного ref в разных env — busy-check per env их не сериализует.
	if _, err := m.Deploy(ctx, devV); err != nil {
		t.Fatalf("deploy dev: %v", err)
	}
	if _, err := m.Deploy(ctx, prodV); err != nil {
		t.Fatalf("deploy prod (must not be blocked by dev prepull — busy per env): %v", err)
	}
	if m.PendingNodes(devV) != 1 || m.PendingNodes(prodV) != 1 {
		t.Fatalf("want each job pending 1 node: dev=%d prod=%d", m.PendingNodes(devV), m.PendingNodes(prodV))
	}

	// C2: отчёт prod-ноды закрывает ИМЕННО prod-джоб (node ∈ prod.pending),
	// хотя image_ref совпадает с dev-джобом.
	report(m, prodNode, ref, "pulled")

	if got := versionState(t, st, prodV); got != "active" {
		t.Fatalf("prod deploy must flip on its own node's report, got %s", got)
	}
	if got := versionState(t, st, devV); got != "prepulling" {
		t.Fatalf("dev deploy must stay pending (its node has not reported), got %s", got)
	}
	if m.PendingNodes(devV) != 1 {
		t.Fatalf("dev job must keep its pending node, got %d", m.PendingNodes(devV))
	}
	if m.PendingNodes(prodV) != 0 {
		t.Fatalf("prod job must be done, got %d pending", m.PendingNodes(prodV))
	}

	// dev закрывается СВОИМ отчётом.
	report(m, f.NodeID, ref, "pulled")
	if got := versionState(t, st, devV); got != "active" {
		t.Fatalf("dev deploy must flip on its own node report, got %s", got)
	}
}
