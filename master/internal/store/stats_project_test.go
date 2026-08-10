package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	"github.com/ufna/birdman/master/internal/utctime"
)

// Проектное измерение статистики (мультипроект W3, миграция 000017).

// Чтение с фильтром по проекту НЕ подмешивает неатрибутированные строки
// (project = ''): подмешивание засчитало бы одни и те же матчи в отчётность
// КАЖДОГО тенанта. Это осознанно строже, чем не-скрывающий фильтр событий в
// панели: показать событие лишний раз безвредно, а число — нет.
func TestRollupDimsProjectFilterExcludesUnattributed(t *testing.T) {
	st := testdb.New(t)
	ctx := t.Context()
	day := utctime.StartOfDay(time.Now().UTC().AddDate(0, 0, -5))

	dims := []store.RollupDim{
		{Day: day, Region: "eu", Semver: "1.0.0", Env: "dev", Project: "game", Matches: 3},
		{Day: day, Region: "eu", Semver: "9.9.9", Env: "dev", Project: "arena", Matches: 5},
		// Историческая строка, которую бэкфилл не смог атрибутировать.
		{Day: day, Region: "eu", Semver: "0.1.0", Env: "dev", Project: "", Matches: 7},
	}
	if err := st.UpsertRollupDay(ctx, day, dims, 0, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := st.RollupDims(ctx, day, day, store.RollupFilter{Project: "game"})
	if err != nil {
		t.Fatalf("dims game: %v", err)
	}
	if len(got) != 1 || got[0].Semver != "1.0.0" || got[0].Matches != 3 {
		t.Fatalf("проектный фильтр должен отдать ТОЛЬКО строки game: %+v", got)
	}

	// Контроль: без фильтра видны все три — иначе тест выше проходил бы и на
	// запросе, который просто ничего не находит.
	all, err := st.RollupDims(ctx, day, day, store.RollupFilter{})
	if err != nil {
		t.Fatalf("dims all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("без фильтра должны быть все три строки, got %d", len(all))
	}
}

// Пик CCU: платформенная строка (project = '') пишется ВСЕГДА и остаётся
// маркером «день посчитан», проектные срезы ложатся рядом. Пик не аддитивен,
// поэтому платформенный не равен сумме проектных — это и проверяется.
func TestRollupPeakCCUPerProjectKeepsPlatformRow(t *testing.T) {
	st := testdb.New(t)
	ctx := t.Context()
	day := utctime.StartOfDay(time.Now().UTC().AddDate(0, 0, -5))
	dk := utctime.DayKey(day)

	// Пики проектов пришлись на разные моменты суток: платформенный пик (10)
	// МЕНЬШЕ суммы проектных (6+8=14).
	if err := st.UpsertRollupDay(ctx, day, nil, 10, map[string]int{"game": 6, "arena": 8}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	platform, err := st.RollupPeakCCU(ctx, day, day, "")
	if err != nil {
		t.Fatalf("peak platform: %v", err)
	}
	if platform[dk] != 10 {
		t.Fatalf("платформенный пик должен остаться 10, got %v", platform)
	}
	gamePeak, err := st.RollupPeakCCU(ctx, day, day, "game")
	if err != nil {
		t.Fatalf("peak game: %v", err)
	}
	if gamePeak[dk] != 6 {
		t.Fatalf("пик game должен быть 6, got %v", gamePeak)
	}

	// Проект без матчей в этот день строки не получает: «ключа нет» у
	// проектного среза значит «матчей не было», а посчитан ли день вообще —
	// отвечает платформенная строка (она есть).
	ghost, err := st.RollupPeakCCU(ctx, day, day, "ghost")
	if err != nil {
		t.Fatalf("peak ghost: %v", err)
	}
	if _, ok := ghost[dk]; ok {
		t.Fatalf("у проекта без матчей строки быть не должно: %v", ghost)
	}
	if _, ok := platform[dk]; !ok {
		t.Fatalf("маркер «день посчитан» (платформенная строка) обязан присутствовать")
	}
}

// Полный пересчёт дня переписывает ВСЕ срезы, а не дописывает к ним: вторая
// запись того же дня не должна оставлять строки первой (иначе роллап рос бы
// при каждом тике джобы).
func TestUpsertRollupDayReplacesAllProjectSlices(t *testing.T) {
	st := testdb.New(t)
	ctx := t.Context()
	day := utctime.StartOfDay(time.Now().UTC().AddDate(0, 0, -5))

	if err := st.UpsertRollupDay(ctx, day, []store.RollupDim{
		{Day: day, Region: "eu", Semver: "1.0.0", Env: "dev", Project: "game", Matches: 1},
		{Day: day, Region: "eu", Semver: "9.9.9", Env: "dev", Project: "arena", Matches: 1},
	}, 5, map[string]int{"game": 3, "arena": 4}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	// Пересчёт: у arena матчей больше нет вовсе.
	if err := st.UpsertRollupDay(ctx, day, []store.RollupDim{
		{Day: day, Region: "eu", Semver: "1.0.0", Env: "dev", Project: "game", Matches: 2},
	}, 3, map[string]int{"game": 3}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	all, err := st.RollupDims(ctx, day, day, store.RollupFilter{})
	if err != nil {
		t.Fatalf("dims: %v", err)
	}
	if len(all) != 1 || all[0].Project != "game" || all[0].Matches != 2 {
		t.Fatalf("пересчёт должен ЗАМЕНИТЬ день целиком: %+v", all)
	}
	arena, err := st.RollupPeakCCU(ctx, day, day, "arena")
	if err != nil {
		t.Fatalf("peak arena: %v", err)
	}
	if len(arena) != 0 {
		t.Fatalf("срез исчезнувшего проекта должен уйти: %v", arena)
	}
}

// Миграция 000017 на НЕПУСТОЙ таблице: шагаем вниз до v16 (эра без проекта),
// сеем роллап-строки и матчи, применяем up и проверяем бэкфилл — однозначная
// комбинация атрибутируется, разделённая двумя проектами остаётся с ''.
func TestStatsProjectMigration000017Backfill(t *testing.T) {
	st, m := stepDownToV16(t)
	ctx := context.Background()
	pool := st.Pool

	var gameID, arenaID string
	for slug, dst := range map[string]*string{"game": &gameID, "arena": &arenaID} {
		if err := pool.QueryRow(ctx,
			`insert into projects (slug) values ($1) returning id::text`, slug).Scan(dst); err != nil {
			t.Fatalf("seed project %s: %v", slug, err)
		}
		// Сырой INSERT минует ensureProject, который сеет dev/prod, — а без
		// окружения versions_env_fk не пустит версию.
		if _, err := pool.Exec(ctx, `
			insert into environments (project_id, name, production, auto_deploy, retention_keep)
			values ($1::uuid, 'dev', false, true, 20)`, *dst); err != nil {
			t.Fatalf("seed env for %s: %v", slug, err)
		}
	}
	day := utctime.StartOfDay(time.Now().UTC().AddDate(0, 0, -10))

	// Версии + матчи: 1.0.0 только у game (однозначно), 2.0.0 у ОБОИХ
	// проектов в тот же день/регион/env (коллизия semver — сплиту не
	// поддаётся, агрегаты уже просуммированы). Матч требует server_id, тот —
	// ноду, поэтому у каждого проекта своя нода.
	nodeOf := map[string]string{}
	port := 30000
	seedMatch := func(projectID, slug, semver string) {
		t.Helper()
		if _, ok := nodeOf[slug]; !ok {
			var nodeID string
			if err := pool.QueryRow(ctx, `
				insert into nodes (project_id, region, hostname, public_ip, capacity_slots, env, last_heartbeat_at)
				values ($1::uuid, 'eu', $2, '203.0.113.10', 10, 'dev', now()) returning id::text`,
				projectID, "node-"+slug).Scan(&nodeID); err != nil {
				t.Fatalf("seed node: %v", err)
			}
			nodeOf[slug] = nodeID
		}
		var versionID string
		if err := pool.QueryRow(ctx, `
			insert into versions (project_id, semver, image_ref, env, state)
			values ($1::uuid, $2, 'ghcr.io/x/y:1', 'dev', 'active') returning id::text`,
			projectID, semver).Scan(&versionID); err != nil {
			t.Fatalf("seed version: %v", err)
		}
		port++
		var serverID string
		if err := pool.QueryRow(ctx, `
			insert into servers (project_id, node_id, version_id, state, port, env)
			values ($1::uuid, $2::uuid, $3::uuid, 'allocated', $4, 'dev') returning id::text`,
			projectID, nodeOf[slug], versionID, port).Scan(&serverID); err != nil {
			t.Fatalf("seed server: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			insert into matches (project_id, server_id, region, state, version_id, env, players_peak,
			                     created_at, started_at, ended_at)
			values ($1::uuid, $2::uuid, 'eu', 'finished', $3::uuid, 'dev', 4, $4, $4, $5)`,
			projectID, serverID, versionID, day.Add(9*time.Hour), day.Add(9*time.Hour+10*time.Minute)); err != nil {
			t.Fatalf("seed match: %v", err)
		}
	}
	seedMatch(gameID, "game", "1.0.0")
	seedMatch(gameID, "game", "2.0.0")
	seedMatch(arenaID, "arena", "2.0.0")

	// Роллап-строки в форме ДО миграции (без колонки project).
	for _, semver := range []string{"1.0.0", "2.0.0"} {
		if _, err := pool.Exec(ctx, `
			insert into match_stats_daily (day, region, semver, env, matches, players_peak_sum,
			                               dur_sum_seconds, dur_count, slot_seconds)
			values ($1, 'eu', $2, 'dev', 1, 4, 600, 1, 600)`, day, semver); err != nil {
			t.Fatalf("seed rollup %s: %v", semver, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`insert into match_ccu_daily (day, peak_ccu) values ($1, 8)`, day); err != nil {
		t.Fatalf("seed ccu: %v", err)
	}

	if err := m.Migrate(17); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("apply 000017: %v", err)
	}

	got := map[string]string{}
	rows, err := pool.Query(ctx, `select semver, project from match_stats_daily where day = $1`, day)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var semver, project string
		if err := rows.Scan(&semver, &project); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[semver] = project
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if got["1.0.0"] != "game" {
		t.Fatalf("однозначная комбинация должна атрибутироваться в game, got %q", got["1.0.0"])
	}
	// Молчаливого дефолта на первый проект тут быть не должно — это была бы
	// ложь в отчётности; честный маркер «не знаем» лучше.
	if got["2.0.0"] != "" {
		t.Fatalf("комбинация двух проектов должна остаться неатрибутированной, got %q", got["2.0.0"])
	}

	// Платформенная строка CCU пережила миграцию и стала срезом project=''.
	var peak int
	if err := pool.QueryRow(ctx,
		`select peak_ccu from match_ccu_daily where day = $1 and project = ''`, day).Scan(&peak); err != nil {
		t.Fatalf("ccu after migrate: %v", err)
	}
	if peak != 8 {
		t.Fatalf("платформенный пик должен уцелеть (8), got %d", peak)
	}
}

// Откат 000017 схлопывает проектные строки обратно в одну на
// (day, region, semver, env), СУММИРУЯ аддитивные агрегаты; пик при этом НЕ
// суммируется (он не аддитивен) — остаётся платформенная строка.
func TestStatsProjectMigration000017Down(t *testing.T) {
	st, m := stepDownToV16(t)
	ctx := context.Background()
	pool := st.Pool
	day := utctime.StartOfDay(time.Now().UTC().AddDate(0, 0, -10))

	if err := m.Migrate(17); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up: %v", err)
	}
	for _, p := range []struct {
		project string
		matches int
	}{{"game", 3}, {"arena", 5}} {
		if _, err := pool.Exec(ctx, `
			insert into match_stats_daily (day, region, semver, env, project, matches,
			                               players_peak_sum, dur_sum_seconds, dur_count, slot_seconds)
			values ($1, 'eu', '1.0.0', 'dev', $2, $3, 10, 60, 1, 60)`, day, p.project, p.matches); err != nil {
			t.Fatalf("seed %s: %v", p.project, err)
		}
		if _, err := pool.Exec(ctx,
			`insert into match_ccu_daily (day, project, peak_ccu) values ($1, $2, 7)`, day, p.project); err != nil {
			t.Fatalf("seed ccu %s: %v", p.project, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`insert into match_ccu_daily (day, project, peak_ccu) values ($1, '', 9)`, day); err != nil {
		t.Fatalf("seed platform ccu: %v", err)
	}

	if err := m.Migrate(16); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("down: %v", err)
	}

	var rowCount, matches int
	if err := pool.QueryRow(ctx,
		`select count(*), coalesce(sum(matches), 0) from match_stats_daily where day = $1`, day).
		Scan(&rowCount, &matches); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rowCount != 1 || matches != 8 {
		t.Fatalf("откат должен схлопнуть 2 строки в 1 с суммой 3+5=8, got rows=%d matches=%d", rowCount, matches)
	}
	var ccuRows, peak int
	if err := pool.QueryRow(ctx,
		`select count(*), coalesce(max(peak_ccu), 0) from match_ccu_daily where day = $1`, day).
		Scan(&ccuRows, &peak); err != nil {
		t.Fatalf("ccu read back: %v", err)
	}
	if ccuRows != 1 || peak != 9 {
		t.Fatalf("пик НЕ суммируется: должна остаться платформенная строка 9, got rows=%d peak=%d", ccuRows, peak)
	}
}

// stepDownToV16 provisions a fresh DB (migrated to head) and steps the schema
// DOWN to v16 — форма до проектного измерения статистики.
func stepDownToV16(t *testing.T) (*store.Store, *migrate.Migrate) {
	t.Helper()
	st, dsn := testdb.NewWithCodec(t, codecFilled(t, 0x17))
	m := newMigrateHandle(t, stripPoolParams(t, dsn))
	if err := m.Migrate(16); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("step down to v16: %v", err)
	}
	return st, m
}
