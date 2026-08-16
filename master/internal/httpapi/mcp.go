package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP-сервер мастера: `/v1/mcp`, транспорт streamable HTTP.
//
// ЗАЧЕМ ВНУТРИ МАСТЕРА, А НЕ ОТДЕЛЬНЫМ БИНАРЁМ. Отдельный процесс завёл бы
// собственный релизный цикл, собственную версию и собственную копию знаний об
// API — то есть ровно тот дрейф, от которого уходит таблица маршрутов. Здесь
// же MCP наследует даром всё, что у мастера уже есть: авторизацию по ключу,
// привязку ключа к паре (project, env), рейтлимит матчмейкинга, формат ошибок.
// Установка у оператора нулевая: есть мастер — есть MCP.
//
// ГЛАВНОЕ СВОЙСТВО: ИНСТРУМЕНТ — ЭТО ВЫЗОВ API, А НЕ ЕГО ПЕРЕСКАЗ. Обработчик
// собирает настоящий *http.Request и прогоняет его через тот же s.mux, что и
// внешний вызов, с тем же заголовком Authorization. Поэтому MCP не может
// разойтись с REST по правам, фильтрации или поведению: он через него и ходит.
//
// ДВА НЕЗАВИСИМЫХ ГЕЙТА ЗАПИСИ, ОБА ЗАКРЫТЫ ПО УМОЛЧАНИЮ:
//  1. tools/list собирается ПОД СКОУПЫ ПРЕДЪЯВЛЕННОГО КЛЮЧА — readonly-ключ
//     пишущих инструментов не видит вовсе: не тратит на них контекст и не
//     может попробовать вызвать;
//  2. рубильник мастера (`mcp_write_enabled`, деф. false) — пока выключен,
//     пишущих инструментов нет ни у кого, включая админский ключ.
//
// Второй гейт нужен именно как отдельный: скоупы ключа заводятся под человека
// и живут долго, а «пустить агента менять флот» — решение другого рода и
// другого срока, и оператор обязан принимать его отдельно и явно.

const (
	mcpPath       = "/v1/mcp"
	mcpServerName = "birdman-master"
)

// mcpArgIn — куда уезжает значение аргумента в HTTP-запросе.
type mcpArgIn uint8

const (
	argQuery mcpArgIn = iota // ?name=value
	argPath                  // подстановка в {name} шаблона пути
	argBody                  // поле JSON-тела
)

type mcpArg struct {
	Name        string
	Description string
	Type        string // "string" | "integer" | "boolean"
	Required    bool
	In          mcpArgIn
}

// mcpTool — инструмент. Method/Path обязаны совпадать с записью таблицы
// маршрутов: за этим следит TestMCPToolsMatchRouteTable, поэтому переехавшая
// или удалённая ручка красит тесты, а не тихо ломает агента в проде.
type mcpTool struct {
	Name        string
	Title       string
	Description string
	Method      string
	Path        string
	Scope       string // требуемый скоуп — тот же, что у маршрута
	Write       bool   // требует рубильника mcp_write_enabled
	Args        []mcpArg
}

// Общие аргументы сужения: привязанный ключ и так видит только свою пару, но
// глобальному ключу без них не объяснить, о каком проекте вопрос.
var (
	argProject = mcpArg{Name: "project", Description: "Project slug to narrow to.", Type: "string", In: argQuery}
	argEnv     = mcpArg{Name: "env", Description: "Environment name to narrow to (dev, prod, …).", Type: "string", In: argQuery}
)

