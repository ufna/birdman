package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// --- канарейка апстрима: сужение не должно молча НЕ сработать (tracker #1007) ---
//
// ЗАЧЕМ. Сужение запроса привязанного ключа (#994) исполняет не master, а
// АПСТРИМ: master лишь дописывает `extra_stream_filters={project="…",env="…"}`
// к запросу в VictoriaLogs и `extra_label=project=…`+`extra_label=env=…` к
// запросу в VictoriaMetrics. У HTTP нет способа отличить «параметр принят» от
// «параметр не понят»: незнакомый query-параметр по умолчанию МОЛЧА
// игнорируется. Замерено на живых апстримах: `extra_stream_filterZ=…` → `200`
// со всем флотом, `extra_labeZ=…` → `200` с сериями всех проектов. То есть
// апстрим не той версии, Loki-совместимая замена в `victorialogs_url` или
// прокси, срезающий query, возвращали дыру #990 обратно — с кодом 200 и без
// единой строчки в логе. Это единственное место, где #994 был ХУЖЕ, чем #990:
// тот отвечал глухим 403 и не тёк никогда.
//
// ЧЕМ ЛОВИМ. Тем же замером, только с другой стороны: КРИВОЕ значение
// ЗНАКОМОЙ ручки апстрим отвергает (VL v1.51.0: `extra_stream_filters={project=}`
// → `400 cannot parse value for tag "project"`; VM v1.102.1:
// `extra_label=project` → `422 must have the format name=value`, на обеих
// ручках `/api/v1/query` и `/query_range`), а незнакомое ИМЯ — проглатывает.
// Канарейка потому и кривая ПО ЗНАЧЕНИЮ, а не по имени.
//
// Проба ДВУСТОРОННЯЯ. Один только 4xx на канарейке ничего не значит: апстрим,
// отвечающий 4xx на всё подряд (нужна авторизация, прокси не пускает), прочёлся
// бы как «ручку понимает». Поэтому рядом идёт КОНТРОЛЬНЫЙ запрос — тот же URL
// без ручки — и он обязан дать 2xx. Понимает = контроль 2xx И канарейка 4xx.
//
// Проба идёт по ТОМУ ЖЕ базовому URL и ТОМУ ЖЕ пути, что и боевой запрос,
// поэтому в неё попадает вся цепочка развёртывания, а не только версия апстрима
// по документации: прокси, срезающий или переписывающий query, ловится ровно
// так же, как старая версия, — потому что канарейка едет его же маршрутом.
// Граница и здесь есть: посредник, который срезает параметр ИЗБИРАТЕЛЬНО (по
// тексту запроса, по заголовку, по времени суток), пробу пройдёт — она
// доказывает маршрут, а не намерения того, кто на нём стоит.
//
// ЧЕГО ПРОБА НЕ ДОКАЗЫВАЕТ (граница знания, названа и в docs/specs/master.md
// §6, и в обеих локалях self-host): она доказывает, что ручка РАЗБИРАЕТСЯ, а не
// что она ЭНФОРСИТСЯ. Апстрим, который параметр парсит, валидирует и при этом
// семантически игнорирует, канарейку пройдёт. Store-независимой пробы на
// энфорсмент не существует: «фильтр не вернул чужого» на пустом индексе
// неотличимо от «фильтр проигнорирован», а делать границу доступа зависящей от
// того, есть ли сейчас данные в индексе, нельзя. Что VL/VM фильтр действительно
// чтят — доказывают TestLiveUpstream* на живых апстримах.

