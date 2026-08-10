package store

import (
	"context"
	"time"

	"github.com/ufna/birdman/master/internal/utctime"
)

// Rollup persistence for the panel Statistics/Cost-view screens
// (docs/specs/panel.md §3, master.md §6; «Статистика v1» T8). match_stats_daily
// / match_ccu_daily hold the precomputed dimensional aggregation
// (stats.AggregateDaily, T7) so long windows don't need to rescan raw
// matches; the rollup-maintenance job (internal/statsrollup, T9) is the sole
// writer, recomputing and replacing one UTC day at a time. The /v1/stats/*
// handlers read these tables for every axis day up to today-2 and recompute
// only the trailing two days live from raw matches (T10 — see
// httpapi/stats.go's statsDims); StatMatchesOverlapping below is that
// raw-match input, shared by the job and the live tail.

// RollupDim is the persisted shape of one match_stats_daily row: a
// day×region×version×env bucket of additive aggregates (env added in
// environments v1, I5 — match_stats_daily PK is now (day, region, semver,
// env); match_ccu_daily stays global, PK (day)). It mirrors stats.DailyDim
// field-for-field — in fact stats.DailyDim is a type alias for this very
// struct (see stats.go's comment on DailyDim): stats already imports store
// for its raw-row input types, so store cannot import stats without a cycle,
// and the shared shape is declared here, in the package that doesn't need the
// back-reference.
type RollupDim struct {
	Day            time.Time
	Region, Semver string
	Env            string
	Project        string // слаг проекта (мультипроект W3); '' = не атрибутировано
	Matches        int
	PlayersPeakSum int64
	DurSumSeconds  float64
	DurCount       int
	SlotSeconds    float64
}

