// Package httpapi is the public REST API of birdman-master
// (docs/specs/master.md §6, v0 subset). The panel and CLI are plain clients
// of this API — no private side doors (ADR-9).
//
// Node drain/undrain, the server logs proxy, agent self-upgrade and the
// read-only metrics proxy (итерация 4) live in ops.go.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/panelui"
	"github.com/ufna/birdman/master/internal/store"
)

// mmRateLimit: 5 rps per player_id on matchmaking endpoints
// (docs/specs/protocol.md §3).
const mmRateLimit = 5

type Server struct {
	st            *store.Store
	m             *metrics.Metrics
	mm            *matchmaker.Matchmaker
	dep           *deploy.Manager
	sender        CommandSender        // agent command dispatch (agentlink.Hub)
	logs          *agentlink.LogRouter // TailLogs chunk router
	vmURL         string               // VictoriaMetrics base URL for the metrics proxy
	vlURL         string               // VictoriaLogs base URL for the logs query proxy
	vmalertURL    string               // vmalert base URL for the alerts endpoints
	alertsLogPath string               // alert sink log for GET /v1/alerts/history
	mmLimit       *rateLimiter
	auth          *authenticator
	log           *slog.Logger
	mux           *http.ServeMux

	// onRegistriesChanged fires after a successful POST/PATCH/DELETE
	// /v1/registries (registries.go). nil-safe — an unset hook is simply not
	// called. T3 wires it to broadcast a fresh SetRegistries snapshot to
	// connected agents (docs/superpowers/specs/2026-07-09-registries-design.md §2).
	onRegistriesChanged func(context.Context)

	// backups + backupS3Test back the Backups v1 write routes (backups.go),
	// wired by WithBackups. Both nil-safe: an unwired runner/s3-test answers 503
	// (the read routes GET settings/runs work regardless — they hit the store).
	backups      BackupRunner
	backupS3Test func(context.Context) error

	// silences mirrors mute changes into alertmanager silences (alerts.go),
	// wired by WithSilenceMirror. nil-safe — an unwired mirror leaves mutes in
	// pure v0 annotation semantics (tracker #245).
	silences SilenceMirror
}

