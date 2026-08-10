package httpapi

import (
	"strings"
	"testing"
	"time"
)

// ts parses an RFC3339 UTC timestamp, panicking on a malformed literal — a
// test-only convenience so cases can write times as plain strings.
func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestParseAlertsLog(t *testing.T) {
	now := ts("2026-07-08T12:00:00Z")
	lines := []string{
		// batch delivery: one firing (zero endsAt), one resolved.
		`{"received_at":"2026-07-08T09:00:00Z","alerts":[` +
			`{"status":"firing","labels":{"alertname":"NodeDown","severity":"critical","node":"n1","region":"eu"},"annotations":{"description":"node is unreachable","description_ru":"нода недоступна"},"startsAt":"2026-07-08T08:59:00Z","endsAt":"0001-01-01T00:00:00Z"},` +
			`{"status":"resolved","labels":{"alertname":"CrashLoop","severity":"critical","node":"n2","region":"us"},"annotations":{"description":"краш-луп"},"startsAt":"2026-07-08T09:30:00Z","endsAt":"2026-07-08T09:45:00Z"}]}`,
		// bare single-alert object; node falls back to instance; summary annotation.
		`{"status":"firing","labels":{"alertname":"DiskHigh","severity":"warning","instance":"host3","region":"eu"},"annotations":{"summary":"диск"},"startsAt":"2026-07-08T11:00:00Z","endsAt":""}`,
		`{bad json`, // skipped
		``,          // skipped
		`   `,       // whitespace only, skipped
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	got := parseAlertsLog(data, now, 100, "")
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
	// No description_ru annotation → empty (panel falls back to Description).
	if disk.DescriptionRu != "" {
		t.Fatalf("disk should have no description_ru: %+v", disk)
	}
	crash := byName["CrashLoop"]
	if crash.Active || crash.Node != "n2" { // resolved → inactive
		t.Fatalf("crashloop (resolved should be inactive): %+v", crash)
	}
	nd := byName["NodeDown"]
	if !nd.Active || nd.Node != "n1" || nd.Region != "eu" || nd.Description != "node is unreachable" {
		t.Fatalf("nodedown: %+v", nd)
	}
	// Bilingual: description_ru carries the RU text alongside the EN description.
	if nd.DescriptionRu != "нода недоступна" {
		t.Fatalf("nodedown description_ru: %+v", nd)
	}
	if nd.ReceivedAt != "2026-07-08T09:00:00Z" { // batch received_at propagated
		t.Fatalf("received_at propagation: %+v", nd)
	}

	// limit caps to the newest N; DiskHigh (newest) is always kept.
	if lim := parseAlertsLog(data, now, 1, ""); len(lim) != 1 || lim[0].Name != "DiskHigh" {
		t.Fatalf("limit=1: %+v", lim)
	}
	// Empty input → [] (never nil).
	if empty := parseAlertsLog(nil, now, 100, ""); empty == nil || len(empty) != 0 {
		t.Fatalf("empty input should give []: %+v", empty)
	}
	// No project labels anywhere in this log → every alert is platform-scoped,
	// so even an explicit ?project= keeps all three (non-hiding filter).
	for _, a := range got {
		if a.Project != "" || a.Scope != alertScopePlatform {
			t.Fatalf("unlabelled alert must be platform-scoped: %+v", a)
		}
	}
	if kept := parseAlertsLog(data, now, 100, "alpha"); len(kept) != 3 {
		t.Fatalf("platform alerts must survive ?project=alpha, got %d: %+v", len(kept), kept)
	}
}

// TestParseAlertsLogProjectFilterBeforeLimit: the ?project= filter is applied
// while reading, BEFORE the limit cap. Otherwise a busy neighbour project would
// eat the whole budget and ?limit=2&project=alpha would come back near-empty
// even though alpha has plenty of history.
func TestParseAlertsLogProjectFilterBeforeLimit(t *testing.T) {
	now := ts("2026-07-08T12:00:00Z")
	line := func(recv, name, project string) string {
		labels := `"alertname":"` + name + `","severity":"warning"`
		if project != "" {
			labels += `,"project":"` + project + `"`
		}
		return `{"received_at":"` + recv + `","alerts":[{"status":"firing","labels":{` + labels +
			`},"annotations":{"description":"x"},"startsAt":"` + recv + `","endsAt":""}]}`
	}
	// Newest first: three beta alerts, then two alpha, then one platform.
	data := []byte(strings.Join([]string{
		line("2026-07-08T11:00:00Z", "BufferEmpty", "beta"),
		line("2026-07-08T10:50:00Z", "BufferEmpty", "beta"),
		line("2026-07-08T10:40:00Z", "AllocationFailures", "beta"),
		line("2026-07-08T10:30:00Z", "BufferEmpty", "alpha"),
		line("2026-07-08T10:20:00Z", "AllocationFailures", "alpha"),
		line("2026-07-08T10:10:00Z", "NodeDown", ""),
	}, "\n") + "\n")

	// Filtering AFTER the cap would return 0 here (the newest 2 are both beta).
	got := parseAlertsLog(data, now, 2, "alpha")
	if len(got) != 2 {
		t.Fatalf("want 2 alpha-visible alerts within limit=2, got %d: %+v", len(got), got)
	}
	for _, a := range got {
		if a.Project == "beta" {
			t.Fatalf("beta alert leaked into ?project=alpha: %+v", a)
		}
	}
	// Unlimited: 2 alpha + 1 platform, никакой beta.
	all := parseAlertsLog(data, now, 100, "alpha")
	if len(all) != 3 {
		t.Fatalf("want alpha(2)+platform(1)=3, got %d: %+v", len(all), all)
	}
	var platform, project int
	for _, a := range all {
		switch a.Scope {
		case alertScopePlatform:
			platform++
		case alertScopeProject:
			project++
		default:
			t.Fatalf("unknown scope %q: %+v", a.Scope, a)
		}
	}
	if platform != 1 || project != 2 {
		t.Fatalf("scope split: platform=%d project=%d (%+v)", platform, project, all)
	}
}

func TestKeepAlertForProject(t *testing.T) {
	cases := []struct {
		alert, want string
		keep        bool
	}{
		{"alpha", "", true},      // no filter → everything
		{"", "", true},           //
		{"alpha", "alpha", true}, // own project
		{"beta", "alpha", false}, // explicitly someone else's → the ONLY drop
		{"", "alpha", true},      // platform alert stays visible under any selection
	}
	for _, c := range cases {
		if got := keepAlertForProject(c.alert, c.want); got != c.keep {
			t.Fatalf("keepAlertForProject(%q,%q) = %v, want %v", c.alert, c.want, got, c.keep)
		}
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
