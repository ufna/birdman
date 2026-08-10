package statsrollup

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/stats"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	"github.com/ufna/birdman/master/internal/utctime"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// insertJobMatch inserts a matches row with explicit absolute
// started_at/ended_at (nil endedAt = still running).
func insertJobMatch(t *testing.T, st *store.Store, serverID, region string, playersPeak int, startedAt time.Time, endedAt *time.Time) {
	t.Helper()
	state := "finished"
	if endedAt == nil {
		state = "running"
	}
	_, err := st.Pool.Exec(context.Background(), `
		insert into matches (project_id, server_id, version_id, region, state, players_peak, created_at, started_at, ended_at)
		select s.project_id, s.id, s.version_id, $2, $3, $4, $5, $5, $6
		from servers s where s.id = $1::uuid`,
		serverID, region, state, playersPeak, startedAt, endedAt)
	if err != nil {
		t.Fatalf("insert match: %v", err)
	}
}

// insertJobMatchEnv is insertJobMatch with an explicit env (matches.env, the
// I6 source of truth for a match's environment) — the env grouping test needs
// dev and prod matches on the same day/region.
func insertJobMatchEnv(t *testing.T, st *store.Store, serverID, region, env string, playersPeak int, startedAt time.Time, endedAt *time.Time) {
	t.Helper()
	state := "finished"
	if endedAt == nil {
		state = "running"
	}
	_, err := st.Pool.Exec(context.Background(), `
		insert into matches (project_id, server_id, version_id, region, env, state, players_peak, created_at, started_at, ended_at)
		select s.project_id, s.id, s.version_id, $2, $3, $4, $5, $6, $6, $7
		from servers s where s.id = $1::uuid`,
		serverID, region, env, state, playersPeak, startedAt, endedAt)
	if err != nil {
		t.Fatalf("insert env match: %v", err)
	}
}

func findDim(dims []store.RollupDim, day time.Time, region string) (store.RollupDim, bool) {
	for _, d := range dims {
		if d.Day.Equal(day) && d.Region == region {
			return d, true
		}
	}
	return store.RollupDim{}, false
}