func mcpToolTable() []mcpTool {
	return []mcpTool{
		// --- Чтение: состояние флота ----------------------------------------
		{
			Name: "fleet_overview", Title: "Fleet overview",
			Description: "Start here. Aggregate state of the fleet: live matches, players online, " +
				"warm buffer, nodes and active versions. Answers \"how is the fleet doing right now\".",
			Method: "GET", Path: "/v1/stats/overview", Scope: ScopeReadonly,
			Args: []mcpArg{argProject, argEnv},
		},
		{
			Name: "list_nodes", Title: "List nodes",
			Description: "Fleet machines with their state (active, draining, dead), region and capacity.",
			Method:      "GET", Path: "/v1/nodes", Scope: ScopeReadonly,
			Args: []mcpArg{argProject, argEnv},
		},
		{
			Name: "list_servers", Title: "List dedicated servers",
			Description: "Dedicated server processes and their states: ready (warm buffer), allocated " +
				"(serving a match), draining, creating.",
			Method: "GET", Path: "/v1/servers", Scope: ScopeReadonly,
			Args: []mcpArg{
				argProject, argEnv,
				{Name: "region", Description: "Region to filter by.", Type: "string", In: argQuery},
				{Name: "state", Description: "State to filter by: ready, allocated, draining, creating.", Type: "string", In: argQuery},
			},
		},
		{
			Name: "list_matches", Title: "List matches",
			Description: "Matches with their server, region and lifecycle state.",
			Method:      "GET", Path: "/v1/matches", Scope: ScopeReadonly,
			Args: []mcpArg{argProject, argEnv},
		},
		{
			Name: "get_match", Title: "Get one match",
			Description: "Full record of a single match by id — start here when investigating one bad session.",
			Method:      "GET", Path: "/v1/matches/{id}", Scope: ScopeReadonly,
			Args: []mcpArg{
				{Name: "id", Description: "Match id.", Type: "string", Required: true, In: argPath},
			},
		},
		{
			Name: "list_versions", Title: "List versions",
			Description: "Build versions known to the fleet and what each one is doing: active, " +
				"deprecated but still draining, or pre-pulling.",
			Method: "GET", Path: "/v1/versions", Scope: ScopeReadonly,
			Args: []mcpArg{argProject, argEnv},
		},
		{
			Name: "list_events", Title: "Recent fleet events",
			Description: "The event feed: allocations, deploys, node state changes, failures. " +
				"The fastest way to see what changed recently.",
			Method: "GET", Path: "/v1/events", Scope: ScopeReadonly,
			Args: []mcpArg{
				argProject, argEnv,
				{Name: "limit", Description: "How many events to return (newest first).", Type: "integer", In: argQuery},
			},
		},
		{
			Name: "list_projects", Title: "List projects",
			Description: "Projects on this master. Use it first when you do not know the project slug.",
			Method:      "GET", Path: "/v1/projects", Scope: ScopeReadonly,
		},
		{
			Name: "list_environments", Title: "List environments",
			Description: "Environments of a project (dev, prod, …).",
			Method:      "GET", Path: "/v1/environments", Scope: ScopeReadonly,
			Args: []mcpArg{argProject},
		},

		// --- Чтение: диагностика --------------------------------------------
		{
			Name: "get_server_logs", Title: "Dedicated server logs",
			Description: "Log tail of one dedicated server. History outlives the server itself, so this " +
				"works on a dedic that has already been reaped.",
			Method: "GET", Path: "/v1/servers/{id}/logs", Scope: ScopeReadonly,
			Args: []mcpArg{
				{Name: "id", Description: "Dedicated server id.", Type: "string", Required: true, In: argPath},
				{Name: "tail", Description: "How many trailing lines to return.", Type: "integer", In: argQuery},
			},
		},
		{
			Name: "query_logs", Title: "Search logs",
			Description: "Fleet-wide log search (LogsQL, proxied to VictoriaLogs). Use it to find which " +
				"servers hit an error, not to read one server's tail — get_server_logs is better for that.",
			Method: "GET", Path: "/v1/logs/query", Scope: ScopeReadonly,
			Args: []mcpArg{
				{Name: "query", Description: "LogsQL query.", Type: "string", Required: true, In: argQuery},
				{Name: "start", Description: "Start of the window (RFC3339 or a relative value such as -1h).", Type: "string", In: argQuery},
				{Name: "end", Description: "End of the window.", Type: "string", In: argQuery},
				{Name: "limit", Description: "Maximum lines to return.", Type: "integer", In: argQuery},
				argProject, argEnv,
			},
		},
		{
			Name: "query_metrics", Title: "Metrics: instant query",
			Description: "PromQL query at a single instant (proxied to VictoriaMetrics).",
			Method:      "GET", Path: "/v1/metrics/query", Scope: ScopeReadonly,
			Args: []mcpArg{
				{Name: "query", Description: "PromQL expression.", Type: "string", Required: true, In: argQuery},
				{Name: "time", Description: "Evaluation instant (RFC3339 or unix seconds); default now.", Type: "string", In: argQuery},
				argProject, argEnv,
			},
		},
		{
			Name: "query_metrics_range", Title: "Metrics: range query",
			Description: "PromQL query over a time window — use it to show a trend rather than a point.",
			Method:      "GET", Path: "/v1/metrics/query_range", Scope: ScopeReadonly,
			Args: []mcpArg{
				{Name: "query", Description: "PromQL expression.", Type: "string", Required: true, In: argQuery},
				{Name: "start", Description: "Window start (RFC3339 or unix seconds).", Type: "string", Required: true, In: argQuery},
				{Name: "end", Description: "Window end.", Type: "string", Required: true, In: argQuery},
				{Name: "step", Description: "Resolution step, e.g. 30s or 5m.", Type: "string", Required: true, In: argQuery},
				argProject, argEnv,
			},
		},
		{
			Name: "list_alerts", Title: "Firing alerts",
			Description: "Alerts firing right now. Check list_alert_mutes too before concluding " +
				"that nothing is wrong — a muted alert does not show up here.",
			Method: "GET", Path: "/v1/alerts/active", Scope: ScopeReadonly,
			Args: []mcpArg{argProject, argEnv},
		},
		{
			Name: "alert_history", Title: "Alert history",
			Description: "Alerts that fired earlier — use it to tell a new problem from a recurring one.",
			Method:      "GET", Path: "/v1/alerts/history", Scope: ScopeReadonly,
			Args: []mcpArg{
				{Name: "limit", Description: "How many records to return (newest first).", Type: "integer", In: argQuery},
				argProject, argEnv,
			},
		},
		{
			Name: "list_alert_mutes", Title: "List alert mutes",
			Description: "Active alert mutes. Check this before concluding that an alert is not firing.",
			Method:      "GET", Path: "/v1/alerts/mutes", Scope: ScopeReadonly,
			Args: []mcpArg{
				{Name: "all", Description: "Set true to include expired mutes.", Type: "boolean", In: argQuery},
				argProject, argEnv,
			},
		},
		{
			Name: "get_cost", Title: "Cost and utilization",
			Description: "Cost aggregates and slot utilization — what the fleet is spending and how full it is.",
			Method:      "GET", Path: "/v1/stats/cost", Scope: ScopeReadonly,
			Args: []mcpArg{argProject, argEnv},
		},

		// --- Запись ----------------------------------------------------------
		{
			Name: "drain_node", Title: "Drain a node",
			Description: "Stop placing new dedicated servers on a node. Matches already running on it " +
				"keep playing to their end — draining never drops a live match.",
			Method: "POST", Path: "/v1/nodes/{id}/drain", Scope: ScopeAdmin, Write: true,
			Args: []mcpArg{
				{Name: "id", Description: "Node id.", Type: "string", Required: true, In: argPath},
			},
		},
		{
			Name: "undrain_node", Title: "Undrain a node",
			Description: "Return a drained node to service so the fleet places servers on it again.",
			Method:      "POST", Path: "/v1/nodes/{id}/undrain", Scope: ScopeAdmin, Write: true,
			Args: []mcpArg{
				{Name: "id", Description: "Node id.", Type: "string", Required: true, In: argPath},
			},
		},
		{
			Name: "mute_alert", Title: "Mute an alert",
			Description: "Silence an alert, optionally until a given time. Prefer a bounded mute: " +
				"an unbounded one outlives the reason it was created for.",
			Method: "POST", Path: "/v1/alerts/mutes", Scope: ScopeAdmin, Write: true,
			Args: []mcpArg{
				{Name: "alertname", Description: "Alert name to mute.", Type: "string", Required: true, In: argBody},
				{Name: "region", Description: "Limit the mute to one region; omit for all regions.", Type: "string", In: argBody},
				{Name: "project", Description: "Limit the mute to one project; omit for all projects.", Type: "string", In: argBody},
				{Name: "note", Description: "Why it is muted — write it for the next person on call.", Type: "string", In: argBody},
				{Name: "expires_at", Description: "RFC3339 timestamp in the future; omit to mute indefinitely.", Type: "string", In: argBody},
			},
		},
		{
			Name: "deploy_version", Title: "Deploy a version",
			Description: "Roll a registered version out to the fleet. The new version JOINS the running " +
				"one rather than replacing it: the previous version drains as its matches end.",
			Method: "POST", Path: "/v1/deploy", Scope: ScopeDeploy, Write: true,
			Args: []mcpArg{
				{Name: "version_id", Description: "Version id to make active.", Type: "string", Required: true, In: argBody},
				argProject, argEnv,
			},
		},
		{
			Name: "rollback_version", Title: "Roll back",
			Description: "Return a region to the previous active version. Same mechanism as a deploy, " +
				"in reverse: nobody is dropped mid-match.",
			Method: "POST", Path: "/v1/rollback", Scope: ScopeDeploy, Write: true,
			Args: []mcpArg{
				{Name: "project", Description: "Project slug.", Type: "string", Required: true, In: argBody},
				{Name: "region", Description: "Region to roll back.", Type: "string", Required: true, In: argBody},
				{Name: "env", Description: "Environment; omit to let the master resolve it.", Type: "string", In: argBody},
			},
		},
		{
			Name: "promote_version", Title: "Promote a version",
			Description: "Promote a version into another environment, e.g. from dev to prod.",
			Method:      "POST", Path: "/v1/promote", Scope: ScopeDeploy, Write: true,
			Args: []mcpArg{
				{Name: "version_id", Description: "Version id to promote.", Type: "string", Required: true, In: argBody},
				{Name: "to_env", Description: "Target environment.", Type: "string", Required: true, In: argBody},
			},
		},
		{
			Name: "run_backup", Title: "Run a backup now",
			Description: "Start a Postgres backup immediately, outside the schedule.",
			Method:      "POST", Path: "/v1/backups/run", Scope: ScopeAdmin, Write: true,
		},
	}
}

