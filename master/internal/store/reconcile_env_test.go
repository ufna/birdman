package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Environments v1 — C1 (docs/superpowers/specs/2026-07-13-environments-v1-design.md
// §3): PlanFleet идёт по (project, env, region) и КАЖДАЯ из шести выборок
// серверов env-скоуплена. Без этого dev-проход реапит/дренит prod-серверы
// соседнего флота того же региона (и наоборот). Ноды и серверы обоих env живут
// в ОДНОМ регионе — именно эту коллизию и проверяем.

// mustUpsertFleet ставит флот (project, env, region) с active-версией и делает
// версию действительно active — компактный «живой» флот окружения для
// cross-env тестов.
func mustUpsertFleet(t *testing.T, st *store.Store, project, env, region, activeVersion string) store.FleetConfig {
	t.Helper()
	ctx := context.Background()
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: project, Env: env, Region: region, ActiveVersion: &activeVersion,
	}); err != nil {
		t.Fatalf("upsert fleet %s/%s/%s: %v", project, env, region, err)
	}
	if _, err := st.Pool.Exec(ctx,
		`update versions set state = 'active' where id = $1::uuid`, activeVersion); err != nil {
		t.Fatalf("activate version: %v", err)
	}
	fleets, err := st.ListFleetConfigs(ctx)
	if err != nil {
		t.Fatalf("list fleets: %v", err)
	}
	for _, fc := range fleets {
		if fc.Project == project && fc.Env == env && fc.Region == region {
			return fc
		}
	}
	t.Fatalf("fleet %s/%s/%s not found after upsert", project, env, region)
	return store.FleetConfig{}
}

func assertServerEnv(t *testing.T, st *store.Store, serverID, want string) {
	t.Helper()
	var env string
	if err := st.Pool.QueryRow(context.Background(),
		`select env from servers where id = $1::uuid`, serverID).Scan(&env); err != nil {
		t.Fatalf("server %s env: %v", serverID, err)
	}
	if env != want {
		t.Fatalf("server %s: want env %q, got %q (cross-env leak)", serverID, want, env)
	}
}

func assertNodeEnv(t *testing.T, st *store.Store, nodeID, want string) {
	t.Helper()
	var env string
	if err := st.Pool.QueryRow(context.Background(),
		`select env from nodes where id = $1::uuid`, nodeID).Scan(&env); err != nil {
		t.Fatalf("node %s env: %v", nodeID, err)
	}
	if env != want {
		t.Fatalf("node %s: want env %q, got %q (placed on foreign-env node)", nodeID, want, env)
	}
}

// TestUpsertFleetRejectsForeignEnvVersion: active_version из ЧУЖОГО env
// (prod-версия в dev-флоте) отбивается составным FK fleet_active_version_env_fk
// (C3) и мапится в понятную ErrNotFound-семантику — НЕ сырой 500 (перенесённый
// Minor ревью Task 1: пре-чек active_version не знал env). httpapi отдаёт 400.
func TestUpsertFleetRejectsForeignEnvVersion(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev-фикстура (проект game, env dev)
	ctx := context.Background()

	prodV := f.AddVersion(t, "2.0.0", "prod")
	_, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu", ActiveVersion: &prodV,
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("prod version in dev fleet: want clean ErrNotFound-wrapped error, got %v", err)
	}
}

// TestPlanFleetEnvIsolation: dev-флот не видит и не реапит prod-серверы того же
// региона (C1). Также фиксирует I6 — insert servers проставляет env флота.
func TestPlanFleetEnvIsolation(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // project game, dev node A, dev version 1.0.0
	ctx := context.Background()

	// prod: env-нода, версия, флот в ТОМ ЖЕ регионе eu.
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10) // входит как dev
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatalf("move node to prod: %v", err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	mustUpsertFleet(t, st, "game", "prod", "eu", prodV)

	devFleet := f.UpsertFleet(t, 2, 50) // dev-флот, active = 1.0.0, buffer 2

	// prod-сервер ready на prod-ноде (env=prod).
	prodReady := f.InsertServerOn(t, prodNode, prodV, "ready")

	starts, stops, drains, locked, err := st.PlanFleet(ctx, devFleet, nil, map[string][]string{})
	if err != nil || !locked {
		t.Fatalf("plan: locked=%v err=%v", locked, err)
	}

	// dev-план не трогает чужой env: всё, что дренит/стопает — dev; всё, что
	// стартует — на dev-нодах и с env=dev (I6).
	for _, s := range stops {
		assertServerEnv(t, st, s.ServerID, "dev")
	}
	for _, s := range drains {
		assertServerEnv(t, st, s.ServerID, "dev")
	}
	for _, s := range starts {
		assertNodeEnv(t, st, s.NodeID, "dev")
		assertServerEnv(t, st, s.ServerID, "dev")
	}
	// prod-ready жив (не съеден реапом вне окна dev).
	if sv, err := st.GetServer(ctx, prodReady); err != nil || sv.State != "ready" {
		t.Fatalf("prod ready server must be untouched: state=%s err=%v", sv.State, err)
	}
	// dev добирает буфер полностью — prod-нагрузка не в total dev-флота.
	if len(starts) != 2 {
		t.Fatalf("dev deficit: want 2 starts, got %d", len(starts))
	}
}

// TestPlanFleetDrainAllocatedEnvScoped: дрейн live-матчей вне окна берёт только
// свой env. Позитивный контроль — dev-матч старой (вне окна) версии дренится.
func TestPlanFleetDrainAllocatedEnvScoped(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	devOld := f.VersionID
	devNew := f.AddVersion(t, "1.1.0", "dev")
	buffer := int32(0)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu", ActiveVersion: &devNew, BufferReady: &buffer,
	}); err != nil {
		t.Fatalf("dev fleet: %v", err)
	}
	devFleet := f.FleetConfig(t, "dev", "eu")

	// prod: нода + версия + живой prod-матч (allocated) того же региона.
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatal(err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	prodMatch := f.InsertServerOn(t, prodNode, prodV, "allocated")

	// dev: живой матч на СТАРОЙ dev-версии (вне окна dev).
	devOldMatch := f.InsertServerOn(t, f.NodeID, devOld, "allocated")

	_, _, drains, locked, err := st.PlanFleet(ctx, devFleet, nil, map[string][]string{})
	if err != nil || !locked {
		t.Fatalf("plan: locked=%v err=%v", locked, err)
	}
	if len(drains) != 1 || drains[0].ServerID != devOldMatch {
		t.Fatalf("want exactly 1 drain of dev old-version match %s, got %+v", devOldMatch, drains)
	}
	assertServerEnv(t, st, drains[0].ServerID, "dev")
	if sv, err := st.GetServer(ctx, prodMatch); err != nil || sv.State != "allocated" {
		t.Fatalf("prod match must keep playing: state=%s err=%v", sv.State, err)
	}
}

// TestPlanFleetSurplusEnvScoped: реап surplus/ready dev-флота не трогает
// prod-ready того же региона (вне окна dev, но чужой env).
func TestPlanFleetSurplusEnvScoped(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 20)
	ctx := context.Background()

	buffer := int32(1)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu", ActiveVersion: &f.VersionID, BufferReady: &buffer,
	}); err != nil {
		t.Fatalf("dev fleet: %v", err)
	}
	devFleet := f.FleetConfig(t, "dev", "eu")

	// 3 dev-ready активной версии → surplus 2 (buffer 1).
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 3*time.Hour)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20002, 2*time.Hour)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20003, time.Hour)

	// prod: нода + ready-сервер прод-версии того же региона.
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 20)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatal(err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	prodReady := f.InsertServerOn(t, prodNode, prodV, "ready")

	_, stops, _, locked, err := st.PlanFleet(ctx, devFleet, nil, map[string][]string{})
	if err != nil || !locked {
		t.Fatalf("plan: locked=%v err=%v", locked, err)
	}
	if len(stops) != 2 {
		t.Fatalf("want 2 dev surplus stops, got %d", len(stops))
	}
	for _, s := range stops {
		assertServerEnv(t, st, s.ServerID, "dev")
	}
	if sv, err := st.GetServer(ctx, prodReady); err != nil || sv.State != "ready" {
		t.Fatalf("prod ready must survive dev surplus reap: state=%s err=%v", sv.State, err)
	}
}

