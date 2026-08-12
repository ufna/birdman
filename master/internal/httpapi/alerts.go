package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// Alerts endpoints for the panel П2 Alerts screen (docs/specs/panel.md §3,
// ops.md §1). master proxies the vmalert stack on the box: rules and live
// firing state come from the vmalert HTTP API, history from the alert sink log
// (/var/log/birdman/alerts.log). Alert rules carry a `description` annotation
// (EN, canonical) plus an optional `description_ru`; master passes both through
// verbatim and the panel picks the text by locale, falling back to
// `description` when `description_ru` is absent (self-host operators need not
// write bilingual rules).
//
// Project scoping (tracker #955): /v1/alerts/{active,history} report `project`
// (from the alert's label) and a derived `scope` ("project"|"platform"), and
// accept ?project= — a NON-hiding filter, see keepAlertForProject below. The
// slug itself is validated against the DB (validateProjectParam via tenantScope,
// tracker #961): a typo is a 400, never a quietly narrowed screen.
//
// Key binding (tracker #995): on top of that filter these two reads enforce the
// tenant boundary — for a BOUND key an alert is visible only when its `project`
// equals the binding or is EMPTY (alertVisibility below). GET /v1/alerts/mutes
// inherits the same rule; GET /v1/alerts/rules deliberately does not (see there).
//
// Mutes (POST/GET/DELETE /v1/alerts/mutes) are master state (table alert_mutes,
// store/alerts.go) — the source of truth. A mute is reflected as muted:true on
// matching alerts in /v1/alerts/{active,history} so the panel can dim/hide them
// and an audit trail exists, AND is mirrored into a real alertmanager silence
// best-effort (SilenceMirror below, internal/amsilence, ops.md §1, tracker #245)
// — real suppression of the log sink/Discord. Without a reachable alertmanager
// the mirror is a silent no-op and the mute degrades to pure v0 annotation
// semantics.

const alertsUpstreamTimeout = 15 * time.Second

// --- vmalert-backed reads (rules, active) ---

// vmRulesResponse is the vmalert /api/v1/rules payload (Prometheus-compatible).
type vmRulesResponse struct {
	Data struct {
		Groups []struct {
			Name  string `json:"name"`
			File  string `json:"file"`
			Rules []struct {
				Name        string            `json:"name"`
				Query       string            `json:"query"`
				Duration    float64           `json:"duration"` // "for", seconds
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"` // inactive|pending|firing
				Type        string            `json:"type"`  // alerting|recording
			} `json:"rules"`
		} `json:"groups"`
	} `json:"data"`
}

type alertRule struct {
	Name          string `json:"name"`
	Group         string `json:"group"`
	Severity      string `json:"severity"`
	Expr          string `json:"expr"`
	For           string `json:"for"`
	State         string `json:"state"`
	Description   string `json:"description"`              // EN, canonical
	DescriptionRu string `json:"description_ru,omitempty"` // optional RU; panel falls back to Description
}