// --- сборка сервера под конкретный ключ --------------------------------------

// WithMCP включает пишущие инструменты MCP. Сеттер, как WithBackups и прочие,
// чтобы сигнатура New и её вызовы не менялись. По умолчанию (сеттер не вызван)
// `/v1/mcp` отдаёт только чтение — какой бы ключ ни предъявили.
func (s *Server) WithMCP(writeEnabled bool) *Server {
	s.mcpWrite = writeEnabled
	return s
}

// handleMCP — точка входа транспорта. Аутентификация идёт ДО передачи запроса
// в SDK: неавторизованный вызов обязан получить обычную 401 в формате API, а не
// протокольную ошибку MCP, — иначе оператор, перепутавший ключ, увидит невнятный
// сбой сессии вместо «ключ не подошёл».
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.auth.authenticate(r); !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
		return
	}
	s.mcpHandler().ServeHTTP(w, r)
}

// mcpHandler лениво строит транспорт SDK. getServer вызывается на КАЖДЫЙ
// HTTP-запрос — именно там набор инструментов и сужается под предъявленный ключ.
func (s *Server) mcpHandler() http.Handler {
	s.mcpOnce.Do(func() {
		s.mcpHTTP = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			key, _, ok := s.auth.authenticate(r)
			if !ok {
				return nil // SDK ответит 400; до сюда доходит только уже проверенный запрос
			}
			return s.mcpServerFor(key.Scopes, r.Header.Get("Authorization"))
		}, nil)
	})
	return s.mcpHTTP
}