const (
	// narrowProbeOKTTL — как долго master верит вердикту «апстрим ручку
	// понимает». Вердикт старше TTL не используется, поэтому апстрим,
	// подменённый под работающим master'ом, обслуживается по старому вердикту не
	// дольше TTL. НЕ мониторинг: проба ЛЕНИВАЯ, она пересчитается на первом
	// запросе привязанного ключа ПОСЛЕ протухания, а не сама по таймеру — пока
	// таких запросов нет, никто ничего и не узнает (и никому ничего не отдаётся).
	narrowProbeOKTTL = 5 * time.Minute
	// narrowProbeFailTTL короче, чтобы оператор, ПОЧИНИВШИЙ апстрим, не ждал
	// пять минут; и чтобы упавший апстрим не собирал пробу на каждый запрос.
	narrowProbeFailTTL = 30 * time.Second
	// narrowProbeBudget ограничивает пробу ЦЕЛИКОМ, а не один её запрос: у
	// метрик запросов четыре (две двери × канарейка+контроль), и потолок на
	// каждый дал бы учетверённое ожидание. Проба лежит на пути первого
	// привязанного запроса после протухания вердикта и держит мьютекс, поэтому
	// потолок заметно меньше 15с боевого прокси — и он ровно один.
	//
	// Цена короткого бюджета названа честно: апстрим, который отвечает МЕДЛЕННЕЕ
	// него, привязанному ключу выдаст 502, а глобальному тот же апстрим ответит
	// 200 (боевому пути дано 15с). Выбор в пользу короткого сделан потому, что
	// запросы пробы тривиальны — `query=1` у VM вообще не трогает индекс, у VL
	// это `*` с limit=1, — так что 5с здоровому апстриму хватает с запасом в три
	// порядка, а платит за длинный бюджет живой пользовательский запрос.
	narrowProbeBudget = 5 * time.Second
)

type narrowVerdict int

const (
	// narrowUnknown — вердикта ещё нет (проба не запускалась).
	narrowUnknown narrowVerdict = iota
	// narrowHonoured — контроль 2xx, канарейка 4xx: ручка разобрана, сужать можно.
	narrowHonoured
	// narrowIgnored — канарейка прошла как 2xx: ручка проглочена молча, сужать НЕЛЬЗЯ.
	narrowIgnored
	// narrowUnreachable — проба не доехала (сеть, 5xx, контроль не 2xx): вердикта нет,
	// а значит сужать тоже нельзя.
	narrowUnreachable
)

// narrowCanary — одна пара запросов (контроль + канарейка) к одному пути
// апстрима. args возвращает ВАЛИДНЫЕ параметры пути: контроль обязан быть 2xx,
// иначе 4xx канарейки нельзя приписать ручке.
type narrowCanary struct {
	path string
	args func() url.Values
	bad  string // заведомо кривое значение ручки
}

// narrowProbe кеширует вердикт по одному апстриму. Базовый URL за время жизни
// процесса не меняется (он приходит из конфига в httpapi.New), поэтому в ключ
// кеша не входит.
type narrowProbe struct {
	kind    string // "logs" | "metrics" — код ошибки и поле лога
	knob    string // имя ручки, на которой держится сужение
	urlOpt  string // имя опции конфига, которую чинить оператору
	product string // что должно стоять на том конце
	minVer  string // минимальная версия, на которой это проверено
	cases   []narrowCanary

	// Мьютекс держится НА ВРЕМЯ пробы: параллельные запросы панели дают один
	// пробник, остальные ждут его и читают свежий вердикт.
	mu      sync.Mutex
	verdict narrowVerdict
	detail  string // безопасная для клиента причина (без тела апстрима)
	exp     time.Time
}

func newLogsNarrowProbe() *narrowProbe {
	return &narrowProbe{
		kind: "logs", knob: "extra_stream_filters",
		urlOpt: "victorialogs_url", product: "VictoriaLogs", minVer: "v1.51.0",
		cases: []narrowCanary{{
			path: "/select/logsql/query",
			// `*` с limit=1 — самый дешёвый валидный LogsQL: апстрим, ручку
			// ПРОГЛОТИВШИЙ, выполнит его целиком, поэтому выдача зажата в одну
			// строку. Тело пробы не читается дальше первых байт.
			args: func() url.Values { return url.Values{"query": {"*"}, "limit": {"1"}} },
			// `{project=}` — не фильтр, а синтаксическая ошибка: пустое значение
			// тега. Такое значение НЕ МОЖЕТ быть исполнено как фильтр даже
			// частично, поэтому канарейка не способна ничего расширить.
			bad: "{project=}",
		}},
	}
}

