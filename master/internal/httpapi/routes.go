package httpapi

import (
	"net/http"

	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/store"
)

// Таблица маршрутов публичного REST API — ЕДИНСТВЕННЫЙ источник правды о его
// поверхности (docs/specs/master.md §6).
//
// ЗАЧЕМ именно таблица, а не прежний плоский список `s.mux.HandleFunc(...)`.
// Машиночитаемого контракта у API не было, и решение #972 запрещало заводить
// рукописный `openapi.yaml` рядом с кодом: такой файл — вторая копия правды,
// которая расходится с роутером молча. Таблица снимает возражение вместо того,
// чтобы его обходить: роутер РЕГИСТРИРУЕТСЯ ИЗ НЕЁ (see registerRoutes), то
// есть ручка, которой в таблице нет, физически не отвечает, а генератор
// OpenAPI и MCP-инструменты читают тот же массив. Копия одна.
//
// Правило для новой ручки: добавить запись сюда — и всё. Ни к mux, ни к
// генератору спеки, ни к MCP отдельно ходить не нужно; `openapi.yaml`
// пересобирается `go generate ./...` в master/api, CI падает на расхождении.

// authMode — как маршрут аутентифицируется. Явное перечисление вместо
// «пустой Scope значит публичная» нужно, чтобы генератор спеки отличал ручку,
// открытую ПО ЗАМЫСЛУ (`/v1/qos`), от ручки, которая проверяет ключ ВНУТРИ
// хендлера (`/v1/session`: логин несёт ключ в теле, логаут — в куке). Для
// читателя спеки это разные вещи, а по отсутствию скоупа они неразличимы.
type authMode uint8

const (
	// authScope — обычный случай: обёртка requireScope(Scope, h).
	authScope authMode = iota
	// authNone — открыта по замыслу, ключ не спрашивается вовсе.
	authNone
	// authHandler — хендлер аутентифицирует запрос сам, и учётные данные едут в
	// ТЕЛЕ запроса: логин панели предъявляет ключ в JSON, а не заголовком.
	// Схемы авторизации к такой ручке не применяется ни одной.
	authHandler
	// authCookie — сессионная кука панели либо обычный bearer-ключ.
	authCookie
)

// route описывает одну ручку публичного API.
//
// Req/Resp — ОБРАЗЦЫ типов (nil-значения нужного типа, например
// `[]nodeDTO(nil)`), по которым генератор спеки строит JSON-схемы рефлексией.
// Пустой Resp допустим только у маршрутов с RespOpaque: у прокси в
// VictoriaMetrics/VictoriaLogs тело приходит от апстрима и своего Go-типа не
// имеет, у SSE-стрима тела в смысле JSON нет вообще. Всем остальным Resp
// обязателен — за этим следит TestRoutesHaveResponseSamples.
type route struct {
	Method  string
	Path    string
	Auth    authMode
	Scope   string // требуемый скоуп; заполняется тогда и только тогда, когда Auth == authScope
	Summary string // однострочное описание для OpenAPI и для описаний MCP-инструментов (по-английски: спека публичная)
	Req     any    // образец тела запроса; nil, если тела нет
	Resp    any    // образец тела ответа; nil только вместе с RespOpaque
	// RespOpaque помечает ответ, у которого нет собственной Go-схемы:
	// проксированный ответ апстрима, поток SSE, не-JSON тело или 204 вовсе без
	// тела. В спеке такая ручка описывается без схемы ответа, а не выпадает из неё.
	RespOpaque bool
	// Status — код успешного ответа; 0 читается как 200. Вывести его из типа
	// нельзя: 201 у создания и 200 у чтения отличаются только аргументом
	// writeJSON внутри хендлера.
	Status int
	// H — выражение метода (`(*Server).handleFoo`), а не замыкание: таблица
	// объявлена на уровне пакета, до того как существует *Server.
	H func(*Server, http.ResponseWriter, *http.Request)
}

