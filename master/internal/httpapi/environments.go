package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// Environments API (docs/superpowers/specs/2026-07-13-environments-v1-design.md
// §2). Read is readonly-scoped; create/patch/delete and the node-move PATCH are
// admin. Guardrail production⇒!auto_deploy and the name shape/reserved rules are
// validated in the store (clean 400s), backed by DB CHECKs.
//
// Удаление окружения («удаление непустого окружения»): GET .../usage отдаёт состав
// (панель показывает его в диалоге), DELETE сносит окружение ВМЕСТЕ с содержимым —
// но только при нуле нод (иначе 409) и с подтверждением вводом имени в теле
// {"confirm":"<name>"} (иначе 400). Пустое окружение удаляется как раньше: 204 без
// тела и без confirm.

type createEnvironmentRequest struct {
	Project       string `json:"project"`
	Name          string `json:"name"`
	Production    bool   `json:"production"`
	AutoDeploy    bool   `json:"auto_deploy"`
	RetentionKeep int    `json:"retention_keep"`
}

// deleteEnvironmentRequest — тело DELETE. Необязательное: у пустого окружения его
// нет вовсе (совместимость), у непустого confirm обязан ТОЧНО совпасть с именем.
type deleteEnvironmentRequest struct {
	Confirm string `json:"confirm"`
}

// patchEnvironmentRequest is the PATCH body — every field a pointer so an absent
// field means "leave unchanged". Name is immutable (not a field).
type patchEnvironmentRequest struct {
	Production    *bool `json:"production"`
	AutoDeploy    *bool `json:"auto_deploy"`
	RetentionKeep *int  `json:"retention_keep"`
}

type setNodeEnvRequest struct {
	Env string `json:"env"`
}

// handleListEnvironments is GET /v1/environments?project= (readonly). Для
// глобального ключа поведение прежнее: project резолвится в единственный проект
// при пустом параметре (single-project convention), явный слаг валидируется по
// БД (tracker #961) — раньше опечатка отдавала пустой список окружений,
// неотличимо от «у проекта их нет». Резолв проверять незачем — такой проект
// существует по построению.
//
// Привязанный ключ (tenantScope, #993) получает ровно ОДНО окружение — своё:
// проект берётся из привязки (sole-резолв ему не нужен и на мультипроектной
// платформе больше не даёт `400 project is required`), а список фильтруется до
// имени пары. Фильтр здесь, а не в сторе, потому что сужение до одного имени —
// свойство ГРАНИЦЫ, а не запроса: `ListEnvironments` остаётся «все окружения
// проекта» для всех остальных вызывающих.
//
// Именно эта ручка была главным перечислителем чужих имён окружений: #989
// закрыл `GET /v1/environments/{project}/{name}/usage` пар-точным гейтом, но
// имена соседних окружений (и своего проекта, и чужого) отдавал этот листинг.
// Теперь пар-точность одинакова на обеих ручках.
func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.tenantScope(w, r, false)
	if !ok {
		return
	}
	if project == "" {
		p, err := s.st.SoleProjectSlug(r.Context())
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "project is required: "+err.Error())
			return
		}
		project = p
	}
	envs, err := s.st.ListEnvironments(r.Context(), project)
	if err != nil {
		storeError(w, err)
		return
	}
	if env != "" {
		envs = keepEnvironment(envs, env)
	}
	writeJSON(w, http.StatusOK, environmentsResp{Environments: emptyNotNull(envs)})
}

