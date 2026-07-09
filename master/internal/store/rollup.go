package store

import (
	"context"
	"time"
)

// Rollup persistence for the panel Statistics/Cost-view screens
// (docs/specs/panel.md §3, master.md §6; «Статистика v1» T8). match_stats_daily
// / match_ccu_daily hold the precomputed dimensional aggregation
// (stats.AggregateDaily, T7) so long windows don't need to rescan raw
// matches; the rollup-maintenance job (internal/statsrollup, T9) is the sole
// writer, recomputing and replacing one UTC day at a time. The read path
// switching over to these tables is a later task (T10) — today's
// /v1/stats/* handlers still read raw matches (see stats.go in this
// package).

// RollupDim is the persisted shape of one match_stats_daily row: a
// day×region×version bucket of additive aggregates. It mirrors
// stats.DailyDim field-for-field — in fact stats.DailyDim is a type alias
// for this very struct (see stats.go's comment on DailyDim): stats already
// imports store for its raw-row input types, so store cannot import stats
// without a cycle, and the shared shape is declared here, in the package
// that doesn't need the back-reference.
type RollupDim struct {
	Day            time.Time
	Region, Semver string
	Matches        int
	PlayersPeakSum int64
	DurSumSeconds  float64
	DurCount       int
	SlotSeconds    float64
}

// normalizeRollupDay truncates t to its UTC calendar date (midnight UTC) —
// the granularity every rollup table/method operates at.
func normalizeRollupDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// rollupDayKey is dayKey in the same "2006-01-02" UTC format the stats
// package uses, so map keys agree between the two packages without either
// importing the other's private helper.
func rollupDayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

// UpsertRollupDay replaces the stored rollup for exactly one UTC day
// (day, normalized): inside one transaction, it deletes any existing
// match_stats_daily/match_ccu_daily rows for that day, inserts dims, and
// ALWAYS inserts one match_ccu_daily row (day, peakCCU) — even when dims is
// empty and peakCCU is 0 — so the day is marked processed: present (with an
// ok-map lookup) in RollupPeakCCU's result, distinguishing a legitimate
// empty day (peak 0, but present) from one never rolled up at all (absent).
// Every written row uses the `day` parameter (not each dim's own Day field)
// as its day column, so the single-day replace stays correct even if a
// caller's dims aren't perfectly pre-filtered.
func (s *Store) UpsertRollupDay(ctx context.Context, day time.Time, dims []RollupDim, peakCCU int) error {
	d := normalizeRollupDay(day)

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
				(day, region, semver, matches, players_peak_sum, dur_sum_seconds, dur_count, slot_seconds)
			values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			d, dim.Region, dim.Semver, dim.Matches, dim.PlayersPeakSum,
			dim.DurSumSeconds, dim.DurCount, dim.SlotSeconds,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		`insert into match_ccu_daily (day, peak_ccu) values ($1, $2)`, d, peakCCU,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RollupDims returns the stored dims for days in [from,to] (inclusive, by
// UTC date), oldest first (then region, semver).
func (s *Store) RollupDims(ctx context.Context, from, to time.Time) ([]RollupDim, error) {
	f, t := normalizeRollupDay(from), normalizeRollupDay(to)
	rows, err := s.Pool.Query(ctx, `
		select day, region, semver, matches, players_peak_sum, dur_sum_seconds, dur_count, slot_seconds
		from match_stats_daily
		where day between $1 and $2
		order by day, region, semver`, f, t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RollupDim
	for rows.Next() {
		var d RollupDim
		if err := rows.Scan(&d.Day, &d.Region, &d.Semver, &d.Matches, &d.PlayersPeakSum,
			&d.DurSumSeconds, &d.DurCount, &d.SlotSeconds); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RollupPeakCCU returns dayKey("2006-01-02")->peak_ccu for days in [from,to]
// (inclusive, by UTC date).
func (s *Store) RollupPeakCCU(ctx context.Context, from, to time.Time) (map[string]int, error) {
	f, t := normalizeRollupDay(from), normalizeRollupDay(to)
	rows, err := s.Pool.Query(ctx,
		`select day, peak_ccu from match_ccu_daily where day between $1 and $2`, f, t)
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
		out[rollupDayKey(day)] = peak
	}
	return out, rows.Err()
}

// StatMatchesOverlapping returns started matches overlapping [from,to)
// (started_at < to and (ended_at is null or ended_at >= from)), oldest
// first — the rollup job's tail-recompute input (matches(started_at) index;
// same row shape/columns as StatMatches in stats.go).
func (s *Store) StatMatchesOverlapping(ctx context.Context, from, to time.Time) ([]StatMatch, error) {
	rows, err := s.Pool.Query(ctx, `
		select m.region, v.semver, m.players_peak, m.created_at, m.started_at, m.ended_at
		from matches m
		join versions v on v.id = m.version_id
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
		if err := rows.Scan(&sm.Region, &sm.Semver, &sm.PlayersPeak,
			&sm.CreatedAt, &sm.StartedAt, &sm.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}
