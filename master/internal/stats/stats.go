// Package stats holds the pure statistics aggregation for the panel П2
// Statistics/Cost-view screens (docs/specs/panel.md §3, docs/specs/master.md
// §6). It has no DB/HTTP dependencies: inputs are the raw rows the store
// layer already fetched (store.StatMatch, store.RegionUtil); outputs are the
// exact response shapes the panel consumes. Keeping this pure makes it
// unit-testable without a DB and shareable between the request path and the
// rollup job (internal/statsrollup).
//
// Two aggregation paths exist and must agree byte-for-byte (proven by
// golden_test.go):
//   - the on-the-fly path (BuildOverview/BuildCost) walks raw matches for the
//     whole window — v0's only path, kept as the golden reference.
//   - the dimensional path (AggregateDaily → BuildOverviewFromDaily/
//     BuildCostFromDaily) pre-aggregates matches into one row per
//     day×region×semver (DailyDim) plus a per-day peak CCU — the shape the
//     match_stats_daily/match_ccu_daily rollups persist (store/rollup.go,
//     maintained by internal/statsrollup) and the /v1/stats/* handlers now
//     serve, so long windows don't rescan raw matches.
//
// Every series is UTC, day-bucketed, and zero-filled (empty periods are 0,
// never gaps) so the panel can plot it directly. All series carry an
// explicit unit.
package stats

import (
	"math"
	"sort"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/utctime"
)

// unit labels, shared between the on-the-fly and dimensional paths so they
// can never drift apart.
const (
	unitMatchesPerDay = "matches/day"
	unitPlayersPerDay = "players/day (sum players_peak)"
	unitSlotHours     = "slot-hours"
	unitSeconds       = "seconds"
	unitPeakCCU       = "players (peak concurrent)"

	utilizationNote = "current, platform-wide snapshot (allocated/ready/draining vs active-node " +
		"capacity across ALL environments — the ?env filter does not narrow this capacity view); " +
		"utilization over time is available via the metrics proxy (birdman_servers, query_range)"
)

// --- chart-ready series shapes ---

// simplePoint is one {date, value} sample of a single series.
type simplePoint struct {
	Date  string  `json:"date"` // YYYY-MM-DD, UTC
	Value float64 `json:"value"`
}

type simpleSeries struct {
	Unit   string        `json:"unit"`
	Points []simplePoint `json:"points"`
}

// stackedPoint is one day of a stacked series: Total plus the per-key
// breakdown (every key in the series is present, zero-filled).
type stackedPoint struct {
	Date   string             `json:"date"`
	Total  float64            `json:"total"`
	Values map[string]float64 `json:"values"`
}

type stackedSeries struct {
	Unit   string         `json:"unit"`
	Keys   []string       `json:"keys"` // stack keys present in the window, sorted
	Points []stackedPoint `json:"points"`
}

type versionShare struct {
	Version string  `json:"version"`
	Matches int     `json:"matches"`
	Share   float64 `json:"share"` // 0..1 of matches in the window
}

// timeToMatch reports fill-rate percentiles. v0 source: matches
// created_at→started_at (allocation to match_start), always available from
// SQL. The true queue wait (ticket submit → matched) lives in the
// birdman_mm_time_to_match_seconds histogram, reachable via the metrics proxy
// (GET /v1/metrics/query) — see Note.
type timeToMatch struct {
	P50Seconds *float64 `json:"p50_seconds"`
	P95Seconds *float64 `json:"p95_seconds"`
	Samples    int      `json:"samples"`
	Source     string   `json:"source"`
	Note       string   `json:"note"`
}

// OverviewResponse is the Statistics screen shape.
type OverviewResponse struct {
	Days                    int            `json:"days"`
	Timezone                string         `json:"timezone"`
	GeneratedAt             time.Time      `json:"generated_at"`
	MatchesPerDay           stackedSeries  `json:"matches_per_day"`
	PlayersPerDay           stackedSeries  `json:"players_per_day"`
	PeakCCUPerDay           simpleSeries   `json:"peak_ccu_per_day"`
	PeakCCU                 int            `json:"peak_ccu"`
	AvgMatchDurationSeconds *float64       `json:"avg_match_duration_seconds"`
	AvgDurationPerDay       simpleSeries   `json:"avg_match_duration_per_day"`
	VersionDistribution     []versionShare `json:"version_distribution"`
	TimeToMatch             timeToMatch    `json:"time_to_match"`
}

