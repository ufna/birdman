package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
)

// Deploy endpoints (итерация 3, docs/specs/master.md §5–6, scope `deploy`):
//
//	POST /v1/deploy   {version_id}               → 202 prepulling | 200 active
//	POST /v1/rollback {project?, env?, region?}  → 200 rolled back (seconds)
//	POST /v1/promote  {version_id, to_env}       → 202 prepulling | 200 active
//	                                               (environments v1 §4)

type deployRequest struct {
	VersionID string `json:"version_id"`
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := uuid.Parse(req.VersionID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "version_id must be a version id (uuid)")
		return
	}
	// Binding (environments v1 §5): the target is the version's own (project,
	// env). Load it first so a key bound to another env is refused (403) without
	// side effects; an unknown version stays a 404 (same as Deploy would report).
	// Гейт РАНЬШЕ стора здесь невозможен по построению (tracker #1004): проект
	// не адресуется телом, а выводится из версии по uuid — тот же случай, что
	// `GET /v1/matches/{id}` (#974), где поточечная проверка после резолва и
	// является правилом.
	v, err := s.st.GetVersion(r.Context(), req.VersionID)
	if err != nil {
		deployError(w, err)
		return
	}
	if !s.requireBinding(w, r, v.Project, v.Env) {
		return
	}
	st, err := s.dep.Deploy(r.Context(), req.VersionID)
	if err != nil {
		deployError(w, err)
		return
	}
	code := http.StatusOK // already active (idempotent repeat)
	if st.State == "prepulling" {
		code = http.StatusAccepted // flip happens when all nodes report pulled
	}
	writeJSON(w, code, map[string]any{"deploy": st})
}

type rollbackRequest struct {
	Project string `json:"project"`
	Env     string `json:"env,omitempty"`
	Region  string `json:"region"`
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req rollbackRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Гейт привязки по ПРОЕКТУ — ПЕРВЫМ, до единого похода в стор (tracker
	// #1004). Раньше он стоял после резолва env, и чужой слаг успевал уехать в
	// EnvsWithDeprecated: привязанный deploy-ключ различал состояние ЧУЖОГО
	// проекта по ответу (409 «нет deprecated-версии» / 409 «env is required» /
	// 403) — оракул того же класса, что закрыли #974/#988/#989. Отказ пишется
	// общим writeBindingDenied, побайтово тем же, что на листингах.
	//
	// Вторая половина гейта — прежняя: привязка ДЕФОЛТИТ проект (environments
	// v1 §5), поэтому CI одного проекта поле не шлёт; глобальному ключу
	// остаётся v0-удобство sole-project ниже.
	project, ok := bindProjectGate(w, r, req.Project)
	if !ok {
		return
	}
	if project == "" {
		var err error
		project, err = s.st.SoleProjectSlug(r.Context())
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "project is required: "+err.Error())
			return
		}
	}
	// env-резолв (environments v1 §3, I3): явный env — как есть; иначе смотрим,
	// у скольких окружений проекта есть deprecated-окно — ровно одно → откат туда
	// (sole-fallback), ноль → нечего откатывать (409), больше одного → env обязателен.
	env := req.Env
	if env == "" {
		envs, err := s.st.EnvsWithDeprecated(r.Context(), project)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		switch len(envs) {
		case 0:
			writeError(w, http.StatusConflict, "conflict",
				"project "+project+" has no deprecated version to roll back to")
			return
		case 1:
			env = envs[0]
		default:
			writeError(w, http.StatusConflict, "conflict",
				"env is required: multiple environments have a rollback window")
			return
		}
	}
	// Binding enforced on the resolved target (environments v1 §5): a key bound to
	// another env cannot roll back this (project, env).
	if !s.requireBinding(w, r, project, env) {
		return
	}
	// w8: явный env обязан существовать. Иначе несуществующий env доходил бы до
	// RollbackTarget и всплывал вводящим в заблуждение 409 «нет deprecated-версии»
	// (ErrVersionState) — типо в имени env это 400. Sole-fallback-путь (env
	// выведен из EnvsWithDeprecated) заведомо существует, его не перепроверяем.
	// storeError сам отдаёт ErrBadEnv как 400 «no such environment <p>/<e>» (v3),
	// а реальный сбой стора — как 500.
	if req.Env != "" {
		if _, err := s.st.GetEnvironment(r.Context(), project, env); err != nil {
			storeError(w, err)
			return
		}
	}
	var regions []string
	if req.Region != "" {
		regions = []string{req.Region}
	}
	res, err := s.dep.Rollback(r.Context(), project, env, regions)
	if err != nil {
		deployError(w, err)
		return
	}
	rollback := map[string]any{
		"version":    res.Version,
		"regions":    res.Regions,
		"old_semver": res.PrevSemver,
	}
	// Симметрично `{"deploy":{"warning":…}}` (tracker #1071/#1088): откат при
	// нуле живых нод переключает активную версию так же вхолостую, и тому, кто
	// катит его курлом в панике, знать об этом надо В МОМЕНТ ДЕЙСТВИЯ. Ключ
	// добавляется только непустым — как omitempty у деплоя, чтобы обычный ответ
	// не менялся ни на байт.
	if res.Warning != "" {
		rollback["warning"] = res.Warning
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollback": rollback})
}

