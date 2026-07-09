package utctime

import (
	"testing"
	"time"
)

func TestStartOfDay(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"UTC midday truncates down", time.Date(2026, 7, 8, 12, 34, 56, 789, time.UTC), time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)},
		{"UTC midnight is a fixed point", time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)},
		// 01:30 MSK on the 9th is 22:30 UTC on the 8th: the UTC date wins.
		{"non-UTC input uses the UTC date, not the local one", time.Date(2026, 7, 9, 1, 30, 0, 0, msk), time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got := StartOfDay(c.in)
		if !got.Equal(c.want) || got.Location() != time.UTC {
			t.Fatalf("%s: StartOfDay(%v) = %v (%v), want %v UTC", c.name, c.in, got, got.Location(), c.want)
		}
	}
}

func TestDayKey(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	if got := DayKey(time.Date(2026, 7, 8, 23, 59, 59, 0, time.UTC)); got != "2026-07-08" {
		t.Fatalf("DayKey = %q, want 2026-07-08", got)
	}
	// 01:30 MSK on the 9th is still the 8th in UTC.
	if got := DayKey(time.Date(2026, 7, 9, 1, 30, 0, 0, msk)); got != "2026-07-08" {
		t.Fatalf("DayKey(non-UTC) = %q, want 2026-07-08", got)
	}
	// The key of a truncated day equals the key of the instant itself — the
	// cross-package invariant every day-bucketed map lookup relies on.
	now := time.Date(2026, 2, 1, 5, 0, 0, 0, time.UTC)
	if DayKey(StartOfDay(now)) != DayKey(now) {
		t.Fatalf("DayKey(StartOfDay(t)) != DayKey(t) for %v", now)
	}
}