// mcpServerFor собирает сервер под конкретный набор скоупов.
func (s *Server) mcpServerFor(scopes []string, authHeader string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    mcpServerName,
		Title:   "birdman fleet",
		Version: "v1",
	}, nil)

	for _, tool := range mcpToolTable() {
		if !s.mcpToolAllowed(tool, scopes) {
			continue
		}
		srv.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Title:       tool.Title,
			Description: tool.Description,
			InputSchema: tool.inputSchema(),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: !tool.Write},
		}, s.mcpToolHandler(tool, authHeader))
	}

	s.addMCPResources(srv, authHeader)
	addMCPPrompts(srv)
	return srv
}

// mcpToolAllowed — оба гейта записи в одном месте: рубильник мастера и скоуп
// ключа. Инструмент, не прошедший их, в tools/list не попадает вовсе.
func (s *Server) mcpToolAllowed(tool mcpTool, scopes []string) bool {
	if tool.Write && !s.mcpWrite {
		return false
	}
	return slices.Contains(scopes, tool.Scope) || slices.Contains(scopes, ScopeAdmin)
}

// inputSchema — JSON Schema аргументов. Пишется вручную (а не выводится из
// Go-типа), потому что описания аргументов — это то, ЧТО ЧИТАЕТ МОДЕЛЬ: они
// определяют, позовёт ли она инструмент верно, и стоят отдельного внимания.
func (t mcpTool) inputSchema() map[string]any {
	props := map[string]any{}
	var required []string
	for _, a := range t.Args {
		props[a.Name] = map[string]any{"type": a.Type, "description": a.Description}
		if a.Required {
			required = append(required, a.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// --- исполнение ---------------------------------------------------------------

func (s *Server) mcpToolHandler(tool mcpTool, authHeader string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return mcpErrorResult("arguments are not a JSON object: " + err.Error()), nil
			}
		}
		httpReq, err := tool.buildRequest(ctx, args, authHeader)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}

		rec := &mcpRecorder{header: http.Header{}, status: http.StatusOK}
		s.mux.ServeHTTP(rec, httpReq)

		body := rec.body.String()
		if rec.status >= 400 {
			// Тело ошибки API уже машиночитаемо ({"error","detail"}) — отдаём как
			// есть: агенту полезнее увидеть код, чем наш пересказ.
			return mcpErrorResult(fmt.Sprintf("%s %s → %d: %s", tool.Method, httpReq.URL.RequestURI(), rec.status, body)), nil
		}
		if strings.TrimSpace(body) == "" {
			body = fmt.Sprintf("%d, empty body", rec.status)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil
	}
}

// buildRequest превращает аргументы инструмента в настоящий HTTP-запрос к
// собственному роутеру. Заголовок Authorization переносится с MCP-запроса — то
// есть ходит инструмент РОВНО ПОД ТЕМ ЖЕ КЛЮЧОМ, что предъявил клиент, со всеми
// его скоупами и привязкой к паре (project, env).
//
// Сессия MCP живёт дольше одного HTTP-запроса, и заголовок захватывается при её
// создании — но правами это не торгует: авторизацию исполняет requireScope на
// КАЖДОМ вызове инструмента, уже по живому состоянию ключа. Отозванный
// посреди сессии ключ упирается в 401 на первом же следующем вызове, как и на
// обычной ручке.
func (t mcpTool) buildRequest(ctx context.Context, args map[string]any, authHeader string) (*http.Request, error) {
	path := t.Path
	query := url.Values{}
	body := map[string]any{}

	for _, a := range t.Args {
		raw, present := args[a.Name]
		if !present || raw == nil || raw == "" {
			if a.Required {
				return nil, fmt.Errorf("argument %q is required", a.Name)
			}
			continue
		}
		switch a.In {
		case argPath:
			// Значение уезжает в путь, поэтому экранируется как сегмент: id со
			// слэшем иначе увёл бы вызов на соседнюю ручку.
			path = strings.ReplaceAll(path, "{"+a.Name+"}", url.PathEscape(fmt.Sprint(raw)))
		case argQuery:
			query.Set(a.Name, fmt.Sprint(raw))
		case argBody:
			body[a.Name] = raw
		}
	}
	if i := strings.Index(path, "{"); i >= 0 {
		return nil, fmt.Errorf("path template %q left unfilled at %q", t.Path, path[i:])
	}

	var reader io.Reader
	if len(body) > 0 {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}

	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, t.Method, target, reader)
	if err != nil {
		return nil, err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Тело читается через http.MaxBytesReader в ServeHTTP, которому нужен
	// ненулевой ContentLength — у собранного вручную запроса он уже проставлен
	// NewRequestWithContext для bytes.Reader.
	return req, nil
}

func mcpErrorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// mcpRecorder — минимальный http.ResponseWriter. Свой, а не httptest: тот живёт
// в тестовом дереве стандартной библиотеки, и тащить его в боевой путь ради
// двадцати строк незачем.
type mcpRecorder struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func (r *mcpRecorder) Header() http.Header { return r.header }

func (r *mcpRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
}

func (r *mcpRecorder) Write(p []byte) (int, error) {
	r.wroteHeader = true
	return r.body.Write(p)
}

// --- ресурсы и промпты --------------------------------------------------------

func (s *Server) addMCPResources(srv *mcp.Server, authHeader string) {
	srv.AddResource(&mcp.Resource{
		URI:         "birdman://openapi",
		Name:        "openapi",
		Title:       "birdman master OpenAPI contract",
		Description: "The full REST contract of this master, generated from its route table.",
		MIMEType:    "application/yaml",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		spec, err := OpenAPISpec()
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: "birdman://openapi", MIMEType: "application/yaml", Text: string(spec),
		}}}, nil
	})

	srv.AddResource(&mcp.Resource{
		URI:         "birdman://operator-guide",
		Name:        "operator-guide",
		Title:       "How this fleet is put together",
		Description: "Short orientation for an agent seeing this fleet for the first time.",
		MIMEType:    "text/markdown",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: "birdman://operator-guide", MIMEType: "text/markdown", Text: mcpOperatorGuide,
		}}}, nil
	})
}