// TestPlanFleetTotalEnvScoped: prod-нагрузка того же региона не съедает
// max_servers dev-флота (total env-скоуплен), и prod-allocated не дренится.
func TestPlanFleetTotalEnvScoped(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 20)
	ctx := context.Background()

	buffer, maxServers := int32(2), int32(2) // тесный кап
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu", ActiveVersion: &f.VersionID,
		BufferReady: &buffer, MaxServers: &maxServers,
	}); err != nil {
		t.Fatalf("dev fleet: %v", err)
	}
	devFleet := f.FleetConfig(t, "dev", "eu")

	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 20)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatal(err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	p1 := f.InsertServerOn(t, prodNode, prodV, "allocated")
	p2 := f.InsertServerOn(t, prodNode, prodV, "allocated")

	starts, _, _, locked, err := st.PlanFleet(ctx, devFleet, nil, map[string][]string{})
	if err != nil || !locked {
		t.Fatalf("plan: locked=%v err=%v", locked, err)
	}
	if len(starts) != 2 {
		t.Fatalf("want 2 dev starts (prod must not eat max_servers), got %d", len(starts))
	}
	for _, s := range starts {
		assertNodeEnv(t, st, s.NodeID, "dev")
	}
	for _, id := range []string{p1, p2} {
		if sv, err := st.GetServer(ctx, id); err != nil || sv.State != "allocated" {
			t.Fatalf("prod allocated must survive: state=%s err=%v", sv.State, err)
		}
	}
}

// TestPlanFleetCandidateNodesEnvScoped: дефицит dev-флота не переливается на
// prod-ноду того же региона (n.env-скоуп кандидатов размещения, C1).
func TestPlanFleetCandidateNodesEnvScoped(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 1) // dev-нода A, capacity 1
	ctx := context.Background()

	// Забить dev-ноду (1/1) живым dev-сервером.
	f.InsertServer(t, f.NodeID, f.VersionID, "allocated", 20001, 0)

	buffer := int32(2)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu", ActiveVersion: &f.VersionID, BufferReady: &buffer,
	}); err != nil {
		t.Fatalf("dev fleet: %v", err)
	}
	devFleet := f.FleetConfig(t, "dev", "eu")

	// prod-нода со свободной ёмкостью того же региона.
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatal(err)
	}

	starts, _, _, locked, err := st.PlanFleet(ctx, devFleet, nil, map[string][]string{})
	if err != nil || !locked {
		t.Fatalf("plan: locked=%v err=%v", locked, err)
	}
	if len(starts) != 0 {
		t.Fatalf("dev deficit must not spill onto a prod node, got %d starts", len(starts))
	}
}

// TestPlanFleetLockKeyPerEnv: advisory-ключ различает env (M3). Держим
// сессионную блокировку на dev-ключе — dev-план не может залочиться, а
// prod-план того же региона берёт СВОЙ ключ и проходит (не сериализуется за dev).
func TestPlanFleetLockKeyPerEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	devFleet := f.UpsertFleet(t, 2, 50)

	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatal(err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	prodFleet := mustUpsertFleet(t, st, "game", "prod", "eu", prodV)

	// Держим СЕССИОННУЮ advisory-блокировку на dev-ключе (то же выражение
	// ключа, что берёт PlanFleet: project:env:region) на отдельном соединении.
	conn, err := st.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	devKey := devFleet.ProjectID + ":dev:eu"
	if _, err := conn.Exec(ctx,
		`select pg_advisory_lock(hashtextextended($1, 42))`, devKey); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `select pg_advisory_unlock_all()`)

	// dev-план не берёт свой (занятый) ключ → locked=false (try-lock, без вечной блокировки).
	if _, _, _, lockedDev, err := st.PlanFleet(ctx, devFleet, nil, map[string][]string{}); err != nil {
		t.Fatal(err)
	} else if lockedDev {
		t.Fatalf("dev plan must fail to lock while its key is held")
	}
	// prod-план того же региона берёт СВОЙ ключ (…:prod:eu) — env в ключе.
	if _, _, _, lockedProd, err := st.PlanFleet(ctx, prodFleet, nil, map[string][]string{}); err != nil {
		t.Fatal(err)
	} else if !lockedProd {
		t.Fatalf("prod plan must lock — different env key, not serialized behind dev")
	}
}
