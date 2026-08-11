package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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
