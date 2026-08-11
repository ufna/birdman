package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

// API scopes (docs/specs/master.md §6).
const (
	ScopeAdmin       = "admin"
	ScopeDeploy      = "deploy"
	ScopeMatchmaking = "matchmaking"
	ScopeAllocate    = "allocate"
	ScopeReadonly    = "readonly"
)

// authCacheTTL bounds how long a bcrypt-verified key is trusted from memory.
// Consequence: a revoked key may keep working for up to this TTL.
const authCacheTTL = 5 * time.Minute

type cachedKey struct {
	key store.APIKey
	exp time.Time
}

type authenticator struct {
	st       *store.Store
	sessions *sessionStore

	mu    sync.Mutex
	cache map[[32]byte]cachedKey
}

func newAuthenticator(st *store.Store) *authenticator {
	return &authenticator{
		st:       st,
		sessions: newSessionStore(sessionTTL),
		cache:    map[[32]byte]cachedKey{},
	}
}

// authenticate resolves the request to an API key: `Authorization: Bearer`
// first, then the panel session cookie (session.go). viaCookie tells
// requireScope to apply the CSRF check. bcrypt verification results are cached
// (sha256(token) → the whole store.APIKey, the (project, env) binding included,
// so binding reaches enforcement through the cache too) — hot paths like
// /v1/allocate stay well under the 50ms SLO.
func (a *authenticator) authenticate(r *http.Request) (key store.APIKey, viaCookie, ok bool) {
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	token = strings.TrimSpace(token) // tolerate paste/whitespace artifacts
	if !ok || token == "" {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			key, ok := a.sessions.get(c.Value)
			return key, true, ok
		}
		return store.APIKey{}, false, false
	}
	sum := sha256.Sum256([]byte(token))

	a.mu.Lock()
	if c, ok := a.cache[sum]; ok && time.Now().Before(c.exp) {
		a.mu.Unlock()
		return c.key, false, true
	}
	a.mu.Unlock()

	key, err := a.st.AuthAPIKey(r.Context(), token)
	if err != nil {
		return store.APIKey{}, false, false
	}
	a.mu.Lock()
	a.cache[sum] = cachedKey{key: key, exp: time.Now().Add(authCacheTTL)}
	a.mu.Unlock()
	return key, false, true
}

// invalidateKey drops every cached bcrypt verification and every panel session
// for the given key id — called on revoke so a revoked key stops
// authenticating at once instead of after authCacheTTL. The cache is keyed by
// sha256(token), so we scan by the stored key.ID (revokes are rare).
func (a *authenticator) invalidateKey(keyID string) {
	a.mu.Lock()
	for sum, c := range a.cache {
		if c.key.ID == keyID {
			delete(a.cache, sum)
		}
	}
	a.mu.Unlock()
	a.sessions.deleteByKey(keyID)
}

// ctxKey is the private type for request-context keys set by this package.
type ctxKey int

const apiKeyCtxKey ctxKey = iota

// keyFromContext returns the authenticated API key that requireScope resolved
// for this request. Handlers that need the caller's identity (e.g. audit
// created_by) read it from here instead of re-authenticating.
func keyFromContext(ctx context.Context) (store.APIKey, bool) {
	k, ok := ctx.Value(apiKeyCtxKey).(store.APIKey)
	return k, ok
}

// requireScope wraps h: the request must carry a key with the scope (or
// admin, which implies everything). Cookie-authenticated non-GET requests
// must also carry the CSRF header (session.go). On success the resolved key is
// stashed in the request context (keyFromContext) for handlers that audit who
// acted.
func (s *Server) requireScope(scope string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, viaCookie, ok := s.auth.authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		if viaCookie && r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.Header.Get(csrfHeader) == "" {
			writeError(w, http.StatusForbidden, "csrf_required", csrfHeader+" header is required")
			return
		}
		if !slices.Contains(key.Scopes, scope) && !slices.Contains(key.Scopes, ScopeAdmin) {
			writeError(w, http.StatusForbidden, "forbidden", "scope "+scope+" required")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey, key)))
	}
}

// keyAllowed reports whether a key may act on the (project, env) target
// (environments v1 §5). A global key (Project nil — the pre-env default, and the
// only shape an admin key can take) is allowed everywhere; a bound key must
// match the target pair exactly.
func keyAllowed(key store.APIKey, project, env string) bool {
	if key.Project == nil {
		return true
	}
	return *key.Project == project && key.Env != nil && *key.Env == env
}

// requireBinding enforces the request key's (project, env) binding against a
// target on the deploy surface (environments v1 §5 — versions/deploy/rollback/
// fleets). It returns true when the request may proceed; otherwise it writes the
// 403 and returns false. A global/admin key always passes. The key is the one
// requireScope stashed in the context; an absent key (a wiring impossibility on
// a scoped route) is treated as unbound and passes — the scope gate already ran.
func (s *Server) requireBinding(w http.ResponseWriter, r *http.Request, project, env string) bool {
	key, _ := keyFromContext(r.Context())
	if keyAllowed(key, project, env) {
		return true
	}
	// key.Project здесь гарантированно не nil: для глобального ключа keyAllowed
	// вернул бы true.
	writeBindingDenied(w, r)
	return false
}