// keepEnvironment оставляет окружение с этим именем (0 или 1 строка: имя
// уникально внутри проекта). Вызывается только для привязанного ключа.
func keepEnvironment(envs []store.Environment, name string) []store.Environment {
	out := make([]store.Environment, 0, 1)
	for _, e := range envs {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// handleCreateEnvironment is POST /v1/environments (admin). 201 on success;
// 409 for a duplicate; 400 for a guardrail/name/FK/CHECK violation (the store
// validates production⇒!auto_deploy and the name shape/reserved rules).
func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request) {
	var req createEnvironmentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	env, err := s.st.CreateEnvironment(r.Context(), store.CreateEnvironmentParams{
		Project:       req.Project,
		Name:          req.Name,
		Production:    req.Production,
		AutoDeploy:    req.AutoDeploy,
		RetentionKeep: req.RetentionKeep,
	})
	if errors.Is(err, store.ErrConflict) {
		storeError(w, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, environmentResp{Environment: env})
}

// handlePatchEnvironment is PATCH /v1/environments/{project}/{name} (admin).
// 200 on success; 404 for an unknown env; 400 when the resulting state breaks
// the production⇒!auto_deploy guardrail (in any field order).
func (s *Server) handlePatchEnvironment(w http.ResponseWriter, r *http.Request) {
	var req patchEnvironmentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	env, err := s.st.PatchEnvironment(r.Context(), r.PathValue("project"), r.PathValue("name"),
		store.EnvironmentPatch{
			Production:    req.Production,
			AutoDeploy:    req.AutoDeploy,
			RetentionKeep: req.RetentionKeep,
		})
	if errors.Is(err, store.ErrNotFound) {
		storeError(w, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, environmentResp{Environment: env})
}

// handleEnvironmentUsage is GET /v1/environments/{project}/{name}/usage (readonly):
// сколько версий/флотов/нод/серверов/матчей/живых ключей держит окружение. Панель
// зовёт его при открытии диалога удаления: показать состав и заблокировать
// удаление, пока в окружении есть ноды. 404 для неизвестного окружения.
//
// Привязка ключа ЭНФОРСИТСЯ (#989) — третья ручка того же класса, что закрыли
// #974 (чтение матча) и #988 (логи дедика): скоуп readonly, а адресуется ручка
// парой (project, name), поэтому привязанный ключ проекта А читал состав
// окружения проекта Б, просто подставив чужой слаг — а слаги перечисляет
// `GET /v1/projects` того же скоупа.
//
// ПОРЯДОК ЗДЕСЬ ОБРАТНЫЙ #988 — гейт ДО резолва, и это осознанно, а не по
// невнимательности. В #988 пара (project, env) была известна только ПОСЛЕ
// похода в стор за сервером по uuid, поэтому «резолв → гейт» был там
// единственным возможным порядком. Здесь пара — САМ АДРЕС, гейту БД не нужна
// вовсе, и потому отказ формируется РАНЬШЕ, чем о существовании окружения
// что-либо известно: чужой ключ получает байт-в-байт один и тот же 403 и на
// живое окружение, и на выдуманное, и на выдуманный проект.
//
// 403, а не 404 в стиле requireTicketBinding (#963). Осторожно с причинностью:
// оракул существования, который у #974/#988 есть (чужому ключу существующий
// объект отвечает 403, несуществующий 404), следует НЕ из вынужденного порядка,
// а из выбранного кода — #963 сидит в такой же вынужденной позиции и оракула не
// имеет, потому что отвечает 404 тем же телом, что и честный промах. Тот 404
// покупал неотличимость дёшево: тикет адресуется непредсказуемым uuid. Здесь
// неотличимость уже дана порядком, покупать её кодом незачем, а `403 key is
// bound to X/Y` — честный ответ своему же оператору, промахнувшемуся окружением.
//
// ГРАНИЦЫ (переписано #993). Когда #989 закрыл эту ручку, имена чужих окружений
// по-прежнему выдавали ЛИСТИНГИ с ?project= (`/v1/environments`, `/v1/nodes`
// полем env, …), а сама валидация ?env= (#971) работала оракулом имён
// (`?project=X&env=ghost` → 400 «no such environment»). Решением владельца по
// #993 привязка стала арендаторской границей на ЧТЕНИЯХ: листинги сужаются
// привязкой (tenantScope), оракул ?env= для привязанного ключа закрыт. Так что
// enumerability чужих окружений закрыта теперь не «только на этой ручке», а на
// классе — но ровно для ПРИВЯЗАННОГО ключа: глобальный видит платформу по
// построению, и это не дефект, а определение глобального ключа.
func (s *Server) handleEnvironmentUsage(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("name")
	if !s.requireBinding(w, r, project, name) {
		return
	}
	usage, err := s.st.EnvironmentUsage(r.Context(), project, name)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, environmentUsageResp{Usage: usage})
}

// handleDeleteEnvironment is DELETE /v1/environments/{project}/{name} (admin).
// Тело необязательное: {"confirm":"<name>"}.
//
//   - 404 — неизвестное окружение;
//   - 409 — в окружении есть ноды (предусловие; НЕЗАВИСИМО от confirm: сначала
//     переведите их в другое окружение, PATCH /v1/nodes/{id});
//   - 204 — окружение пустое (никогда не использовалось): удалено, confirm не нужен;
//   - 400 — окружение НЕПУСТОЕ, а confirm отсутствует/не равен имени ТОЧНО;
//   - 200 {"deleted": {...}} — непустое окружение снесено каскадом (состав в теле).
func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	var req deleteEnvironmentRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	res, err := s.st.DeleteEnvironment(r.Context(), r.PathValue("project"), r.PathValue("name"), req.Confirm)
	if errors.Is(err, store.ErrConfirmRequired) {
		writeError(w, http.StatusBadRequest, "bad_request", "confirm must equal the environment name")
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	// Отозванные ключи обязаны умереть сразу: authenticator кэширует проверенные
	// ключи (и держит сессии панели) — гасим кэш, как это делает revoke-хендлер.
	for _, id := range res.RevokedKeyIDs {
		s.auth.invalidateKey(id)
	}
	if res.WasEmpty {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, environmentDeletedResp{Deleted: res})
}

// handleSetNodeEnv is PATCH /v1/nodes/{id} {env} (admin): move a node to another
// environment. 200 with the updated node; 400 for a non-uuid id, a missing env or
// an unknown env (ErrBadEnv — окружение здесь ССЫЛКА в теле, опечатка в ней это
// плохой ввод, v3); 404 for an unknown node (адресуемый ресурс); 409 when the node
// is dead or carries live servers (drain it first). Emits node_env_changed.
func (s *Server) handleSetNodeEnv(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "node id must be a uuid")
		return
	}
	var req setNodeEnvRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Env == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "env is required")
		return
	}
	node, err := s.st.SetNodeEnv(r.Context(), id, req.Env)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nodeResp{Node: node})
}
