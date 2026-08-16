package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/store"
)

// --- nodes ---

type createNodeRequest struct {
	Project       string         `json:"project"`
	Region        string         `json:"region"`
	Hostname      string         `json:"hostname"`
	PublicIP      string         `json:"public_ip"`
	CapacitySlots int32          `json:"capacity_slots"`
	Labels        map[string]any `json:"labels"`
	// Env — необязательное окружение регистрации (w2); пусто → "dev". Нужен, когда
	// dev у проекта удалён (после seed-on-insert он больше не воскресает) или когда
	// ноду сразу заводят в prod, не гоняя её через PATCH /v1/nodes/{id}.
	Env string `json:"env,omitempty"`
}

func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	node, token, err := s.st.CreateNode(r.Context(), store.CreateNodeParams{
		Project:       req.Project,
		Region:        req.Region,
		Hostname:      req.Hostname,
		PublicIP:      req.PublicIP,
		CapacitySlots: req.CapacitySlots,
		Labels:        req.Labels,
		Env:           req.Env,
	})
	// Всё, что отдаёт CreateNode (валидация полей, ErrBadEnv) — плохой ввод: 400.
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"node": node,
		// Shown exactly once; only a bcrypt hash is stored.
		"node_token": token,
	})
}

// handleListNodes is GET /v1/nodes?project=&env= (readonly). Оба фильтра
// необязательны; для глобального ключа пустые = весь флот (поведение до
// мультипроекта W2), слаг валидируется по БД — опечатка даёт 400, а не молча
// суженный флот (tracker #961). Привязанный ключ видит ТОЛЬКО ноды своей пары,
// в том числе без параметров вовсе (tenantScope, #993): раньше отсюда утекали
// hostname и public_ip чужих нод.
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.tenantScope(w, r, true)
	if !ok {
		return
	}
	nodes, err := s.st.ListNodes(r.Context(), store.NodeFilter{
		Project: project,
		Env:     env,
	})
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nodesResp{Nodes: emptyNotNull(nodes)})
}

// --- servers ---

// handleListServers is GET /v1/servers?project=&region=&state= (readonly).
// `project` валидируется по БД (tracker #961), привязанный ключ сужается до
// своей пары (tenantScope, #993). `?env=` ручка НЕ принимает и не начинает
// принимать здесь — env приходит только из привязки, поэтому передаётся в
// фильтр стора, а не читается из query.
func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.tenantScope(w, r, false)
	if !ok {
		return
	}
	servers, err := s.st.ListServers(r.Context(), store.ServerFilter{
		Project: project,
		Env:     env,
		Region:  r.URL.Query().Get("region"),
		State:   r.URL.Query().Get("state"),
	})
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, serversResp{Servers: emptyNotNull(servers)})
}

// --- versions ---

type createVersionRequest struct {
	Project  string `json:"project"`
	Semver   string `json:"semver"`
	ImageRef string `json:"image_ref"`
	Env      string `json:"env"`
}

