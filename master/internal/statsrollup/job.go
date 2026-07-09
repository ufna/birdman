// Package statsrollup maintains the match_stats_daily/match_ccu_daily
// rollup tables (store.UpsertRollupDay et al., «Статистика v1» T8) so long
// statistics windows don't need to rescan raw matches (internal/stats, T7).
// Two passes share one recompute primitive (recomputeDay):
//
//   - Backfill (run once, at startup): unconditionally recomputes every day
//     in [today-29, today-2] from raw matches, replacing any rollup already
//     there. Raw matches are never pruned in v1, so any day in this
//     immutable range can always be correctly re-derived, which makes every
//     restart self-correcting: if the master was down for the entirety of
//     some day D+1, the tick below never got a chance to finalize D while D
//     was still in its own trailing window, and D would otherwise carry a
//     stale/partial rollup forever once it aged out of that window into
//     Backfill's range — recomputing unconditionally (not just days
//     missing a rollup) means the next restart overwrites it from scratch
//     instead of skipping it as "already done".
//   - the tick (every Interval): recomputes the trailing two UTC days
//     [today-1, today] unconditionally. This is where matches are assumed
//     to still be able to change (settle/finish) — true so long as a match
//     settles within ≤2 days of starting, which holds for this platform's
//     session-based dedik matches (minutes-long). A match still running
//     when its start day rolls out of this tail and into Backfill's range
//     freezes that day's avg_match_duration/players_peak (understated)
//     until a later Backfill re-derives it — self-correcting on the
//     master's next restart per above, but not before.
//
// recomputeDay(D) re-derives D's rollup from raw matches every time, so
// recomputing a day twice is idempotent (store.UpsertRollupDay deletes D's
// existing rows before inserting — replace, not append). Backfill logs and
// continues past a single day's error rather than aborting the rest of the
// range — mirrors dbCollector.Collect's query-and-continue pattern
// (internal/metrics/metrics.go).
package statsrollup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ufna/birdman/master/internal/stats"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/utctime"
)

const (
	// backfillDays bounds Backfill's window: [today-(backfillDays-1), today-tailDays].
	backfillDays = 30
	// tailDays is how many trailing UTC days (today and the ones before it)
	// the tick always recomputes; today-tailDays is Backfill's upper bound.
	tailDays = 2
	// defaultInterval is the tick period used when New is given a
	// non-positive interval — matches config.defaults()'s
	// StatsRollupInterval (master/internal/config/config.go) so a
	// zero-value/misconfigured interval still behaves like the documented
	// default instead of panicking.
	defaultInterval = 2 * time.Minute
)

type Job struct {
	st       *store.Store
	interval time.Duration
	log      *slog.Logger
}

// New builds a rollup-maintenance job. Call Run in its own goroutine. A
// non-positive interval is clamped to defaultInterval: time.NewTicker
// (used by Run) panics when given a non-positive duration, and Run executes
// in a background goroutine where an uncaught panic would crash the whole
// process — mirrors matchmaker.Config.withDefaults' identical guard on Tick
// (master/internal/matchmaker/matchmaker.go).
func New(st *store.Store, interval time.Duration, log *slog.Logger) *Job {
	if interval <= 0 {
		interval = defaultInterval
	}
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

// Backfill unconditionally recomputes every day in
// [today-(backfillDays-1), today-tailDays] from raw matches, replacing any
// rollup already there for that day — see the package doc for why
// unconditional (not just missing days) is what makes this self-correcting
// after an outage. Raw matches are never pruned in v1, so re-deriving every
// day in this immutable range is always sound, and cheap enough for a
// one-time pass at process start (index-backed per-day queries; the
// per-request read path, which only reads rollups, is unaffected). Days
// newer than this range (the mutable tail) are left to tick.
//
// A day's recomputeDay error is logged (with the day) and does not abort
// the remaining, newer days — mirrors dbCollector.Collect's
// query-and-continue pattern (internal/metrics/metrics.go), so one bad day
// doesn't freeze backfilling for every day after it. The returned error is
// nil iff every day succeeded; Run logs a non-nil result and proceeds to
// the tick loop regardless (the next restart's Backfill, or tick once a
// day enters its range, gets another chance at a day that failed here).
func (j *Job) Backfill(ctx context.Context) error {
	now := time.Now().UTC()
	today := utctime.StartOfDay(now)
	from := today.AddDate(0, 0, -(backfillDays - 1))
	to := today.AddDate(0, 0, -tailDays)

	var errs []error
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if err := j.recomputeDay(ctx, d, now); err != nil {
			j.log.Error("statsrollup: backfill day failed", "day", utctime.DayKey(d), "err", err)
			errs = append(errs, fmt.Errorf("backfill day %s: %w", utctime.DayKey(d), err))
			continue
		}
		j.log.Info("statsrollup: backfilled day", "day", utctime.DayKey(d))
	}
	return errors.Join(errs...)
}

// tick recomputes the mutable tail: today and yesterday (UTC). Idempotent —
// safe to call repeatedly (store.UpsertRollupDay replaces, never appends).
func (j *Job) tick(ctx context.Context) error {
	now := time.Now().UTC()
	today := utctime.StartOfDay(now)
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
	dStart := utctime.StartOfDay(d)
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
	return j.st.UpsertRollupDay(ctx, dStart, filtered, peakByDay[utctime.DayKey(dStart)])
}