// TestRollupJob seeds matches around "now" (today/yesterday, a match
// spanning midnight UTC, and a match that started 5 days ago and is still
// running), runs Backfill then a tick, and checks:
//   - Backfill fills every missing immutable day [today-29, today-2]
//     (incl. the long-running match's own start day, attributed there in
//     full) and does NOT touch the tick's [today-1, today] range.
//   - tick recomputes yesterday/today matching a direct
//     StatMatchesOverlapping+AggregateDaily reference, day-filtered: a match
//     that started before the recomputed day contributes slot_seconds to
//     that day but not matches/players (the day-filter rule) — exercised by
//     both the midnight-spanning match (start day = yesterday) and the
//     5-day-old still-running match (start day well before either).
//   - two ticks in a row are idempotent: no duplicate rows, deterministic
//     fields unchanged, only the still-running match's "today" slot_seconds
//     may grow (time keeps passing).
func TestRollupJob(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()
	srv := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 20001, 0)

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	fiveDaysAgo := today.AddDate(0, 0, -5)

	// Fully within today: matches/players/duration/slot all land on today.
	insertJobMatch(t, st, srv, "today-match", 10, today.Add(2*time.Hour), tPtr(today.Add(2*time.Hour+30*time.Minute)))
	// Fully within yesterday.
	insertJobMatch(t, st, srv, "yesterday-match", 4, yesterday.Add(9*time.Hour), tPtr(yesterday.Add(9*time.Hour+20*time.Minute)))
	// Spans midnight UTC: start day = yesterday (matches/players/duration
	// attributed wholly there), slot_seconds split 30min/30min.
	insertJobMatch(t, st, srv, "midnight-match", 8, yesterday.Add(23*time.Hour+30*time.Minute), tPtr(today.Add(30*time.Minute)))
	// Started well before either recomputed day and still running: must
	// contribute slot_seconds to BOTH yesterday and today, but zero
	// matches/players/duration to either (day-filter rule) — its own
	// matches/players belong on its true start day, fiveDaysAgo, which
	// Backfill (not tick) is responsible for.
	insertJobMatch(t, st, srv, "running-match", 6, fiveDaysAgo.Add(12*time.Hour), nil)

	job := New(st, time.Minute, testLog())

	// --- Backfill: fills [today-29, today-2], leaves the tick's range alone.
	if err := job.Backfill(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// Presence is read via RollupPeakCCU's map (every processed day gets a
	// match_ccu_daily row, even an empty/zero-peak one -- see
	// UpsertRollupDay's doc comment), since RolledUpDays was removed once
	// Backfill stopped needing a which-days-are-missing scan (Fix 1: it now
	// recomputes every day unconditionally, see TestRollupJobBackfillSelfHeals below).
	backfillFrom, backfillTo := today.AddDate(0, 0, -29), today.AddDate(0, 0, -2)
	peaksBackfill, err := st.RollupPeakCCU(ctx, backfillFrom, backfillTo, "")
	if err != nil {
		t.Fatalf("peak ccu (backfill range): %v", err)
	}
	if len(peaksBackfill) != 28 {
		t.Fatalf("backfill: want 28 rolled-up days in [today-29,today-2], got %d: %+v", len(peaksBackfill), peaksBackfill)
	}
	peaksTail, err := st.RollupPeakCCU(ctx, yesterday, today, "")
	if err != nil {
		t.Fatalf("peak ccu (tail range): %v", err)
	}
	if len(peaksTail) != 0 {
		t.Fatalf("backfill must not touch [today-1,today], got: %+v", peaksTail)
	}

	// fiveDaysAgo is the long-running match's true start day: Backfill
	// attributes its matches/players there in full (deterministic — the
	// match's clamp-to-now end is always past fiveDaysAgo+1day by 5 days).
	fdDims, err := st.RollupDims(ctx, fiveDaysAgo, fiveDaysAgo, store.RollupFilter{Env: ""})
	if err != nil {
		t.Fatalf("dims fiveDaysAgo: %v", err)
	}
	fdDim, ok := findDim(fdDims, fiveDaysAgo, "running-match")
	if !ok {
		t.Fatalf("running-match dim missing on its start day fiveDaysAgo: %+v", fdDims)
	}
	if fdDim.Matches != 1 || fdDim.PlayersPeakSum != 6 || fdDim.DurCount != 0 || fdDim.SlotSeconds != 12*3600 {
		t.Fatalf("fiveDaysAgo running-match dim: %+v", fdDim)
	}
	fdPeaks, err := st.RollupPeakCCU(ctx, fiveDaysAgo, fiveDaysAgo, "")
	if err != nil {
		t.Fatalf("peaks fiveDaysAgo: %v", err)
	}
	if fdPeaks[utctime.DayKey(fiveDaysAgo)] != 6 {
		t.Fatalf("fiveDaysAgo peak ccu = %v, want 6", fdPeaks[utctime.DayKey(fiveDaysAgo)])
	}

	// --- tick: recompute the trailing two days [yesterday, today].
	beforeTick := time.Now().UTC()
	if err := job.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	afterTick := time.Now().UTC()

	yestDims, err := st.RollupDims(ctx, yesterday, yesterday, store.RollupFilter{Env: ""})
	if err != nil {
		t.Fatalf("dims yesterday: %v", err)
	}
	todayDims, err := st.RollupDims(ctx, today, today, store.RollupFilter{Env: ""})
	if err != nil {
		t.Fatalf("dims today: %v", err)
	}

	// yesterday-match: ordinary same-day attribution.
	if d, ok := findDim(yestDims, yesterday, "yesterday-match"); !ok || d.Matches != 1 || d.PlayersPeakSum != 4 ||
		d.DurSumSeconds != 1200 || d.DurCount != 1 || d.SlotSeconds != 1200 {
		t.Fatalf("yesterday-match dim: %+v (ok=%v)", d, ok)
	}
	// midnight-match: matches/players/duration wholly on yesterday (its
	// start day); slot_seconds only the 30 minutes before midnight.
	if d, ok := findDim(yestDims, yesterday, "midnight-match"); !ok || d.Matches != 1 || d.PlayersPeakSum != 8 ||
		d.DurSumSeconds != 3600 || d.DurCount != 1 || d.SlotSeconds != 1800 {
		t.Fatalf("yesterday midnight-match dim: %+v (ok=%v)", d, ok)
	}
	// midnight-match must NOT appear at all on today (only slot-relevant
	// dims of matches overlapping today are kept, and its start day isn't
	// today) -- checked implicitly below by the today-match/running-match
	// dim count; explicit negative check:
	if d, ok := findDim(todayDims, today, "yesterday-match"); ok {
		t.Fatalf("yesterday-match must not appear on today: %+v", d)
	}
	// running-match on yesterday: day-filter rule -- slot_seconds only, no
	// matches/players/duration. Its window [today-1,today) is fully in the
	// past relative to "now" (now >= today always), so this is deterministic
	// regardless of what instant the test runs at.
	if d, ok := findDim(yestDims, yesterday, "running-match"); !ok || d.Matches != 0 || d.PlayersPeakSum != 0 ||
		d.DurCount != 0 || d.SlotSeconds != 86400 {
		t.Fatalf("yesterday running-match dim (day-filter rule): %+v (ok=%v)", d, ok)
	}

	// today-match: ordinary same-day attribution.
	if d, ok := findDim(todayDims, today, "today-match"); !ok || d.Matches != 1 || d.PlayersPeakSum != 10 ||
		d.DurSumSeconds != 1800 || d.DurCount != 1 || d.SlotSeconds != 1800 {
		t.Fatalf("today-match dim: %+v (ok=%v)", d, ok)
	}
	// midnight-match on today: only the post-midnight 30 minutes of
	// slot_seconds, zero matches/players/duration (attributed to yesterday).
	if d, ok := findDim(todayDims, today, "midnight-match"); !ok || d.Matches != 0 || d.PlayersPeakSum != 0 ||
		d.DurCount != 0 || d.SlotSeconds != 1800 {
		t.Fatalf("today midnight-match dim (day-filter rule): %+v (ok=%v)", d, ok)
	}
	// running-match on today: day-filter rule again, but the slot_seconds
	// value is volatile (grows with real time) -- bound it between
	// before/after-tick snapshots instead of asserting an exact number.
	runToday, ok := findDim(todayDims, today, "running-match")
	if !ok {
		t.Fatalf("running-match dim missing on today: %+v", todayDims)
	}
	if runToday.Matches != 0 || runToday.PlayersPeakSum != 0 || runToday.DurCount != 0 {
		t.Fatalf("today running-match must not carry matches/players/duration (day-filter rule): %+v", runToday)
	}
	minSlot, maxSlot := beforeTick.Sub(today).Seconds(), afterTick.Sub(today).Seconds()
	if runToday.SlotSeconds <= 0 || runToday.SlotSeconds < minSlot-0.01 || runToday.SlotSeconds > maxSlot+0.01 {
		t.Fatalf("today running-match slot_seconds = %v, want in [%v,%v]", runToday.SlotSeconds, minSlot, maxSlot)
	}

	// Peak CCU: yesterday's window is fully elapsed relative to "now", so
	// it's deterministic: running-match(6) alone all day, +4 during
	// yesterday-match's 20min (=10), +8 during midnight-match's last 30min
	// (=14) -> peak 14. Today's peak depends on how far "now" has gotten
	// into the day (whether running-match's clamp-to-now reaches
	// today-match's 2h mark), so only bound it loosely.
	peaksYest, err := st.RollupPeakCCU(ctx, yesterday, yesterday, "")
	if err != nil {
		t.Fatalf("peaks yesterday: %v", err)
	}
	if peaksYest[utctime.DayKey(yesterday)] != 14 {
		t.Fatalf("yesterday peak ccu = %v, want 14", peaksYest[utctime.DayKey(yesterday)])
	}
	peaksToday, err := st.RollupPeakCCU(ctx, today, today, "")
	if err != nil {
		t.Fatalf("peaks today: %v", err)
	}
	if p := peaksToday[utctime.DayKey(today)]; p < 10 || p > 16 {
		t.Fatalf("today peak ccu = %v, want in [10,16]", p)
	}

	// --- idempotency: a second tick must not duplicate rows, and every
	// deterministic (non-running-match) field must be byte-identical.
	if err := job.tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	yestDims2, err := st.RollupDims(ctx, yesterday, yesterday, store.RollupFilter{Env: ""})
	if err != nil {
		t.Fatalf("dims yesterday (2nd tick): %v", err)
	}
	todayDims2, err := st.RollupDims(ctx, today, today, store.RollupFilter{Env: ""})
	if err != nil {
		t.Fatalf("dims today (2nd tick): %v", err)
	}
	if len(yestDims2) != len(yestDims) {
		t.Fatalf("second tick duplicated yesterday rows: want %d, got %d", len(yestDims), len(yestDims2))
	}
	if len(todayDims2) != len(todayDims) {
		t.Fatalf("second tick duplicated today rows: want %d, got %d", len(todayDims), len(todayDims2))
	}
	if !reflectEqualDims(yestDims, yestDims2) {
		t.Fatalf("second tick changed yesterday (should be immutable/idempotent):\nfirst=%+v\nsecond=%+v", yestDims, yestDims2)
	}
	// today: everything but running-match's slot_seconds must be identical;
	// running-match's slot_seconds must not have decreased.
	for _, d1 := range todayDims {
		d2, ok := findDim(todayDims2, today, d1.Region)
		if !ok {
			t.Fatalf("second tick dropped today dim %+v", d1)
		}
		if d1.Region == "running-match" {
			if d2.SlotSeconds < d1.SlotSeconds {
				t.Fatalf("running-match today slot_seconds decreased: %v -> %v", d1.SlotSeconds, d2.SlotSeconds)
			}
			continue
		}
		if !sameDim(d1, d2) {
			t.Fatalf("second tick changed today dim %s: %+v -> %+v", d1.Region, d1, d2)
		}
	}
}

