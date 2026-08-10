package store_test

import (
	"testing"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Порядок ListProjects — ЧАСТЬ КОНТРАКТА, а не деталь: селектор проекта в
// панели берёт первую строку как выбор по умолчанию (resolveProject), поэтому
// неполный порядок означал бы, что дефолт прыгает между перезагрузками.
//
// Здесь проверяется именно тайбрейк по слагу: created_at сам по себе полного
// порядка не задаёт — он per-transaction (`now()`), и две регистрации,
// приехавшие одновременно (или восстановленные из дампа), законно делят
// микросекунду. Равенство здесь ставится явным UPDATE — воспроизводить гонку
// часов в тесте бессмысленно и флейко.
func TestListProjectsSlugTiebreakOnEqualCreatedAt(t *testing.T) {
	st := testdb.New(t)
	ctx := t.Context()
	for _, slug := range []string{"zulu", "alpha", "mike"} {
		if _, err := st.SetProjectMatchSize(ctx, slug, 2); err != nil {
			t.Fatalf("create %s: %v", slug, err)
		}
	}
	// Схлопываем все три в одну метку времени: остаётся только тайбрейк.
	if _, err := st.Pool.Exec(ctx, `update projects set created_at = '2026-08-01T00:00:00Z'`); err != nil {
		t.Fatalf("flatten created_at: %v", err)
	}

	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, 0, len(projects))
	for _, p := range projects {
		got = append(got, p.Slug)
	}
	want := []string{"alpha", "mike", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("порядок при равном created_at должен быть по слагу: want %v, got %v", want, got)
		}
	}
}

// created_at выигрывает у слага: более ранний проект первым, даже если его слаг
// лексикографически больше. Без этого теста тайбрейк выше мог бы «пройти»
// сортировкой по одному слагу, потеряв главный ключ.
func TestListProjectsCreatedAtOutranksSlug(t *testing.T) {
	st := testdb.New(t)
	ctx := t.Context()
	for _, slug := range []string{"zulu", "alpha"} {
		if _, err := st.SetProjectMatchSize(ctx, slug, 2); err != nil {
			t.Fatalf("create %s: %v", slug, err)
		}
	}
	if _, err := st.Pool.Exec(ctx,
		`update projects set created_at = '2026-08-01T00:00:00Z' where slug = 'zulu'`); err != nil {
		t.Fatalf("age zulu: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`update projects set created_at = '2026-08-02T00:00:00Z' where slug = 'alpha'`); err != nil {
		t.Fatalf("age alpha: %v", err)
	}

	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(projects) != 2 || projects[0].Slug != "zulu" || projects[1].Slug != "alpha" {
		t.Fatalf("старейший должен быть первым независимо от слага: %v", projects)
	}
}

// ListNodes/ListVersions с фильтром по проекту (мультипроект W2): чужие строки
// не доезжают до панели вовсе, а не отфильтровываются в браузере.
func TestListNodesAndVersionsFilterByProject(t *testing.T) {
	st := testdb.New(t)
	ctx := t.Context()
	testdb.Seed(t, st, "eu", 10) // проект "game": нода node-1 + версия 1.0.0 в dev

	// Второй проект со своей нодой и своей версией.
	if _, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "arena", Region: "eu", Hostname: "arena-1",
		PublicIP: "203.0.113.99", CapacitySlots: 4,
	}); err != nil {
		t.Fatalf("create arena node: %v", err)
	}
	if _, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "arena", Semver: "9.9.9", ImageRef: "ghcr.io/example/arena:9.9.9", Env: "dev",
	}); err != nil {
		t.Fatalf("create arena version: %v", err)
	}

	nodes, err := st.ListNodes(ctx, store.NodeFilter{Project: "game"})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Project != "game" {
		t.Fatalf("фильтр по проекту должен отдать только ноды game: %v", nodes)
	}
	// Контроль: без фильтра видны обе — иначе тест выше проходил бы и на
	// сломанном запросе, который просто ничего не отдаёт.
	all, err := st.ListNodes(ctx, store.NodeFilter{})
	if err != nil {
		t.Fatalf("list all nodes: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("без фильтра должны быть обе ноды, got %d", len(all))
	}

	versions, err := st.ListVersions(ctx, store.VersionFilter{Project: "arena"})
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 1 || versions[0].Project != "arena" || versions[0].Semver != "9.9.9" {
		t.Fatalf("фильтр по проекту должен отдать только версии arena: %v", versions)
	}
	allV, err := st.ListVersions(ctx, store.VersionFilter{})
	if err != nil {
		t.Fatalf("list all versions: %v", err)
	}
	if len(allV) != 2 {
		t.Fatalf("без фильтра должны быть обе версии, got %d", len(allV))
	}

	// Фильтры складываются: env сужает уже суженное по проекту.
	prodV, err := st.ListVersions(ctx, store.VersionFilter{Project: "arena", Env: "prod"})
	if err != nil {
		t.Fatalf("list arena/prod: %v", err)
	}
	if len(prodV) != 0 {
		t.Fatalf("в arena/prod версий нет, got %v", prodV)
	}
}
