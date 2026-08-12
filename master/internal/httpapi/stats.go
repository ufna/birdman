package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/ufna/birdman/master/internal/stats"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/utctime"
)

// Statistics/Cost-view endpoints for the panel П2 screens
// (docs/specs/panel.md §3, master.md §6). Product-mode aggregates are served
// from the match_stats_daily/match_ccu_daily rollups (internal/store's
// rollup.go; maintained by internal/statsrollup) for every axis day up to
// today-2, and recomputed live from raw matches for the trailing two days
// (yesterday+today) on every request — so the response reflects raw data for
// those two days immediately, without waiting on the rollup-maintenance
// job's next tick («Статистика v1» T10). This live recompute is also where
// matches are assumed to still be able to change (settle/finish) before
// their day is handed off to the immutable rollup range — true so long as a
// match settles within ≤2 days of starting, which holds for this platform's
// session-based dedik matches (minutes-long); see
// internal/statsrollup/job.go's package doc for what happens on the
// (currently untriggered) assumption violation. The pure aggregation
// (series shapes, day bucketing, sweep-line CCU, percentiles) lives in
// internal/stats — no DB/HTTP dependencies there, so it's unit-testable and
// shared between the on-the-fly path (BuildOverview/BuildCost — still the
// golden reference, see stats/golden_test.go), the rollup job, and this
// handler's dims path (BuildOverviewFromDaily/BuildCostFromDaily). This file
// is only the HTTP surface: parse ?days=, assemble the dims via the split
// read-path below, hand them to the stats package, write JSON. The wire
// contract (URL, ?days= semantics, JSON shape) is unchanged by this split —
// see TestStatsOverviewRollupBacked/TestStatsCostRollupBacked in
// stats_test.go, which prove the rollup-backed response equals the
// on-the-fly one (both under the same settle-within-tail assumption above).

const (
	statsDefaultDays = 7
	// statsMaxDays is a hard 30-day statistics ceiling (product decision:
	// older data isn't served) that also matches statsrollup's backfill
	// horizon (backfillDays=30) — days beyond it would silently read
	// incomplete rollup data for the tail of the range.
	statsMaxDays = 30
)

// --- handlers ---

// handleStatsOverview is GET /v1/stats/overview?days=&project=&env= (readonly).
// Гейт привязки (tenantScope, #993) стоит ПЕРВЫМ — раньше разбора `?days=`: по
// правилу «гейт настолько рано, насколько позволяет адресация» (#989). Для
// привязанного ключа агрегаты считаются по его паре, чужой явный слаг → 403.
func (s *Server) handleStatsOverview(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.tenantScope(w, r, true)
	if !ok {
		return
	}
	days, ok := statsDays(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	axis := stats.DayAxisUTC(now, days)
	dims, peak, err := s.statsDims(r.Context(), axis, now, project, env)
	if err != nil {
		storeError(w, err)
		return
	}
	ttm, err := s.st.StatMatchesTTM(r.Context(), axis[0], project, env)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats.BuildOverviewFromDaily(dims, peak, ttm, axis, days, now))
}

// handleStatsCost is GET /v1/stats/cost?days=&project=&env= (readonly). Гейт
// привязки — первым, как в overview выше.
func (s *Server) handleStatsCost(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.tenantScope(w, r, true)
	if !ok {
		return
	}
	days, ok := statsDays(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	axis := stats.DayAxisUTC(now, days)
	dims, _, err := s.statsDims(r.Context(), axis, now, project, env)
	if err != nil {
		storeError(w, err)
		return
	}
	// Utilization is a current capacity snapshot, left unscoped by the ?env=/
	// ?project= QUERY on purpose: it is orthogonal to the historical, env-scoped
	// slot-hours the filter narrows above, и панель подписывает его как
	// платформенный. Сужается он ТОЛЬКО привязкой ключа (#993) — иначе снимок
	// «сколько всего слотов в eu и сколько из них занято» оставался бы
	// платформенным агрегатом в ответе арендатору, при том что сами его ноды и
	// серверы (`/v1/nodes`, `/v1/servers`) уже сужены. Именно ПРИВЯЗКА, а не
	// разобранная пара: у глобального ключа bound=false → фильтр пуст → ответ
	// байт-в-байт прежний, включая случай `?project=game` в панели.
	// Фильтр СОБИРАЕТСЯ ОДИН РАЗ и уезжает в оба места — в выборку снимка и в
	// его подпись (tracker #1009). Раньше подпись была константой и после
	// сужения #993 противоречила собственным данным: привязанному ключу
	// отдавали ЕГО слоты, подписанные как «platform-wide … across ALL
	// environments». Одно значение на оба потребителя — единственный способ,
	// которым они не разъедутся снова.
	bp, be, _ := keyBinding(r)
	scope := store.RegionUtilFilter{Project: bp, Env: be}
	util, err := s.st.RegionUtilization(r.Context(), scope)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats.BuildCostFromDaily(dims, util, scope, axis, days, now))
}

// statsDays parses ?days=N (default 7, 1..30). Writes the 400 itself.
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

// --- rollup-backed read-path (shared by overview/cost) ---

// statsDims assembles the daily dims + per-day peak-CCU map for `axis`
// (oldest day first, ending "today" — stats.DayAxisUTC's shape): the
// immutable portion, every axis day up to and including today-2, is read
// from the match_stats_daily/match_ccu_daily rollups (RollupDims/
// RollupPeakCCU); the mutable tail, yesterday and today, is recomputed live
// from raw matches every call (StatMatchesOverlapping + stats.AggregateDaily),
// so the result reflects the latest raw data for those two days regardless
// of when the rollup-maintenance job (internal/statsrollup) last ticked.
// This live tail is where matches are assumed to still be able to change
// (settle/finish) — an assumption that holds so long as a match settles
// within ≤2 days of starting, true for this platform's session-based dedik
// matches (minutes-long); see internal/statsrollup/job.go's package doc for
// what happens on the (currently untriggered) assumption violation. When
// axis is entirely within the tail (days<=2), the immutable range is empty
// and is skipped rather than queried.
//
// AggregateDaily can emit a dim for a match's start day even when that day
// falls before the live window (e.g. a match that started five days ago and
// is still running, seen while recomputing just yesterday+today) —
// filterDimsFrom drops it, or it would leak a day outside axis and, worse,
// double-count a day already served from the immutable range. This mirrors
// statsrollup/job.go's recomputeDay, which drops the same kind of stray dim
// for the same reason.
func (s *Server) statsDims(ctx context.Context, axis []time.Time, now time.Time, project, env string) ([]stats.DailyDim, map[string]int, error) {
	axis0 := axis[0]
	today := utctime.StartOfDay(now)
	liveStart := today.AddDate(0, 0, -1)        // 00:00 UTC yesterday
	immutableEnd := liveStart.AddDate(0, 0, -1) // today-2: immutable range's last day

	var dims []stats.DailyDim
	peak := map[string]int{}
	if !axis0.After(immutableEnd) {
		// Dims сужаются колонками env и project (пусто = всё). Пик CCU читается
		// из своего проектного среза match_ccu_daily: project="" — платформенный
		// (все проекты), непустой — пик ЭТОГО проекта. По env пик по-прежнему НЕ
		// делится (решение I5): окружения делят одну ёмкость флота, а проекты —
		// непересекающиеся тенанты, поэтому измерение добавлено только второму.
		immutableDims, err := s.st.RollupDims(ctx, axis0, immutableEnd, store.RollupFilter{Env: env, Project: project})
		if err != nil {
			return nil, nil, err
		}
		dims = immutableDims

		immutablePeak, err := s.st.RollupPeakCCU(ctx, axis0, immutableEnd, project)
		if err != nil {
			return nil, nil, err
		}
		peak = immutablePeak
	}

	liveAxis := statsLiveAxis(axis, liveStart)
	matches, err := s.st.StatMatchesOverlapping(ctx, liveStart, now)
	if err != nil {
		return nil, nil, err
	}
	// Живой хвост считается так же, как иммутабельная часть, иначе два конца
	// одного ряда разъехались бы: проект сужает ВСЁ, включая пик (проекты —
	// непересекающиеся тенанты), а env — только dims, оставляя пик
	// платформенным (I5, панель так его и подписывает).
	scoped := matches
	if project != "" {
		scoped = filterMatchesByProject(matches, project)
	}
	liveDims, livePeak := stats.AggregateDaily(scoped, liveAxis, now)
	if env != "" {
		liveDims, _ = stats.AggregateDaily(filterMatchesByEnv(scoped, env), liveAxis, now)
	}
	dims = append(dims, filterDimsFrom(liveDims, liveAxis[0])...)
	for dk, v := range livePeak { // live always wins for its own days
		peak[dk] = v
	}
	return dims, peak, nil
}

// filterMatchesByProject keeps only the matches of one project — the live-tail
// counterpart of RollupDims' project-column filter (мультипроект W3). Only
// called with a non-empty project.
func filterMatchesByProject(matches []store.StatMatch, project string) []store.StatMatch {
	out := make([]store.StatMatch, 0, len(matches))
	for _, m := range matches {
		if m.Project == project {
			out = append(out, m)
		}
	}
	return out
}

// filterMatchesByEnv keeps only the matches whose execution env (matches.env,
// I6) equals env — the live-tail counterpart of RollupDims' env-column filter
// (statsDims uses it for env-scoped dims while keeping the unfiltered set for
// the platform-wide peak CCU). Only called with a non-empty env.
func filterMatchesByEnv(matches []store.StatMatch, env string) []store.StatMatch {
	out := make([]store.StatMatch, 0, len(matches))
	for _, m := range matches {
		if m.Env == env {
			out = append(out, m)
		}
	}
	return out
}

// statsLiveAxis returns the axis days on/after liveStart, oldest first — the
// trailing window statsDims recomputes live instead of reading from
// rollups. Never empty: axis's last element is always "today"
// (stats.DayAxisUTC), and today is never before liveStart (yesterday) —
// note this can differ from "yesterday and today" literally: for a 1-day
// window (axis = [today] only), the live axis is just [today], since
// yesterday was never part of the request.
func statsLiveAxis(axis []time.Time, liveStart time.Time) []time.Time {
	var out []time.Time
	for _, d := range axis {
		if !d.Before(liveStart) {
			out = append(out, d)
		}
	}
	return out
}

// filterDimsFrom keeps only the dims on/after `from` (oldest-first input,
// order preserved) — see statsDims' doc comment for why AggregateDaily's
// output needs this filter.
func filterDimsFrom(dims []stats.DailyDim, from time.Time) []stats.DailyDim {
	var out []stats.DailyDim
	for _, d := range dims {
		if !d.Day.Before(from) {
			out = append(out, d)
		}
	}
	return out
}