// UpsertRollupDay replaces the stored rollup for exactly one UTC day
// (day, normalized): inside one transaction, it deletes any existing
// match_stats_daily/match_ccu_daily rows for that day (across ALL env AND all
// projects — a full-day recompute rewrites every slice), inserts dims (one
// match_stats_daily row per region×semver×env×project, I5 + мультипроект W3),
// and ALWAYS inserts the platform-wide match_ccu_daily row (day, '', peakCCU)
// — even when dims is empty and peakCCU is 0 — so the day is marked
// processed: present (with an ok-map lookup) in RollupPeakCCU's result,
// distinguishing a legitimate empty day (peak 0, but present) from one never
// rolled up at all (absent).
//
// peakByProject добавляет проектные строки РЯДОМ с платформенной, не вместо
// неё: пик не аддитивен (сумма проектных пиков больше реального
// одновременного пика платформы), поэтому вывести один срез из другого
// нельзя — их считают отдельно и хранят оба. Проект с нулём матчей в этот
// день строки не получает: «дня нет» у проектного среза значит «матчей не
// было», а посчитан ли день вообще, отвечает ''-строка.
//
// Every written row uses the `day` parameter (not each dim's own Day field)
// as its day column, so the single-day replace stays correct even if a
// caller's dims aren't perfectly pre-filtered.
func (s *Store) UpsertRollupDay(ctx context.Context, day time.Time, dims []RollupDim, peakCCU int, peakByProject map[string]int) error {
	d := utctime.StartOfDay(day)

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `delete from match_stats_daily where day = $1`, d); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from match_ccu_daily where day = $1`, d); err != nil {
		return err
	}
	for _, dim := range dims {
		if _, err := tx.Exec(ctx, `
			insert into match_stats_daily
				(day, region, semver, env, project, matches, players_peak_sum, dur_sum_seconds, dur_count, slot_seconds)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			d, dim.Region, dim.Semver, dim.Env, dim.Project, dim.Matches, dim.PlayersPeakSum,
			dim.DurSumSeconds, dim.DurCount, dim.SlotSeconds,
		); err != nil {
			return err
		}
	}
	// Платформенная строка — всегда: она же маркер «день посчитан».
	if _, err := tx.Exec(ctx,
		`insert into match_ccu_daily (day, project, peak_ccu) values ($1, '', $2)`, d, peakCCU,
	); err != nil {
		return err
	}
	for project, peak := range peakByProject {
		if project == "" { // платформенный срез уже записан выше
			continue
		}
		if _, err := tx.Exec(ctx,
			`insert into match_ccu_daily (day, project, peak_ccu) values ($1, $2, $3)`, d, project, peak,
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RollupFilter narrows RollupDims; a zero value means "everything" (пустое
// поле — отсутствие условия, идиома ServerFilter/NodeFilter).
type RollupFilter struct {
	Env     string // окружение (environments v1, I5)
	Project string // слаг проекта (мультипроект W3)
}

// RollupDims returns the stored dims for days in [from,to] (inclusive, by
// UTC date), oldest first (then region, semver, env, project).
//
// A non-empty Project scopes to that project's rows and thereby EXCLUDES
// unattributed rows (project = '', см. миграцию 000017): подмешивать их в
// каждый проект нельзя — одни и те же матчи попали бы в отчётность каждого
// тенанта. Это осознанно строже, чем не-скрывающий фильтр СОБЫТИЙ в панели
// (W2): показать событие лишний раз безвредно, а число — нет.
func (s *Store) RollupDims(ctx context.Context, from, to time.Time, f RollupFilter) ([]RollupDim, error) {
	from0, to0 := utctime.StartOfDay(from), utctime.StartOfDay(to)
	rows, err := s.Pool.Query(ctx, `
		select day, region, semver, env, project, matches, players_peak_sum, dur_sum_seconds, dur_count, slot_seconds
		from match_stats_daily
		where day between $1 and $2 and ($3 = '' or env = $3) and ($4 = '' or project = $4)
		order by day, region, semver, env, project`, from0, to0, f.Env, f.Project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RollupDim
	for rows.Next() {
		var d RollupDim
		if err := rows.Scan(&d.Day, &d.Region, &d.Semver, &d.Env, &d.Project, &d.Matches, &d.PlayersPeakSum,
			&d.DurSumSeconds, &d.DurCount, &d.SlotSeconds); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RollupPeakCCU returns utctime.DayKey ("2006-01-02") -> peak_ccu for days in
// [from,to] (inclusive, by UTC date) for ONE project slice.
//
// project = "" — платформенный пик (все проекты разом) И одновременно маркер
// «день посчитан»: наличие ключа в результате отличает законно пустой день
// (пик 0, строка есть) от дня, который ещё не роллапился (строки нет). Именно
// эта строка писалась и до мультипроекта, поэтому инвариант дословно прежний.
// Непустой project — пик ЭТОГО проекта; отсутствие ключа тогда значит «в этот
// день у проекта не было матчей», а посчитан ли день, по-прежнему говорит
// ''-срез.
func (s *Store) RollupPeakCCU(ctx context.Context, from, to time.Time, project string) (map[string]int, error) {
	from0, to0 := utctime.StartOfDay(from), utctime.StartOfDay(to)
	rows, err := s.Pool.Query(ctx,
		`select day, peak_ccu from match_ccu_daily where day between $1 and $2 and project = $3`,
		from0, to0, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var day time.Time
		var peak int
		if err := rows.Scan(&day, &peak); err != nil {
			return nil, err
		}
		out[utctime.DayKey(day)] = peak
	}
	return out, rows.Err()
}

// StatMatchesOverlapping returns started matches overlapping [from,to)
// (started_at < to and (ended_at is null or ended_at >= from)), oldest
// first — the rollup job's tail-recompute input (matches(started_at) index;
// same row shape/columns as StatMatches in stats.go). Returns all
// environments (each row carries its matches.env in Env, I5): the rollup job
// groups them per-env in stats.AggregateDaily, and the live-tail read path
// slices in memory while keeping the platform-wide peak CCU over the whole
// (unfiltered) set.
func (s *Store) StatMatchesOverlapping(ctx context.Context, from, to time.Time) ([]StatMatch, error) {
	rows, err := s.Pool.Query(ctx, `
		select m.region, v.semver, m.env, p.slug, m.players_peak, m.created_at, m.started_at, m.ended_at
		from matches m
		join versions v on v.id = m.version_id
		join projects p on p.id = m.project_id
		where m.started_at is not null and m.started_at < $2
		  and (m.ended_at is null or m.ended_at >= $1)
		order by m.started_at`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatMatch
	for rows.Next() {
		var sm StatMatch
		if err := rows.Scan(&sm.Region, &sm.Semver, &sm.Env, &sm.Project, &sm.PlayersPeak,
			&sm.CreatedAt, &sm.StartedAt, &sm.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}
