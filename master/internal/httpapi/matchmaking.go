package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/store"
)

// Matchmaking endpoints (docs/specs/master.md §4/§6, protocol.md §3).
// Transport: long-poll `?wait=25s`; SSE stays panel-only (later iteration).

// maxWait caps GET ?wait= (spec suggests 25s; anything longer risks
// intermediary timeouts).
const maxWait = 30 * time.Second

type submitTicketRequest struct {
	Project       string                  `json:"project,omitempty"`
	Env           string                  `json:"env,omitempty"`
	PlayerID      string                  `json:"player_id"`
	ClientVersion string                  `json:"client_version"`
	Regions       []matchmaker.RegionPing `json:"regions"`
}

func (s *Server) handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	var req submitTicketRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.PlayerID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "player_id is required")
		return
	}
	if !s.mmLimit.allow(req.PlayerID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"matchmaking rate limit: 5 rps per player_id")
		return
	}
	// Binding (environments v1 §3/§5): a bound key defaults AND constrains both
	// project and env; an explicit field that disagrees with the binding is
	// refused (403) here, so the matchmaker never learns about keys. A global key
	// passes its request fields through unchanged (sole-project/sole-env resolve
	// downstream in Submit).
	project, ok := bindProjectGate(w, r, req.Project)
	if !ok {
		return
	}
	env := req.Env
	if env == "" {
		if key, ok := keyFromContext(r.Context()); ok && key.Env != nil {
			env = *key.Env
		}
	}
	if !s.requireBinding(w, r, project, env) {
		return
	}
	t, err := s.mm.Submit(r.Context(), matchmaker.SubmitParams{
		Project:       project,
		Env:           env,
		PlayerID:      req.PlayerID,
		ClientVersion: req.ClientVersion,
		Regions:       req.Regions,
	})
	switch {
	case errors.Is(err, matchmaker.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case errors.Is(err, store.ErrBadEnv), errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrConflict):
		// Unknown project / unknown env (ErrBadEnv, v3) / ambiguous default project.
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// ticketNotFound is the single answer for «no such ticket» — both when the id is
// genuinely unknown and when the ticket belongs to another (project, env) than
// the request key is bound to (requireTicketBinding). One byte-identical body,
// by design: see the comment there.
func ticketNotFound(w http.ResponseWriter, id string) {
	writeError(w, http.StatusNotFound, "not_found", "ticket "+id+" not found")
}

// requireTicketBinding enforces the request key's (project, env) binding against
// an EXISTING ticket — the read/cancel counterpart of requireBinding
// (environments v1 §5, tracker #963). Returns true when the request may proceed;
// otherwise it writes the answer and returns false. A global (unbound) key
// passes everywhere, exactly as before: binding is optional by design
// (NULL/NULL = global key), and the game backend that never bound its key must
// keep working.
//
// 404, NOT the 403 requireBinding writes on the deploy surface, and with the
// very same body a genuinely unknown id gets: a 403 «key is bound to X/Y» would
// answer «this ticket exists, it is just not yours» and turn the handle into an
// existence oracle over foreign tickets keyed by uuid. Ownership of a ticket
// inside one's own (project, env) is still not checked at all — that is a
// deliberate v0 gap (architecture.md, «Модель доверия»); this only restores the
// (project, env) containment the binding promises.
//
// ORDER MATTERS: callers must run this BEFORE the per-player_id rate limiter.
// The limiter is keyed by the TICKET's player_id, i.e. by the victim — checking
// it first would (a) let a foreign caller burn someone else's budget knowing
// only a ticket_id, and (b) leak existence anyway, since an unknown uuid never
// touches a bucket while a foreign ticket would start answering 429 instead of
// 404 on the sixth request.
func (s *Server) requireTicketBinding(w http.ResponseWriter, r *http.Request, t matchmaker.Ticket) bool {
	key, _ := keyFromContext(r.Context())
	if keyAllowed(key, t.Project, t.Env) {
		return true
	}
	ticketNotFound(w, t.ID)
	return false
}

func (s *Server) handleGetTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := s.mm.Get(id)
	if !ok {
		ticketNotFound(w, id)
		return
	}
	if !s.requireTicketBinding(w, r, t) {
		return
	}
	if !s.mmLimit.allow(t.PlayerID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"matchmaking rate limit: 5 rps per player_id")
		return
	}

	wait := time.Duration(0)
	if raw := r.URL.Query().Get("wait"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				"wait must be a duration like 25s")
			return
		}
		wait = min(d, maxWait)
	}

	// The (project, env) of a ticket is immutable and Wait returns the same id,
	// so the binding checked above still holds — no re-check needed.
	t, ok = s.mm.Wait(r.Context(), id, wait)
	if !ok {
		ticketNotFound(w, id)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleCancelTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := s.mm.Get(id)
	if !ok {
		ticketNotFound(w, id)
		return
	}
	if !s.requireTicketBinding(w, r, t) {
		return
	}
	if !s.mmLimit.allow(t.PlayerID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"matchmaking rate limit: 5 rps per player_id")
		return
	}
	t, ok = s.mm.Cancel(id)
	if !ok {
		ticketNotFound(w, id)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleQoS lists per-region ping targets (public by design, master.md §6).
// The UDP echo responder on nodes ships with the agent in iteration 4; the
// endpoint already returns the correct host list of live nodes.
func (s *Server) handleQoS(w http.ResponseWriter, r *http.Request) {
	// Public endpoint: project and env are optional query params (environments
	// v1 §3, M8). project falls back to the sole project; env to the sole
	// environment with active nodes — ambiguous env → 400 env_required.
	//
	// This is the one read where ?project= is deliberately NOT validated against
	// the DB (tracker #961 draws the line here): the endpoint is unauthenticated,
	// so a "no such project <slug>" would hand every player a free oracle over
	// project slugs. Elsewhere the 400 tells an authorized caller nothing that
	// readonly GET /v1/projects wouldn't; here there is no such caller. An unknown
	// project simply resolves to an empty QoS list.
	project := r.URL.Query().Get("project")
	if project == "" {
		var err error
		project, err = s.st.SoleProjectSlug(r.Context())
		if err != nil {
			storeError(w, err)
			return
		}
	}
	env := r.URL.Query().Get("env")
	if env == "" {
		// Зеркалим allocate-путь (T5-m1): только ErrConflict (ноль/несколько env с
		// активными нодами) → 400 env_required; реальный сбой БД идёт в storeError,
		// а не маскируется под env_required.
		resolved, err := s.st.SoleEnvWithActiveNodes(r.Context(), project)
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusBadRequest, "env_required",
				"env is required (zero or several environments have active nodes)")
			return
		}
		if err != nil {
			storeError(w, err)
			return
		}
		env = resolved
	}
	eps, err := s.st.ListQoSEndpoints(r.Context(), project, env)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, qosResp{QoS: emptyNotNull(eps)})
}