func New(st *store.Store, m *metrics.Metrics, mm *matchmaker.Matchmaker, dep *deploy.Manager, sender CommandSender, logs *agentlink.LogRouter, vmURL, vlURL string, log *slog.Logger) *Server {
	s := &Server{
		st: st, m: m, mm: mm, dep: dep, sender: sender, logs: logs, vmURL: vmURL, vlURL: vlURL,
		mmLimit: newRateLimiter(mmRateLimit, mmRateLimit),
		auth:    newAuthenticator(st), log: log, mux: http.NewServeMux(),
	}

	s.mux.HandleFunc("GET /healthz", s.handleHealthz) // no auth by design
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))

	s.mux.HandleFunc("POST /v1/nodes", s.requireScope(ScopeAdmin, s.handleCreateNode))
	s.mux.HandleFunc("GET /v1/nodes", s.requireScope(ScopeReadonly, s.handleListNodes))
	s.mux.HandleFunc("POST /v1/nodes/{id}/drain", s.requireScope(ScopeAdmin, s.handleDrainNode))
	s.mux.HandleFunc("POST /v1/nodes/{id}/undrain", s.requireScope(ScopeAdmin, s.handleUndrainNode))
	// Вывод ноды из флота навсегда: state → dead (ops.go). Отдельный глагол, а не
	// DELETE: строка ноды остаётся (на неё ссылается история серверов и матчей).
	s.mux.HandleFunc("POST /v1/nodes/{id}/revoke", s.requireScope(ScopeAdmin, s.handleRevokeNode))
	// Move a node to another environment (environments v1 §2, environments.go).
	s.mux.HandleFunc("PATCH /v1/nodes/{id}", s.requireScope(ScopeAdmin, s.handleSetNodeEnv))
	// Public internal-CA cert bundle (mTLS agentlink v1, ca.go) — ansible
	// delivers it to nodes; cert-only, the CA key cannot leak (design §5).
	s.mux.HandleFunc("GET /v1/ca", s.requireScope(ScopeReadonly, s.handleGetCA))
	s.mux.HandleFunc("GET /v1/servers", s.requireScope(ScopeReadonly, s.handleListServers))
	s.mux.HandleFunc("GET /v1/servers/{id}/logs", s.requireScope(ScopeReadonly, s.handleServerLogs))
	s.mux.HandleFunc("POST /v1/agent-upgrade", s.requireScope(ScopeAdmin, s.handleAgentUpgrade))
	s.mux.HandleFunc("GET /v1/metrics/query", s.requireScope(ScopeReadonly, s.handleMetricsQuery))
	s.mux.HandleFunc("GET /v1/metrics/query_range", s.requireScope(ScopeReadonly, s.handleMetricsQueryRange))
	s.mux.HandleFunc("GET /v1/logs/query", s.requireScope(ScopeReadonly, s.handleLogsQuery))
	s.mux.HandleFunc("POST /v1/versions", s.requireScope(ScopeDeploy, s.handleCreateVersion))
	s.mux.HandleFunc("GET /v1/versions", s.requireScope(ScopeReadonly, s.handleListVersions))
	s.mux.HandleFunc("POST /v1/deploy", s.requireScope(ScopeDeploy, s.handleDeploy))
	s.mux.HandleFunc("POST /v1/rollback", s.requireScope(ScopeDeploy, s.handleRollback))
	// Promote a version into another environment (environments v1 §4, deploy.go).
	s.mux.HandleFunc("POST /v1/promote", s.requireScope(ScopeDeploy, s.handlePromote))
	s.mux.HandleFunc("PUT /v1/fleets/{region}", s.requireScope(ScopeAdmin, s.handleUpsertFleet))
	// Список проектов — readonly (селектор проекта в панели, мультипроект W1);
	// правка match_size остаётся admin.
	s.mux.HandleFunc("GET /v1/projects", s.requireScope(ScopeReadonly, s.handleListProjects))
	s.mux.HandleFunc("PUT /v1/projects/{slug}", s.requireScope(ScopeAdmin, s.handleUpsertProject))
	// Явное управление проектами из админки (matchmaking.go): POST создаёт и
	// падает на занятом слаге (в отличие от идемпотентного PUT), DELETE сносит
	// каскадом с подтверждением вводом слага, usage показывает состав заранее.
	s.mux.HandleFunc("POST /v1/projects", s.requireScope(ScopeAdmin, s.handleCreateProject))
	s.mux.HandleFunc("GET /v1/projects/{slug}/usage", s.requireScope(ScopeAdmin, s.handleProjectUsage))
	s.mux.HandleFunc("DELETE /v1/projects/{slug}", s.requireScope(ScopeAdmin, s.handleDeleteProject))
	// Environments CRUD (environments v1 §2, environments.go). List/usage are
	// readonly; create/patch/delete are admin. DELETE сносит окружение вместе с
	// содержимым (нужен confirm с именем), usage — состав для диалога удаления.
	s.mux.HandleFunc("GET /v1/environments", s.requireScope(ScopeReadonly, s.handleListEnvironments))
	s.mux.HandleFunc("POST /v1/environments", s.requireScope(ScopeAdmin, s.handleCreateEnvironment))
	s.mux.HandleFunc("GET /v1/environments/{project}/{name}/usage", s.requireScope(ScopeReadonly, s.handleEnvironmentUsage))
	s.mux.HandleFunc("PATCH /v1/environments/{project}/{name}", s.requireScope(ScopeAdmin, s.handlePatchEnvironment))
	s.mux.HandleFunc("DELETE /v1/environments/{project}/{name}", s.requireScope(ScopeAdmin, s.handleDeleteEnvironment))
	s.mux.HandleFunc("GET /v1/events", s.requireScope(ScopeReadonly, s.handleListEvents))
	s.mux.HandleFunc("GET /v1/events/stream", s.requireScope(ScopeReadonly, s.handleEventsStream))
	s.mux.HandleFunc("GET /v1/matches", s.requireScope(ScopeReadonly, s.handleListMatches))
	s.mux.HandleFunc("GET /v1/matches/{id}", s.requireScope(ScopeReadonly, s.handleGetMatch))
	s.mux.HandleFunc("POST /v1/allocate", s.requireScope(ScopeAllocate, s.handleAllocate))

	// API-key management (П2 Access, apikeys.go); stats aggregates (П2
	// Statistics/Cost-view, stats.go); alerts (П2 Alerts, alerts.go).
	s.mux.HandleFunc("GET /v1/apikeys", s.requireScope(ScopeAdmin, s.handleListAPIKeys))
	s.mux.HandleFunc("POST /v1/apikeys", s.requireScope(ScopeAdmin, s.handleCreateAPIKey))
	s.mux.HandleFunc("DELETE /v1/apikeys/{id}", s.requireScope(ScopeAdmin, s.handleRevokeAPIKey))
	// Private registry credentials (П4 Admin/Реестры, registries.go) — admin
	// scope on every route, including the list read (secret-adjacent).
	s.mux.HandleFunc("GET /v1/registries", s.requireScope(ScopeAdmin, s.handleListRegistries))
	s.mux.HandleFunc("POST /v1/registries", s.requireScope(ScopeAdmin, s.handleCreateRegistry))
	s.mux.HandleFunc("PATCH /v1/registries/{id}", s.requireScope(ScopeAdmin, s.handlePatchRegistry))
	s.mux.HandleFunc("DELETE /v1/registries/{id}", s.requireScope(ScopeAdmin, s.handleDeleteRegistry))
	// Backups v1 (П4 Admin/Backups, backups.go) — policy is secret-adjacent, so
	// admin scope on every route, including the reads.
	s.mux.HandleFunc("GET /v1/backups/settings", s.requireScope(ScopeAdmin, s.handleGetBackupSettings))
	s.mux.HandleFunc("PATCH /v1/backups/settings", s.requireScope(ScopeAdmin, s.handlePatchBackupSettings))
	s.mux.HandleFunc("GET /v1/backups/runs", s.requireScope(ScopeAdmin, s.handleListBackupRuns))
	s.mux.HandleFunc("POST /v1/backups/run", s.requireScope(ScopeAdmin, s.handleRunBackup))
	s.mux.HandleFunc("POST /v1/backups/s3/test", s.requireScope(ScopeAdmin, s.handleTestBackupS3))
	s.mux.HandleFunc("GET /v1/stats/overview", s.requireScope(ScopeReadonly, s.handleStatsOverview))
	s.mux.HandleFunc("GET /v1/stats/cost", s.requireScope(ScopeReadonly, s.handleStatsCost))
	s.mux.HandleFunc("GET /v1/alerts/rules", s.requireScope(ScopeReadonly, s.handleAlertRules))
	s.mux.HandleFunc("GET /v1/alerts/history", s.requireScope(ScopeReadonly, s.handleAlertHistory))
	s.mux.HandleFunc("GET /v1/alerts/active", s.requireScope(ScopeReadonly, s.handleAlertsActive))
	s.mux.HandleFunc("POST /v1/alerts/mutes", s.requireScope(ScopeAdmin, s.handleCreateAlertMute))
	s.mux.HandleFunc("GET /v1/alerts/mutes", s.requireScope(ScopeReadonly, s.handleListAlertMutes))
	s.mux.HandleFunc("DELETE /v1/alerts/mutes/{id}", s.requireScope(ScopeAdmin, s.handleDeleteAlertMute))

	s.mux.HandleFunc("POST /v1/matchmaking/tickets", s.requireScope(ScopeMatchmaking, s.handleCreateTicket))
	s.mux.HandleFunc("GET /v1/matchmaking/tickets/{id}", s.requireScope(ScopeMatchmaking, s.handleGetTicket))
	s.mux.HandleFunc("DELETE /v1/matchmaking/tickets/{id}", s.requireScope(ScopeMatchmaking, s.handleCancelTicket))
	s.mux.HandleFunc("GET /v1/qos", s.handleQoS) // public by design (master.md §6)

	// Browser sessions for the panel (session.go); auth is inside the
	// handlers (login carries the key in the body, logout the cookie).
	s.mux.HandleFunc("POST /v1/session", s.handleCreateSession)
	s.mux.HandleFunc("GET /v1/session", s.handleGetSession)
	s.mux.HandleFunc("DELETE /v1/session", s.handleDeleteSession)

	// Embedded panel SPA: `/`, `/assets/*` and SPA-fallback routes
	// (panelui). Registered last — "/" catches everything unrouted.
	s.mux.Handle("/", panelui.Handler())

	return s
}

