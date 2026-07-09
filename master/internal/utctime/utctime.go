// Package utctime holds the two UTC-calendar-day helpers the statistics
// stack shares. Every layer buckets data by UTC day — stats aggregates onto a
// day axis, httpapi splits it into a rollup range and a live tail, statsrollup
// recomputes one day at a time, store persists day-keyed rollup rows — and
// their day keys MUST agree across package boundaries: store.RollupPeakCCU's
// map is looked up with stats-produced keys, and the rollup tables' `day`
// column round-trips through the same truncation. Keeping the truncation and
// the "2006-01-02" format in one leaf package (imports nothing but stdlib
// time, so anything may depend on it without cycles) makes that agreement
// structural instead of hand-synced copies.
package utctime

import "time"

// StartOfDay truncates t to its UTC calendar date (midnight UTC).
func StartOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// DayKey is t's UTC calendar date as "2006-01-02" — the day-bucket map and
// series key shape shared across the statistics packages.
func DayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }
