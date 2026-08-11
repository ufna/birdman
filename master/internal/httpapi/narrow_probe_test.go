package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Канарейка апстрима (tracker #1007). Сужение запроса привязанного ключа (#994)
// исполняет АПСТРИМ: master лишь дописывает `extra_stream_filters` (VL) и
// `extra_label` (VM). Апстрим, который такой ручки не знает, не отвечает
// ошибкой — он молча её игнорирует и отдаёт `200` со всем флотом. Здесь
// проверяется гейт: master сперва УБЕЖДАЕТСЯ, что ручка на том конце
// разбирается, и не убедившись — ОТКАЗЫВАЕТ, а не отдаёт чужое.
//
// Что этот файл доказать НЕ может: что настоящие VL/VM ведут себя на канарейке
// так, как здесь смоделировано. Это доказывает TestLiveUpstreamNarrowingProbe
// на живых апстримах — включая случай «настоящий VictoriaLogs за прокси,
// срезающим ручку».

// narrowAwareUpstream оборачивает фейковый апстрим так, чтобы он отвечал на
// канарейку как НАСТОЯЩИЙ VL/VM: кривое значение ЗНАКОМОЙ ручки — 4xx (замерено
// на живых: VL `extra_stream_filters={project=}` → 400, VM `extra_label=project`
// → 422). Без этой обёртки фейк неотличим от апстрима, который ручку не знает,
// и master его законно закроет — поэтому обёртка нужна всем тестам сужения.
func narrowAwareUpstream(knob string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, v := range r.URL.Query()[knob] {
			if !narrowValueParses(knob, v) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "cannot parse value for "+knob+": "+v)
				return
			}
		}
		h(w, r)
	}
}

// narrowValueParses — грубая модель разбора значения ручки: ровно столько,
// чтобы отличить НАШ фильтр от заведомо кривого значения канарейки.
func narrowValueParses(knob, v string) bool {
	switch knob {
	case "extra_stream_filters": // {project="p",env="e"}
		if !strings.HasPrefix(v, "{") || !strings.HasSuffix(v, "}") {
			return false
		}
		for _, pair := range strings.Split(strings.Trim(v, "{}"), ",") {
			name, val, ok := strings.Cut(pair, "=")
			if !ok || name == "" || val == "" {
				return false
			}
		}
		return true
	case "extra_label": // name=value
		name, val, ok := strings.Cut(v, "=")
		return ok && name != "" && val != ""
	}
	return true
}

// errCodeOf вытаскивает машинный код из тела ошибки master'а.
func errCodeOf(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("тело ошибки не разбирается как JSON: %s", body)
	}
	return e.Error
}

// upstreamMode — поведение стенда-апстрима. Отличаются они РОВНО ответом на
// канарейку; боевой запрос все, кроме «сломанных», обслуживают одинаково.
type upstreamMode string

const (
	// ручку разбирает: кривое значение отвергает, как настоящие VL/VM.
	modeHonours upstreamMode = "honours"
	// ручку не знает: молча глотает ЛЮБОЕ значение и отвечает 200 — ровно так
	// выглядит апстрим старше ручки, Loki-совместимая замена и прокси, срезающий
	// query. Это и есть дыра, ради которой заведена карта.
	modeIgnores upstreamMode = "ignores"
	// отвечает 403 на ВСЁ (нужна авторизация, прокси не пускает): канарейка даёт
	// 4xx, и односторонняя проба прочла бы это как «ручку понимает». Ради этого
	// случая у пробы есть КОНТРОЛЬНЫЙ запрос без ручки.
	modeDenies upstreamMode = "denies"
	// 500 на всё: вердикта нет — значит сужать нельзя.
	modeBroken upstreamMode = "broken"
)

// fakeUpstream — стенд одного апстрима. Считает запросы двух сортов: БОЕВЫЕ (в
// них есть marker — так тест видит утечку) и пробные.
type fakeUpstream struct {
	url      string
	realHits atomic.Int64
	probes   atomic.Int64

	mu         sync.Mutex
	probePaths []string // пути, по которым ходила ПРОБА (без боевых запросов)
}

