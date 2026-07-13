package httpapi

import (
	"errors"
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
	})
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

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.st.ListNodes(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": emptyNotNull(nodes)})
}

// --- servers ---

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.st.ListServers(r.Context(), store.ServerFilter{
		Project: r.URL.Query().Get("project"),
		Region:  r.URL.Query().Get("region"),
		State:   r.URL.Query().Get("state"),
	})
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": emptyNotNull(servers)})
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
	project := bindProject(r, req.Project)
	// Пустоту env проверяем ДО binding-guard (w10, паритет с handleUpsertFleet):
	// bound-CI без поля env должен получить «env is required» (400), а не «key is
	// bound to …» (403). project уже дефолтнут из привязки строкой выше.
	if req.Env == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "env is required")
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
	if errors.Is(err, store.ErrConflict) {
		storeError(w, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
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

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := s.st.ListVersions(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": emptyNotNull(versions)})
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
	project := bindProject(r, req.Project)
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
	writeJSON(w, http.StatusOK, map[string]any{"fleet": f})
}

// --- events ---

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		limit = n
	}
	events, err := s.st.ListEvents(r.Context(), limit)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": emptyNotNull(events)})
}

// --- allocation (docs/specs/master.md §3) ---

type allocateRequest struct {
	Project   string  `json:"project"`
	Env       string  `json:"env,omitempty"`
	Region    string  `json:"region"`
	VersionID *string `json:"version_id"`
	MatchID   string  `json:"match_id"`
}

func (s *Server) handleAllocate(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var req allocateRequest
	if !decodeJSON(w, r, &req) {
		s.m.AllocFailures.WithLabelValues("bad_request").Inc()
		return
	}
	if req.Project == "" || req.Region == "" {
		s.m.AllocFailures.WithLabelValues("bad_request").Inc()
		writeError(w, http.StatusBadRequest, "bad_request", "project and region are required")
		return
	}
	if _, err := uuid.Parse(req.MatchID); err != nil {
		s.m.AllocFailures.WithLabelValues("bad_request").Inc()
		writeError(w, http.StatusBadRequest, "bad_request", "match_id must be a uuid")
		return
	}
	if req.VersionID != nil {
		if _, err := uuid.Parse(*req.VersionID); err != nil {
			s.m.AllocFailures.WithLabelValues("bad_request").Inc()
			writeError(w, http.StatusBadRequest, "bad_request", "version_id must be a uuid")
			return
		}
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
			s.m.AllocFailures.WithLabelValues("bad_request").Inc()
			writeError(w, http.StatusConflict, "env_required", err.Error())
			return
		case err != nil:
			s.m.AllocFailures.WithLabelValues("internal").Inc()
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
	alloc, err := s.st.Allocate(r.Context(), req.Project, env, req.Region, req.VersionID, req.MatchID, 0)
	switch {
	case errors.Is(err, store.ErrNoCapacity):
		s.writeAllocNoCapacity(w, r, req)
		return
	case errors.Is(err, store.ErrNotFound):
		s.m.AllocFailures.WithLabelValues("bad_request").Inc()
		storeError(w, err)
		return
	case err != nil:
		s.m.AllocFailures.WithLabelValues("internal").Inc()
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
	s.m.AllocFailures.WithLabelValues("no_capacity").Inc()
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
