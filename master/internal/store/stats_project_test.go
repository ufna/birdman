package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
	day := utctime.StartOfDay(time.Now().UTC().AddDate(0, 0, -10))

	// Версии + матчи: 1.0.0 только у game (однозначно), 2.0.0 у ОБОИХ
	// проектов в тот же день/регион/env (коллизия semver — сплиту не
	// поддаётся, агрегаты уже просуммированы).
	seed := newStatsSeed(t, pool)
	for _, s := range []struct{ slug, semver string }{
		{"game", "1.0.0"}, {"game", "2.0.0"}, {"arena", "2.0.0"},
	} {
		seed.match(s.slug, s.semver, day.Add(9*time.Hour), day.Add(9*time.Hour+10*time.Minute))
	}

	// Роллап-строки в форме ДО миграции (без колонки project).
	for _, semver := range []string{"1.0.0", "2.0.0"} {
		seedRollupRow(t, pool, day, semver, 1, 600)
	}
	if _, err := pool.Exec(ctx,
		`insert into match_ccu_daily (day, peak_ccu) values ($1, 8)`, day); err != nil {
		t.Fatalf("seed ccu: %v", err)
	}

	if err := m.Migrate(17); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("apply 000017: %v", err)
	}

	got := rollupProjects(t, pool)
	if got[rollupKey(day, "1.0.0")] != "game" {
		t.Fatalf("однозначная комбинация должна атрибутироваться в game, got %q", got[rollupKey(day, "1.0.0")])
	}
	// Молчаливого дефолта на первый проект тут быть не должно — это была бы
	// ложь в отчётности; честный маркер «не знаем» лучше.
	if got[rollupKey(day, "2.0.0")] != "" {
		t.Fatalf("комбинация двух проектов должна остаться неатрибутированной, got %q", got[rollupKey(day, "2.0.0")])
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

// Бэкфилл 000017 обязан брать день В UTC, а не в таймзоне СЕССИИ: started_at —
// timestamptz, и голый date() привёл бы его к зоне сервера, тогда как
// роллап-строки живут в UTC-днях (utctime.StartOfDay по всему коду
// статистики). Матчи здесь стартуют в 22:00 UTC, то есть в Europe/Moscow это
// уже СЛЕДУЮЩИЙ день, — сдвиг ловится сразу двумя способами: строка своего дня
// остаётся неатрибутированной (промах), а строка следующего дня получает ЧУЖОЙ
// проект. Штатный деплой (docker postgres:16) идёт с UTC, но self-host на
// чужом инстансе с локальной TZ — рядовой случай, и дефект там не спящий.
func TestStatsProjectMigration000017BackfillIsUTCAnchored(t *testing.T) {
	st, m := stepDownToV16InTimeZone(t, "Europe/Moscow")
	pool := st.Pool
	day := utctime.StartOfDay(time.Now().UTC().AddDate(0, 0, -10))
	next := day.AddDate(0, 0, 1)

	// Один и тот же semver у двух проектов, но в РАЗНЫЕ дни: коллизии в
	// пределах дня нет, обе строки обязаны атрибутироваться однозначно.
	seed := newStatsSeed(t, pool)
	seed.match("game", "1.0.0", day.Add(22*time.Hour), day.Add(22*time.Hour+10*time.Minute))
	seed.match("arena", "1.0.0", next.Add(22*time.Hour), next.Add(22*time.Hour+10*time.Minute))
	seedRollupRow(t, pool, day, "1.0.0", 1, 600)
	seedRollupRow(t, pool, next, "1.0.0", 1, 600)

	if err := m.Migrate(17); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("apply 000017: %v", err)
	}

	got := rollupProjects(t, pool)
	if p := got[rollupKey(day, "1.0.0")]; p != "game" {
		t.Fatalf("строка дня старта обязана достаться game, got %q — день посчитан в зоне сессии, а не в UTC", p)
	}
	if p := got[rollupKey(next, "1.0.0")]; p != "arena" {
		t.Fatalf("строка следующего дня обязана достаться arena, got %q — в неё утёк вчерашний сосед", p)
	}
}