// CostResponse is the Cost-view screen shape.
type CostResponse struct {
	Days                     int                `json:"days"`
	Timezone                 string             `json:"timezone"`
	GeneratedAt              time.Time          `json:"generated_at"`
	SlotHoursPerDayByRegion  stackedSeries      `json:"slot_hours_per_day_by_region"`
	SlotHoursPerDayByVersion stackedSeries      `json:"slot_hours_per_day_by_version"`
	SlotHoursTotal           float64            `json:"slot_hours_total"`
	Utilization              []store.RegionUtil `json:"utilization"`
	UtilizationNote          string             `json:"utilization_note"`
}

// --- on-the-fly aggregation (golden reference) ---

// BuildOverview shapes the Statistics screen from the window's started
// matches, computed fresh on every call. This is the golden reference:
// BuildOverviewFromDaily must always produce the identical result from
// AggregateDaily's output (see golden_test.go).
func BuildOverview(matches []store.StatMatch, axis []time.Time, days int, now time.Time) OverviewResponse {
	ccuSeries, peakCCU := peakCCUPerDay(matches, axis, now)
	return OverviewResponse{
		Days:        days,
		Timezone:    "UTC",
		GeneratedAt: now,
		MatchesPerDay: stackByRegionDay(matches, axis, unitMatchesPerDay,
			func(store.StatMatch) float64 { return 1 }),
		PlayersPerDay: stackByRegionDay(matches, axis, unitPlayersPerDay,
			func(m store.StatMatch) float64 { return float64(m.PlayersPeak) }),
		PeakCCUPerDay:           ccuSeries,
		PeakCCU:                 peakCCU,
		AvgMatchDurationSeconds: avgDurationOverall(matches),
		AvgDurationPerDay:       avgDurationPerDay(matches, axis),
		VersionDistribution:     versionDistribution(matches),
		TimeToMatch:             timeToMatchStats(matches),
	}
}

// BuildCost shapes the Cost-view screen: slot-hours (a proxy for hardware
// cost — allocated dedik time) split across UTC days by region and by
// version, plus a current utilization snapshot. Golden reference for
// BuildCostFromDaily.
func BuildCost(matches []store.StatMatch, util []store.RegionUtil, axis []time.Time, days int, now time.Time) CostResponse {
	byRegion := newStackAccum()
	byVersion := newStackAccum()
	total := 0.0
	for _, m := range matches {
		end := matchEnd(m, now)
		for _, day := range axis {
			secs := overlapSeconds(m.StartedAt.UTC(), end, day, day.AddDate(0, 0, 1))
			if secs <= 0 {
				continue
			}
			hours := secs / 3600
			dk := utctime.DayKey(day)
			byRegion.add(dk, m.Region, hours)
			byVersion.add(dk, m.Semver, hours)
			total += hours
		}
	}
	return CostResponse{
		Days:                     days,
		Timezone:                 "UTC",
		GeneratedAt:              now,
		SlotHoursPerDayByRegion:  byRegion.series(unitSlotHours, axis),
		SlotHoursPerDayByVersion: byVersion.series(unitSlotHours, axis),
		SlotHoursTotal:           round2(total),
		Utilization:              emptyNotNull(util),
		UtilizationNote:          utilizationNote,
	}
}

// --- dimensional daily aggregation (rollup-ready) ---

// DailyDim is one day × region × version of additive aggregates — a type
// alias for store.RollupDim (Task 8's match_stats_daily row shape). The
// canonical definition lives in store, not here: this package already
// imports store for its raw-row input types (StatMatch, RegionUtil), so
// store cannot import stats back without a cycle. Declaring the identity
// here instead keeps the dependency one-way while leaving stats.DailyDim and
// store.RollupDim fully interchangeable (literally the same type) for every
// caller on both sides.
type DailyDim = store.RollupDim

// AggregateDaily turns raw started-matches into daily dimensions + per-day
// peak CCU. matches/players_peak_sum/duration are attributed to each match's
// UTC start day (date(started_at), mirroring stackByRegionDay); slot_seconds
// are distributed across every axis day the match overlaps (mirroring
// BuildCost's inner loop, via overlapSeconds — matchEnd clamps a still-running
// match to now). Dims are keyed by day×region×semver×env (environments v1,
// I5): a match contributes to its own env's dim only, so the rollup persists
// one match_stats_daily row per environment and the read path can slice by
// env. Ключ dim'а несёт ещё и ПРОЕКТ (мультипроект W3), так что роллап
// хранит по строке на (region, semver, env, project) и читающая сторона
// сужает по проекту так же, как по env.
//
// peakByDay reuses the exact sweep-line peakCCUPerDay uses — over ALL
// matches, no env split — so occupancy stays comparable between the two
// paths; проектный срез пика считает AggregateDailyPeakByProject тем же
// сканом по подмножеству матчей (пик НЕ аддитивен, вывести один срез из
// другого нельзя).
//
// A match's start day need not itself be in axis (e.g. a tail-recompute of a
// single day D may see a match that started the day before but still
// overlaps D) — matches/players/duration are still attributed to that match's
// real start day in that case, per the rule above.
func AggregateDaily(matches []store.StatMatch, axis []time.Time, now time.Time) (dims []DailyDim, peakByDay map[string]int) {
	type key struct{ day, region, semver, env, project string }
	idx := map[key]int{}
	dimIndex := func(day time.Time, region, semver, env, project string) int {
		k := key{utctime.DayKey(day), region, semver, env, project}
		if i, ok := idx[k]; ok {
			return i
		}
		i := len(dims)
		idx[k] = i
		dims = append(dims, DailyDim{
			Day: utctime.StartOfDay(day), Region: region, Semver: semver, Env: env, Project: project,
		})
		return i
	}

	for _, m := range matches {
		si := dimIndex(m.StartedAt, m.Region, m.Semver, m.Env, m.Project)
		dims[si].Matches++
		dims[si].PlayersPeakSum += int64(m.PlayersPeak)
		if m.EndedAt != nil {
			dims[si].DurSumSeconds += m.EndedAt.UTC().Sub(m.StartedAt.UTC()).Seconds()
			dims[si].DurCount++
		}

		end := matchEnd(m, now)
		for _, day := range axis {
			secs := overlapSeconds(m.StartedAt.UTC(), end, day, day.AddDate(0, 0, 1))
			if secs <= 0 {
				continue
			}
			di := dimIndex(day, m.Region, m.Semver, m.Env, m.Project)
			dims[di].SlotSeconds += secs
		}
	}
	return dims, peakCCUByDay(matches, axis, now)
}

// AggregateDailyPeakByProject returns project slug -> (day key -> peak CCU),
// the per-project slices of the same sweep-line AggregateDaily uses for its
// platform-wide peak (мультипроект W3).
//
// Считается отдельным проходом по подмножеству матчей КАЖДОГО проекта, а не
// делением платформенного пика: пик — не аддитивная величина, сумма проектных
// пиков строго больше реального одновременного пика платформы, когда пики
// проектов пришлись на разные моменты суток. Поэтому оба среза хранятся, и ни
// один не выводится из другого (store.UpsertRollupDay пишет их рядом).
func AggregateDailyPeakByProject(matches []store.StatMatch, axis []time.Time, now time.Time) map[string]map[string]int {
	byProject := map[string][]store.StatMatch{}
	for _, m := range matches {
		byProject[m.Project] = append(byProject[m.Project], m)
	}
	out := make(map[string]map[string]int, len(byProject))
	for project, ms := range byProject {
		out[project] = peakCCUByDay(ms, axis, now)
	}
	return out
}

// BuildOverviewFromDaily builds the same OverviewResponse as BuildOverview,
// but from pre-aggregated daily dimensions (the rollup read-path) instead of
// raw matches: version mix sums Matches by semver, matches/players stack by
// region-day, avg duration is ΣDurSum/ΣDurCount. ttm is computed from the raw
// []store.StatMatch as before — percentiles are non-additive and can't be
// derived from dims.
func BuildOverviewFromDaily(dims []DailyDim, peakByDay map[string]int, ttm []store.StatMatch, axis []time.Time, days int, now time.Time) OverviewResponse {
	versionCounts := map[string]int{}
	totalMatches := 0
	durSumOverall, durCountOverall := 0.0, 0
	durSumByDay := map[string]float64{}
	durCountByDay := map[string]int{}
	for _, d := range dims {
		versionCounts[d.Semver] += d.Matches
		totalMatches += d.Matches
		durSumOverall += d.DurSumSeconds
		durCountOverall += d.DurCount
		dk := utctime.DayKey(d.Day)
		durSumByDay[dk] += d.DurSumSeconds
		durCountByDay[dk] += d.DurCount
	}
	ccuSeries, peakCCU := simpleSeriesFromCounts(unitPeakCCU, axis, peakByDay)
	return OverviewResponse{
		Days:        days,
		Timezone:    "UTC",
		GeneratedAt: now,
		MatchesPerDay: stackFromDims(dims, axis, unitMatchesPerDay,
			func(d DailyDim) float64 { return float64(d.Matches) }),
		PlayersPerDay: stackFromDims(dims, axis, unitPlayersPerDay,
			func(d DailyDim) float64 { return float64(d.PlayersPeakSum) }),
		PeakCCUPerDay:           ccuSeries,
		PeakCCU:                 peakCCU,
		AvgMatchDurationSeconds: avgPtr(durSumOverall, durCountOverall),
		AvgDurationPerDay:       avgSecondsSeries(axis, durSumByDay, durCountByDay),
		VersionDistribution:     versionShareList(dropZeroMatchSemvers(versionCounts), totalMatches),
		TimeToMatch:             timeToMatchStats(ttm),
	}
}

// dropZeroMatchSemvers removes semvers whose SUMMED Matches across all dims
// is 0, in place, returning the same map. A still-running match that
// started before the window contributes a slot-only dim (SlotSeconds>0,
// Matches==0) for every window day it overlaps, carrying its real Semver but
// zero started-matches; occupancy (peak_ccu/slot_hours) correctly counts
// that match (see AggregateDaily/BuildCostFromDaily), but version_distribution
// must not, or a version with zero matches in the window would surface as a
// phantom {Matches:0, Share:0} entry. Only the FromDaily path needs this: the
// on-the-fly versionDistribution only increments a semver's count when a
// match actually started in the window, so every entry it produces already
// has Matches >= 1.
func dropZeroMatchSemvers(counts map[string]int) map[string]int {
	for v, c := range counts {
		if c == 0 {
			delete(counts, v)
		}
	}
	return counts
}

// BuildCostFromDaily builds the same CostResponse as BuildCost, but from
// pre-aggregated daily dimensions instead of raw matches: slot-hours are
// ΣSlotSeconds/3600, stacked by region-day and by version-day.
func BuildCostFromDaily(dims []DailyDim, util []store.RegionUtil, axis []time.Time, days int, now time.Time) CostResponse {
	byRegion := newStackAccum()
	byVersion := newStackAccum()
	total := 0.0
	for _, d := range dims {
		if d.SlotSeconds <= 0 {
			continue // mirrors BuildCost's overlapSeconds<=0 guard: no phantom keys
		}
		hours := d.SlotSeconds / 3600
		dk := utctime.DayKey(d.Day)
		byRegion.add(dk, d.Region, hours)
		byVersion.add(dk, d.Semver, hours)
		total += hours
	}
	return CostResponse{
		Days:                     days,
		Timezone:                 "UTC",
		GeneratedAt:              now,
		SlotHoursPerDayByRegion:  byRegion.series(unitSlotHours, axis),
		SlotHoursPerDayByVersion: byVersion.series(unitSlotHours, axis),
		SlotHoursTotal:           round2(total),
		Utilization:              emptyNotNull(util),
		UtilizationNote:          utilizationNote,
	}
}

// --- shared helpers (used by both aggregation paths) ---

// DayAxisUTC returns the last `days` UTC calendar days ending today
// (inclusive), oldest first — the x-axis every series is zero-filled onto.
func DayAxisUTC(now time.Time, days int) []time.Time {
	today := utctime.StartOfDay(now)
	axis := make([]time.Time, days)
	for i := 0; i < days; i++ {
		axis[i] = today.AddDate(0, 0, -(days - 1 - i))
	}
	return axis
}

// matchEnd is a match's effective end: ended_at, or now for a running match.
func matchEnd(m store.StatMatch, now time.Time) time.Time {
	if m.EndedAt != nil {
		return m.EndedAt.UTC()
	}
	return now.UTC()
}

// stackByRegionDay buckets a per-match value by (started-at day, region).
func stackByRegionDay(matches []store.StatMatch, axis []time.Time, unit string, val func(store.StatMatch) float64) stackedSeries {
	acc := newStackAccum()
	for _, m := range matches {
		acc.add(utctime.DayKey(m.StartedAt), m.Region, val(m))
	}
	return acc.series(unit, axis)
}

// stackFromDims buckets a per-dim value by (day, region), summing across
// semver — the dims-path counterpart of stackByRegionDay.
func stackFromDims(dims []DailyDim, axis []time.Time, unit string, val func(DailyDim) float64) stackedSeries {
	acc := newStackAccum()
	for _, d := range dims {
		acc.add(utctime.DayKey(d.Day), d.Region, val(d))
	}
	return acc.series(unit, axis)
}

// peakCCUPerDay computes the per-day peak concurrent players via a sweep line
// over match intervals, using each match's players_peak as its occupancy
// (v0 proxy: peak, not an instantaneous series — it can overestimate true
// simultaneous CCU, matching the "max одновременных allocated-players" intent).
// Returns the per-day series and the window-wide peak.
func peakCCUPerDay(matches []store.StatMatch, axis []time.Time, now time.Time) (simpleSeries, int) {
	return simpleSeriesFromCounts(unitPeakCCU, axis, peakCCUByDay(matches, axis, now))
}

// peakCCUByDay is the sweep-line itself: utctime.DayKey → peak concurrent players for
// matches overlapping that day. Shared by peakCCUPerDay (on-the-fly path) and
// AggregateDaily (dims path) so they can never disagree.
func peakCCUByDay(matches []store.StatMatch, axis []time.Time, now time.Time) map[string]int {
	out := make(map[string]int, len(axis))
	for _, day := range axis {
		d0, d1 := day, day.AddDate(0, 0, 1)
		type ev struct {
			t     time.Time
			delta int
		}
		var evs []ev
		for _, m := range matches {
			start, end := m.StartedAt.UTC(), matchEnd(m, now)
			if !start.Before(d1) || !end.After(d0) { // no overlap with [d0,d1)
				continue
			}
			if m.PlayersPeak == 0 {
				continue
			}
			a, b := start, end
			if a.Before(d0) {
				a = d0
			}
			if b.After(d1) {
				b = d1
			}
			evs = append(evs, ev{a, m.PlayersPeak}, ev{b, -m.PlayersPeak})
		}
		// Sort by time; at equal instants apply additions before subtractions
		// so a shared boundary counts as overlap (peak-favoring).
		sort.Slice(evs, func(a, b int) bool {
			if evs[a].t.Equal(evs[b].t) {
				return evs[a].delta > evs[b].delta
			}
			return evs[a].t.Before(evs[b].t)
		})
		cur, peak := 0, 0
		for _, e := range evs {
			cur += e.delta
			if cur > peak {
				peak = cur
			}
		}
		out[utctime.DayKey(day)] = peak
	}
	return out
}

// simpleSeriesFromCounts zero-fills a day→count map onto axis, returning the
// series plus the window-wide max (used for both peak-CCU shapes).
func simpleSeriesFromCounts(unit string, axis []time.Time, counts map[string]int) (simpleSeries, int) {
	overall := 0
	points := make([]simplePoint, len(axis))
	for i, day := range axis {
		dk := utctime.DayKey(day)
		v := counts[dk]
		points[i] = simplePoint{Date: dk, Value: float64(v)}
		if v > overall {
			overall = v
		}
	}
	return simpleSeries{Unit: unit, Points: points}, overall
}

// avgDurationOverall is the mean finished-match duration in seconds over the
// window, or nil when no match has finished yet.
func avgDurationOverall(matches []store.StatMatch) *float64 {
	var sum float64
	var n int
	for _, m := range matches {
		if m.EndedAt == nil {
			continue // only finished matches have a real duration
		}
		sum += m.EndedAt.UTC().Sub(m.StartedAt.UTC()).Seconds()
		n++
	}
	return avgPtr(sum, n)
}

// avgPtr is round2(sum/n), or nil when n == 0 — shared by the overall-average
// on-the-fly and dims paths.
func avgPtr(sum float64, n int) *float64 {
	if n == 0 {
		return nil
	}
	avg := round2(sum / float64(n))
	return &avg
}

// avgDurationPerDay is the mean finished-match duration (seconds) bucketed by
// the day the match started. Days with no finished match are 0.
func avgDurationPerDay(matches []store.StatMatch, axis []time.Time) simpleSeries {
	sum := map[string]float64{}
	cnt := map[string]int{}
	for _, m := range matches {
		if m.EndedAt == nil {
			continue
		}
		k := utctime.DayKey(m.StartedAt)
		sum[k] += m.EndedAt.UTC().Sub(m.StartedAt.UTC()).Seconds()
		cnt[k]++
	}
	return avgSecondsSeries(axis, sum, cnt)
}

// avgSecondsSeries zero-fills day→(sum,count) maps onto axis as a
// round2(sum/count) series — shared by the on-the-fly and dims paths.
func avgSecondsSeries(axis []time.Time, sum map[string]float64, cnt map[string]int) simpleSeries {
	points := make([]simplePoint, len(axis))
	for i, day := range axis {
		k := utctime.DayKey(day)
		v := 0.0
		if cnt[k] > 0 {
			v = round2(sum[k] / float64(cnt[k]))
		}
		points[i] = simplePoint{Date: k, Value: v}
	}
	return simpleSeries{Unit: unitSeconds, Points: points}
}

// versionDistribution counts matches per semver over the window, most first.
func versionDistribution(matches []store.StatMatch) []versionShare {
	counts := map[string]int{}
	for _, m := range matches {
		counts[m.Semver]++
	}
	return versionShareList(counts, len(matches))
}

// versionShareList turns semver→count into a sorted []versionShare (most
// matches first, ties broken by version) — shared by the on-the-fly and dims
// paths.
func versionShareList(counts map[string]int, total int) []versionShare {
	out := make([]versionShare, 0, len(counts))
	for v, c := range counts {
		share := 0.0
		if total > 0 {
			share = round4(float64(c) / float64(total))
		}
		out = append(out, versionShare{Version: v, Matches: c, Share: share})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Matches != out[b].Matches {
			return out[a].Matches > out[b].Matches
		}
		return out[a].Version < out[b].Version
	})
	return out
}

// timeToMatchStats derives p50/p95 of allocation→match_start over the window.
func timeToMatchStats(matches []store.StatMatch) timeToMatch {
	secs := make([]float64, 0, len(matches))
	for _, m := range matches {
		d := m.StartedAt.UTC().Sub(m.CreatedAt.UTC()).Seconds()
		if d < 0 {
			d = 0
		}
		secs = append(secs, d)
	}
	ttm := timeToMatch{
		Samples: len(secs),
		Source:  "matches.created_at→started_at",
		Note: "time from match allocation to match_start (server readiness), a fill-rate proxy; " +
			"true queue wait (ticket submit→matched) is the birdman_mm_time_to_match_seconds histogram via the metrics proxy",
	}
	if len(secs) == 0 {
		return ttm
	}
	sort.Float64s(secs)
	p50 := round2(percentile(secs, 50))
	p95 := round2(percentile(secs, 95))
	ttm.P50Seconds = &p50
	ttm.P95Seconds = &p95
	return ttm
}

// percentile is nearest-rank on an ascending-sorted slice (p in 0..100).
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// overlapSeconds is the length in seconds of [aStart,aEnd) ∩ [bStart,bEnd).
func overlapSeconds(aStart, aEnd, bStart, bEnd time.Time) float64 {
	start := aStart
	if bStart.After(start) {
		start = bStart
	}
	end := aEnd
	if bEnd.Before(end) {
		end = bEnd
	}
	if !end.After(start) {
		return 0
	}
	return end.Sub(start).Seconds()
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// stackAccum accumulates day→key→value, tracking the set of keys seen.
type stackAccum struct {
	perDay map[string]map[string]float64
	keys   map[string]struct{}
}

func newStackAccum() *stackAccum {
	return &stackAccum{perDay: map[string]map[string]float64{}, keys: map[string]struct{}{}}
}

func (a *stackAccum) add(dayKey, key string, v float64) {
	if key == "" {
		key = "unknown"
	}
	if a.perDay[dayKey] == nil {
		a.perDay[dayKey] = map[string]float64{}
	}
	a.perDay[dayKey][key] += v
	a.keys[key] = struct{}{}
}

// series materializes the accumulator onto the axis, zero-filling every key on
// every day so stacks align and empty periods are 0 (not gaps).
func (a *stackAccum) series(unit string, axis []time.Time) stackedSeries {
	keys := make([]string, 0, len(a.keys))
	for k := range a.keys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	points := make([]stackedPoint, len(axis))
	for i, day := range axis {
		dk := utctime.DayKey(day)
		values := make(map[string]float64, len(keys))
		total := 0.0
		for _, k := range keys {
			v := round2(a.perDay[dk][k])
			values[k] = v
			total += v
		}
		points[i] = stackedPoint{Date: dk, Total: round2(total), Values: values}
	}
	return stackedSeries{Unit: unit, Keys: keys, Points: points}
}

// emptyNotNull keeps JSON arrays as [] instead of null. A small local copy of
// httpapi's helper of the same name — kept here too so this package has no
// dependency on httpapi (which depends on stats, not the other way round).
func emptyNotNull[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