// routeTable — вся поверхность API. Порядок записей значения не имеет (ServeMux
// Go 1.22+ разбирает конфликты по специфичности шаблона, а не по порядку
// регистрации), поэтому они сгруппированы по подсистемам — как читателю удобно.
//
// Функция, а не переменная пакета: `GET /v1/openapi.yaml` отдаёт спеку, которая
// собирается ИЗ ЭТОЙ ЖЕ таблицы, и как переменная таблица ссылалась бы на
// хендлер, ссылающийся на неё, — Go отвергает такую инициализацию циклом.
func routeTable() []route {
	return []route{
		// --- Служебные ---------------------------------------------------------
		{Method: "GET", Path: "/healthz", Auth: authNone,
			Summary: "Liveness probe",
			Resp:    healthzResp{},
			H:       (*Server).handleHealthz},
		// Прометеевской экспозиции на API-листенере БОЛЬШЕ НЕТ (tracker #1003) —
		// она уехала на свой адрес, см. MetricsHandler в server.go. Явный 404
		// вместо «просто не регистрировать»: последним зарегистрирован катч-олл
		// панели («/»), и без этой записи скрейпер получал бы на `/metrics`
		// HTML-страницу с кодом 200 — то есть переезд выглядел бы как испорченная
		// экспозиция, а не как переезд. Тело называет новый адрес и не несёт
		// ничего тенантного.
		{Method: "GET", Path: "/metrics", Auth: authNone,
			Summary: "Gone: the Prometheus exposition moved to its own listener",
			Resp:    apiError{},
			H:       (*Server).handleMetricsMoved},
		// Контракт отдаёт сам мастер: агент или интегратор, у которого есть только
		// адрес, забирает спеку оттуда же, куда потом ходит. Открыта по замыслу —
		// поверхность API и так лежит в публичном репозитории под MIT, так что
		// закрывать её ключом значило бы добавить трения, а не защиты.
		{Method: "GET", Path: "/v1/openapi.yaml", Auth: authNone,
			Summary:    "This API's own OpenAPI 3.1 contract, generated from the route table",
			RespOpaque: true,
			H:          (*Server).handleOpenAPI},

		// MCP-эндпоинт (mcp.go). Три метода — этого требует транспорт streamable
		// HTTP: POST несёт вызовы, GET открывает серверный поток, DELETE закрывает
		// сессию. authHandler, а не authScope: ключ проверяется внутри, потому что
		// набор инструментов зависит от его скоупов, а не только факт доступа.
		{Method: "POST", Path: mcpPath, Auth: authHandler,
			Summary:    "MCP endpoint: call a tool (Model Context Protocol, streamable HTTP)",
			RespOpaque: true,
			H:          (*Server).handleMCP},
		{Method: "GET", Path: mcpPath, Auth: authHandler,
			Summary:    "MCP endpoint: open the server-to-client stream",
			RespOpaque: true,
			H:          (*Server).handleMCP},
		{Method: "DELETE", Path: mcpPath, Auth: authHandler,
			Summary:    "MCP endpoint: end the session",
			RespOpaque: true,
			H:          (*Server).handleMCP},

		// --- Ноды и флот -------------------------------------------------------
		{Method: "POST", Path: "/v1/nodes", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Enroll a node into the fleet",
			Status:  201,
			H:       (*Server).handleCreateNode},
		{Method: "GET", Path: "/v1/nodes", Auth: authScope, Scope: ScopeReadonly,
			Summary: "List fleet nodes",
			Resp:    nodesResp{},
			H:       (*Server).handleListNodes},
		{Method: "POST", Path: "/v1/nodes/{id}/drain", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Drain a node: stop placing new servers on it",
			Resp:    nodeResp{},
			H:       (*Server).handleDrainNode},
		{Method: "POST", Path: "/v1/nodes/{id}/undrain", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Undrain a node: allow placement again",
			Resp:    nodeResp{},
			H:       (*Server).handleUndrainNode},
		// Вывод ноды из флота навсегда: state → dead (ops.go). Отдельный глагол, а
		// не DELETE: строка ноды остаётся (на неё ссылается история серверов и матчей).
		{Method: "POST", Path: "/v1/nodes/{id}/revoke", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Retire a node permanently and revoke its certificate",
			Resp:    nodeResp{},
			H:       (*Server).handleRevokeNode},
		// Move a node to another environment (environments v1 §2, environments.go).
		{Method: "PATCH", Path: "/v1/nodes/{id}", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Move a node to another environment",
			Resp:    nodeResp{},
			H:       (*Server).handleSetNodeEnv},
		{Method: "PUT", Path: "/v1/fleets/{region}", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Set the desired warm-pool size for a region",
			H:       (*Server).handleUpsertFleet},
		// Public internal-CA cert bundle (mTLS agentlink v1, ca.go) — ansible
		// delivers it to nodes; cert-only, the CA key cannot leak (design §5).
		{Method: "GET", Path: "/v1/ca", Auth: authScope, Scope: ScopeReadonly,
			Summary:    "Internal CA certificate bundle (certificates only)",
			RespOpaque: true,
			H:          (*Server).handleGetCA},
		{Method: "POST", Path: "/v1/agent-upgrade", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Roll out an agent self-upgrade across the fleet",
			Status:  202,
			H:       (*Server).handleAgentUpgrade},

		// --- Дедики ------------------------------------------------------------
		{Method: "GET", Path: "/v1/servers", Auth: authScope, Scope: ScopeReadonly,
			Summary: "List dedicated servers and their states",
			Resp:    serversResp{},
			H:       (*Server).handleListServers},
		{Method: "GET", Path: "/v1/servers/{id}/logs", Auth: authScope, Scope: ScopeReadonly,
			Summary:    "Fetch the log tail of one dedicated server",
			RespOpaque: true,
			H:          (*Server).handleServerLogs},

		// --- Наблюдаемость: прокси в VictoriaMetrics / VictoriaLogs ------------
		// Тело этих трёх ответов формирует апстрим — своей Go-схемы у него нет,
		// поэтому RespOpaque.
		{Method: "GET", Path: "/v1/metrics/query", Auth: authScope, Scope: ScopeReadonly,
			Summary: "Instant metrics query (VictoriaMetrics proxy)", RespOpaque: true,
			H: (*Server).handleMetricsQuery},
		{Method: "GET", Path: "/v1/metrics/query_range", Auth: authScope, Scope: ScopeReadonly,
			Summary: "Range metrics query (VictoriaMetrics proxy)", RespOpaque: true,
			H: (*Server).handleMetricsQueryRange},
		{Method: "GET", Path: "/v1/logs/query", Auth: authScope, Scope: ScopeReadonly,
			Summary: "Log search (VictoriaLogs proxy)", RespOpaque: true,
			H: (*Server).handleLogsQuery},

		// --- Версии и деплои ---------------------------------------------------
		{Method: "POST", Path: "/v1/versions", Auth: authScope, Scope: ScopeDeploy,
			Summary: "Register a build version",
			Status:  201,
			H:       (*Server).handleCreateVersion},
		{Method: "GET", Path: "/v1/versions", Auth: authScope, Scope: ScopeReadonly,
			Summary: "List versions with their fleet state",
			Resp:    versionsResp{},
			H:       (*Server).handleListVersions},
		{Method: "POST", Path: "/v1/deploy", Auth: authScope, Scope: ScopeDeploy,
			Summary: "Roll a version out to the fleet",
			H:       (*Server).handleDeploy},
		{Method: "POST", Path: "/v1/rollback", Auth: authScope, Scope: ScopeDeploy,
			Summary: "Roll back to the previous active version",
			H:       (*Server).handleRollback},
		// Promote a version into another environment (environments v1 §4, deploy.go).
		{Method: "POST", Path: "/v1/promote", Auth: authScope, Scope: ScopeDeploy,
			Summary: "Promote a version into another environment",
			H:       (*Server).handlePromote},

		// --- Проекты -----------------------------------------------------------
		// Список проектов — readonly (селектор проекта в панели, мультипроект W1);
		// правка match_size остаётся admin.
		{Method: "GET", Path: "/v1/projects", Auth: authScope, Scope: ScopeReadonly,
			Summary: "List projects",
			Resp:    projectsResp{},
			H:       (*Server).handleListProjects},
		{Method: "PUT", Path: "/v1/projects/{slug}", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Create or update a project (idempotent)",
			Resp:    projectResp{},
			H:       (*Server).handleUpsertProject},
		// Явное управление проектами из админки (matchmaking.go): POST создаёт и
		// падает на занятом слаге (в отличие от идемпотентного PUT), DELETE сносит
		// каскадом с подтверждением вводом слага, usage показывает состав заранее.
		{Method: "POST", Path: "/v1/projects", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Create a project; fails if the slug is taken",
			Resp:    projectResp{},
			Status:  201,
			H:       (*Server).handleCreateProject},
		{Method: "GET", Path: "/v1/projects/{slug}/usage", Auth: authScope, Scope: ScopeAdmin,
			Summary: "What a project currently holds (pre-delete inventory)",
			Resp:    projectUsageResp{},
			H:       (*Server).handleProjectUsage},
		{Method: "DELETE", Path: "/v1/projects/{slug}", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Delete a project and everything under it",
			Resp:    projectDeletedResp{},
			H:       (*Server).handleDeleteProject},

		// --- Окружения ---------------------------------------------------------
		// Environments CRUD (environments v1 §2, environments.go). List/usage are
		// readonly; create/patch/delete are admin. DELETE сносит окружение вместе с
		// содержимым (нужен confirm с именем), usage — состав для диалога удаления.
		{Method: "GET", Path: "/v1/environments", Auth: authScope, Scope: ScopeReadonly,
			Summary: "List environments",
			Resp:    environmentsResp{},
			H:       (*Server).handleListEnvironments},
		{Method: "POST", Path: "/v1/environments", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Create an environment",
			Resp:    environmentResp{},
			Status:  201,
			H:       (*Server).handleCreateEnvironment},
		{Method: "GET", Path: "/v1/environments/{project}/{name}/usage", Auth: authScope, Scope: ScopeReadonly,
			Summary: "What an environment currently holds (pre-delete inventory)",
			Resp:    environmentUsageResp{},
			H:       (*Server).handleEnvironmentUsage},
		{Method: "PATCH", Path: "/v1/environments/{project}/{name}", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Update an environment",
			Resp:    environmentResp{},
			H:       (*Server).handlePatchEnvironment},
		{Method: "DELETE", Path: "/v1/environments/{project}/{name}", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Delete an environment and everything in it",
			Resp:    environmentDeletedResp{},
			H:       (*Server).handleDeleteEnvironment},

		// --- События -----------------------------------------------------------
		{Method: "GET", Path: "/v1/events", Auth: authScope, Scope: ScopeReadonly,
			Summary: "List fleet events",
			Resp:    eventsResp{},
			H:       (*Server).handleListEvents},
		// SSE: тела в смысле JSON у ответа нет — поток кадров, отсюда RespOpaque.
		{Method: "GET", Path: "/v1/events/stream", Auth: authScope, Scope: ScopeReadonly,
			Summary: "Live event stream (SSE, resumable via after_id)", RespOpaque: true,
			H: (*Server).handleEventsStream},

		// --- Матчи и аллокация -------------------------------------------------
		{Method: "GET", Path: "/v1/matches", Auth: authScope, Scope: ScopeReadonly,
			Summary: "List matches",
			Resp:    matchesResp{},
			H:       (*Server).handleListMatches},
		{Method: "GET", Path: "/v1/matches/{id}", Auth: authScope, Scope: ScopeReadonly,
			Summary: "Get one match",
			Resp:    matchResp{},
			H:       (*Server).handleGetMatch},
		{Method: "POST", Path: "/v1/allocate", Auth: authScope, Scope: ScopeAllocate,
			Summary: "Allocate a ready dedicated server directly",
			Resp:    store.Allocation{},
			H:       (*Server).handleAllocate},

		// --- Матчмейкинг -------------------------------------------------------
		{Method: "POST", Path: "/v1/matchmaking/tickets", Auth: authScope, Scope: ScopeMatchmaking,
			Summary: "File a matchmaking ticket",
			Status:  201,
			Resp:    matchmaker.Ticket{},
			H:       (*Server).handleCreateTicket},
		{Method: "GET", Path: "/v1/matchmaking/tickets/{id}", Auth: authScope, Scope: ScopeMatchmaking,
			Summary: "Poll a matchmaking ticket",
			Resp:    matchmaker.Ticket{},
			H:       (*Server).handleGetTicket},
		{Method: "DELETE", Path: "/v1/matchmaking/tickets/{id}", Auth: authScope, Scope: ScopeMatchmaking,
			Summary: "Cancel a matchmaking ticket",
			Resp:    matchmaker.Ticket{},
			H:       (*Server).handleCancelTicket},
		// public by design (master.md §6): игровой клиент измеряет RTT до флота
		// ДО того, как у него появляется хоть какой-то ключ.
		{Method: "GET", Path: "/v1/qos", Auth: authNone,
			Summary: "QoS probe endpoints for client-side RTT measurement",
			Resp:    qosResp{},
			H:       (*Server).handleQoS},

		// --- Ключи -------------------------------------------------------------
		// API-key management (П2 Access, apikeys.go).
		{Method: "GET", Path: "/v1/apikeys", Auth: authScope, Scope: ScopeAdmin,
			Summary: "List API keys (never returns secrets)",
			Resp:    apiKeysResp{},
			H:       (*Server).handleListAPIKeys},
		{Method: "POST", Path: "/v1/apikeys", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Create an API key; the secret is returned once",
			Resp:    apiKeyCreatedResp{},
			Status:  201,
			H:       (*Server).handleCreateAPIKey},
		{Method: "DELETE", Path: "/v1/apikeys/{id}", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Revoke an API key",
			Resp:    apiKeyResp{},
			H:       (*Server).handleRevokeAPIKey},

		// --- Реестры -----------------------------------------------------------
		// Private registry credentials (П2 Admin/Реестры, registries.go) — admin
		// scope on every route, including the list read (secret-adjacent).
		{Method: "GET", Path: "/v1/registries", Auth: authScope, Scope: ScopeAdmin,
			Summary: "List container registries",
			Resp:    registriesResp{},
			H:       (*Server).handleListRegistries},
		{Method: "POST", Path: "/v1/registries", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Add a container registry",
			Resp:    registryResp{},
			Status:  201,
			H:       (*Server).handleCreateRegistry},
		{Method: "PATCH", Path: "/v1/registries/{id}", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Update registry credentials",
			Resp:    registryResp{},
			H:       (*Server).handlePatchRegistry},
		{Method: "DELETE", Path: "/v1/registries/{id}", Auth: authScope, Scope: ScopeAdmin,
			Summary:    "Delete a registry",
			RespOpaque: true,
			Status:     204,
			H:          (*Server).handleDeleteRegistry},

		// --- Бэкапы ------------------------------------------------------------
		// Backups v1 (П2 Backups — its own screen, not a section of Admin;
		// backups.go) — policy is secret-adjacent, so admin scope on every route,
		// including the reads.
		{Method: "GET", Path: "/v1/backups/settings", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Get the backup schedule and target",
			Resp:    backupSettingsResp{},
			H:       (*Server).handleGetBackupSettings},
		{Method: "PATCH", Path: "/v1/backups/settings", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Update the backup schedule and target",
			Resp:    backupSettingsResp{},
			H:       (*Server).handlePatchBackupSettings},
		{Method: "GET", Path: "/v1/backups/runs", Auth: authScope, Scope: ScopeAdmin,
			Summary: "List backup runs",
			Resp:    backupRunsResp{},
			H:       (*Server).handleListBackupRuns},
		{Method: "POST", Path: "/v1/backups/run", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Run a backup now",
			Resp:    backupStartedResp{},
			Status:  202,
			H:       (*Server).handleRunBackup},
		{Method: "POST", Path: "/v1/backups/s3/test", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Test the S3 offsite target",
			Resp:    okResp{},
			H:       (*Server).handleTestBackupS3},

		// --- Статистика --------------------------------------------------------
		// stats aggregates (П2 Statistics/Cost-view, stats.go).
		{Method: "GET", Path: "/v1/stats/overview", Auth: authScope, Scope: ScopeReadonly,
			Summary: "Fleet overview aggregates",
			Resp:    statsOverviewResp{},
			H:       (*Server).handleStatsOverview},
		{Method: "GET", Path: "/v1/stats/cost", Auth: authScope, Scope: ScopeReadonly,
			Summary: "Cost aggregates",
			Resp:    statsCostResp{},
			H:       (*Server).handleStatsCost},

		// --- Алерты ------------------------------------------------------------
		// alerts (П2 Alerts, alerts.go).
		{Method: "GET", Path: "/v1/alerts/rules", Auth: authScope, Scope: ScopeReadonly,
			Summary: "List alert rules",
			Resp:    alertRulesResp{},
			H:       (*Server).handleAlertRules},
		{Method: "GET", Path: "/v1/alerts/history", Auth: authScope, Scope: ScopeReadonly,
			Summary: "Alert history",
			Resp:    alertHistoryResp{},
			H:       (*Server).handleAlertHistory},
		{Method: "GET", Path: "/v1/alerts/active", Auth: authScope, Scope: ScopeReadonly,
			Summary: "Currently firing alerts",
			Resp:    activeAlertsResp{},
			H:       (*Server).handleAlertsActive},
		{Method: "POST", Path: "/v1/alerts/mutes", Auth: authScope, Scope: ScopeAdmin,
			Summary: "Mute an alert",
			Resp:    alertMuteResp{},
			Status:  201,
			H:       (*Server).handleCreateAlertMute},
		{Method: "GET", Path: "/v1/alerts/mutes", Auth: authScope, Scope: ScopeReadonly,
			Summary: "List alert mutes",
			Resp:    alertMutesResp{},
			H:       (*Server).handleListAlertMutes},
		{Method: "DELETE", Path: "/v1/alerts/mutes/{id}", Auth: authScope, Scope: ScopeAdmin,
			Summary:    "Remove an alert mute",
			RespOpaque: true,
			Status:     204,
			H:          (*Server).handleDeleteAlertMute},

		// --- Сессии панели -----------------------------------------------------
		// Browser sessions for the panel (session.go); auth is inside the handlers
		// (login carries the key in the body, logout the cookie) — отсюда authHandler.
		{Method: "POST", Path: "/v1/session", Auth: authHandler,
			Summary: "Exchange an API key for a browser session cookie",
			Resp:    sessionResponse{},
			H:       (*Server).handleCreateSession},
		{Method: "GET", Path: "/v1/session", Auth: authCookie,
			Summary: "Describe the current browser session",
			Resp:    sessionResponse{},
			H:       (*Server).handleGetSession},
		{Method: "DELETE", Path: "/v1/session", Auth: authCookie,
			Summary: "Log out: drop the browser session",
			H:       (*Server).handleDeleteSession},
	}
}

// pattern — шаблон маршрута в форме, которую понимает http.ServeMux Go 1.22+.
func (r route) pattern() string { return r.Method + " " + r.Path }

// registerRoutes навешивает таблицу на mux. Единственное место, где ручка
// становится отвечающей: обойти таблицу и зарегистрироваться мимо неё нельзя,
// потому что New() больше ничего к mux не привязывает — кроме катч-олла панели.
func (s *Server) registerRoutes() {
	for _, rt := range routeTable() {
		h := func(w http.ResponseWriter, req *http.Request) { rt.H(s, w, req) }
		if rt.Auth == authScope {
			h = s.requireScope(rt.Scope, h)
		}
		s.mux.HandleFunc(rt.pattern(), h)
	}
}

// handleMetricsMoved — см. запись `GET /metrics` в таблице.
func (s *Server) handleMetricsMoved(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found",
		"metrics moved to the dedicated metrics listener (config listen_metrics, default 127.0.0.1:9102)")
}
