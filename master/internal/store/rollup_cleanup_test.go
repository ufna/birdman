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
//
// Второй круг ревью нашёл ТОТ ЖЕ класс в СОСЕДНЕЙ таблице: колонку `project`
// той же миграцией 000017 получила и match_ccu_daily, а каскад проекта её не
// чистил — новый проект с тем же слагом наследовал пик CCU мёртвого. Это
// закрыто ниже (TestDeleteProjectClearsOwnPeakCCU), и там же закреплена
// разница в семантике ''-строки между двумя таблицами.

import (
	"context"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	"github.com/ufna/birdman/master/internal/utctime"
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

// seedCCU кладёт строку дневного пика CCU для (day, project) — PK таблицы после
// миграции 000017 ровно эта пара.
func seedCCU(t *testing.T, st *store.Store, day time.Time, project string, peak int) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`insert into match_ccu_daily (day, project, peak_ccu) values ($1, $2, $3)`,
		day, project, peak); err != nil {
		t.Fatalf("seed ccu %s/%q: %v", utctime.DayKey(day), project, err)
	}
}

func ccuCount(t *testing.T, st *store.Store, project string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(),
		`select count(*) from match_ccu_daily where project = $1`, project).Scan(&n); err != nil {
		t.Fatalf("count ccu %q: %v", project, err)
	}
	return n
}

// TestDeleteProjectClearsOwnPeakCCU: тот же класс порчи, что у match_stats_daily
// выше, но в СОСЕДНЕЙ таблице — колонку `project` обеим дала одна миграция
// 000017, а каскад чистил только первую. Читает match_ccu_daily проектным срезом
// store.RollupPeakCCU, им закрыта иммутабельная часть окна /v1/stats/overview.
//
// Видно это ровно на пересоздании проекта с ТЕМ ЖЕ слагом — а это не край:
// переименования слага нет by design, и пересоздать проект под тем же именем —
// штатный путь исправления ошибки.
//
// Три вещи проверяются ОДНОВРЕМЕННО, потому что порознь каждую легко выполнить
// неверной правкой: свои строки ушли; ЧУЖИЕ (соседний проект) целы; ''-строка
// цела — в ЭТОЙ таблице она не «не атрибутировано», как в match_stats_daily, а
// платформенный пик И маркер «день посчитан», на наличии которого держится
// различение «день был пустой» / «день не роллапился» (RollupPeakCCU).
func TestDeleteProjectClearsOwnPeakCCU(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatalf("create neighbour: %v", err)
	}

	// День из ИММУТАБЕЛЬНОЙ части окна: [today-29, today-2] пересчитывает
	// statsrollup.Backfill при рестарте мастера, а тик — только [today-1, today].
	// Поэтому без уборки строка живёт от удаления до ближайшего рестарта.
	day := utctime.StartOfDay(time.Now().UTC().AddDate(0, 0, -10))
	seedCCU(t, st, day, f.Project, 42)
	seedCCU(t, st, day, "neighbour", 7)
	seedCCU(t, st, day, "", 100) // платформенный пик + маркер «день посчитан»

	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke node: %v", err)
	}
	if _, err := st.DeleteProject(ctx, f.Project, f.Project); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	if n := ccuCount(t, st, f.Project); n != 0 {
		t.Fatalf("пик CCU удалённого проекта остался (%d строк): его унаследует новый проект с тем же слагом", n)
	}
	if n := ccuCount(t, st, "neighbour"); n != 1 {
		t.Fatalf("снесён пик CCU ЧУЖОГО проекта: осталось %d, ждём 1", n)
	}
	if n := ccuCount(t, st, ""); n != 1 {
		t.Fatalf("снесена ''-строка: это платформенный пик И маркер «день посчитан», осталось %d, ждём 1", n)
	}

	// Зеркало пробника ревьюера: пересоздаём проект под тем же слагом и
	// спрашиваем ровно тем вызовом, которым ходит /v1/stats/overview|cost.
	if _, err := st.CreateProject(ctx, f.Project, 10); err != nil {
		t.Fatalf("recreate project: %v", err)
	}
	peaks, err := st.RollupPeakCCU(ctx, day, day, f.Project)
	if err != nil {
		t.Fatalf("peak ccu of recreated project: %v", err)
	}
	if len(peaks) != 0 {
		t.Fatalf("новый проект с тем же слагом видит пик CCU мёртвого: %v", peaks)
	}

	// Контроль к предыдущей проверке: пустая выдача обязана значить «строк
	// проекта нет», а не «запрос сломан» — платформенный срез того же дня цел.
	platform, err := st.RollupPeakCCU(ctx, day, day, "")
	if err != nil {
		t.Fatalf("platform peak ccu: %v", err)
	}
	if platform[utctime.DayKey(day)] != 100 {
		t.Fatalf("платформенный пик за день потерян — сломан маркер «день посчитан»: %v", platform)
	}
}