// WithAlertsSources wires the vmalert base URL and the alert-sink log path for
// the П2 alerts endpoints (config.Alerts; alerts.go). Kept a setter rather than
// a New parameter so the existing New signature — and its call sites — stay
// untouched; the alert handlers read these at request time. Returns s for
// chaining. Empty vmalert URL → the rules/active endpoints answer 503; a
// missing log file → history answers an empty list.
func (s *Server) WithAlertsSources(vmalertURL, alertsLogPath string) *Server {
	s.vmalertURL = vmalertURL
	s.alertsLogPath = alertsLogPath
	return s
}

// WithRegistriesHook wires a callback invoked after a successful registries
// change (POST/DELETE /v1/registries) — T3 uses this to broadcast a fresh
// SetRegistries snapshot to connected agents
// (docs/superpowers/specs/2026-07-09-registries-design.md §2). Kept a setter
// rather than a New parameter, like WithAlertsSources, so the existing New
// signature and its call sites stay untouched. Nil-safe: an unset hook is
// simply not called. Returns s for chaining.
func (s *Server) WithRegistriesHook(fn func(context.Context)) *Server {
	s.onRegistriesChanged = fn
	return s
}

// WithBackups wires the Backups v1 runner (manual run-now) and the s3-test
// callback (backups.go). Kept a setter rather than a New parameter, like
// WithAlertsSources/WithRegistriesHook, so the existing New signature and its
// call sites stay untouched. Both are nil-safe: POST /v1/backups/run and
// /v1/backups/s3/test answer 503 until wired, while the GET reads work off the
// store regardless. Returns s for chaining.
func (s *Server) WithBackups(r BackupRunner, s3Test func(context.Context) error) *Server {
	s.backups = r
	s.backupS3Test = s3Test
	return s
}