func newMetricsNarrowProbe() *narrowProbe {
	// Обе двери, а не одна: сужение стоит и на /query, и на /query_range, и
	// прокси перед апстримом вполне может резать параметры на одном пути и не
	// резать на другом. Гарантия обязана покрывать ровно ту поверхность,
	// которую мы сужаем.
	instant := func() url.Values { return url.Values{"query": {"1"}} }
	rangeArgs := func() url.Values {
		return url.Values{"query": {"1"}, "start": {"0"}, "end": {"0"}, "step": {"60"}}
	}
	return &narrowProbe{
		kind: "metrics", knob: "extra_label",
		urlOpt: "victoriametrics_url", product: "VictoriaMetrics", minVer: "v1.102.1",
		cases: []narrowCanary{
			// query=1 — скалярная константа: апстрим не трогает индекс вообще.
			{path: "/api/v1/query", args: instant, bad: "project"},
			{path: "/api/v1/query_range", args: rangeArgs, bad: "project"},
		},
	}
}

// check возвращает вердикт по апстриму base, пересчитывая его не чаще, чем раз
// в TTL. Мьютекс держится НА ВРЕМЯ пробы, поэтому ожидание вызывающего
// ограничено сверху narrowProbeBudget.
func (p *narrowProbe) check(base string, log *slog.Logger) (narrowVerdict, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.verdict != narrowUnknown && time.Now().Before(p.exp) {
		return p.verdict, p.detail
	}
	verdict, detail, full := p.run(base)
	ttl := narrowProbeFailTTL
	if verdict == narrowHonoured {
		ttl = narrowProbeOKTTL
	}
	if verdict != p.verdict && log != nil {
		// Логируем СМЕНУ вердикта, а не каждую пробу: иначе сломанный апстрим
		// залил бы лог, а починенный остался бы незамеченным.
		switch verdict {
		case narrowHonoured:
			log.Info("upstream narrowing verified", "upstream", p.kind, "knob", p.knob, "url", base)
		case narrowIgnored:
			log.Error("upstream IGNORES the narrowing query arg — bound keys are refused instead of being served the whole fleet",
				"upstream", p.kind, "knob", p.knob, "url", base,
				"fix", p.urlOpt+" must point at "+p.product+" "+p.minVer+" or newer", "detail", full)
		case narrowUnreachable:
			log.Error("upstream narrowing probe failed — bound keys are refused until it succeeds",
				"upstream", p.kind, "knob", p.knob, "url", base, "detail", full)
		}
	}
	p.verdict, p.detail, p.exp = verdict, detail, time.Now().Add(ttl)
	return verdict, detail
}

// run исполняет пробу: для каждого пути сперва канарейка (в здоровом случае она
// отваливается 4xx, не выполняя запрос), затем контроль. Первый же путь, на
// котором ручка проглочена, закрывает весь апстрим.
//
// Возвращает ДВЕ причины: короткую, которая уйдёт клиенту, и полную — с телом
// ответа апстрима — которая уйдёт только в лог master'а. Разделение не
// косметика: тело незнакомого апстрима на не-2xx может содержать что угодно, а
// запрос пробы НЕ сужен парой привязки (он наш собственный, `query=*`), и
// отдавать его отголоски привязанному ключу было бы новой дырой вместо
// закрытой. Оператору для диагностики хватает кода ответа, а тело у него есть
// в логе.
func (p *narrowProbe) run(base string) (verdict narrowVerdict, detail, full string) {
	base = strings.TrimRight(base, "/")
	// ОДИН бюджет на всю пробу. Контекст СВОЙ, а не запроса: вердикт кладётся в
	// кеш и обслуживает чужие запросы, поэтому он не должен зависеть от того,
	// отменил ли своё соединение тот клиент, чей запрос оплатил пробу.
	ctx, cancel := context.WithTimeout(context.Background(), narrowProbeBudget)
	defer cancel()
	for _, c := range p.cases {
		// args() отдаёт СВЕЖУЮ карту на каждый вызов: url.Values — это map, и
		// Set на общей карте испортил бы контрольный запрос канареечным
		// значением (проба тогда всегда отвечала бы «не проверить»).
		canary := c.args()
		canary.Set(p.knob, c.bad)
		code, body, err := probeGet(ctx, base+c.path+"?"+canary.Encode())
		switch {
		case err != nil:
			d := fmt.Sprintf("%s: %v", c.path, err)
			return narrowUnreachable, d, d
		case code >= 500:
			d := fmt.Sprintf("%s: canary answered %d", c.path, code)
			return narrowUnreachable, d, d + ": " + body
		case code < 400:
			// Апстрим принял значение, которое разобрать НЕЛЬЗЯ, — значит он не
			// разбирал его вовсе. Ровно так выглядит и наш настоящий фильтр.
			d := fmt.Sprintf("%s: %s=%q accepted with %d (a malformed value of a parsed arg must be rejected)",
				c.path, p.knob, c.bad, code)
			return narrowIgnored, d, d
		}
		// 4xx на канарейке ещё не доказывает, что дело в ручке: апстрим может
		// отвечать 4xx на всё. Контроль — тот же запрос БЕЗ ручки.
		code, body, err = probeGet(ctx, base+c.path+"?"+c.args().Encode())
		switch {
		case err != nil:
			d := fmt.Sprintf("%s: %v", c.path, err)
			return narrowUnreachable, d, d
		case code >= 300:
			d := fmt.Sprintf("%s: control request (no %s) answered %d", c.path, p.knob, code)
			return narrowUnreachable, d, d + ": " + body
		}
	}
	return narrowHonoured, "", ""
}

