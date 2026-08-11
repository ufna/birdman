package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

// TestNarrowScopeFailsClosed — единственная ветка, оставшаяся от глухого гейта
// #990: пара привязки, которую нельзя безопасно вклеить в фильтр запроса,
// закрывает проксию, а не сужает её «как получится».
//
// Тест внутренний (package httpapi), потому что через БД такую пару не создать:
// схема держит слаг проекта и имя окружения тем же алфавитом. Это и есть смысл
// проверки — она про то, что случится, если инвариант БД когда-нибудь ослабнет
// (ряд старше CHECK'а, расширение алфавита, ручная правка): фильтр
// `{project="a"} or ...` или `extra_label=project=a,env=b` не должен родиться
// НИКОГДА, лучше 403.
func TestNarrowScopeFailsClosed(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name       string
		project    string
		env        *string
		wantNarrow bool
	}{
		{"обычная пара", "game", ptr("dev"), true},
		{"кавычка в проекте", `a" or project="b`, ptr("dev"), false},
		{"фигурная скобка в окружении", "game", ptr(`dev"},{project="b`), false},
		{"запятая в окружении", "game", ptr("dev,prod"), false},
		{"знак равенства", "game", ptr("dev=prod"), false},
		{"пустое окружение (полупара)", "game", ptr(""), false},
		{"env=nil (полупара по схеме)", "game", nil, false},
		{"верхний регистр", "Game", ptr("dev"), false},
		{"слеш (путь)", "game/../other", ptr("dev"), false},
		{"пробел", "game", ptr("dev prod"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := store.APIKey{Project: &c.project, Env: c.env}
			r := httptest.NewRequest(http.MethodGet, "/v1/logs/query?query=*", nil)
			r = r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey, key))
			w := httptest.NewRecorder()
			project, env, narrow, ok := s.narrowScope(w, r)
			if c.wantNarrow {
				if !ok || !narrow {
					t.Fatalf("ok=%v narrow=%v, want true/true (%d %s)", ok, narrow, w.Code, w.Body)
				}
				if project != c.project || env != *c.env {
					t.Fatalf("пара = %q/%q, want %q/%q", project, env, c.project, *c.env)
				}
				return
			}
			if ok {
				t.Fatalf("непригодная пара пропущена в фильтр: narrow=%v project=%q env=%q", narrow, project, env)
			}
			if w.Code != http.StatusForbidden {
				t.Fatalf("код = %d, want 403 (fail-closed)", w.Code)
			}
		})
	}
}

// TestNarrowScopeGlobalKeyPasses: глобальный/admin-ключ сужать не по чему —
// passthrough остаётся passthrough.
func TestNarrowScopeGlobalKeyPasses(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest(http.MethodGet, "/v1/logs/query?query=*", nil)
	r = r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey, store.APIKey{}))
	w := httptest.NewRecorder()
	_, _, narrow, ok := s.narrowScope(w, r)
	if !ok || narrow {
		t.Fatalf("ok=%v narrow=%v, want true/false", ok, narrow)
	}
}

// TestNarrowedQueriesDropClientFilterKnobs — сборка запроса к апстриму: белый
// список плюс НАШ фильтр, всё остальное отброшено. Проверяется на уровне
// функций, а не только через HTTP, потому что мутация «дописать фильтр в
// пришедшие url.Values вместо пересборки» иначе видна лишь по одному
// параметру из трёх.
func TestNarrowedQueriesDropClientFilterKnobs(t *testing.T) {
	in := url.Values{
		"query":                {`{server_id="s1"} | sort by (_time) desc`},
		"start":                {"0"},
		"end":                  {"10"},
		"limit":                {"1000"},
		"extra_stream_filters": {`{project="game"}`},
		"extra_filters":        {`{project="game"}`},
		"extra_label":          {"project=game"},
		"unknown":              {"x"},
	}
	got := narrowedLogsQuery(in, "neighbour", "dev")
	if v := got.Get("extra_stream_filters"); v != `{project="neighbour",env="dev"}` {
		t.Fatalf("extra_stream_filters = %q", v)
	}
	if len(got["extra_stream_filters"]) != 1 {
		t.Fatalf("extra_stream_filters пришёл в двух экземплярах: %q", got["extra_stream_filters"])
	}
	for _, k := range []string{"extra_filters", "extra_label", "unknown"} {
		if _, ok := got[k]; ok {
			t.Fatalf("параметр %q не отброшен: %q", k, got[k])
		}
	}
	if got.Get("query") != in.Get("query") {
		t.Fatalf("query переписан: %q", got.Get("query"))
	}

	inM := url.Values{
		"query":           {"up"},
		"step":            {"15"},
		"extra_label":     {"project=game"},
		"extra_filters[]": {`{project="game"}`},
		"nocache":         {"1"},
	}
	gotM := narrowedMetricsQuery(inM, "neighbour", "dev")
	if want := []string{"project=neighbour", "env=dev"}; len(gotM["extra_label"]) != 2 ||
		gotM["extra_label"][0] != want[0] || gotM["extra_label"][1] != want[1] {
		t.Fatalf("extra_label = %q, want %q", gotM["extra_label"], want)
	}
	for _, k := range []string{"extra_filters[]", "nocache"} {
		if _, ok := gotM[k]; ok {
			t.Fatalf("параметр %q не отброшен: %q", k, gotM[k])
		}
	}
	if gotM.Get("query") != "up" || gotM.Get("step") != "15" {
		t.Fatalf("белый список потерял параметры: %v", gotM)
	}
}

func ptr[T any](v T) *T { return &v }

// TestNarrowProbeVerdictExpires (tracker #1007): вердикт живёт не дольше своего
// TTL, и по истечении проба идёт в апстрим ЗАНОВО. Без этой проверки мутация
// «TTL = 100 часов» остаётся зелёной на всей батарее, а обещание «апстрим,
// подменённый под работающим master'ом, обслуживается по старому вердикту не
// дольше TTL» — ничем не подкреплённым.
//
// Тест внутренний и БЕЗ sleep: протухание имитируется сдвигом p.exp назад.
// Спать на настоящем TTL значило бы менять честность теста на его флакость.
func TestNarrowProbeVerdictExpires(t *testing.T) {
	// Потолок на сам TTL, а не только на его применение: пока вердикт не протух,
	// подменённый апстрим обслуживается по СТАРОМУ вердикту, то есть дыра #990
	// открыта заново и молча. Окно измеряется минутами; часами — уже не гарантия.
	if narrowProbeOKTTL > 15*time.Minute {
		t.Fatalf("окно жизни устаревшего вердикта = %s: столько времени подмена апстрима заново открывает дыру #990 молча", narrowProbeOKTTL)
	}
	if narrowProbeFailTTL > narrowProbeOKTTL {
		t.Fatalf("отказной TTL (%s) длиннее успешного (%s) — починенный апстрим ждал бы дольше сломанного",
			narrowProbeFailTTL, narrowProbeOKTTL)
	}

	var parses atomic.Bool
	parses.Store(true)
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Пока parses=true — ведёт себя как настоящий VL: кривое значение
		// знакомой ручки отвергает. Потом «апстрим подменили»: то же значение
		// проглатывается молча.
		if parses.Load() && r.URL.Query().Get("extra_stream_filters") == "{project=}" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	p := newLogsNarrowProbe()
	if v, _ := p.check(up.URL, nil); v != narrowHonoured {
		t.Fatalf("вердикт = %v, want narrowHonoured", v)
	}
	if got := time.Until(p.exp); got > narrowProbeOKTTL || got < narrowProbeOKTTL-time.Minute {
		t.Fatalf("срок годности вердикта = %s, want ≈%s (okTTL не применён)", got, narrowProbeOKTTL)
	}
	afterFirst := hits.Load()
	if afterFirst != 2 {
		t.Fatalf("запросов пробы = %d, want 2 (канарейка + контроль)", afterFirst)
	}

	// Внутри TTL апстрим не трогается вовсе — иначе «кешируется» было бы словом.
	parses.Store(false)
	if v, _ := p.check(up.URL, nil); v != narrowHonoured || hits.Load() != afterFirst {
		t.Fatalf("внутри TTL: вердикт = %v, запросов = %d (want narrowHonoured, %d)", v, hits.Load(), afterFirst)
	}

	// Вердикт протух — апстрим опрашивается заново, и подмена замечена.
	p.exp = time.Now().Add(-time.Second)
	v, detail := p.check(up.URL, nil)
	if v != narrowIgnored {
		t.Fatalf("после протухания: вердикт = %v, want narrowIgnored — подмена апстрима не замечена", v)
	}
	if hits.Load() <= afterFirst {
		t.Fatalf("после протухания проба в апстрим не ходила (запросов %d)", hits.Load())
	}
	if !strings.Contains(detail, "extra_stream_filters") {
		t.Fatalf("причина отказа не называет ручку: %q", detail)
	}
	// Отказной вердикт живёт КОРОЧЕ, чтобы починенный апстрим не ждал 5 минут.
	if got := time.Until(p.exp); got > narrowProbeFailTTL {
		t.Fatalf("срок годности отказа = %s, want ≤%s", got, narrowProbeFailTTL)
	}
}
