package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
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

	got := parseAlertsLog(data, now, 100, alertVisibility{})
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
	if lim := parseAlertsLog(data, now, 1, alertVisibility{}); len(lim) != 1 || lim[0].Name != "DiskHigh" {
		t.Fatalf("limit=1: %+v", lim)
	}
	// Empty input → [] (never nil).
	if empty := parseAlertsLog(nil, now, 100, alertVisibility{}); empty == nil || len(empty) != 0 {
		t.Fatalf("empty input should give []: %+v", empty)
	}
	// No project labels anywhere in this log → every alert is platform-scoped,
	// so even an explicit ?project= keeps all three (non-hiding filter).
	for _, a := range got {
		if a.Project != "" || a.Scope != alertScopePlatform {
			t.Fatalf("unlabelled alert must be platform-scoped: %+v", a)
		}
	}
	if kept := parseAlertsLog(data, now, 100, alertVisibility{project: "alpha"}); len(kept) != 3 {
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
	got := parseAlertsLog(data, now, 2, alertVisibility{project: "alpha"})
	if len(got) != 2 {
		t.Fatalf("want 2 alpha-visible alerts within limit=2, got %d: %+v", len(got), got)
	}
	for _, a := range got {
		if a.Project == "beta" {
			t.Fatalf("beta alert leaked into ?project=alpha: %+v", a)
		}
	}
	// Unlimited: 2 alpha + 1 platform, никакой beta.
	all := parseAlertsLog(data, now, 100, alertVisibility{project: "alpha"})
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

// Правило привязки (tracker #995) — рядом с не-скрывающим, потому что разница
// между ними и есть суть карточки: пустой `want` у одного значит «не фильтровать
// вовсе», у другого — «только платформенные». Первая строка каждой таблицы — та
// самая, из-за которой признак `bound` хранится ЯВНО, а не выводится из
// непустоты проекта.
func TestAlertVisibility(t *testing.T) {
	cases := []struct {
		name  string
		vis   alertVisibility
		alert string
		keep  bool
	}{
		{"непривязанный без фильтра видит чужой", alertVisibility{}, "game", true},
		{"привязанный с пустой привязкой не видит ничей", alertVisibility{bound: true}, "game", false},
		{"привязанный видит платформенный", alertVisibility{project: "neighbour", bound: true}, "", true},
		{"привязанный видит свой", alertVisibility{project: "neighbour", bound: true}, "neighbour", true},
		{"привязанный не видит чужой", alertVisibility{project: "neighbour", bound: true}, "game", false},
		{"непривязанный с фильтром видит платформенный", alertVisibility{project: "neighbour"}, "", true},
		{"непривязанный с фильтром не видит чужой", alertVisibility{project: "neighbour"}, "game", false},
	}
	for _, c := range cases {
		if got := c.vis.keep(c.alert); got != c.keep {
			t.Fatalf("%s: keep(%q) при %+v = %v, want %v", c.name, c.alert, c.vis, got, c.keep)
		}
	}
	// keepForBinding — то же правило, отдельно: его вторым вызывающим является
	// листинг mute'ов, у которого своего теста-таблицы нет.
	for _, c := range []struct {
		obj, binding string
		keep         bool
	}{
		{"", "neighbour", true},
		{"neighbour", "neighbour", true},
		{"game", "neighbour", false},
		{"game", "", false},
	} {
		if got := keepForBinding(c.obj, c.binding); got != c.keep {
			t.Fatalf("keepForBinding(%q,%q) = %v, want %v", c.obj, c.binding, got, c.keep)
		}
	}
}

// ПРОВОДКА признака привязки — отдельным тестом, потому что e2e её не видит:
// при непустой привязке не-скрывающее правило и правило границы дают
// одинаковый ответ на всех достижимых входах, поэтому мутация «bound: false» в
// конструкторе оставалась ЗЕЛЁНОЙ на всём пакете (найдено вторым проходом).
// Отсюда и этот тест: он смотрит на то, ЧТО построено, а не только на то, что
// в итоге выдано, — иначе защита от будущего смягчения #955 не заперта ничем.
//
// БД тут не нужна и это часть проверки: для привязанного ключа tenantScope
// обязан отвечать, не сходив в стор (правило #993 «гейт первым»), поэтому
// пустой &Server{} — достаточный сервер, и падение с nil-паникой означало бы,
// что гейт полез в БД.
func TestAlertVisibilityWiring(t *testing.T) {
	req := func(query string, key *store.APIKey) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/alerts/active"+query, nil)
		if key != nil {
			r = r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey, *key))
		}
		return r
	}
	project, env := "neighbour", "dev"
	bound := &store.APIKey{Project: &project, Env: &env}

	s := &Server{}
	// Привязанный ключ: правило построено СКРЫВАЮЩЕЕ и по его паре.
	vis, ok := s.alertVisibility(httptest.NewRecorder(), req("", bound))
	if !ok || !vis.bound || vis.project != "neighbour" {
		t.Fatalf("привязанный ключ: vis=%+v ok=%v, want {neighbour true}", vis, ok)
	}
	// Свой слаг в параметре ничего не меняет.
	if vis, ok := s.alertVisibility(httptest.NewRecorder(), req("?project=neighbour", bound)); !ok ||
		!vis.bound || vis.project != "neighbour" {
		t.Fatalf("привязанный ключ со своим ?project=: vis=%+v ok=%v", vis, ok)
	}
	// Чужой слаг: отказ пишется ЗДЕСЬ (ok=false), до апстрима и до БД.
	w := httptest.NewRecorder()
	if _, ok := s.alertVisibility(w, req("?project=game", bound)); ok {
		t.Fatal("привязанный ключ с чужим ?project=: want отказ, got продолжение")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("чужой ?project= привязанному ключу: want 403, got %d (%s)", w.Code, w.Body)
	}
	// `?env=` алерты не читают: параметр не влияет ни на выдачу, ни на отказ —
	// поэтому чужой env даёт то же самое правило, а не 403 (иначе мы отвечали бы
	// по-разному на запросы с одинаковым результатом, #993).
	if vis, ok := s.alertVisibility(httptest.NewRecorder(), req("?env=prod", bound)); !ok ||
		!vis.bound || vis.project != "neighbour" {
		t.Fatalf("привязанный ключ с ?env=: vis=%+v ok=%v", vis, ok)
	}
	// Непривязанный ключ: правило НЕ скрывающее (bound=false), пустой параметр
	// не сужает ничего.
	if vis, ok := s.alertVisibility(httptest.NewRecorder(), req("", nil)); !ok ||
		vis.bound || vis.project != "" {
		t.Fatalf("непривязанный ключ: vis=%+v ok=%v, want {\"\" false}", vis, ok)
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