// WithSilenceMirror wires the alertmanager silence mirror (amsilence.Mirror) so
// POST/DELETE /v1/alerts/mutes mirror into real silences best-effort (alerts.go).
// Kept a setter rather than a New parameter, like WithAlertsSources/WithBackups,
// so the existing New signature and its call sites stay untouched. Nil-safe: an
// unwired mirror leaves mutes in pure v0 annotation semantics — the mute/unmute
// still succeeds, it just does not create/remove an AM silence. Returns s for
// chaining.
func (s *Server) WithSilenceMirror(m SilenceMirror) *Server {
	s.silences = m
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]string{"error": code, "detail": detail})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	return true
}

// decodeOptionalJSON — decodeJSON для запросов с НЕОБЯЗАТЕЛЬНЫМ телом (DELETE
// /v1/environments/{p}/{n} c {"confirm": ...}): пустое тело — не ошибка, v
// остаётся нулевым. Непустое разбирается по тем же правилам (DisallowUnknownFields).
// Тело уже ограничено MaxBytesReader в ServeHTTP, поэтому ReadAll безопасен.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	return true
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.st.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "db": err.Error()})
		return
	}
	// panel: embedded|placeholder — вкомпилирована ли в этот бинарь панель.
	// Здоровье БД про неё не знает ничего, поэтому бинарь без панели проходил
	// health-gate деплоера идеально: откат не срабатывал, deployed.json писался,
	// метрика успеха обновлялась, и дефект замечал только человек, открывший
	// панель через несколько часов (#983). Признак отдаёт САМ мастер — деплоеру
	// не приходится знать, как выглядит непособранная панель.
	panelState := "placeholder"
	if panelui.Embedded() {
		panelState = "embedded"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "panel": panelState})
}