func (s *Server) handleCreateVersion(w http.ResponseWriter, r *http.Request) {
	var req createVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Binding (environments v1 §5): a bound key defaults its project (so CI can
	// POST just {semver, image_ref, env}) and may register only in its own
	// (project, env) — enforced before the version is created.
	project, ok := bindProjectGate(w, r, req.Project)
	if !ok {
		return
	}
	// Пустоту env проверяем ДО binding-guard (w10, паритет с handleUpsertFleet):
	// bound-CI без поля env должен получить «env is required» (400), а не «key is
	// bound to …» (403). project уже дефолтнут из привязки строкой выше.
	if req.Env == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "env is required")
		return
	}
	// Обязательные поля тела проверяем ЗДЕСЬ, а не полагаемся на плоскую ошибку
	// стора (v3): ниже не-sentinel ошибка = 500, поэтому клиентский ввод обязан
	// отсекаться на входе, иначе пустой semver уехал бы в «internal».
	if req.Semver == "" || req.ImageRef == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "semver and image_ref are required")
		return
	}
	// project — тот же класс клиентского ввода (M1 ревью follow-up): непривязанный
	// ключ без поля project иначе уехал бы в «internal» через плоскую ошибку
	// ensureProject.
	if project == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "project is required")
		return
	}
	if !s.requireBinding(w, r, project, req.Env) {
		return
	}
	v, err := s.st.CreateVersion(r.Context(), store.CreateVersionParams{
		Project:  project,
		Semver:   req.Semver,
		ImageRef: req.ImageRef,
		Env:      req.Env,
	})
	// ErrBadEnv → 400 «no such environment …», ErrConflict → 409, всё остальное →
	// 500 (v3): раньше здесь стоял безусловный 400 и он глотал инфра-сбои стора.
	if err != nil {
		storeError(w, err)
		return
	}
	resp := map[string]any{"version": v}
	// Авто-деплой dev-потока (environments v1 §4): регистрация в auto_deploy-env
	// немедленно гонит цепочку «только вперёд». TryAutoDeploy сам no-op'ит на
	// не-auto env (prod → AutoDeployNoop), поэтому зовём безусловно и добавляем
	// поле лишь на реальном авто-пути. Синхронно, как ручной deploy: BeginDeploy
	// быстрый, а prepull-Send неблокирующий.
	switch s.dep.TryAutoDeploy(r.Context(), project, req.Env) {
	case deploy.AutoDeployStarted:
		resp["auto_deploy"] = "started"
	case deploy.AutoDeployQueued:
		resp["auto_deploy"] = "queued"
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleListVersions is GET /v1/versions?project=&env= (readonly). Оба фильтра
// необязательны; для глобального ключа пустые = все версии (поведение до
// мультипроекта W2), слаг валидируется по БД (tracker #961). Привязанный ключ
// видит только версии своей пары (tenantScope, #993) — отсюда утекали
// `image_ref` чужих сборок.
func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.tenantScope(w, r, true)
	if !ok {
		return
	}
	versions, err := s.st.ListVersions(r.Context(), store.VersionFilter{
		Project: project,
		Env:     env,
	})
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, versionsResp{Versions: emptyNotNull(versions)})
}

// --- fleets ---

type upsertFleetRequest struct {
	Project       string  `json:"project"`
	Env           string  `json:"env"`
	ActiveVersion *string `json:"active_version"`
	BufferReady   *int32  `json:"buffer_ready"`
	MaxServers    *int32  `json:"max_servers"`
	ReapTTLMin    *int32  `json:"reap_ttl_min"`
}