// probeGet — один запрос пробы. Тело читается обрезанным и уходит только в лог
// master'а (см. run): клиенту от него достаётся один код ответа.
func probeGet(ctx context.Context, target string) (code int, body string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return resp.StatusCode, strings.TrimSpace(string(raw)), nil
}

// requireNarrowableUpstream — гейт fail-CLOSED перед сужаемым запросом: master
// пропускает его в апстрим, только если ПРОВЕРИЛ, что тот ручку сужения
// разбирает. Зовётся только для привязанного ключа: глобальному/admin сужать
// нечего, он passthrough как был — и остаётся рабочим инструментом диагностики
// на сломанном развёртывании.
func (s *Server) requireNarrowableUpstream(w http.ResponseWriter, p *narrowProbe, base string) bool {
	switch verdict, detail := p.check(base, s.log); verdict {
	case narrowHonoured:
		return true
	case narrowIgnored:
		// 503, а не 403: у вызывающего есть право на эти данные, сломано
		// РАЗВЁРТЫВАНИЕ. 403 здесь был бы ровно тем «отказом без объяснений»,
		// от которого уходил #994, — и увёл бы оператора искать проблему в
		// правах ключа. Отдать 200 со всем флотом нельзя тем более.
		writeError(w, http.StatusServiceUnavailable, p.kind+"_narrowing_unsupported", fmt.Sprintf(
			"%s ignores the %s query arg, so a query from a project-bound key cannot be narrowed to its (project, env) pair; "+
				"refusing instead of answering with the whole fleet. Point %s at %s %s or newer (see docs/self-host.md §4). Probe: %s",
			p.urlOpt, p.knob, p.urlOpt, p.product, p.minVer, detail))
	default:
		// Вердикта нет — сужать нельзя ровно так же. Код `upstream` тот же, что
		// у неотвечающего апстрима в боевом пути: это он и есть.
		writeError(w, http.StatusBadGateway, "upstream", fmt.Sprintf(
			"cannot verify that %s honours the %s query arg, so a project-bound key is refused: %s", p.urlOpt, p.knob, detail))
	}
	return false
}

// WarmNarrowProbes прогревает канарейки на старте master'а: оператор видит
// сломанный апстрим в логе при загрузке, а не в момент, когда об него споткнётся
// первый привязанный ключ. НЕ гейт старта: наблюдаемость не имеет права ронять
// аллокацию, поэтому отказ пробы здесь только пишется в лог — закрывает он
// ровно те две ручки, где сужение и живёт.
func (s *Server) WarmNarrowProbes(ctx context.Context) {
	for _, pr := range []struct {
		p    *narrowProbe
		base string
	}{{s.logsProbe, s.vlURL}, {s.metricsProbe, s.vmURL}} {
		if ctx.Err() != nil {
			return
		}
		if pr.base == "" {
			continue // апстрим не настроен — проксия и так отвечает 503
		}
		pr.p.check(pr.base, s.log)
	}
}
