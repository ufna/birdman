package httpapi

import (
	"context"
	"crypto/sha256"
	"net/http"
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
// requireScope to apply the CSRF check. bcrypt verification results are
// cached (sha256(token) → scopes) so hot paths like /v1/allocate stay well
// under the 50ms SLO.
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