func (s *Server) handleUpsertFleet(w http.ResponseWriter, r *http.Request) {
	var req upsertFleetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// env обязателен (I3) — внешних потребителей PUT /v1/fleets нет, без фоллбека.
	if req.Env == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "env is required")
		return
	}
	if req.ActiveVersion != nil {
		if _, err := uuid.Parse(*req.ActiveVersion); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "active_version must be a version id (uuid)")
			return
		}
	}
	// Binding (environments v1 §5): a bound key defaults its project and may
	// configure only its own (project, env). The route is admin-scoped and admin
	// keys are never bound, so this is defense in depth against an out-of-band
	// bound-admin row (TestAPIKeyBindingFleetGuard).
	project, ok := bindProjectGate(w, r, req.Project)
	if !ok {
		return
	}
	if !s.requireBinding(w, r, project, req.Env) {
		return
	}
	f, err := s.st.UpsertFleet(r.Context(), store.UpsertFleetParams{
		Project:       project,
		Env:           req.Env,
		Region:        r.PathValue("region"),
		ActiveVersion: req.ActiveVersion,
		BufferReady:   req.BufferReady,
		MaxServers:    req.MaxServers,
		ReapTTLMin:    req.ReapTTLMin,
	})
	// Bad env / active_version не из (project, env) — это некорректный ввод PUT,
	// а не отсутствующий ресурс: понятный 400 (design §2/§10, FK-ошибки → 400,
	// не 500 и не 404). active_version-ошибка несёт ErrNotFound-семантику на
	// уровне store, но на HTTP это 400.
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Первый флот auto_deploy-env разблокирует зависшую цепочку (environments v1
	// §4/§6, W2-реестр): версия, зарегистрированная в auto_deploy-env ДО появления
	// флота, получила AutoDeployQueued (ErrNoFleet, отметка не двигалась) — теперь
	// катим её. Безусловный вызов: TryAutoDeploy сам no-op'ит на не-auto env и на
	// исчерпанной цепочке. Результат в ответ PUT /v1/fleets не выносим (админ-роут,
	// не CI-регистрация) — состояние видно в Deploys/событиях.
	s.dep.TryAutoDeploy(r.Context(), project, req.Env)
	resp := map[string]any{"fleet": f}
	// Третий (и на сегодня последний) путь смены активной версии — этот
	// bootstrap/ops-override (tracker #1088). Он идёт мимо deploy-менеджера
	// прямым UPSERT'ом, поэтому сигнал «катить некуда» приходится звать здесь.
	// Предупреждаем ТОЛЬКО когда версию переставляли ЯВНО: сам UPSERT без
	// active_version никакой версии не включает — на апдейте `coalesce`
	// оставляет текущую, на вставке нового флота колонка получает NULL
	// (store.UpsertFleet).
	//
	// Но «PUT без active_version активную версию не меняет» было бы шире правды,
	// и вот граница: строкой выше стоит БЕЗУСЛОВНЫЙ TryAutoDeploy, а он на
	// auto_deploy-env уходит в startJob и флип делает — то есть буферный PUT
	// тоже способен переставить версию при нуле живых нод. Молчания там нет:
	// этот путь и есть исходная воронка #1071, событие deploy_no_nodes пишет
	// она сама. Не доезжает только поле в ОТВЕТЕ — результат TryAutoDeploy сюда
	// не выносится сознательно (строкой выше), и менять это здесь не будем.
	//
	// СОБЫТИЯ здесь НЕТ, и это асимметрия по решению, а не пропуск. Во-первых,
	// вид события — deploy_no_nodes, а PUT флота деплоем не является. Во-вторых
	// и главное: bootstrap ПО ПОРЯДКУ идёт раньше регистрации нод, так что ноль
	// живых нод — штатное состояние этого вызова, и событие горело бы на
	// НОРМАЛЬНОМ пути, обучая оператора его игнорировать. Предупреждение в
	// ответе этой цены не несёт: оно адресовано ровно тому, кто здесь бывает
	// (curl/ops — панель этот путь не выставляет), и молчит, когда ноды есть.
	if req.ActiveVersion != nil {
		if warn := s.dep.NoNodesWarning(r.Context(), f.ProjectID, f.Env, *req.ActiveVersion); warn != "" {
			resp["warning"] = warn
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- events ---

// handleListEvents is GET /v1/events?project=&limit= (readonly). Слаг
// валидируется по БД (#961): опечатка → 400, а не молча суженная лента.
// Привязанный ключ сужается до своего проекта (tenantScope, #993).
//
// ГЕЙТ ПЕРВЫМ, до разбора `?limit=`: иначе `?limit=abc&project=<чужой>` отвечал
// бы 400 вместо 403 — не оракул сам по себе (limit о тенанте ничего не знает),
// но правило «гейт настолько рано, насколько позволяет адресация» (#989) стоит
// держать буквально, чтобы следующий не выводил из порядка исключений.
//
// ДВЕ ГРАНИЦЫ этого сужения, названные явно:
//   - у событий НЕТ измерения env (колонка `events.project_id` есть с миграции
//     000019, `env` — нет), поэтому привязанный ключ видит события своего
//     проекта по ВСЕМ его окружениям, а не только по своей паре;
//   - фильтр остаётся НЕ СКРЫВАЮЩИМ (эпик #968): платформенное событие ПО
//     РОЖДЕНИЮ — бекапы, CA, сессии панели, у которых нет ни `project_id`, ни
//     снимка слага — видно под любым фильтром, в том числе привязанному ключу.
//     Это та же осознанная сторона, что у алертов (#995) и у пика CCU (I5):
//     показать лишнее безопаснее, чем спрятать «мастер лежит». Одного
//     `project_id is null` для «платформенного» НЕ ДОСТАТОЧНО (tracker #1083):
//     FK объявлен `on delete set null`, поэтому у истории удалённого проекта
//     project_id обнуляется — её отсекает снимок слага (миграция 000020,
//     store.ListEvents), и привязанному ключу соседа она больше не приезжает.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	project, _, ok := s.tenantScope(w, r, false)
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
	events, err := s.st.ListEvents(r.Context(), limit, project)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, eventsResp{Events: emptyNotNull(events)})
}

// --- allocation (docs/specs/master.md §3) ---

type allocateRequest struct {
	Project   string  `json:"project"`
	Env       string  `json:"env,omitempty"`
	Region    string  `json:"region"`
	VersionID *string `json:"version_id"`
	MatchID   string  `json:"match_id"`

	// Per-match payload for the dedik, delivered verbatim as liba's
	// `allocated.metadata` (protocol.md §2 — the field the SDK has carried
	// since the v0 freeze). The external matchmaker's channel for a join
	// secret, a mode, a map name. Caps below keep the agentlink frame small;
	// the agent and this master never read the contents.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// The allocate metadata caps: enough for a join secret and a handful of knobs,
// small enough that the frame stays a frame. Violations are a 400 with the
// offending key named — a truncated secret admitted nobody.
const (
	maxAllocMetadataKeys     = 16
	maxAllocMetadataKeyLen   = 64
	maxAllocMetadataValueLen = 512
)

func validateAllocMetadata(md map[string]string) string {
	if len(md) > maxAllocMetadataKeys {
		return fmt.Sprintf("metadata: at most %d keys", maxAllocMetadataKeys)
	}
	for k, v := range md {
		if k == "" || len(k) > maxAllocMetadataKeyLen {
			return fmt.Sprintf("metadata: key %q must be 1..%d bytes", k, maxAllocMetadataKeyLen)
		}
		if len(v) > maxAllocMetadataValueLen {
			return fmt.Sprintf("metadata: value of %q exceeds %d bytes", k, maxAllocMetadataValueLen)
		}
	}
	return ""
}

func (s *Server) handleAllocate(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var req allocateRequest
	if !decodeJSON(w, r, &req) {
		// The body never parsed — the project is genuinely unknown here, so the
		// label is empty rather than guessed (an empty label reads as absent, i.e.
		// the resulting alert stays platform-scoped and is never hidden by a
		// project filter). Every later site knows req.Project.
		s.m.AllocFailures.WithLabelValues("bad_request", "").Inc()
		return
	}
	if req.Project == "" || req.Region == "" {
		s.m.AllocFailures.WithLabelValues("bad_request", req.Project).Inc()
		writeError(w, http.StatusBadRequest, "bad_request", "project and region are required")
		return
	}
	if _, err := uuid.Parse(req.MatchID); err != nil {
		s.m.AllocFailures.WithLabelValues("bad_request", req.Project).Inc()
		writeError(w, http.StatusBadRequest, "bad_request", "match_id must be a uuid")
		return
	}
	if req.VersionID != nil {
		if _, err := uuid.Parse(*req.VersionID); err != nil {
			s.m.AllocFailures.WithLabelValues("bad_request", req.Project).Inc()
			writeError(w, http.StatusBadRequest, "bad_request", "version_id must be a uuid")
			return
		}
	}
	if problem := validateAllocMetadata(req.Metadata); problem != "" {
		s.m.AllocFailures.WithLabelValues("bad_request", req.Project).Inc()
		writeError(w, http.StatusBadRequest, "bad_request", problem)
		return
	}

	// Environment resolution (environments v1 §3, I4): explicit field → the key's
	// binding → the sole env with ready servers in the region → 409 env_required.
	// A global allocate key must not claim a server of a random env.
	env := req.Env
	if env == "" {
		// Шаг привязки ключа (W-I2): bound(dev)-ключ без поля env резолвит env из
		// своей привязки — иначе при двух env с ready он ловил бы 409 env_required
		// от sole-фоллбека (или 403 от собственного enforcement в чужом env).
		// Enforcement по резолвнутому env остаётся ниже (requireBinding).
		if key, ok := keyFromContext(r.Context()); ok && key.Env != nil {
			env = *key.Env
		}
	}
	// version_id несёт своё окружение (T5): env-less allocate с явной версией
	// резолвит env ИЗ НЕЁ (GetVersion → env) ДО sole-with-ready — версия
	// однозначно указывает env, гадать по готовому пулу незачем (и это не 409
	// env_required там, где версия уже всё сказала). Неизвестная версия → та же
	// нехватка ёмкости, что и claim по несуществующей версии (no_capacity), не
	// сбивающая контракт на 404/500.
	if env == "" && req.VersionID != nil {
		v, err := s.st.GetVersion(r.Context(), *req.VersionID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.writeAllocNoCapacity(w, r, req)
			return
		case err != nil:
			s.m.AllocFailures.WithLabelValues("internal", req.Project).Inc()
			storeError(w, err)
			return
		}
		env = v.Env
	}
	if env == "" {
		resolved, err := s.st.SoleEnvWithReady(r.Context(), req.Project, req.Region)
		switch {
		case errors.Is(err, store.ErrNoCapacity):
			// Env-less allocate над ПУСТЫМ пулом: не двусмысленность, а нехватка
			// ёмкости — no_capacity как до волны env (резолвить было нечего), а не
			// env_required. Тот же 409 + метрика + событие, что и на claim-пути ниже.
			s.writeAllocNoCapacity(w, r, req)
			return
		case errors.Is(err, store.ErrConflict):
			// 409-ambiguous env (several envs with ready servers): its own
			// reason label so AllocationFailures dashboards separate «operator
			// must name env» from malformed requests (environments v1 §7). The
			// BufferEmpty rule filters {reason="no_capacity"}, so it is untouched.
			s.m.AllocFailures.WithLabelValues("env_required", req.Project).Inc()
			writeError(w, http.StatusConflict, "env_required", err.Error())
			return
		case err != nil:
			s.m.AllocFailures.WithLabelValues("internal", req.Project).Inc()
			storeError(w, err)
			return
		}
		env = resolved
	}
	// Enforcement (environments v1 §5): a bound key may allocate only in its own
	// (project, env); a global key passes.
	if !s.requireBinding(w, r, req.Project, env) {
		return
	}

	// players_expected 0 = unknown: the external matchmaker behind this API
	// does not report the match size (spec'd request shape, master.md §3).
	alloc, err := s.st.Allocate(r.Context(), req.Project, env, req.Region, req.VersionID, req.MatchID, 0, req.Metadata)
	switch {
	case errors.Is(err, store.ErrNoCapacity):
		s.writeAllocNoCapacity(w, r, req)
		return
	case errors.Is(err, store.ErrNotFound):
		s.m.AllocFailures.WithLabelValues("bad_request", req.Project).Inc()
		storeError(w, err)
		return
	case err != nil:
		s.m.AllocFailures.WithLabelValues("internal", req.Project).Inc()
		storeError(w, err)
		return
	}
	s.m.AllocDuration.Observe(time.Since(started).Seconds())
	writeJSON(w, http.StatusOK, alloc)
}

// writeAllocNoCapacity emits the shared no_capacity outcome (409
// {"error":"no_capacity"} + AllocFailures{no_capacity} + allocation_failed
// event). Both the empty-pool env fallback (env-less allocate, zero ready) and
// the claim path that finds no server land here — one place, one contract.
func (s *Server) writeAllocNoCapacity(w http.ResponseWriter, r *http.Request, req allocateRequest) {
	s.m.AllocFailures.WithLabelValues("no_capacity", req.Project).Inc()
	payload := map[string]any{
		"project": req.Project, "region": req.Region, "reason": "no_capacity",
	}
	if req.VersionID != nil {
		payload["version_id"] = *req.VersionID
	}
	mid := req.MatchID
	if evErr := s.st.InsertEvent(r.Context(), store.EventAllocationFailed,
		store.EventRef{MatchID: &mid}, payload); evErr != nil {
		s.log.Error("allocate: event write failed", "err", evErr)
	}
	writeJSON(w, http.StatusConflict, map[string]string{"error": "no_capacity"})
}

// emptyNotNull keeps JSON arrays as [] instead of null.
func emptyNotNull[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