// tenantScope — ЕДИНЫЙ вход всех ЛИСТИНГОВ и агрегатов (tracker #993): он и
// разбирает `?project=`/`?env=`, и энфорсит привязку ключа. Возвращает пару
// (project, env), которой сужается выдача; пустое поле = «не сужать».
//
// Решение владельца по #993: привязка ключа — АРЕНДАТОРСКАЯ ГРАНИЦА НА ЧТЕНИЯХ,
// а не только поточечный гейт над адресуемым объектом (#974/#988/#989). До неё
// привязанный readonly-ключ проекта Б читал через `?project=<чужой>` имена
// окружений, hostname и public_ip нод, image_ref версий, матчи, ленту событий и
// агрегаты проекта А — замерено в карточке. Отсюда две ОБЯЗАТЕЛЬНЫЕ половины:
//
//   - явный ЧУЖОЙ `?project=` (или `?env=`) → `403 key is bound to X/Y`;
//   - ПУСТОЙ параметр у привязанного ключа → выдача сужается до его пары, а НЕ
//     до всей платформы. Без этой половины дыра обходится удалением параметра,
//     то есть первая половина не значит ничего.
//
// Совместимость для тех, кто читал всю платформу привязанным ключом, сломана
// СОЗНАТЕЛЬНО — принятая цена решения. Глобальный ключ, admin и сессия панели
// работают как раньше: у них привязки нет (admin+binding запрещён на создании
// ключа), поэтому ветка ниже даже не выполняется.
//
// ПОРЯДОК: для привязанного ключа гейт стоит ПЕРВЫМ и в БД не ходит ВООБЩЕ —
// пара известна из самого ключа, сверять её с чем-то в сторе незачем (правило
// #989: гейт настолько рано, насколько позволяет адресация). Следствие, ради
// которого это и сделано так: отказ формируется раньше, чем о существовании
// проекта/окружения что-либо известно, поэтому `?project=game` (живой чужой) и
// `?project=zzz` (выдуманный) дают БАЙТ-В-БАЙТ один ответ. Провалидируй мы слаг
// до гейта — код ответа (400 против 403) сам стал бы оракулом существования,
// то есть дыра переехала бы, а не закрылась.
//
// ЭТИМ ЖЕ ГЕЙТОМ закрыт оракул `?env=` (#971), и это часть приёмки #993, а не
// follow-up: до него `scopeFilter` валидировал `?env=` по паре и отдавал ЛЮБОМУ
// readonly-ключу `400 no such environment game/ghost`, то есть перечислял
// окружения чужих проектов. Привязанный ключ теперь не доходит до
// GetEnvironment/EnvironmentNameExists ни на одном пути, и `?env=ghost`
// неотличим от `?env=<существующее чужое>`. Перебор закрыт и ВНУТРИ своего
// проекта: привязка — пара, поэтому чужой env своего проекта тоже 403 (та же
// пар-точность, что у `GET /v1/environments/{project}/{name}/usage`, #989).
//
// readEnv=true — только для ручек, которые `?env=` РЕАЛЬНО читают (`/v1/nodes`,
// `/v1/versions`, `/v1/stats/*`). На остальных параметр как игнорировался, так
// и игнорируется — гейтить то, что не влияет на выдачу, значило бы отвечать
// по-разному на запросы с одинаковым результатом. Сужение по env там всё равно
// происходит: env берётся из ПРИВЯЗКИ, а не из query.
func (s *Server) tenantScope(w http.ResponseWriter, r *http.Request, readEnv bool) (project, env string, ok bool) {
	if bp, be, bound := keyBinding(r); bound {
		q := r.URL.Query()
		if p := q.Get("project"); p != "" && p != bp {
			writeBindingDenied(w, r)
			return "", "", false
		}
		if readEnv {
			if e := q.Get("env"); e != "" && e != be {
				writeBindingDenied(w, r)
				return "", "", false
			}
		}
		return bp, be, true
	}
	project, ok = s.validateProjectParam(w, r)
	if !ok {
		return "", "", false
	}
	if !readEnv {
		return project, "", true
	}
	return s.validateEnvParam(w, r, project)
}