// TestRollupJobBackfillSelfHeals is the Fix-1 regression test for the
// CRITICAL bug: a day inside Backfill's range that already carries a rollup
// row -- however wrong -- must be OVERWRITTEN by the next Backfill, not
// skipped.
//
// This reproduces the real outage scenario: a tick finalizes a day D while D
// is still "yesterday" within its own trailing window; normally that's the
// day's one and only rollup write, since D then ages out of the tick's
// [today-1,today] range forever. But if the tick instead wrote a partial
// snapshot (e.g. a match on D was still running at tick time) and the
// master then goes down for the entirety of the following day, D rolls out
// of the tail and into Backfill's immutable range still carrying that
// partial row -- nothing ever revisits it under the pre-fix "skip days
// already present" logic, silently under-counting D forever. Simulating
// this directly at the store level (rather than via wall-clock timing,
// which Job's Backfill/tick can't be given a fake clock for) makes the
// scenario deterministic: seed D's real matches, then use UpsertRollupDay
// to plant an empty/zero rollup for D exactly as a partial tick snapshot
// would leave behind, and confirm Backfill corrects it.
//
// Before Fix 1 (RolledUpDays-gated skip in Backfill) this fails: D is
// already "present" in match_ccu_daily, so Backfill silently leaves the
// stale empty/zero row in place instead of recomputing it.
func TestRollupJobBackfillSelfHeals(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()
	srv := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 20001, 0)

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	// 10 days ago: comfortably inside [today-29, today-2] regardless of
	// where in the current day "now" falls.
	d := today.AddDate(0, 0, -10)

	// Two finished matches, fully within D (no midnight-spanning, no
	// still-running clamp-to-now involved -- both fully settled, so the
	// reference computed below cannot itself be time-of-test-run-dependent).
	insertJobMatch(t, st, srv, "eu", 10, d.Add(1*time.Hour), tPtr(d.Add(1*time.Hour+30*time.Minute)))
	insertJobMatch(t, st, srv, "eu", 6, d.Add(5*time.Hour), tPtr(d.Add(5*time.Hour+45*time.Minute)))

	// Plant a stale PARTIAL rollup for D: empty dims, zero peak -- exactly
	// what a tick's UpsertRollupDay call would leave behind if it ran while
	// D's matches hadn't happened yet (or, in the real outage, while a
	// match on D was still running and got dropped by the day-filter). This
	// also marks D "present" in match_ccu_daily, which is what the old
	// RolledUpDays-gated skip kept it from ever fixing.
	if err := st.UpsertRollupDay(ctx, d, nil, 0, nil); err != nil {
		t.Fatalf("seed stale partial rollup for d: %v", err)
	}

	job := New(st, time.Minute, testLog())
	if err := job.Backfill(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// Reference: what D's rollup should be, computed directly and
	// independently of the job (mirrors recomputeDay's own recipe, but
	// spelled out here rather than calling it, so this test doesn't just
	// check "did recomputeDay run" via itself).
	matches, err := st.StatMatchesOverlapping(ctx, d, d.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("overlap: %v", err)
	}
	wantDims, wantPeak := stats.AggregateDaily(matches, []time.Time{d}, now)
	wantDim, ok := findDim(wantDims, d, "eu")
	if !ok {
		t.Fatalf("reference computation produced no eu dim on d: %+v", wantDims)
	}

	gotDims, err := st.RollupDims(ctx, d, d, store.RollupFilter{Env: ""})
	if err != nil {
		t.Fatalf("rollup dims: %v", err)
	}
	gotPeaks, err := st.RollupPeakCCU(ctx, d, d, "")
	if err != nil {
		t.Fatalf("rollup peak ccu: %v", err)
	}

	if len(gotDims) != 1 {
		t.Fatalf("backfill did not overwrite the stale partial rollup: want 1 dim on d, got %d: %+v", len(gotDims), gotDims)
	}
	if !sameDim(gotDims[0], wantDim) {
		t.Fatalf("backfilled dim doesn't match the freshly-computed reference (stale row survived):\ngot=%+v\nwant=%+v", gotDims[0], wantDim)
	}
	// Belt-and-suspenders literal check, independent of the reference
	// computation above, so this isn't just testing AggregateDaily against
	// itself: two matches (10+6 players), not the stale empty/zero row.
	if gotDims[0].Matches != 2 || gotDims[0].PlayersPeakSum != 16 {
		t.Fatalf("backfilled dim still looks stale: %+v", gotDims[0])
	}
	if got, want := gotPeaks[utctime.DayKey(d)], wantPeak[utctime.DayKey(d)]; got != want {
		t.Fatalf("backfilled peak ccu = %v, want %v (fresh reference; stale 0 was not overwritten)", got, want)
	}
	if gotPeaks[utctime.DayKey(d)] == 0 {
		t.Fatalf("backfilled peak ccu is still the stale 0 -- day was not recomputed")
	}
}