// writeBindingDenied пишет отказ по привязке — ОДНО тело на все поверхности:
// поточечный гейт над адресуемым объектом (requireBinding) и арендаторская
// граница на листингах (tenantScope, #993) отвечают байт-в-байт одинаково.
// Это не косметика: на листингах отказ обязан быть неотличим для живого и для
// выдуманного проекта/окружения, а разные тексты в двух местах — тот же оракул,
// только на строке вместо статуса (урок #989). Звать только для привязанного
// ключа (key.Project != nil); у глобального отказа быть не может.
func writeBindingDenied(w http.ResponseWriter, r *http.Request) {
	key, _ := keyFromContext(r.Context())
	writeError(w, http.StatusForbidden, "forbidden",
		fmt.Sprintf("key is bound to %s", bindingLabel(key)))
}

// keyBinding отдаёт пару (project, env), к которой привязан ключ запроса, и
// bound=false для глобального ключа. Admin привязать нельзя (store.CreateAPIKey
// отвергает admin+binding), сессия панели наследует ключ логина целиком —
// поэтому и admin-ключ, и admin-сессия здесь всегда bound=false и проходят
// чтения как раньше.
//
// Полупара (Project задан, Env nil) недостижима при живом CHECK
// api_keys_binding_all_or_nothing, но достижима по схеме: тогда env="" и
// сужение идёт только по проекту — строго уже, чем «вся платформа» до #993,
// и без разыменования nil.
func keyBinding(r *http.Request) (project, env string, bound bool) {
	key, ok := keyFromContext(r.Context())
	if !ok || key.Project == nil {
		return "", "", false
	}
	if key.Env != nil {
		env = *key.Env
	}
	return *key.Project, env, true
}

// bindingLabel форматирует пару привязки для тела 403. Полупара (Project задан,
// Env nil) недостижима при живом CHECK api_keys_binding_all_or_nothing, но
// достижима по схеме — не разыменовываем key.Env вслепую, иначе форматирование
// 403 паникнет (w11). Вызывать только для привязанного ключа (Project != nil).
func bindingLabel(key store.APIKey) string {
	keyEnv := "<nil>"
	if key.Env != nil {
		keyEnv = *key.Env
	}
	return *key.Project + "/" + keyEnv
}

// scopeLabelRe — тот же алфавит, что у слагов проекта и имени окружения
// (store.projectSlugRe / store.envNameRe). Пара из привязки ключа проверяется
// им ПОВТОРНО, перед тем как уехать в фильтр запроса к апстриму: значение
// склеивается в выражение на чужом языке (LogsQL `{project="…"}`,
// `extra_label=project=…`), и единственная причина, по которой склейка
// безопасна, — что в паре нет ни кавычки, ни фигурной скобки, ни запятой, ни
// знака равенства. Строка, пришедшая из БД мимо этого алфавита (ряд старше
// CHECK'а, будущее расширение алфавита), обязана падать ЗАКРЫТО — обратно в
// дверь #990, а не в фильтр, который апстрим разберёт не так, как мы задумали.
var scopeLabelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// narrowScope возвращает пару (project, env), которой сужается запрос к сырой
// query-проксии (LogsQL → VictoriaLogs, PromQL → VictoriaMetrics).
//
// narrow=false — ключ глобальный/admin (привязка с admin несовместима):
// passthrough как был, сужать не по чему и незачем.
// ok=false — ответ уже написан (403), вызывающий обязан выйти.
//
// Это замена глухого гейта #990 (tracker #994): у запроса по-прежнему нет
// объекта, чью пару можно сверить как в requireBinding, — есть произвольная
// программа на языке апстрима. Но с тех пор у стримов VL появились лейблы
// project/env (их чеканит агент в пути файла лога), а значит сузить запрос
// МОЖНО, не разбирая его: master пересобирает query-строку апстрима из белого
// списка параметров и добавляет фильтр САМ (ops.go). Фильтровать ОТВЕТ вместо
// запроса по-прежнему нельзя: состав и значения полей ответа задаёт сам запрос.
func (s *Server) narrowScope(w http.ResponseWriter, r *http.Request) (project, env string, narrow, ok bool) {
	key, _ := keyFromContext(r.Context())
	if key.Project == nil {
		return "", "", false, true
	}
	env = ""
	if key.Env != nil {
		env = *key.Env
	}
	if !scopeLabelRe.MatchString(*key.Project) || !scopeLabelRe.MatchString(env) {
		writeError(w, http.StatusForbidden, "forbidden",
			fmt.Sprintf("key is bound to %s: scope is not narrowable, raw query proxy is global-key only",
				bindingLabel(key)))
		return "", "", false, false
	}
	return *key.Project, env, true, true
}

// bindProject resolves the project a request acts on when the field is optional:
// an explicit value wins; otherwise a bound key contributes its own project
// (environments v1 §5 — a bound key defaults project, not only validates it), so
// CI keyed to one project can omit it. A global key with no explicit project
// yields "" unchanged (the sole-project / ensureProject conventions apply
// downstream).
func bindProject(r *http.Request, project string) string {
	if project != "" {
		return project
	}
	if key, ok := keyFromContext(r.Context()); ok && key.Project != nil {
		return *key.Project
	}
	return project
}