// validateProjectParam разбирает необязательный `?project=` — ТОЛЬКО валидация
// параметра, БЕЗ арендаторской границы (tracker #961). Пусто = вся платформа
// (поведение до мультипроекта). Непустой слаг ВАЛИДИРУЕТСЯ по БД: опечатка даёт
// `400 bad_request "no such project <slug>"`, а не молча суженную выдачу. Пишет
// свой ответ об ошибке и возвращает ok=false, когда запрос продолжать нельзя.
//
// НОВОМУ ЧТЕНИЮ НУЖЕН tenantScope, а не эта функция: она namesake прежнего
// `projectFilter` и переименована именно затем, чтобы безопасным по умолчанию
// входом стал гейт, а исключение было громким. Прямых вызывающих осталось
// ровно два — `GET /v1/alerts/active` и `/history`: у алертов не-скрывающий
// контракт («платформенный алерт виден под любым фильтром»), поэтому граница
// там строится по своему правилу и отдельной карточкой (#995).
//
// ПРАВИЛО (docs/specs/master.md §6): `?project=` на аутентифицированном чтении
// валидируется ВСЕГДА. Раньше правила не было, и каждая волна выбирала сама:
// листинги W2 (`/v1/{nodes,servers,versions,matches}`) и алерты #955 не
// валидировали, stats W3 — валидировал, то есть API отвечал на один и тот же
// параметр двумя способами. Хуже всего это било по алертам: там пустой экран —
// ЖЕЛАННОЕ состояние, поэтому «алертов нет» и «я опечатался в проекте»
// выглядели одинаково и оба радовали.
//
// 400, а не 404: сам ресурс (список/статистика) существует, плох ВВОД в
// query-параметре — тот же класс, что `?days=`/`?limit=`/`?state=`. Оракулом
// существования чужих сущностей ручка при этом не становится: все они
// readonly-скоупа, а `GET /v1/projects` (тот же скоуп) и так перечисляет все
// слаги. Панель опечатку послать не может по построению — `resolveProject`
// (panel/src/lib/project.tsx) отдаёт только слаг из ответа `/v1/projects`.
//
// Исключения — только три, каждое по своей причине:
//   - `GET /v1/qos?project=` — ручка ПУБЛИЧНАЯ, там 400 «no such project» был бы
//     бесплатным оракулом слагов для кого угодно (аргумент «у вызывающего уже
//     есть readonly» не работает);
//   - `GET /v1/alerts/rules` — фильтр не принимает вовсе (каталог конфигурации);
//   - `project` в теле `POST /v1/alerts/mutes` — матчер хранимого правила, а не
//     фильтр над данными (см. handleCreateAlertMute).
func (s *Server) validateProjectParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	project := r.URL.Query().Get("project")
	if project == "" { // пусто = вся платформа, к БД не ходим
		return "", true
	}
	if _, err := s.st.GetProject(r.Context(), project); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "bad_request", "no such project "+project)
			return "", false
		}
		storeError(w, err)
		return "", false
	}
	return project, true
}

// validateEnvParam валидирует `?env=` при уже разобранном project — вторая
// половина прежнего `scopeFilter` (tracker #971). Вызывается ТОЛЬКО с
// непривязанного пути tenantScope: привязанному ключу сюда дороги нет, иначе
// эта же валидация работала бы оракулом ИМЁН окружений (см. tenantScope).
//
// Раньше эта проверка жила только в статистике (statsScope), а листинги нод и
// версий брали ?env= сырым: опечатка молча давала пустой список — ровно то
// расхождение, ради которого сводили ?project= (#961). Правило теперь одно на
// все аутентифицированные чтения, и следующий, кто добавит ?env=, не выбирает
// поведение наугад.
//
//   - project задан → env проверяется В ЭТОМ проекте (пара (project, env) — то
//     же, чем живут deploy/versions/promote);
//   - project пуст, env задан → достаточно, чтобы окружение с таким именем
//     существовало хоть у одного проекта: пары без проекта нет, но защита от
//     опечатки остаётся.
func (s *Server) validateEnvParam(w http.ResponseWriter, r *http.Request, project string) (string, string, bool) {
	env := r.URL.Query().Get("env")
	if env == "" {
		return project, "", true
	}
	if project != "" {
		if _, err := s.st.GetEnvironment(r.Context(), project, env); err != nil {
			// ErrBadEnv → 400 «no such environment <project>/<env>» — тот же текст и тот
			// же код, что и на deploy/versions/promote (v3: единый sentinel в storeError);
			// реальный сбой стора → 500, а не «плохой ввод».
			storeError(w, err)
			return "", "", false
		}
		return project, env, true
	}
	exists, err := s.st.EnvironmentNameExists(r.Context(), env)
	if err != nil {
		storeError(w, err)
		return "", "", false
	}
	if !exists {
		writeError(w, http.StatusBadRequest, "bad_request", "no such environment "+env)
		return "", "", false
	}
	return "", env, true
}

// storeError maps store sentinel errors to HTTP responses. ErrBadEnv (окружение,
// названное запросом, не существует) — это плохой ВВОД, а не отсутствующий ресурс:
// единый 400 {"error":"bad_request","detail":"no such environment <project>/<env>"}
// по всем env-поверхностям (v3, store.ErrBadEnv).
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrBadEnv):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