// Кросс-полуночный матч вносит slot_seconds в СЛЕДУЮЩИЙ день, а
// matches/players_peak — в день СТАРТА (штатная семантика occupancy,
// docs/specs/master.md §6, stats.AggregateDaily). Значит строка следующего дня
// может состоять из одного такого «смира», и по дню старта её владелец не
// виден вовсе. Бэкфилл атрибутирует по ПЕРЕКРЫТИЮ спана матча, поэтому:
// чистая смир-строка достаётся своему проекту, а смешанная (смир одного
// проекта + матчи другого под тем же semver) честно остаётся с ''.
func TestStatsProjectMigration000017BackfillAttributesCrossMidnightSmear(t *testing.T) {
	st, m := stepDownToV16(t)
	pool := st.Pool
	day := utctime.StartOfDay(time.Now().UTC().AddDate(0, 0, -10))
	next := day.AddDate(0, 0, 1)
	crossStart, crossEnd := day.Add(23*time.Hour+30*time.Minute), next.Add(30*time.Minute)

	seed := newStatsSeed(t, pool)
	// 1.0.0: смир game в next + собственные матчи arena в next — строка next
	// СМЕШАННАЯ, и по дню старта видно только arena.
	seed.match("game", "1.0.0", crossStart, crossEnd)
	seed.match("arena", "1.0.0", next.Add(12*time.Hour), next.Add(12*time.Hour+10*time.Minute))
	// 3.0.0: только game, разбавить смир некому — строка next чистая.
	seed.match("game", "3.0.0", crossStart, crossEnd)

	seedRollupRow(t, pool, day, "1.0.0", 1, 1800)
	seedRollupRow(t, pool, next, "1.0.0", 1, 2400)
	seedRollupRow(t, pool, day, "3.0.0", 1, 1800)
	// Строка чистого смира: матчей в этот день не начиналось (matches = 0),
	// есть только перенесённые через полночь slot_seconds.
	seedRollupRow(t, pool, next, "3.0.0", 0, 1800)

	if err := m.Migrate(17); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("apply 000017: %v", err)
	}

	got := rollupProjects(t, pool)
	if p := got[rollupKey(day, "1.0.0")]; p != "game" {
		t.Fatalf("день старта под 1.0.0 — только game, got %q", p)
	}
	if p := got[rollupKey(next, "1.0.0")]; p != "" {
		t.Fatalf("смешанная строка (смир game + матчи arena) обязана остаться неатрибутированной, got %q", p)
	}
	if p := got[rollupKey(day, "3.0.0")]; p != "game" {
		t.Fatalf("день старта под 3.0.0 — только game, got %q", p)
	}
	if p := got[rollupKey(next, "3.0.0")]; p != "game" {
		t.Fatalf("строка из одного смира обязана достаться его проекту, got %q", p)
	}
}

// stepDownToV16 provisions a fresh DB (migrated to head) and steps the schema
// DOWN to v16 — форма до проектного измерения статистики.
func stepDownToV16(t *testing.T) (*store.Store, *migrate.Migrate) {
	t.Helper()
	return stepDownToV16InTimeZone(t, "")
}

// stepDownToV16InTimeZone — stepDownToV16 с БД, переключённой на заданную
// таймзону (пустая строка — оставить как есть, у postgres:16 это UTC).
// Переключение делается ДО открытия хендла migrate: `alter database ... set
// TimeZone` действует на НОВЫЕ сессии, а хендл коннектится лениво, на первом
// Migrate, — так миграция приезжает в сессию с не-UTC зоной, ровно как на
// чужом self-host инстансе.
func stepDownToV16InTimeZone(t *testing.T, tz string) (*store.Store, *migrate.Migrate) {
	t.Helper()
	st, dsn := testdb.NewWithCodec(t, codecFilled(t, 0x17))
	dsn = stripPoolParams(t, dsn)
	if tz != "" {
		setDatabaseTimeZone(t, dsn, tz)
	}
	m := newMigrateHandle(t, dsn)
	if err := m.Migrate(16); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("step down to v16: %v", err)
	}
	return st, m
}

// setDatabaseTimeZone переключает таймзону БД и ПРОВЕРЯЕТ её на свежей сессии
// — тем же способом, каким подключится migrate. Проверка обязательна: без неё
// тест на TZ-зависимость «проходил» бы просто потому, что зона не применилась.
func setDatabaseTimeZone(t *testing.T, dsn, tz string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for timezone: %v", err)
	}
	var dbName string
	if err := conn.QueryRow(ctx, `select current_database()`).Scan(&dbName); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("current_database: %v", err)
	}
	_, err = conn.Exec(ctx, fmt.Sprintf(`alter database %s set TimeZone = '%s'`,
		pgx.Identifier{dbName}.Sanitize(), tz))
	_ = conn.Close(ctx)
	if err != nil {
		t.Fatalf("alter database timezone: %v", err)
	}

	fresh, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect after timezone: %v", err)
	}
	defer fresh.Close(ctx)
	var got string
	if err := fresh.QueryRow(ctx, `show TimeZone`).Scan(&got); err != nil {
		t.Fatalf("show TimeZone: %v", err)
	}
	if got != tz {
		t.Fatalf("новая сессия обязана открыться в %s, got %s — тест ловил бы пустоту", tz, got)
	}
}

// statsSeed сеет минимальный граф под миграционные тесты роллапов:
// проект → окружение → нода → версия → сервер → матч. Проекты, ноды и версии
// кэшируются, порт сервера уникален в пределах сида.
type statsSeed struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	project map[string]string
	node    map[string]string
	version map[string]string
	port    int
}

