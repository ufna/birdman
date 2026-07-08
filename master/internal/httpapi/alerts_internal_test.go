package httpapi

import (
	"strings"
	"testing"
	"time"
)

func TestParseAlertsLog(t *testing.T) {
	now := ts("2026-07-08T12:00:00Z")
	lines := []string{
		// batch delivery: one firing (zero endsAt), one resolved.
		`{"received_at":"2026-07-08T09:00:00Z","alerts":[` +
			`{"status":"firing","labels":{"alertname":"NodeDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"нода недоступна"},"startsAt":"2026-07-08T08:59:00Z","endsAt":"0001-01-01T00:00:00Z"},` +
			`{"status":"resolved","labels":{"alertname":"CrashLoop","severity":"critical","node":"n2","region":"us"},"annotations":{"description":"краш-луп"},"startsAt":"2026-07-08T09:30:00Z","endsAt":"2026-07-08T09:45:00Z"}]}`,
		// bare single-alert object; node falls back to instance; summary annotation.
		`{"status":"firing","labels":{"alertname":"DiskHigh","severity":"warning","instance":"host3","region":"eu"},"annotations":{"summary":"диск"},"startsAt":"2026-07-08T11:00:00Z","endsAt":""}`,
		`{bad json`, // skipped
		``,          // skipped
		`   `,       // whitespace only, skipped
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	got := parseAlertsLog(data, now, 100)
	if len(got) != 3 {
		t.Fatalf("want 3 alerts, got %d: %+v", len(got), got)
	}
	// Newest by delivery time first. DiskHigh has no received_at → falls back to
	// its startsAt (11:00), which is newer than the 09:00 batch delivery.
	if got[0].Name != "DiskHigh" {
		t.Fatalf("newest should be DiskHigh, got %s", got[0].Name)
	}
	byName := map[string]alertEvent{}
	for _, a := range got {
		byName[a.Name] = a
	}
	disk := byName["DiskHigh"]
	if !disk.Active || disk.Node != "host3" || disk.Description != "диск" || disk.Severity != "warning" {
		t.Fatalf("disk (bare line, node from instance, summary annotation): %+v", disk)
	}
	crash := byName["CrashLoop"]
	if crash.Active || crash.Node != "n2" { // resolved → inactive
		t.Fatalf("crashloop (resolved should be inactive): %+v", crash)
	}
	nd := byName["NodeDown"]
	if !nd.Active || nd.Node != "n1" || nd.Region != "eu" || nd.Description != "нода недоступна" {
		t.Fatalf("nodedown: %+v", nd)
	}
	if nd.ReceivedAt != "2026-07-08T09:00:00Z" { // batch received_at propagated
		t.Fatalf("received_at propagation: %+v", nd)
	}

	// limit caps to the newest N; DiskHigh (newest) is always kept.
	if lim := parseAlertsLog(data, now, 1); len(lim) != 1 || lim[0].Name != "DiskHigh" {
		t.Fatalf("limit=1: %+v", lim)
	}
	// Empty input → [] (never nil).
	if empty := parseAlertsLog(nil, now, 100); empty == nil || len(empty) != 0 {
		t.Fatalf("empty input should give []: %+v", empty)
	}
}

func TestAlertActive(t *testing.T) {
	now := ts("2026-07-08T12:00:00Z")
	cases := []struct {
		status, endsAt string
		want           bool
	}{
		{"firing", "", true},
		{"firing", "0001-01-01T00:00:00Z", true},
		{"firing", "2026-07-08T13:00:00Z", true},  // future end
		{"firing", "2026-07-08T11:00:00Z", false}, // past end
		{"resolved", "", false},
		{"resolved", "2026-07-08T13:00:00Z", false},
		{"", "garbage", false}, // unparseable endsAt, no firing status → inactive
	}
	for _, c := range cases {
		if got := alertActive(c.status, c.endsAt, now); got != c.want {
			t.Fatalf("alertActive(%q,%q) = %v, want %v", c.status, c.endsAt, got, c.want)
		}
	}
}

func TestDurationSeconds(t *testing.T) {
	if got := durationSeconds(0); got != "0s" {
		t.Fatalf("0 → %q", got)
	}
	if got := durationSeconds(180); got != "3m0s" {
		t.Fatalf("180 → %q", got)
	}
	if got := durationSeconds(90); got != "1m30s" {
		t.Fatalf("90 → %q", got)
	}
}

func TestAlertActiveZeroYearGuard(t *testing.T) {
	// A concrete far-future time is active; the Go zero time is not misread.
	if !alertActive("firing", time.Now().Add(time.Hour).UTC().Format(time.RFC3339), ts("2026-07-08T12:00:00Z")) {
		t.Fatal("future end should be active")
	}
}