// --- projects (match_size, docs/specs/master.md §4) ---

type upsertProjectRequest struct {
	MatchSize int32 `json:"match_size"`
}

type createProjectRequest struct {
	Slug      string `json:"slug"`
	MatchSize int32  `json:"match_size"`
}

type deleteProjectRequest struct {
	Confirm string `json:"confirm"`
}

// handleCreateProject is POST /v1/projects (admin) — ЯВНОЕ заведение проекта из
// админки. Отличие от идемпотентного `PUT /v1/projects/{slug}` не косметическое:
// PUT на опечатке в слаге молча перезаписал бы match_size существующего проекта,
// а форма создания обязана сказать «такой уже есть» (409).
//
// 201 с проектом; 400 — плохой слаг или match_size; 409 — слаг занят.
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := s.st.CreateProject(r.Context(), req.Slug, req.MatchSize)
	if errors.Is(err, store.ErrConflict) {
		storeError(w, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, projectResp{Project: p})
}

// handleProjectUsage is GET /v1/projects/{slug}/usage (admin): состав проекта.
// Панель зовёт его при открытии диалога удаления — оператор видит, что именно
// исчезнет, и почему удаление заблокировано, если в проекте остались ноды.
func (s *Server) handleProjectUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.st.ProjectUsage(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectUsageResp{Usage: usage})
}