func newStatsSeed(t *testing.T, pool *pgxpool.Pool) *statsSeed {
	t.Helper()
	return &statsSeed{
		t: t, ctx: context.Background(), pool: pool,
		project: map[string]string{}, node: map[string]string{}, version: map[string]string{},
		port: 30000,
	}
}

func (s *statsSeed) projectID(slug string) string {
	s.t.Helper()
	if id, ok := s.project[slug]; ok {
		return id
	}
	var id string
	if err := s.pool.QueryRow(s.ctx,
		`insert into projects (slug) values ($1) returning id::text`, slug).Scan(&id); err != nil {
		s.t.Fatalf("seed project %s: %v", slug, err)
	}
	// Сырой INSERT минует ensureProject, который сеет dev/prod, — а без
	// окружения versions_env_fk не пустит версию.
	if _, err := s.pool.Exec(s.ctx, `
		insert into environments (project_id, name, production, auto_deploy, retention_keep)
		values ($1::uuid, 'dev', false, true, 20)`, id); err != nil {
		s.t.Fatalf("seed env for %s: %v", slug, err)
	}
	s.project[slug] = id
	return id
}

// nodeID: матч требует server_id, тот — ноду, поэтому у каждого проекта своя.
func (s *statsSeed) nodeID(slug string) string {
	s.t.Helper()
	if id, ok := s.node[slug]; ok {
		return id
	}
	var id string
	if err := s.pool.QueryRow(s.ctx, `
		insert into nodes (project_id, region, hostname, public_ip, capacity_slots, env, last_heartbeat_at)
		values ($1::uuid, 'eu', $2, '203.0.113.10', 10, 'dev', now()) returning id::text`,
		s.projectID(slug), "node-"+slug).Scan(&id); err != nil {
		s.t.Fatalf("seed node for %s: %v", slug, err)
	}
	s.node[slug] = id
	return id
}

func (s *statsSeed) versionID(slug, semver string) string {
	s.t.Helper()
	if id, ok := s.version[slug+"/"+semver]; ok {
		return id
	}
	var id string
	if err := s.pool.QueryRow(s.ctx, `
		insert into versions (project_id, semver, image_ref, env, state)
		values ($1::uuid, $2, 'ghcr.io/x/y:1', 'dev', 'active') returning id::text`,
		s.projectID(slug), semver).Scan(&id); err != nil {
		s.t.Fatalf("seed version %s/%s: %v", slug, semver, err)
	}
	s.version[slug+"/"+semver] = id
	return id
}

// match сеет один завершённый матч с ЯВНЫМИ границами (обе — UTC): именно они
// решают, в какие дни лягут агрегаты и через какие дни пройдёт спан.
func (s *statsSeed) match(slug, semver string, startedAt, endedAt time.Time) {
	s.t.Helper()
	projectID, versionID := s.projectID(slug), s.versionID(slug, semver)
	s.port++
	var serverID string
	if err := s.pool.QueryRow(s.ctx, `
		insert into servers (project_id, node_id, version_id, state, port, env)
		values ($1::uuid, $2::uuid, $3::uuid, 'allocated', $4, 'dev') returning id::text`,
		projectID, s.nodeID(slug), versionID, s.port).Scan(&serverID); err != nil {
		s.t.Fatalf("seed server: %v", err)
	}
	if _, err := s.pool.Exec(s.ctx, `
		insert into matches (project_id, server_id, region, state, version_id, env, players_peak,
		                     created_at, started_at, ended_at)
		values ($1::uuid, $2::uuid, 'eu', 'finished', $3::uuid, 'dev', 4, $4, $4, $5)`,
		projectID, serverID, versionID, startedAt, endedAt); err != nil {
		s.t.Fatalf("seed match: %v", err)
	}
}

// seedRollupRow пишет роллап-строку в форме ДО миграции 000017 (без колонки
// project). matches = 0 — это день, в который матчей не начиналось, а
// slot_seconds приехали смиром из предыдущего дня.
func seedRollupRow(t *testing.T, pool *pgxpool.Pool, day time.Time, semver string, matches int, slotSeconds float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into match_stats_daily (day, region, semver, env, matches, players_peak_sum,
		                               dur_sum_seconds, dur_count, slot_seconds)
		values ($1, 'eu', $2, 'dev', $3, $4, $5, $3, $6)`,
		day, semver, matches, matches*4, float64(matches)*600, slotSeconds); err != nil {
		t.Fatalf("seed rollup %s/%s: %v", utctime.DayKey(day), semver, err)
	}
}

// rollupProjects читает атрибуцию всех роллап-строк ключом rollupKey.
func rollupProjects(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select day, semver, project from match_stats_daily`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var day time.Time
		var semver, project string
		if err := rows.Scan(&day, &semver, &project); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[rollupKey(day, semver)] = project
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return got
}

func rollupKey(day time.Time, semver string) string { return utctime.DayKey(day) + "/" + semver }
