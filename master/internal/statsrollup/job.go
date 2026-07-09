// Package statsrollup maintains the match_stats_daily/match_ccu_daily
// rollup tables (store.UpsertRollupDay et al., «Статистика v1» T8) so long
// statistics windows don't need to rescan raw matches (internal/stats, T7).
// Two passes share one recompute primitive (recomputeDay):
//
//   - Backfill (run once, at startup): fills any day in [today-29, today-2]
//     that has no rollup yet (store.RolledUpDays) — immutable history,
//     computed once and never touched again.
//   - the tick (every Interval): recomputes the trailing two UTC days
//     [today-1, today] unconditionally — the only days whose matches can
//     still change (a match started yesterday may still be running with no
//     ended_at yet; today is still open by definition).
//
// recomputeDay(D) mirrors AggregateDaily's on-the-fly semantics exactly by
// re-deriving D's rollup from raw matches every time, so recomputing a day
// twice is idempotent (store.UpsertRollupDay deletes D's existing rows
// before inserting — replace, not append).
package statsrollup

import (
	"context"
	"log/slog"
	"time"

	"github.com/ufna/birdman/master/internal/stats"
	"github.com/ufna/birdman/master/internal/store"
)

const (
	// backfillDays bounds Backfill's window: [today-(backfillDays-1), today-tailDays].
	backfillDays = 30
	// tailDays is how many trailing UTC days (today and the ones before it)
	// the tick always recomputes; today-tailDays is Backfill's upper bound.
	tailDays = 2
)

type Job struct {
	st       *store.Store
	interval time.Duration
	log      *slog.Logger
}

// New builds a rollup-maintenance job. Call Run in its own goroutine.
func New(st *store.Store, interval time.Duration, log *slog.Logger) *Job {
	return &Job{st: st, interval: interval, log: log}
}

// Run backfills immutable history once, then recomputes the mutable tail
// every Interval until ctx is done. A failed pass is logged, not fatal —
// the next tick tries again.
func (j *Job) Run(ctx context.Context) {
	if err := j.Backfill(ctx); err != nil && ctx.Err() == nil {
		j.log.Error("statsrollup: backfill failed", "err", err)
	}
	t := time.NewTicker(j.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := j.tick(ctx); err != nil && ctx.Err() == nil {
				j.log.Error("statsrollup: tick failed", "err", err)
			}
		}
	}
}

// Backfill computes and persists the rollup for any day in
// [today-(backfillDays-1), today-tailDays] not already present
// (store.RolledUpDays) — immutable history that, once rolled up, never
// needs recomputing again. Days newer than that (the mutable tail) are left
// to tick.
func (j *Job) Backfill(ctx context.Context) error {
	now := time.Now().UTC()
	today := startOfDayUTC(now)
	from := today.AddDate(0, 0, -(backfillDays - 1))
	to := today.AddDate(0, 0, -tailDays)

	rolled, err := j.st.RolledUpDays(ctx, from, to)
	if err != nil {
		return err
	}
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if rolled[dayKey(d)] {
			continue
		}
		if err := j.recomputeDay(ctx, d, now); err != nil {
			return err
		}
		j.log.Info("statsrollup: backfilled day", "day", dayKey(d))
	}
	return nil
}

// tick recomputes the mutable tail: today and yesterday (UTC). Idempotent —
// safe to call repeatedly (store.UpsertRollupDay replaces, never appends).
func (j *Job) tick(ctx context.Context) error {
	now := time.Now().UTC()
	today := startOfDayUTC(now)
	yesterday := today.AddDate(0, 0, -1)
	if err := j.recomputeDay(ctx, yesterday, now); err != nil {
		return err
	}
	return j.recomputeDay(ctx, today, now)
}

// recomputeDay re-derives day D's rollup from raw matches and replaces the
// stored row(s) for D. matches/players/duration are attributed to D only
// for matches whose real start day is D — AggregateDaily also emits a dim
// for an earlier start day when a still-running (or long) match merely
// overlaps D (carrying D's slot_seconds share), which must be dropped here
// or an earlier, already-immutable day would be double-counted every time D
// is recomputed.
func (j *Job) recomputeDay(ctx context.Context, d, now time.Time) error {
	dStart := startOfDayUTC(d)
	dEnd := dStart.AddDate(0, 0, 1)

	matches, err := j.st.StatMatchesOverlapping(ctx, dStart, dEnd)
	if err != nil {
		return err
	}
	dims, peakByDay := stats.AggregateDaily(matches, []time.Time{dStart}, now)

	var filtered []stats.DailyDim
	for _, dim := range dims {
		if dim.Day.Equal(dStart) {
			filtered = append(filtered, dim)
		}
	}
	return j.st.UpsertRollupDay(ctx, dStart, filtered, peakByDay[dayKey(dStart)])
}

func startOfDayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }
