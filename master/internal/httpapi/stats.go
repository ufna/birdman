package httpapi

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

// Statistics/Cost-view endpoints for the panel П2 screens
// (docs/specs/panel.md §3, master.md §6). v0: aggregates are computed
// on-the-fly from matches/servers/nodes — no materialized rollups yet (fine at
// our volume; a rollup job comes later). Every series is UTC, day-bucketed,
// and zero-filled (empty periods are 0, never gaps) so the panel can plot it
// directly. All series carry an explicit unit.

const (
	statsDefaultDays = 7
	statsMaxDays     = 90
)

// --- chart-ready series shapes ---

// simplePoint is one {date, value} sample of a single series.
type simplePoint struct {
	Date  string  `json:"date"`  // YYYY-MM-DD, UTC
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

type overviewResponse struct {
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

type costResponse struct {
	Days                     int                `json:"days"`
	Timezone                 string             `json:"timezone"`
	GeneratedAt              time.Time          `json:"generated_at"`
	SlotHoursPerDayByRegion  stackedSeries      `json:"slot_hours_per_day_by_region"`
	SlotHoursPerDayByVersion stackedSeries      `json:"slot_hours_per_day_by_version"`
	SlotHoursTotal           float64            `json:"slot_hours_total"`
	Utilization              []store.RegionUtil `json:"utilization"`
	UtilizationNote          string             `json:"utilization_note"`
}

// --- handlers ---

func (s *Server) handleStatsOverview(w http.ResponseWriter, r *http.Request) {
	days, ok := statsDays(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	axis := dayAxisUTC(now, days)
	matches, err := s.st.StatMatches(r.Context(), axis[0])
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildOverview(matches, axis, days, now))
}

func (s *Server) handleStatsCost(w http.ResponseWriter, r *http.Request) {
	days, ok := statsDays(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	axis := dayAxisUTC(now, days)
	matches, err := s.st.StatMatches(r.Context(), axis[0])
	if err != nil {
		storeError(w, err)
		return
	}
	util, err := s.st.RegionUtilization(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildCost(matches, util, axis, days, now))
}

// statsDays parses ?days=N (default 7, 1..90). Writes the 400 itself.
func statsDays(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("days")
	if raw == "" {
		return statsDefaultDays, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > statsMaxDays {
		writeError(w, http.StatusBadRequest, "bad_request",
			"days must be an integer in 1.."+strconv.Itoa(statsMaxDays))
		return 0, false
	}
	return n, true
}

// --- pure aggregation (unit-testable, no DB) ---

// buildOverview shapes the Statistics screen from the window's started matches.
func buildOverview(matches []store.StatMatch, axis []time.Time, days int, now time.Time) overviewResponse {
	ccuSeries, peakCCU := peakCCUPerDay(matches, axis, now)
	return overviewResponse{
		Days:        days,
		Timezone:    "UTC",
		GeneratedAt: now,
		MatchesPerDay: stackByRegionDay(matches, axis, "matches/day",
			func(store.StatMatch) float64 { return 1 }),
		PlayersPerDay: stackByRegionDay(matches, axis, "players/day (sum players_peak)",
			func(m store.StatMatch) float64 { return float64(m.PlayersPeak) }),
		PeakCCUPerDay:           ccuSeries,
		PeakCCU:                 peakCCU,
		AvgMatchDurationSeconds: avgDurationOverall(matches, now),
		AvgDurationPerDay:       avgDurationPerDay(matches, axis, now),
		VersionDistribution:     versionDistribution(matches),
		TimeToMatch:             timeToMatchStats(matches),
	}
}

// buildCost shapes the Cost-view screen: slot-hours (a proxy for hardware cost
// — allocated dedik time) split across UTC days by region and by version, plus
// a current utilization snapshot.
func buildCost(matches []store.StatMatch, util []store.RegionUtil, axis []time.Time, days int, now time.Time) costResponse {
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
			dk := dayKey(day)
			byRegion.add(dk, m.Region, hours)
			byVersion.add(dk, m.Semver, hours)
			total += hours
		}
	}
	return costResponse{
		Days:                     days,
		Timezone:                 "UTC",
		GeneratedAt:              now,
		SlotHoursPerDayByRegion:  byRegion.series("slot-hours", axis),
		SlotHoursPerDayByVersion: byVersion.series("slot-hours", axis),
		SlotHoursTotal:           round2(total),
		Utilization:              emptyNotNull(util),
		UtilizationNote: "current snapshot (allocated/ready/draining vs active-node capacity); " +
			"utilization over time is available via the metrics proxy (birdman_servers, query_range)",
	}
}

// dayAxisUTC returns the last `days` UTC calendar days ending today (inclusive),
// oldest first — the x-axis every series is zero-filled onto.
func dayAxisUTC(now time.Time, days int) []time.Time {
	today := startOfDayUTC(now)
	axis := make([]time.Time, days)
	for i := 0; i < days; i++ {
		axis[i] = today.AddDate(0, 0, -(days - 1 - i))
	}
	return axis
}

func startOfDayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

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
		acc.add(dayKey(m.StartedAt), m.Region, val(m))
	}
	return acc.series(unit, axis)
}

// peakCCUPerDay computes the per-day peak concurrent players via a sweep line
// over match intervals, using each match's players_peak as its occupancy
// (v0 proxy: peak, not an instantaneous series — it can overestimate true
// simultaneous CCU, matching the "max одновременных allocated-players" intent).
// Returns the per-day series and the window-wide peak.
func peakCCUPerDay(matches []store.StatMatch, axis []time.Time, now time.Time) (simpleSeries, int) {
	overall := 0
	points := make([]simplePoint, len(axis))
	for i, day := range axis {
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
		points[i] = simplePoint{Date: dayKey(day), Value: float64(peak)}
		if peak > overall {
			overall = peak
		}
	}
	return simpleSeries{Unit: "players (peak concurrent)", Points: points}, overall
}

// avgDurationOverall is the mean finished-match duration in seconds over the
// window, or nil when no match has finished yet.
func avgDurationOverall(matches []store.StatMatch, now time.Time) *float64 {
	var sum float64
	var n int
	for _, m := range matches {
		if m.EndedAt == nil {
			continue // only finished matches have a real duration
		}
		sum += m.EndedAt.UTC().Sub(m.StartedAt.UTC()).Seconds()
		n++
	}
	if n == 0 {
		return nil
	}
	avg := round2(sum / float64(n))
	return &avg
}

// avgDurationPerDay is the mean finished-match duration (seconds) bucketed by
// the day the match started. Days with no finished match are 0.
func avgDurationPerDay(matches []store.StatMatch, axis []time.Time, now time.Time) simpleSeries {
	sum := map[string]float64{}
	cnt := map[string]int{}
	for _, m := range matches {
		if m.EndedAt == nil {
			continue
		}
		k := dayKey(m.StartedAt)
		sum[k] += m.EndedAt.UTC().Sub(m.StartedAt.UTC()).Seconds()
		cnt[k]++
	}
	points := make([]simplePoint, len(axis))
	for i, day := range axis {
		k := dayKey(day)
		v := 0.0
		if cnt[k] > 0 {
			v = round2(sum[k] / float64(cnt[k]))
		}
		points[i] = simplePoint{Date: k, Value: v}
	}
	return simpleSeries{Unit: "seconds", Points: points}
}

// versionDistribution counts matches per semver over the window, most first.
func versionDistribution(matches []store.StatMatch) []versionShare {
	counts := map[string]int{}
	for _, m := range matches {
		counts[m.Semver]++
	}
	out := make([]versionShare, 0, len(counts))
	total := len(matches)
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
		dk := dayKey(day)
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
