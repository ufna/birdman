package store_test

// Каскады удаления и роллапы статистики (находка ревью по #981).
//
// match_stats_daily не несёт project_id (uuid), но НЕСЁТ колонку `project` —
// она в первичном ключе (миграция 000017). Прежняя адресация «по имени
// окружения, если оно не занято другим проектом» промахивалась в обе стороны, и
// обе — молча:
//
//  1. dev/prod сидируются КАЖДОМУ проекту, поэтому «имя занято другим» истинно
//     почти всегда → не удалялось ничего, и новый проект с тем же слагом
//     унаследовал бы статистику мёртвого;
//  2. на уникальном имени окружения delete срабатывал по имени и уносил строки
//     ЧУЖИХ проектов.
//
// Тесты проверяют оба направления и на удалении проекта, и на удалении
// окружения (там дефект был предсуществующим — из него и скопирован).

import (
	"context"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// seedRollup кладёт строку дневного роллапа для (project, env).
func seedRollup(t *testing.T, st *store.Store, project, env, semver string) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(), `
		insert into match_stats_daily (day, region, semver, env, project, matches)
		values ($1, 'eu', $2, $3, $4, 1)
		on conflict do nothing`,
		time.Now().UTC().Format("2006-01-02"), semver, env, project); err != nil {
		t.Fatalf("seed rollup %s/%s: %v", project, env, err)
	}
}

func rollupCount(t *testing.T, st *store.Store, project string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(),
		`select count(*) from match_stats_daily where project = $1`, project).Scan(&n); err != nil {
		t.Fatalf("count rollups %s: %v", project, err)
	}
	return n
}

// TestDeleteProjectClearsOwnRollupsOnly: свои роллапы уносятся (иначе следующий
// проект с тем же слагом получит чужую статистику), чужие — остаются.
func TestDeleteProjectClearsOwnRollupsOnly(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatalf("create neighbour: %v", err)
	}
	// Одно и то же имя окружения у обоих проектов — нормальный случай (dev/prod
	// сидируются каждому), именно на нём прежний запрос не удалял ничего.
	seedRollup(t, st, f.Project, "dev", "1.0.0")
	seedRollup(t, st, "neighbour", "dev", "1.0.0")

	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke node: %v", err)
	}
	if _, err := st.DeleteProject(ctx, f.Project, f.Project); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	if n := rollupCount(t, st, f.Project); n != 0 {
		t.Fatalf("роллапы удалённого проекта остались (%d): следующий проект с тем же слагом унаследует их", n)
	}
	if n := rollupCount(t, st, "neighbour"); n != 1 {
		t.Fatalf("снесены роллапы ЧУЖОГО проекта: осталось %d, ждём 1", n)
	}
}

// TestDeleteEnvironmentClearsOwnRollupsOnly: то же уровнем ниже. Имя окружения
// уникально для платформы — прежний запрос сносил по нему строки чужого проекта.
func TestDeleteEnvironmentClearsOwnRollupsOnly(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatalf("create neighbour: %v", err)
	}
	for _, p := range []string{f.Project, "neighbour"} {
		if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: p, Name: "staging"}); err != nil {
			t.Fatalf("create staging in %s: %v", p, err)
		}
		seedRollup(t, st, p, "staging", "1.0.0")
	}

	if _, err := st.DeleteEnvironment(ctx, f.Project, "staging", "staging"); err != nil {
		t.Fatalf("delete environment: %v", err)
	}

	var own int
	if err := st.Pool.QueryRow(ctx,
		`select count(*) from match_stats_daily where project = $1 and env = 'staging'`, f.Project).Scan(&own); err != nil {
		t.Fatal(err)
	}
	if own != 0 {
		t.Fatalf("роллапы удалённого окружения остались (%d)", own)
	}
	var neighbour int
	if err := st.Pool.QueryRow(ctx,
		`select count(*) from match_stats_daily where project = 'neighbour' and env = 'staging'`).Scan(&neighbour); err != nil {
		t.Fatal(err)
	}
	if neighbour != 1 {
		t.Fatalf("снесены роллапы чужого проекта: осталось %d, ждём 1", neighbour)
	}
}

// TestDeleteProjectKeepsUnattributedRollups: строка project='' — НЕ ничейный
// мусор, а документированный маркер «не атрибутировано» (миграция 000017):
// комбинация (day, region, semver, env), которую делят два проекта, при
// бэкфилле осознанно осталась без владельца. Каскад одного из проектов такую
// строку трогать не имеет права — в ней и чужие цифры тоже.
func TestDeleteProjectKeepsUnattributedRollups(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	seedRollup(t, st, f.Project, "dev", "1.0.0")
	seedRollup(t, st, "", "dev", "9.9.9") // не атрибутирована

	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke node: %v", err)
	}
	if _, err := st.DeleteProject(ctx, f.Project, f.Project); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	if n := rollupCount(t, st, f.Project); n != 0 {
		t.Fatalf("свои роллапы остались: %d", n)
	}
	if n := rollupCount(t, st, ""); n != 1 {
		t.Fatalf("снесена неатрибутированная строка (в ней есть чужие цифры): осталось %d, ждём 1", n)
	}
}