func (f *fakeUpstream) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.probePaths)
}

const narrowMarker = "s-secret-1007"

func newFakeUpstream(t *testing.T, mode upstreamMode, knob, body, contentType string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	serve := func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case modeDenies:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "forbidden by the gateway")
			return
		case modeBroken:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "upstream on fire")
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}
	inner := http.HandlerFunc(serve)
	if mode == modeHonours {
		inner = narrowAwareUpstream(knob, serve)
	}
	// Счётчик — САМЫМ ВНЕШНИМ слоем: канарейку съедает narrowAwareUpstream, и
	// изнутри её было бы не видно, а считать надо именно ВСЕ запросы пробы.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, narrowMarker) {
			f.realHits.Add(1)
		} else {
			f.probes.Add(1)
			f.mu.Lock()
			f.probePaths = append(f.probePaths, r.URL.Path)
			f.mu.Unlock()
		}
		inner(w, r)
	}))
	t.Cleanup(ts.Close)
	f.url = ts.URL
	return f
}

// TestNarrowingProbeGatesBoundKey: привязанный ключ доходит до апстрима ТОЛЬКО
// через апстрим, у которого проверена ручка сужения. Не проверена — отказ, и
// боевой запрос до апстрима не доезжает вовсе (то есть чужого он не отдаёт даже
// в теле ошибки). Глобальный ключ ничего этого не оплачивает: он passthrough,
// сужать ему нечего — и он остаётся инструментом диагностики на сломанном
// развёртывании.
func TestNarrowingProbeGatesBoundKey(t *testing.T) {
	const logLine = `{"_time":"2026-08-12T10:00:00Z","_msg":"secret dedik output","server_id":"` + narrowMarker + `"}`
	const vmBody = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	logsPath := "/v1/logs/query?query=" + url.QueryEscape(`{server_id="`+narrowMarker+`"}`) + "&limit=100"
	metricsPath := "/v1/metrics/query?query=" + url.QueryEscape(`up{server_id="`+narrowMarker+`"}`)
	rangePath := "/v1/metrics/query_range?query=" + url.QueryEscape(`up{server_id="`+narrowMarker+`"}`) +
		"&start=0&end=10&step=15"

	cases := []struct {
		name      string
		mode      upstreamMode
		wantCode  int
		wantErr   string // машинный код в теле; "" — успех
		wantLogsE string
		leaks     bool // боевой запрос привязанного ключа доехал до апстрима
	}{
		{"апстрим ручку разбирает", modeHonours, 200, "", "", true},
		{"апстрим ручку ГЛОТАЕТ (дыра #990 наизнанку)", modeIgnores, 503,
			"metrics_narrowing_unsupported", "logs_narrowing_unsupported", false},
		{"апстрим отвечает 403 на всё (контроль пробы)", modeDenies, 502, "upstream", "upstream", false},
		{"апстрим отвечает 500 на всё", modeBroken, 502, "upstream", "upstream", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vl := newFakeUpstream(t, c.mode, "extra_stream_filters", logLine+"\n", "application/stream+json")
			vm := newFakeUpstream(t, c.mode, "extra_label", vmBody, "application/json")
			ts, boundSecret, globalSecret := liveServer(t, vm.url, vl.url)
			bound := &client{t: t, base: ts.URL, key: boundSecret}
			global := &client{t: t, base: ts.URL, key: globalSecret}

			for _, p := range []struct {
				name, path, wantErr string
			}{
				{"логи", logsPath, c.wantLogsE},
				{"метрики", metricsPath, c.wantErr},
				{"метрики/range", rangePath, c.wantErr},
			} {
				code, body := bound.doRaw("GET", p.path)
				if code != c.wantCode {
					t.Fatalf("%s, привязанный ключ: %d (%s), want %d", p.name, code, body, c.wantCode)
				}
				if p.wantErr == "" {
					continue
				}
				if got := errCodeOf(t, body); got != p.wantErr {
					t.Fatalf("%s: код ошибки = %q, want %q (оператору self-host он и объясняет, что чинить)", p.name, got, p.wantErr)
				}
				// Отказ обязан ОБЪЯСНЯТЬ. Молчаливый 403 — та самая форма, от
				// которой уходил #994.
				if !strings.Contains(string(body), "detail") || len(body) < 80 {
					t.Fatalf("%s: отказ без объяснения оператору: %s", p.name, body)
				}
				// …но объяснять СВОИМИ словами. Запрос пробы НЕ сужен парой
				// привязки (он наш собственный, `query=*`), поэтому тело ответа
				// апстрима привязанному ключу отдавать нельзя — иначе отказ сам
				// стал бы каналом к тому, что мы закрываем. Тело есть в логе.
				// Отказ обязан называть ТО, ЧТО ЧИНИТЬ. «Ошибка, попробуйте
				// позже» здесь бесполезна: чинит не вызывающий, а оператор
				// развёртывания, и ему нужны ручка, опция конфига и версия.
				if c.mode == modeIgnores {
					wantNamed := []string{"extra_", "_url", "v1."}
					if strings.HasPrefix(p.name, "логи") {
						wantNamed = []string{"extra_stream_filters", "victorialogs_url", "v1.51.0"}
					} else {
						wantNamed = []string{"extra_label", "victoriametrics_url", "v1.102.1"}
					}
					for _, want := range wantNamed {
						if !strings.Contains(string(body), want) {
							t.Fatalf("%s: отказ не называет %q — оператору нечего чинить по этому тексту: %s", p.name, want, body)
						}
					}
				}
				for _, fromUpstream := range []string{"forbidden by the gateway", "upstream on fire"} {
					if strings.Contains(string(body), fromUpstream) {
						t.Fatalf("%s: тело ответа апстрима доехало до привязанного ключа в отказе: %s", p.name, body)
					}
				}
			}

			// Утечка: боевой запрос привязанного ключа не имеет права доехать до
			// непроверенного апстрима. Положительная половина (mode=honours)
			// доказывает, что проверка не выродилась в «всегда отказ».
			if leaked := vl.realHits.Load() > 0; leaked != c.leaks {
				t.Fatalf("боевой запрос привязанного ключа доехал до VL = %v, want %v (hits=%d)",
					leaked, c.leaks, vl.realHits.Load())
			}
			if leaked := vm.realHits.Load() > 0; leaked != c.leaks {
				t.Fatalf("боевой запрос привязанного ключа доехал до VM = %v, want %v (hits=%d)",
					leaked, c.leaks, vm.realHits.Load())
			}

			// ГЛОБАЛЬНЫЙ ключ гейтом не тронут: сужать его нечем, значит и
			// проверять нечего. На сломанном апстриме он видит ровно то, что
			// апстрим ответил, — иначе оператор лишился бы диагностики.
			wantGlobal := 200
			switch c.mode {
			case modeDenies:
				wantGlobal = 403
			case modeBroken:
				wantGlobal = 500
			}
			gCode, gBody := global.doRaw("GET", logsPath)
			if gCode != wantGlobal {
				t.Fatalf("глобальный ключ: %d (%s), want %d — passthrough задет гейтом", gCode, gBody, wantGlobal)
			}
			if c.mode == modeIgnores && !strings.Contains(string(gBody), narrowMarker) {
				t.Fatalf("глобальный ключ не получил байты апстрима: %s", gBody)
			}
		})
	}
}