// TestRollupJobEnvGrouping is the environments-v1 (I5) regression for the
// rollup job: match_stats_daily is grouped by env (source matches.env, I6), so
// two matches on the same day/region/semver but different env produce two
// distinct rollup rows — dev's matches must not fold into prod's slice.
// match_ccu_daily is deliberately left global (PK (day)): the two overlapping
// matches yield one platform-wide peak (dev 10 + prod 5 = 15), never a per-env
// peak.
func TestRollupJobEnvGrouping(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()
	srv := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 20001, 0)

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	// 10 days ago: comfortably inside Backfill's immutable range [today-29,
	// today-2] regardless of the time of day, and fully settled (no clamp-to-now).
	day := today.AddDate(0, 0, -10)

	// Same day/region, distinct env, overlapping in time so the platform-wide
	// peak (15) exceeds either env's own (10 dev / 5 prod).
	insertJobMatchEnv(t, st, srv, "eu", "dev", 10, day.Add(10*time.Hour), tPtr(day.Add(10*time.Hour+30*time.Minute)))
	insertJobMatchEnv(t, st, srv, "eu", "prod", 5, day.Add(10*time.Hour+10*time.Minute), tPtr(day.Add(10*time.Hour+40*time.Minute)))

	if err := New(st, time.Minute, testLog()).Backfill(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// Two rollup rows for the day — one per env — with matches attributed to
	// their own env, not folded together.
	all, err := st.RollupDims(ctx, day, day, store.RollupFilter{Env: ""})
	if err != nil {
		t.Fatalf("rollup dims (all): %v", err)
	}
	byEnv := map[string]store.RollupDim{}
	for _, d := range all {
		byEnv[d.Env] = d
	}
	if len(byEnv) != 2 || byEnv["dev"].Matches != 1 || byEnv["dev"].PlayersPeakSum != 10 ||
		byEnv["prod"].Matches != 1 || byEnv["prod"].PlayersPeakSum != 5 {
		t.Fatalf("env grouping wrong: %+v", all)
	}

	// The env filter isolates one environment's slice.
	prod, err := st.RollupDims(ctx, day, day, store.RollupFilter{Env: "prod"})
	if err != nil {
		t.Fatalf("rollup dims (prod): %v", err)
	}
	if len(prod) != 1 || prod[0].Env != "prod" {
		t.Fatalf("prod slice = %+v, want sole prod dim", prod)
	}

	// match_ccu_daily: one global, platform-wide peak for the day (15), not split.
	peaks, err := st.RollupPeakCCU(ctx, day, day, "")
	if err != nil {
		t.Fatalf("rollup peak ccu: %v", err)
	}
	if len(peaks) != 1 || peaks[utctime.DayKey(day)] != 15 {
		t.Fatalf("peak ccu = %+v, want single global {%s:15}", peaks, utctime.DayKey(day))
	}
}