type promoteRequest struct {
	VersionID string `json:"version_id"`
	ToEnv     string `json:"to_env"`
}

// handlePromote promotes a version into another environment and kicks off the
// normal deploy pipeline (environments v1 §4). Idempotent: a re-promote reuses
// the registered target version (store.PromoteVersion) instead of dead-ending on
// a semver conflict. Enforcement target is the PROMOTE target — (source project,
// to_env) — so a key bound to the source env cannot push into another env.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	var req promoteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := uuid.Parse(req.VersionID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "version_id must be a version id (uuid)")
		return
	}
	if req.ToEnv == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "to_env is required")
		return
	}
	// Load the source first (its project is the enforcement target with to_env),
	// so a wrong-env bound key is refused (403) without side effects; an unknown
	// version stays a 404.
	src, err := s.st.GetVersion(r.Context(), req.VersionID)
	if err != nil {
		deployError(w, err)
		return
	}
	if !s.requireBinding(w, r, src.Project, req.ToEnv) {
		return
	}
	promoted, err := s.st.PromoteVersion(r.Context(), req.VersionID, req.ToEnv)
	if err != nil {
		promoteError(w, err)
		return
	}
	// Deploy is async and separate from the promote transaction: the version is
	// already committed, so a failing deploy here (e.g. no fleet yet) still lets
	// the operator retry the promote idempotently.
	st, err := s.dep.Deploy(r.Context(), promoted.ID)
	if err != nil {
		deployError(w, err)
		return
	}
	code := http.StatusOK // already active (idempotent repeat)
	if st.State == "prepulling" {
		code = http.StatusAccepted // flip happens when all nodes report pulled
	}
	writeJSON(w, code, map[string]any{"version": promoted, "deploy": st})
}

// promoteError maps PromoteVersion sentinels onto HTTP responses: несуществующий
// to_env → 400 (ErrBadEnv — опечатка в теле запроса), неизвестная version_id → 404,
// конфликт состояния/семвера → 409.
//
// default здесь — 500, а НЕ 400 (v3). Раньше плоская ошибка «no such environment»
// вынуждала default→400, и вместе с ней в 400 уезжал ЛЮБОЙ сбой стора (упал pg,
// оборвался tx) — инфра-500 маскировался под «плохой ввод» клиента. Теперь
// bad-env говорит sentinel'ом, а всё непонятное честно 500.
func promoteError(w http.ResponseWriter, err error) {
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

// deployError maps deploy store sentinels onto HTTP responses.
func deployError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrVersionState),
		errors.Is(err, store.ErrDeployInProgress),
		errors.Is(err, store.ErrNoFleet):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