// TestNarrowingProbeChecksBothMetricsDoors: сужение метрик стоит на ДВУХ
// ручках (`/api/v1/query` и `/query_range`), значит и проверять проба обязана
// обе. Здесь апстрим разбирает `extra_label` на первой двери и ГЛОТАЕТ на
// второй — типовая форма для прокси, у которого правило написано на один путь,
// и ровно тот случай, ради которого у метрической пробы два пути. Вердикт
// даётся апстриму ЦЕЛИКОМ: одна слепая дверь закрывает привязанному ключу обе.
//
// Без этого теста мутация «убрать /query_range из пробы» остаётся ЗЕЛЁНОЙ —
// проверено мутационным прогоном.
func TestNarrowingProbeChecksBothMetricsDoors(t *testing.T) {
	const vmBody = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	answer := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, vmBody)
	}
	aware := narrowAwareUpstream("extra_label", answer)
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query_range" {
			answer(w, r) // слепая дверь: любое значение ручки проглочено
			return
		}
		aware(w, r)
	}))
	t.Cleanup(vm.Close)

	ts, boundSecret, _ := liveServer(t, vm.URL, "")
	bound := &client{t: t, base: ts.URL, key: boundSecret}
	for _, p := range []string{
		"/v1/metrics/query?query=up",
		"/v1/metrics/query_range?query=up&start=0&end=10&step=15",
	} {
		code, body := bound.doRaw("GET", p)
		if code != 503 {
			t.Fatalf("%s: %d (%s), want 503 — слепая ВТОРАЯ дверь апстрима осталась незамеченной", p, code, body)
		}
		if got := errCodeOf(t, body); got != "metrics_narrowing_unsupported" {
			t.Fatalf("%s: код = %q, want metrics_narrowing_unsupported", p, got)
		}
	}
}

