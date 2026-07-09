package statsrollup

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
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

func dayKeyOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

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

	backfillFrom, backfillTo := today.AddDate(0, 0, -29), today.AddDate(0, 0, -2)
	rolledBackfill, err := st.RolledUpDays(ctx, backfillFrom, backfillTo)
	if err != nil {
		t.Fatalf("rolled up days (backfill range): %v", err)
	}
	if len(rolledBackfill) != 28 {
		t.Fatalf("backfill: want 28 rolled-up days in [today-29,today-2], got %d: %+v", len(rolledBackfill), rolledBackfill)
	}
	rolledTail, err := st.RolledUpDays(ctx, yesterday, today)
	if err != nil {
		t.Fatalf("rolled up days (tail range): %v", err)
	}
	if len(rolledTail) != 0 {
		t.Fatalf("backfill must not touch [today-1,today], got: %+v", rolledTail)
	}

	// fiveDaysAgo is the long-running match's true start day: Backfill
	// attributes its matches/players there in full (deterministic — the
	// match's clamp-to-now end is always past fiveDaysAgo+1day by 5 days).
	fdDims, err := st.RollupDims(ctx, fiveDaysAgo, fiveDaysAgo)
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
	fdPeaks, err := st.RollupPeakCCU(ctx, fiveDaysAgo, fiveDaysAgo)
	if err != nil {
		t.Fatalf("peaks fiveDaysAgo: %v", err)
	}
	if fdPeaks[dayKeyOf(fiveDaysAgo)] != 6 {
		t.Fatalf("fiveDaysAgo peak ccu = %v, want 6", fdPeaks[dayKeyOf(fiveDaysAgo)])
	}

	// --- tick: recompute the trailing two days [yesterday, today].
	beforeTick := time.Now().UTC()
	if err := job.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	afterTick := time.Now().UTC()

	yestDims, err := st.RollupDims(ctx, yesterday, yesterday)
	if err != nil {
		t.Fatalf("dims yesterday: %v", err)
	}
	todayDims, err := st.RollupDims(ctx, today, today)
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
	peaksYest, err := st.RollupPeakCCU(ctx, yesterday, yesterday)
	if err != nil {
		t.Fatalf("peaks yesterday: %v", err)
	}
	if peaksYest[dayKeyOf(yesterday)] != 14 {
		t.Fatalf("yesterday peak ccu = %v, want 14", peaksYest[dayKeyOf(yesterday)])
	}
	peaksToday, err := st.RollupPeakCCU(ctx, today, today)
	if err != nil {
		t.Fatalf("peaks today: %v", err)
	}
	if p := peaksToday[dayKeyOf(today)]; p < 10 || p > 16 {
		t.Fatalf("today peak ccu = %v, want in [10,16]", p)
	}

	// --- idempotency: a second tick must not duplicate rows, and every
	// deterministic (non-running-match) field must be byte-identical.
	if err := job.tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	yestDims2, err := st.RollupDims(ctx, yesterday, yesterday)
	if err != nil {
		t.Fatalf("dims yesterday (2nd tick): %v", err)
	}
	todayDims2, err := st.RollupDims(ctx, today, today)
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