// handleDeleteProject is DELETE /v1/projects/{slug} (admin), тело
// необязательное: {"confirm":"<slug>"}. Симметрично удалению окружения:
//
//   - 404 — неизвестный проект;
//   - 409 — в проекте есть ЖИВЫЕ ноды (выведенные каскадятся);
//   - 204 — проект пуст: удалён, confirm не нужен;
//   - 400 — проект непустой, а confirm отсутствует или не равен слагу точно;
//   - 200 {"deleted": {...}} — снесён каскадом, состав в теле.
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	var req deleteProjectRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	res, err := s.st.DeleteProject(r.Context(), r.PathValue("slug"), req.Confirm)
	if errors.Is(err, store.ErrConfirmRequired) {
		writeError(w, http.StatusBadRequest, "bad_request", "confirm must equal the project slug")
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	// Отозванные ключи гасим в кэше аутентификатора сразу — иначе ключ
	// удалённого проекта продолжал бы пускать до истечения кэша.
	for _, id := range res.RevokedKeyIDs {
		s.auth.invalidateKey(id)
	}
	// Мьюты проекта каскад уже снял в своей транзакции, но ПОДАВЛЯЕТ на самом
	// деле не строка в БД, а зазеркаленный alertmanager-silence: без этого
	// вызова он продолжал бы глушить sink/Discord по (alertname, region,
	// project) — то есть по слагу, который вот-вот может занять новый арендатор.
	// Дословно то же, что делает handleDeleteAlertMute: best-effort, nil-safe,
	// ответ от AM не зависит — отказ подберёт orphan sweep из amsilence
	// (он решает по id silence'а, а не по матчерам, поэтому нового каскада ему
	// знать не нужно).
	if s.silences != nil {
		for _, mute := range res.RemovedMutes {
			s.silences.MuteDeleted(r.Context(), mute)
		}
	}
	if res.WasEmpty {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, projectDeletedResp{Deleted: res})
}

// handleListProjects is GET /v1/projects (readonly) — the panel's project
// selector reads it (мультипроект W1). Readonly on purpose, unlike the admin
// PUT below: a readonly session must still be able to see WHICH project it is
// looking at, and the payload carries nothing secret (slug, match_size).
//
// ПРИВЯЗАННЫЙ КЛЮЧ ВИДИТ ТОЛЬКО СВОЙ ПРОЕКТ (#993). Раскрытие слагов было
// «известным и принятым» (#961) ровно потому, что привязка тогда НЕ была
// границей на чтениях: перечислять слаги было не страшнее, чем читать по ним
// данные. Решение владельца эту посылку сняло, и оставить перечисление всех
// тенантов значило бы закрыть чтение данных, оставив открытым чтение состава
// платформы — та самая асимметрия «правило есть, но применяется не везде»,
// ради которой заведены #961/#971/#974/#988/#989.
//
// Обоснование 400-не-оракула из `validateProjectParam` (server.go) при этом
// НЕ ломается, хотя и опирается на «`GET /v1/projects` и так перечисляет все
// слаги»: 400 остался достижим только для НЕпривязанного ключа, а он этот
// список и получает целиком. Привязанный до валидации не доходит — tenantScope
// отвечает 403 раньше.
//
// Сужение фильтром, а не запросом: `ListProjects` — «все проекты» для
// остальных вызывающих, а строка привязки существует по FK, так что список из
// одного элемента здесь пустым не бывает.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.st.ListProjects(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	if bp, _, bound := keyBinding(r); bound {
		projects = keepProject(projects, bp)
	}
	writeJSON(w, http.StatusOK, projectsResp{Projects: emptyNotNull(projects)})
}

// keepProject оставляет проект с этим слагом (0 или 1 строка: слаг уникален).
// Вызывается только для привязанного ключа.
func keepProject(projects []store.Project, slug string) []store.Project {
	out := make([]store.Project, 0, 1)
	for _, p := range projects {
		if p.Slug == slug {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleUpsertProject(w http.ResponseWriter, r *http.Request) {
	var req upsertProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := s.st.SetProjectMatchSize(r.Context(), r.PathValue("slug"), req.MatchSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, projectResp{Project: p})
}