// handleAlertRules is GET /v1/alerts/rules (readonly).
//
// Deliberately NOT filtered by ?project= (unlike /active and /history): this is
// the CONFIGURATION CATALOGUE, not a list of alert instances. A rule has no
// project — its expr may fan out into one series per project (BufferEmptyReady*
// aggregates by (region, project)), so the project lives inside the query text,
// not in the static labels vmalert reports here. Filtering by a label that
// structurally cannot be there would return an empty catalogue for every
// project.
//
// Привязка ключа сюда тоже НЕ заводится (решение по #995, разобрано отдельно от
// mute'ов). Причина та же структурная: сузить каталог не по чему, а «нечем
// сузить» на границе арендатора значит выбирать между «отдать всем» и «закрыть
// привязанному вовсе». Выбрано отдать: правила — платформенная КОНФИГУРАЦИЯ, тот
// же класс, что платформенный алерт без лейбла, который привязанный ключ по
// решению владельца видеть ОБЯЗАН; закрыв каталог, мы отняли бы у арендатора
// ровно то, чем он объясняет себе свои же алерты (порог, `for`, текст описания),
// а инстансов — то есть значений и адресов конкретных срабатываний — тут нет
// вовсе.
//
// ЦЕНА названа честно, «ни одной чужой величины» тут было бы неправдой (найдено
// вторым проходом): `state` правила — живой булев сигнал по ВСЕЙ платформе,
// поэтому горящее `BufferEmptyReadyProd` сообщает арендатору «у кого-то на
// платформе опустел прод-буфер», хотя своего инстанса в /active он не видит; и
// чем уже область правила, тем точнее этот сигнал. Второй остаточный канал —
// `expr`: текст пишет оператор self-host'а, и зашитый литерал `{project="game"}`
// уедет привязанному ключу вместе с каталогом. Кодом ни то, ни другое не
// лечится (разбирать чужой PromQL, чтобы угадать намерение автора, — худшее из
// решений), лечится дисциплиной правил: агрегируй по `(project)`, не хардкодь
// слаг. Оба записаны в docs/specs/master.md §6.
func (s *Server) handleAlertRules(w http.ResponseWriter, r *http.Request) {
	if s.vmalertURL == "" {
		writeError(w, http.StatusServiceUnavailable, "alerts_unconfigured",
			"vmalert_url is not set on this master")
		return
	}
	var vr vmRulesResponse
	if err := s.vmalertGet(r.Context(), "/api/v1/rules", &vr); err != nil {
		writeError(w, http.StatusBadGateway, "upstream", err.Error())
		return
	}
	rules := make([]alertRule, 0)
	for _, g := range vr.Data.Groups {
		for _, ru := range g.Rules {
			if ru.Type == "recording" { // alerting rules only
				continue
			}
			rules = append(rules, alertRule{
				Name:          ru.Name,
				Group:         g.Name,
				Severity:      ru.Labels["severity"],
				Expr:          ru.Query,
				For:           durationSeconds(ru.Duration),
				State:         ru.State,
				Description:   annotation(ru.Annotations),
				DescriptionRu: ru.Annotations["description_ru"],
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// vmAlertsResponse is the vmalert /api/v1/alerts payload.
type vmAlertsResponse struct {
	Data struct {
		Alerts []struct {
			State       string            `json:"state"` // pending|firing
			Name        string            `json:"name"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
			ActiveAt    time.Time         `json:"activeAt"`
			Value       string            `json:"value"`
		} `json:"alerts"`
	} `json:"data"`
}

type activeAlert struct {
	Name          string    `json:"name"`
	Severity      string    `json:"severity"`
	Region        string    `json:"region"`
	Node          string    `json:"node"`
	Project       string    `json:"project"` // from the alert's `project` label; "" = platform-wide
	Scope         string    `json:"scope"`   // "project" | "platform" — derived from Project
	State         string    `json:"state"`
	ActiveAt      time.Time `json:"active_at"`
	Value         string    `json:"value"`
	Description   string    `json:"description"`              // EN, canonical
	DescriptionRu string    `json:"description_ru,omitempty"` // optional RU; panel falls back to Description
	Muted         bool      `json:"muted"`                    // an active master mute covers this alertname+region+project
}

func (s *Server) handleAlertsActive(w http.ResponseWriter, r *http.Request) {
	if s.vmalertURL == "" {
		writeError(w, http.StatusServiceUnavailable, "alerts_unconfigured",
			"vmalert_url is not set on this master")
		return
	}
	// Гейт привязки и валидация ?project= — ДО апстрима: опечатка не тратит
	// запрос к vmalert и не превращается в 502, когда vmalert лежит; чужой слаг
	// не тратит его тем более. После 503 alerts_unconfigured выше: «на этом
	// мастере алертов нет вовсе» — факт более фундаментальный, чем ввод, и он
	// одинаков для всех ключей (о существовании чужого проекта 503 не говорит
	// ничего, поэтому порядок гейта относительно него ничего не открывает).
	vis, ok := s.alertVisibility(w, r)
	if !ok {
		return
	}
	var va vmAlertsResponse
	if err := s.vmalertGet(r.Context(), "/api/v1/alerts", &va); err != nil {
		writeError(w, http.StatusBadGateway, "upstream", err.Error())
		return
	}
	out := make([]activeAlert, 0)
	for _, a := range va.Data.Alerts {
		if a.State != "firing" { // active = currently firing
			continue
		}
		if !vis.keep(a.Labels["project"]) {
			continue
		}
		name := a.Name
		if name == "" {
			name = a.Labels["alertname"]
		}
		out = append(out, activeAlert{
			Name:          name,
			Severity:      a.Labels["severity"],
			Region:        a.Labels["region"],
			Node:          alertNode(a.Labels),
			Project:       a.Labels["project"],
			Scope:         alertScope(a.Labels["project"]),
			State:         a.State,
			ActiveAt:      a.ActiveAt,
			Value:         a.Value,
			Description:   annotation(a.Annotations),
			DescriptionRu: a.Annotations["description_ru"],
		})
	}
	mutes, err := s.st.ListAlertMutes(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for i := range out {
		out[i].Muted = anyMuteMatches(mutes, out[i].Name, out[i].Region, out[i].Project)
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out})
}

func (s *Server) vmalertGet(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, alertsUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimURL(s.vmalertURL)+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("vmalert returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func trimURL(u string) string {
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
}

func durationSeconds(secs float64) string {
	if secs <= 0 {
		return "0s"
	}
	return time.Duration(secs * float64(time.Second)).String()
}

func annotation(a map[string]string) string {
	if d := a["description"]; d != "" {
		return d
	}
	return a["summary"]
}

func alertNode(labels map[string]string) string {
	for _, k := range []string{"node", "instance", "hostname", "host"} {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// --- project scoping (tracker #955) ---
//
// An alert is project-scoped iff the metric its expr is built on CARRIES a
// `project` label — nothing else is configured, and no `scope:` label is
// introduced into the rules. That way the self-host operator's OWN rules are
// classified correctly without learning any markup discipline: aggregate over
// a project-labelled series and the alert is a project alert; build it on node
// disk or cert expiry and it is a platform alert. Same trick as the optional
// `description_ru` annotation — the rule file stays the single source of truth
// instead of a catalogue baked into master/panel.
//
// scope is derived rather than passed through so the panel never has to repeat
// the "empty label means platform" rule (docs/specs/master.md §6).

const (
	alertScopeProject  = "project"
	alertScopePlatform = "platform"
)

func alertScope(project string) string {
	if project == "" {
		return alertScopePlatform
	}
	return alertScopeProject
}

// keepAlertForProject is the ?project= filter, and it is deliberately NOT
// hiding: an alert drops out only when it EXPLICITLY belongs to another
// project. An alert with no `project` label (MasterDown, NodeDown, DiskHigh,
// CertExpiry, BackupFailed — platform by nature, I6) stays visible under every
// selection.
//
// Strict "keep only what matches" was rejected: a project has no "All" mode in
// the panel (unlike env), so a strict filter would hide every platform alert
// FOREVER, with no view to get it back — silently swallowing "the master is
// down" is a worse outcome than showing a neighbour's alert. Precedents:
// keepForProject for the event feed (panel/src/lib/project.tsx) and the
// platform slice of peak CCU (store/rollup.go, decision I5/W3).
//
// The requested slug IS validated against the DB before this filter ever runs
// (validateProjectParam via tenantScope, tracker #961) — so `want` here is always
// a real project. Non-hiding and validated are two different questions and both are
// answered "yes": a typo can no longer masquerade as "platform alerts only",
// which on THIS screen is indistinguishable from the desired state of "all
// quiet", while a real project still keeps every platform alert in view.
//
// С #995 эта функция обслуживает ровно НЕПРИВЯЗАННЫЙ ключ (глобальный, admin,
// сессия панели): у привязанного правило другое и скрывающее — keepForBinding
// ниже. Ослабление здесь на границу арендатора больше не влияет — это и было
// целью разделения.
func keepAlertForProject(alertProject, want string) bool {
	return want == "" || alertProject == "" || alertProject == want
}

// --- tenant boundary (tracker #995) ---

// keepForBinding — СКРЫВАЮЩЕЕ правило привязки, решение владельца по #995:
// привязанный ключ видит объект, только если тот принадлежит ЕГО проекту или не
// принадлежит никакому. Вторая половина обязательна: платформенные MasterDown /
// NodeDown лейбла `project` не несут, и спрятать их значило бы не сказать
// арендатору, что лежит мастер, — гейт стал бы вреден. Правило одно на алерты
// (alertVisibility) и на mute'ы (handleListAlertMutes), потому что mute — это
// аннотация НАД алертом: разъехавшись, они показали бы арендатору mute на алерт,
// которого он не видит, или скрыли бы причину `muted:true` на своём.
//
// ЦЕНА второй половины, названная вслух (найдено вторым проходом): платформенный
// алерт несёт `node`/`instance` (см. alertNode), а это hostname КОНКРЕТНОЙ ноды,
// которая может принадлежать соседнему тенанту — `NodeDown`, `DiskHigh*`,
// `CertExpiry`, `AgentlinkPendingStuck` все без `project` и все с адресом узла.
// То есть после #995 привязанный ключ по-прежнему узнаёт hostname'ы чужих нод —
// не через листинги (#993 их закрыл), а через платформенные алерты. Спрятать
// нельзя: правило владельца требует показывать платформенные алерты, а алерт без
// адреса узла бесполезен. Записано в docs/specs/master.md §6.
//
// И перечень выше НЕ исчерпывает цену (tracker #1020): «платформенный» здесь —
// это ОТСУТСТВИЕ лейбла `project`, а не «алерт ничьего тенанта», и отсутствие
// бывает структурным (правило безлейблово по построению — перечисленные выше)
// либо ДЕГРАДИРОВАВШИМ (правило проектное, но атрибуция не состоялась). Во
// втором случае алерт про КОНКРЕТНЫЙ чужой дедик становится виден всем. Сегодня
// такое правило ровно одно — TickDegraded: проект ему доклеивается join'ом с
// birdman_server_info, а ветка `unless` намеренно оставляет алерт гореть без
// лейбла, когда info-серии нет (master лёг, БД моргнула).
//
// НО ЦЕНА ЗДЕСЬ УЖЕ, ЧЕМ ЛЕЙБЛЫ ВЫРАЖЕНИЯ, и это замерено на самом мастере
// (TestDegradedPlatformAlertPayloadForBoundKey): server_id наружу не выходит
// вовсе — поля под него нет ни в activeAlert, ни в alertEvent, а alertNode
// читает только node/instance/hostname/host; текст summary (он и несёт
// server_id с величиной тика) вытесняется description'ом в annotation(), а у
// TickDegraded description непуст всегда. Привязанный ключ получает hostname
// узла и — только на /v1/alerts/active — числовой value тика, то есть узнаёт,
// что у КАКОГО-ТО дедика на такой-то ноде деградировал тик и насколько, но не
// какого именно.
//
// Замерено на живом VM v1.102.1: после #1008 так деградируют
// только дедики, запущенные ДО апгрейда агента (у остальных пару несёт сама
// серия), то есть остаток самоисчерпывается перекруткой. Здесь это не чинится
// и не должно: keepForBinding видит уже готовый лейбл, а ветка существует
// ровно потому, что владельца в этот момент не знает и master.
func keepForBinding(objProject, binding string) bool {
	return objProject == "" || objProject == binding
}

// alertVisibility — правило видимости алерта для ОДНОГО запроса: какой из двух
// фильтров применять и с каким проектом.
//
// bound=false — прежний не-скрывающий `?project=` (#955): алерт выпадает, только
// если ЯВНО чужой запрошенному, пустой параметр не фильтрует вовсе.
// bound=true — граница арендатора: `project` алерта == привязка ИЛИ пусто.
//
// Признак хранится ЯВНО, а не выводится из «project непуст»: для привязанного
// ключа project и так всегда равен привязке, и правило «непустой want ⇒ фильтр
// скрывающий» держалось бы лишь на том, что keepAlertForProject случайно
// совпадает с нужным ответом при непустом want. Расклеится это ровно тогда,
// когда кто-нибудь тронет не-скрывающий фильтр (а он ЗАТЕМ и заведён, чтобы его
// смягчали), и граница молча превратится в фильтр. С явным флагом привязанный
// путь падает ЗАКРЫТО даже в недостижимом случае пустой привязки: он тогда
// покажет только платформенные алерты, а не всё подряд.
type alertVisibility struct {
	project string
	bound   bool
}

func (v alertVisibility) keep(alertProject string) bool {
	if v.bound {
		return keepForBinding(alertProject, v.project)
	}
	return keepAlertForProject(alertProject, v.project)
}

// alertVisibility разбирает запрос в правило видимости: ОДИН вход для
// /v1/alerts/active и /history.
//
// Гейт «явный чужой ?project= → 403» берётся у общего tenantScope (#993), а не
// пишется здесь заново: у алертов своё правило ВИДИМОСТИ, но не свой ответ на
// чужой слаг — иначе тело/код отказа разъехались бы с листингами, а
// неотличимость живого чужого проекта от выдуманного пришлось бы доказывать
// дважды. От tenantScope здесь нужна ровно первая половина: пара привязки как
// значение фильтра и отказ до похода в БД. Вторую половину — сам фильтр — он
// дать не может: у листингов сужение по проекту скрывающее ВСЕГДА, у алертов
// только для привязанного ключа.
//
// readEnv=false, и это НЕ «у алерта нет измерения env» — оно есть: правила
// мониторинга агрегируют по `(region, project, env)`
// (infra/roles/birdman_monitoring_dev/templates/rules.yml.j2), и лейбл `env` у
// части алертов приезжает. Причина в другом: ручка `?env=` не читает вовсе, и
// гейтить параметр, не влияющий на выдачу, значило бы отвечать по-разному на
// запросы с одинаковым результатом (#993).
//
// СЛЕДСТВИЕ, которое из этого вытекает и потому названо вслух: граница на
// алертах проходит по ПРОЕКТУ, а не по паре (project, env) — так сформулировано
// решение владельца по #995. Ключ, привязанный к `neighbour/dev`, видит алерты
// `neighbour/prod`. Пар-точности листингов (#993, где чужой env своего проекта
// даёт 403) здесь НЕТ; сделать её — отдельная развилка и отдельная карточка,
// потому что «сузить до пары» спрячет и алерты без лейбла `env` либо потребует
// второй половины правила («env алерта == привязка ИЛИ пусто»), а это уже
// расширение принятого решения, а не его реализация (tracker #1015).
func (s *Server) alertVisibility(w http.ResponseWriter, r *http.Request) (alertVisibility, bool) {
	project, _, ok := s.tenantScope(w, r, false)
	if !ok {
		return alertVisibility{}, false
	}
	_, _, bound := keyBinding(r)
	return alertVisibility{project: project, bound: bound}, true
}

// --- alert history (log sink) ---

// alertEvent is one normalized firing/resolution from the alert log.
type alertEvent struct {
	Name          string `json:"name"`
	Severity      string `json:"severity"`
	Region        string `json:"region"`
	Node          string `json:"node"`
	Project       string `json:"project"` // from the alert's `project` label; "" = platform-wide
	Scope         string `json:"scope"`   // "project" | "platform" — derived from Project
	StartsAt      string `json:"startsAt"`
	EndsAt        string `json:"endsAt"`
	Description   string `json:"description"`              // EN, canonical
	DescriptionRu string `json:"description_ru,omitempty"` // optional RU; panel falls back to Description
	Active        bool   `json:"active"`
	ReceivedAt    string `json:"received_at,omitempty"`
	Muted         bool   `json:"muted"` // an active master mute covers this alertname+region+project
}

// amAlert is one alert in the alertmanager-v2 webhook shape written to the log.
type amAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt"`
}

// webhookLine is one JSON line: a delivery carrying a batch of alerts.
type webhookLine struct {
	ReceivedAt string    `json:"received_at"`
	Alerts     []amAlert `json:"alerts"`
}

func (s *Server) handleAlertHistory(w http.ResponseWriter, r *http.Request) {
	// Гейт привязки и валидация ?project= — до чтения лога: опечатка обязана
	// давать 400 и на мастере, где лог-сина ещё нет (там ответ иначе неотличим от
	// «всё спокойно»), а чужой слаг — 403 независимо от того, есть ли лог вообще.
	vis, ok := s.alertVisibility(w, r)
	if !ok {
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > 1000 {
		limit = 1000
	}
	data, err := os.ReadFile(s.alertsLogPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || s.alertsLogPath == "" {
			// No sink yet is a normal state, not an error.
			writeJSON(w, http.StatusOK, map[string]any{"alerts": []alertEvent{}})
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	events := parseAlertsLog(data, time.Now(), limit, vis)
	mutes, err := s.st.ListAlertMutes(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for i := range events {
		events[i].Muted = anyMuteMatches(mutes, events[i].Name, events[i].Region, events[i].Project)
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": events})
}

// parseAlertsLog parses newline-delimited JSON deliveries into a normalized,
// newest-first list capped at limit. It is defensive: unparseable lines are
// skipped, and both the {received_at, alerts:[…]} batch shape and a bare
// single-alert object are accepted. Never returns nil (→ [] in JSON).
//
// vis is the visibility rule for this request (the non-hiding ?project= filter,
// or the tenant boundary of a bound key — alertVisibility). It is applied while
// reading, i.e. BEFORE the limit cap, so ?limit=100&project=X really returns up
// to 100 alerts of X instead of whatever survives after neighbours ate the
// budget. For a bound key the order matters for the same reason AND for
// correctness of the boundary's promise: a noisy neighbour must not be able to
// push the tenant's own history out of the window.
func parseAlertsLog(data []byte, now time.Time, limit int, vis alertVisibility) []alertEvent {
	out := []alertEvent{}
	keep := func(a amAlert) bool { return vis.keep(a.Labels["project"]) }
	sc := bufio.NewScanner(bytes.NewReader(data))
	// Alert lines can be long (many labels/annotations) — lift the token cap.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var wl webhookLine
		if err := json.Unmarshal(line, &wl); err == nil && len(wl.Alerts) > 0 {
			for _, a := range wl.Alerts {
				if !keep(a) {
					continue
				}
				out = append(out, normalizeAlert(a, wl.ReceivedAt, now))
			}
			continue
		}
		// Fallback: a bare single-alert object per line.
		var a amAlert
		if err := json.Unmarshal(line, &a); err == nil && len(a.Labels) > 0 && keep(a) {
			out = append(out, normalizeAlert(a, wl.ReceivedAt, now))
		}
	}
	// Newest first by delivery time (received_at); a repeated-firing alert keeps
	// one startsAt but a fresh received_at each delivery, so received_at is the
	// right "latest" key. Fall back to startsAt when a bare line has no
	// received_at. Times are parsed so mixed RFC3339 forms compare correctly.
	sort.SliceStable(out, func(i, j int) bool {
		return alertSortTime(out[i]).After(alertSortTime(out[j]))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func normalizeAlert(a amAlert, receivedAt string, now time.Time) alertEvent {
	return alertEvent{
		Name:          a.Labels["alertname"],
		Severity:      a.Labels["severity"],
		Region:        a.Labels["region"],
		Node:          alertNode(a.Labels),
		Project:       a.Labels["project"],
		Scope:         alertScope(a.Labels["project"]),
		StartsAt:      a.StartsAt,
		EndsAt:        a.EndsAt,
		Description:   annotation(a.Annotations),
		DescriptionRu: a.Annotations["description_ru"],
		Active:        alertActive(a.Status, a.EndsAt, now),
		ReceivedAt:    receivedAt,
	}
}

// alertActive reports whether an alert is still firing, judged by endsAt (per
// spec): empty/zero endsAt or an endsAt in the future ⇒ active; a resolved
// status or a past endsAt ⇒ inactive.
func alertActive(status, endsAt string, now time.Time) bool {
	if status == "resolved" {
		return false
	}
	if endsAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, endsAt)
	if err != nil {
		return status == "firing" // unparseable endsAt: trust only an explicit firing
	}
	if t.Year() <= 1 { // alertmanager zero time "0001-01-01T00:00:00Z" = no end
		return true
	}
	return t.After(now)
}

// alertSortTime is an alert's ordering instant: received_at (delivery), else
// startsAt. Unparseable/absent → zero time (sorts last).
func alertSortTime(a alertEvent) time.Time {
	for _, s := range []string{a.ReceivedAt, a.StartsAt} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// anyMuteMatches reports whether any of the (active) mutes covers the alert.
// The project is part of the match (tracker #957) and is matched STRICTLY —
// see store.AlertMute.Matches: a project-scoped mute never covers a platform
// alert, unlike the deliberately non-hiding keepAlertForProject above.
//
// Сюда СОЗНАТЕЛЬНО передаётся ПОЛНЫЙ список mute'ов, не суженный привязкой
// (#995), и утечки в этом нет именно из-за строгости выше: mute чужого проекта
// не матчит ни один алерт, который привязанный ключ вообще видит (у видимого
// project == привязка или пуст), поэтому на флаг `muted` он повлиять не может.
// Сузь мы список здесь — результат был бы тот же, а вот пропажа mute'а БЕЗ
// проекта (кроет всё) погасила бы флаг на своих же алертах.
func anyMuteMatches(mutes []store.AlertMute, name, region, project string) bool {
	for _, m := range mutes {
		if m.Matches(name, region, project) {
			return true
		}
	}
	return false
}

// --- mute rules (master-level suppression) ---
//
// Contract (docs/specs/master.md §6): POST needs admin, GET needs readonly,
// DELETE needs admin. A mute is master state (the source of truth): it makes
// matching alerts report muted:true (handleAlertsActive/handleAlertHistory) and
// is mirrored best-effort into a real alertmanager silence (SilenceMirror) —
// real suppression of the sink/Discord. Without a reachable alertmanager the
// mute degrades to pure v0 annotation semantics.

// SilenceMirror mirrors mute changes into alertmanager silences best-effort
// (ops.md §1, tracker #245). Implemented by amsilence.Mirror; nil-safe like
// the other optional hooks — an unwired mirror means pure v0 semantics.
type SilenceMirror interface {
	MuteUpserted(ctx context.Context, m store.AlertMute)
	MuteDeleted(ctx context.Context, m store.AlertMute)
}

type createMuteRequest struct {
	Alertname string  `json:"alertname"`
	Region    *string `json:"region"`     // absent/empty → all regions
	Project   *string `json:"project"`    // absent/empty → all projects (tracker #957)
	Note      string  `json:"note"`       // optional, defaults to ""
	ExpiresAt *string `json:"expires_at"` // absent/empty → never; else RFC3339 in the future
}

// handleCreateAlertMute is POST /v1/alerts/mutes (admin). It is an idempotent
// upsert: muting an alertname+region+project that already has an active mute
// updates that mute's note/expires_at in place (store.UpsertAlertMute) rather
// than stacking duplicates — so a repeat POST doubles as "extend/edit" and
// still returns 201 with the resulting mute.
//
// project is NOT validated against the DB — deliberately, and no longer "same
// as the reads": ?project= on the reads IS validated since tracker #961. The
// line is drawn between a FILTER over existing data and a MATCHER of a stored
// rule. Three reasons this field stays on the matcher side:
//   - its neighbour `region` cannot be validated at all (there is no registry of
//     regions), and validating one target component but not the other would be a
//     fresh inconsistency;
//   - the panel fills it from the ALERT'S LABEL (out of the monitoring stack),
//     not from the DB-backed project selector — a label left over from an older
//     series would make the alert unmutable;
//   - a wrong slug here is VISIBLE: the mute shows up in GET /v1/alerts/mutes
//     with its project on display, unlike a filter typo which silently narrows a
//     screen.
//
// A mute WITHOUT a project keeps the pre-#957 meaning — every project, including
// the platform alerts that belong to none.
func (s *Server) handleCreateAlertMute(w http.ResponseWriter, r *http.Request) {
	var req createMuteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Alertname) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "alertname is required")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && strings.TrimSpace(*req.ExpiresAt) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*req.ExpiresAt))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "expires_at must be an RFC3339 timestamp")
			return
		}
		if !t.After(time.Now()) {
			writeError(w, http.StatusBadRequest, "bad_request", "expires_at must be in the future")
			return
		}
		expiresAt = &t
	}
	createdBy := ""
	if key, ok := keyFromContext(r.Context()); ok {
		createdBy = key.Name
	}
	mute, err := s.st.UpsertAlertMute(r.Context(), store.CreateAlertMuteParams{
		Alertname: req.Alertname,
		Region:    req.Region,
		Project:   req.Project,
		Note:      req.Note,
		ExpiresAt: expiresAt,
		CreatedBy: createdBy,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Audit — payload carries no secrets (region/project/expires_at may be null).
	if err := s.st.InsertEvent(r.Context(), store.EventAlertMuted, store.EventRef{}, map[string]any{
		"mute_id": mute.ID, "alertname": mute.Alertname, "region": mute.Region,
		"project": mute.Project, "expires_at": mute.ExpiresAt, "created_by": mute.CreatedBy,
	}); err != nil {
		s.log.Error("alert mute: create event write failed", "mute_id", mute.ID, "err", err)
	}
	// Mirror into a real alertmanager silence best-effort (nil-safe). Synchronous
	// and before writeJSON so a fast AM lets a GET right after this POST already
	// see silence_id; the 201 body still carries the mute AS UPSERTED (silence_id
	// may be null here even on success — the mirror stamps the row, not this copy).
	if s.silences != nil {
		s.silences.MuteUpserted(r.Context(), mute)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"mute": mute})
}

// handleListAlertMutes is GET /v1/alerts/mutes (readonly): active mutes
// newest-first; ?all=1 also returns expired ones.
//
// Привязка (tracker #995). Эта ручка — «листинг БЕЗ фильтра»: `?project=` она не
// принимает вовсе и до #995 отдавала привязанному ключу mute'ы чужих проектов
// целиком, вместе с `note` (свободный текст оператора) и `created_by` (имя
// чужого ключа). Под общее правило алертов она автоматически не подводится —
// фильтра, который можно было бы «сделать скрывающим», тут нет, — но ответ по
// существу тот же и по той же причине: mute ЕСТЬ аннотация над алертом
// (`muted:true` в /active и /history), поэтому его видимость обязана совпадать с
// видимостью алерта, который он гасит. Отсюда keepForBinding: свой проект ИЛИ
// null (mute без проекта кроет всё, включая платформенные алерты арендатора, —
// не покажи мы его, `muted:true` на своём алерте осталось бы без причины).
//
// `?project=` при этом НЕ заводится: новая поверхность API ради того же ответа
// не нужна, а непривязанный ключ (глобальный, admin, сессия панели) получает
// список как раньше — байт-в-байт, включая порядок и `?all=1`.
//
// Сужается ОТВЕТ, а не запрос к стору: это курируемый список из своей же БД, а
// не сырая проксия (#990), состав полей задаём мы, и фильтрация в памяти тут не
// «фильтрация ответа апстрима», а обычный отбор строк. ListAlertMutes читает
// таблицу целиком в обоих случаях (её же читают /active и /history), так что
// отдельный SQL-путь дал бы только вторую реализацию одного правила.
func (s *Server) handleListAlertMutes(w http.ResponseWriter, r *http.Request) {
	mutes, err := s.st.ListAlertMutes(r.Context(), r.URL.Query().Get("all") == "1")
	if err != nil {
		storeError(w, err)
		return
	}
	if binding, _, bound := keyBinding(r); bound {
		kept := make([]store.AlertMute, 0, len(mutes))
		for _, m := range mutes {
			muteProject := ""
			if m.Project != nil {
				muteProject = *m.Project
			}
			if keepForBinding(muteProject, binding) {
				kept = append(kept, m)
			}
		}
		mutes = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"mutes": emptyNotNull(mutes)})
}

// handleDeleteAlertMute is DELETE /v1/alerts/mutes/{id} (admin). 204 on a real
// removal (emits alert_unmuted), 404 for an unknown/already-removed id, 400 for
// a non-uuid id. DELETE is idempotent in effect (end state: not muted); the
// 404 lets the caller tell a real unmute from a no-op and avoids a duplicate
// audit event.
func (s *Server) handleDeleteAlertMute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "mute id must be a uuid")
		return
	}
	mute, deleted, err := s.st.DeleteAlertMute(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "not_found", "no such mute")
		return
	}
	if err := s.st.InsertEvent(r.Context(), store.EventAlertUnmuted, store.EventRef{}, map[string]any{
		"mute_id": mute.ID, "alertname": mute.Alertname, "region": mute.Region,
		"project": mute.Project,
	}); err != nil {
		s.log.Error("alert mute: delete event write failed", "mute_id", mute.ID, "err", err)
	}
	// Remove the mirrored silence best-effort (nil-safe); the 204 never depends on
	// AM — a failed delete is left to the reconcile orphan sweep.
	if s.silences != nil {
		s.silences.MuteDeleted(r.Context(), mute)
	}
	w.WriteHeader(http.StatusNoContent)
}