// mcpOperatorGuide — то, чего нет в схемах: как эти сущности связаны и какие
// выводы из них делать нельзя. Схему инструмента модель прочитает и так, а вот
// что дренаж не роняет матчи и что игроки не ходят через мастер — не выведет.
const mcpOperatorGuide = `# birdman fleet: orientation

birdman runs a fleet of dedicated game servers without Kubernetes. Three parts:
**master** (matchmaker, fleet control, this API), a **node agent** on every game
machine, and an **SDK** linked into the game server itself.

## Entities, in the order you will need them

- **project / environment** — every node, version, match and event belongs to one
  ` + "`(project, env)`" + ` pair. An API key may be bound to a pair, and then it can
  reach nothing outside it. If a tool returns less than you expect, check the binding
  before suspecting the fleet.
- **node** — a machine. States: active, draining, dead.
- **dedicated server** ("dedic") — one game-server process on a node. States:
  creating, ready (sitting in the warm buffer), allocated (serving a match), draining.
- **version** — a build. Several can be live at once.
- **match** — one session, tied to one dedic.

## Things that are easy to get wrong

- **Draining never drops a live match.** A draining node or version keeps serving
  the matches it already holds; it just stops taking new ones. "Draining" is not
  an incident.
- **Two versions live at once is normal**, not a failed deploy: a rollout lets the
  previous version drain as its matches end.
- **Players do not connect through the master.** They talk straight to the dedic's
  host:port, so a master restart never interrupts a live match — and master metrics
  say nothing about in-match network quality.
- **A ready buffer that looks "wasteful" is the product.** Warm dedics are what make
  allocation take milliseconds instead of a container start.
- **The master authenticates infrastructure, not players.** ` + "`player_id`" + ` in a
  matchmaking ticket is an opaque string it trusts and never stores.

## Where to start

` + "`fleet_overview`" + ` first, then ` + "`list_events`" + ` for what changed recently,
then ` + "`list_alerts`" + `. Reach for logs and metrics once you know which server or
node you care about.
`