// TestNarrowingProbeIsCachedAndOffTheHotPath: цена гарантии для здорового
// развёртывания. Проба считается ОДИН раз на апстрим и кешируется, поэтому
// боевой путь не платит лишним запросом за каждый вызов панели; отдельно
// проверяется, что глобальный ключ пробу не запускает вовсе.
func TestNarrowingProbeIsCachedAndOffTheHotPath(t *testing.T) {
	const logLine = `{"_time":"2026-08-12T10:00:00Z","_msg":"mine","server_id":"` + narrowMarker + `"}`
	vl := newFakeUpstream(t, modeHonours, "extra_stream_filters", logLine+"\n", "application/stream+json")
	vm := newFakeUpstream(t, modeHonours, "extra_label", `{"status":"success"}`, "application/json")
	ts, boundSecret, globalSecret := liveServer(t, vm.url, vl.url)
	bound := &client{t: t, base: ts.URL, key: boundSecret}
	global := &client{t: t, base: ts.URL, key: globalSecret}

	logsPath := "/v1/logs/query?query=" + url.QueryEscape(`{server_id="`+narrowMarker+`"}`) + "&limit=10"

	// Глобальный ключ первым: он проходит МИМО пробы, значит после него счётчик
	// пробных запросов обязан быть нулевым.
	for range 3 {
		if code, body := global.doRaw("GET", logsPath); code != 200 {
			t.Fatalf("глобальный ключ: %d (%s)", code, body)
		}
	}
	if n := vl.probes.Load(); n != 0 {
		t.Fatalf("глобальный ключ оплатил %d пробных запросов, want 0 — passthrough дорожать не должен", n)
	}

	for range 5 {
		if code, body := bound.doRaw("GET", logsPath); code != 200 {
			t.Fatalf("привязанный ключ: %d (%s)", code, body)
		}
	}
	// Одна ходка в метрики — чтобы дальше проверить пути ОБЕИХ дверей VM.
	// Маркер в запросе обязателен: без него запрос считался бы пробным.
	if code, body := bound.doRaw("GET", "/v1/metrics/query?query="+url.QueryEscape(`up{server_id="`+narrowMarker+`"}`)); code != 200 {
		t.Fatalf("привязанный ключ, метрики: %d (%s)", code, body)
	}

	// Проба одного пути VL = канарейка (её съедает обёртка, до счётчика она
	// доходит) + контроль. Пять запросов — те же две пробы, не десять.
	if n := vl.probes.Load(); n != 2 {
		t.Fatalf("пробных запросов к VL = %d, want 2 (проба обязана кешироваться, а не ходить на каждый запрос)", n)
	}
	// Проба обязана ходить по ТЕМ ЖЕ путям, что и боевой запрос: посредник может
	// резать параметр на одном пути и не резать на другом, и проверка соседнего
	// пути (`/select/logsql/hits`, `/api/v1/series` — они тоже разбирают ручку)
	// доказывала бы не то. Без этой сверки такая подмена остаётся ЗЕЛЁНОЙ.
	if got := vl.paths(); !slices.Equal(got, []string{"/select/logsql/query", "/select/logsql/query"}) {
		t.Fatalf("проба VL ходила по %q, want дважды /select/logsql/query — это путь боевой проксии", got)
	}
	if got := vm.paths(); !slices.Equal(got, []string{"/api/v1/query", "/api/v1/query", "/api/v1/query_range", "/api/v1/query_range"}) {
		t.Fatalf("проба VM ходила по %q, want обе двери боевой проксии по два раза", got)
	}
	if n := vl.realHits.Load(); n != 8 {
		t.Fatalf("боевых запросов к VL = %d, want 8 (3 глобальных + 5 привязанных)", n)
	}
}