func tPtr(t time.Time) *time.Time { return &t }

// sameDim compares two dims field-by-field (Day via time.Equal, not the raw
// struct ==, since time.Time's built-in equality is picky about internal
// representation even for the same instant).
func sameDim(a, b store.RollupDim) bool {
	return a.Day.Equal(b.Day) && a.Region == b.Region && a.Semver == b.Semver &&
		a.Matches == b.Matches && a.PlayersPeakSum == b.PlayersPeakSum &&
		a.DurSumSeconds == b.DurSumSeconds && a.DurCount == b.DurCount && a.SlotSeconds == b.SlotSeconds
}

// reflectEqualDims compares two dim slices field-by-field, order-independent
// by (day,region) -- used where every field (incl. the volatile-elsewhere
// slot_seconds) is expected to be byte-identical (yesterday is fully
// elapsed, so nothing there should change between ticks).
func reflectEqualDims(a, b []store.RollupDim) bool {
	if len(a) != len(b) {
		return false
	}
	for _, da := range a {
		db, ok := findDim(b, da.Day, da.Region)
		if !ok || !sameDim(da, db) {
			return false
		}
	}
	return true
}

// Роллап разделяет матчи РАЗНЫХ ПРОЕКТОВ по своим строкам (мультипроект W3),
// а пик CCU получает проектные срезы РЯДОМ с платформенным. Симметрично
// TestBackfillSplitsRollupByEnv выше, но по второму измерению — и с той
// разницей, что по проекту делится ещё и пик: окружения делят одну ёмкость
// флота, а проекты — непересекающиеся тенанты.
func TestBackfillSplitsRollupByProject(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // проект "game"
	ctx := context.Background()
	gameSrv := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 20101, 0)

	// Второй проект со своей нодой, версией и дедиком.
	arenaNode, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "arena", Region: "eu", Hostname: "arena-1",
		PublicIP: "203.0.113.77", CapacitySlots: 10,
	})
	if err != nil {
		t.Fatalf("arena node: %v", err)
	}
	arenaVer, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "arena", Semver: "9.9.9", ImageRef: "ghcr.io/example/arena:9.9.9", Env: "dev",
	})
	if err != nil {
		t.Fatalf("arena version: %v", err)
	}
	arenaSrv := f.InsertServer(t, arenaNode.ID, arenaVer.ID, "reaped", 20102, 0)

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	day := today.AddDate(0, 0, -10) // внутри иммутабельного окна Backfill

	// Пересекаются во времени, поэтому платформенный пик (12) больше любого
	// проектного (7 у game, 5 у arena) — на этом и видно, что пик не аддитивен
	// и что проектные срезы считаются отдельно, а не делением платформенного.
	insertJobMatchEnv(t, st, gameSrv, "eu", "dev", 7, day.Add(10*time.Hour), tPtr(day.Add(10*time.Hour+30*time.Minute)))
	insertJobMatchEnv(t, st, arenaSrv, "eu", "dev", 5, day.Add(10*time.Hour+10*time.Minute), tPtr(day.Add(10*time.Hour+40*time.Minute)))

	if err := New(st, time.Minute, testLog()).Backfill(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	all, err := st.RollupDims(ctx, day, day, store.RollupFilter{})
	if err != nil {
		t.Fatalf("rollup dims (all): %v", err)
	}
	byProject := map[string]store.RollupDim{}
	for _, d := range all {
		byProject[d.Project] = d
	}
	if len(byProject) != 2 || byProject["game"].Matches != 1 || byProject["game"].PlayersPeakSum != 7 ||
		byProject["arena"].Matches != 1 || byProject["arena"].PlayersPeakSum != 5 {
		t.Fatalf("проектная группировка сломана: %+v", all)
	}
	// Ни одна строка не осталась неатрибутированной: джоба знает проект каждого
	// матча, '' бывает только у исторических строк из бэкфилла миграции.
	if _, ok := byProject[""]; ok {
		t.Fatalf("джоба не должна писать неатрибутированные строки: %+v", all)
	}

	arenaDims, err := st.RollupDims(ctx, day, day, store.RollupFilter{Project: "arena"})
	if err != nil {
		t.Fatalf("rollup dims (arena): %v", err)
	}
	if len(arenaDims) != 1 || arenaDims[0].Project != "arena" {
		t.Fatalf("фильтр по проекту = %+v, want единственный dim arena", arenaDims)
	}

	dk := utctime.DayKey(day)
	platform, err := st.RollupPeakCCU(ctx, day, day, "")
	if err != nil {
		t.Fatalf("peak platform: %v", err)
	}
	if platform[dk] != 12 {
		t.Fatalf("платформенный пик = %v, want 12", platform)
	}
	gamePeak, err := st.RollupPeakCCU(ctx, day, day, "game")
	if err != nil {
		t.Fatalf("peak game: %v", err)
	}
	arenaPeak, err := st.RollupPeakCCU(ctx, day, day, "arena")
	if err != nil {
		t.Fatalf("peak arena: %v", err)
	}
	if gamePeak[dk] != 7 || arenaPeak[dk] != 5 {
		t.Fatalf("проектные пики = game %v / arena %v, want 7 и 5", gamePeak, arenaPeak)
	}
}