func addMCPPrompts(srv *mcp.Server) {
	srv.AddPrompt(&mcp.Prompt{
		Name:        "triage_incident",
		Title:       "Triage a fleet incident",
		Description: "Walk the fleet from symptom to cause in the order that wastes the least time.",
		Arguments: []*mcp.PromptArgument{
			{Name: "symptom", Description: "What was noticed, in the reporter's own words.", Required: true},
			{Name: "project", Description: "Project slug, if known."},
		},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		symptom := req.Params.Arguments["symptom"]
		project := req.Params.Arguments["project"]
		scope := "Ask list_projects first if you do not know which project this is about."
		if project != "" {
			scope = "Narrow every call to project " + project + "."
		}
		return &mcp.GetPromptResult{
			Description: "Incident triage",
			Messages: []*mcp.PromptMessage{{
				Role: "user",
				Content: &mcp.TextContent{Text: fmt.Sprintf(`Triage this on the birdman fleet: %s

%s

Work in this order and stop as soon as the cause is clear:
1. fleet_overview — is this fleet-wide or local?
2. list_alerts, then list_alert_mutes — a muted alert can hide the very thing you are looking for.
3. list_events — what changed just before the symptom appeared? Deploys and node state changes first.
4. Narrow: list_nodes and list_servers, to one node or one dedic.
5. Only then get_server_logs or query_logs.

Report the cause, the evidence you actually saw, and what you did NOT check.
Do not infer in-match network quality from master-side metrics: players connect
straight to the dedic, so that traffic never passes through here.`, symptom, scope)},
			}},
		}, nil
	})

	srv.AddPrompt(&mcp.Prompt{
		Name:        "preflight_deploy",
		Title:       "Check the fleet before a deploy",
		Description: "Decide whether now is a good moment to roll a build out.",
		Arguments: []*mcp.PromptArgument{
			{Name: "version_id", Description: "Version about to be deployed.", Required: true},
			{Name: "project", Description: "Project slug, if known."},
		},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		version := req.Params.Arguments["version_id"]
		project := req.Params.Arguments["project"]
		scope := ""
		if project != "" {
			scope = " in project " + project
		}
		return &mcp.GetPromptResult{
			Description: "Deploy preflight",
			Messages: []*mcp.PromptMessage{{
				Role: "user",
				Content: &mcp.TextContent{Text: fmt.Sprintf(`Decide whether to deploy version %s%s right now.

Check, in this order:
1. list_versions — is the version registered, and has it finished pre-pulling across the fleet?
2. list_nodes — how many nodes are active, and is any of them draining?
3. fleet_overview — how many matches are in flight, and how deep is the warm buffer?
4. list_alerts — is anything already broken? Deploying onto a firing fleet hides which change caused what.

Answer go / no-go with the numbers you saw. If it is a go, say plainly that the
previous version will keep serving its matches until they end — that is expected,
not a stuck rollout.`, version, scope)},
			}},
		}, nil
	})
}