// TestWarmNarrowProbes: прогрев на старте. Проверяется ровно то, что он
// обещает, — что апстримы опрошены ДО первого запроса (значит сломанный виден в
// логе при загрузке, а не когда об него споткнётся первый привязанный ключ), что
// НЕнастроенный апстрим прогрев не трогает и на нём не падает, и что отменённый
// контекст (shutdown в момент старта) он уважает, а не висит на сети.
//
// Чего этот тест НЕ доказывает: что прогрев вызван из main.go. Эта строчка
// проверена чтением и запуском настоящего бинаря, а не тестом.
func TestWarmNarrowProbes(t *testing.T) {
	vl := newFakeUpstream(t, modeIgnores, "extra_stream_filters", "\n", "application/stream+json")
	vm := newFakeUpstream(t, modeHonours, "extra_label", `{"status":"success"}`, "application/json")
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})

	httpapi.New(st, m, mm, dep, nil, nil, vm.url, vl.url, log).WarmNarrowProbes(t.Context())
	if vl.probes.Load() == 0 || vm.probes.Load() == 0 {
		t.Fatalf("прогрев не опросил апстримы: VL=%d VM=%d", vl.probes.Load(), vm.probes.Load())
	}

	// Ненастроенный апстрим: прогревать нечего, падать не на чем.
	httpapi.New(st, m, mm, dep, nil, nil, "", "", log).WarmNarrowProbes(t.Context())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := vl.probes.Load()
	httpapi.New(st, m, mm, dep, nil, nil, vm.url, vl.url, log).WarmNarrowProbes(ctx)
	if got := vl.probes.Load(); got != before {
		t.Fatalf("прогрев проигнорировал отменённый контекст: проб %d → %d", before, got)
	}
}

// TestNarrowingProbeFailClosedOnDeadUpstream: апстрим, до которого не доехали
// вообще, — тоже «не проверен». Отдельный случай от 5xx: там был ответ, здесь
// нет соединения.
func TestNarrowingProbeFailClosedOnDeadUpstream(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // порт закрыт: connection refused

	ts, boundSecret, _ := liveServer(t, deadURL, deadURL)
	bound := &client{t: t, base: ts.URL, key: boundSecret}
	for _, p := range []string{"/v1/logs/query?query=*", "/v1/metrics/query?query=up"} {
		code, body := bound.doRaw("GET", p)
		if code != 502 {
			t.Fatalf("%s на мёртвом апстриме: %d (%s), want 502 (fail-closed)", p, code, body)
		}
		if got := errCodeOf(t, body); got != "upstream" {
			t.Fatalf("%s: код = %q, want upstream", p, got)
		}
	}
}
