package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

// Browser session auth for the panel (docs/specs/panel.md §1 п.5):
// POST /v1/session exchanges an API key for an HttpOnly cookie; the session
// lives in master memory (TTL 24h) and inherits the key's scopes. A master
// restart drops sessions — the panel falls back to the login screen.
//
// CSRF: SameSite=Lax already blocks cross-site POSTs; on top of that every
// non-GET request authenticated by cookie must carry the custom header
// `X-Birdman-Csrf: 1` (a cross-site page cannot set it).
const (
	sessionCookie  = "birdman_session"
	sessionTTL     = 24 * time.Hour
	csrfHeader     = "X-Birdman-Csrf"
	sessionMaxSize = 10000 // hard cap: login is admin-facing, not public
)

type session struct {
	key store.APIKey
	exp time.Time
}

type sessionStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]session
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, m: map[string]session{}}
}

// create mints a random session id for the key. Expired entries are swept on
// every create — logins are rare, the map stays tiny.
func (ss *sessionStore) create(key store.APIKey) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for sid, s := range ss.m {
		if now.After(s.exp) {
			delete(ss.m, sid)
		}
	}
	if len(ss.m) >= sessionMaxSize {
		return "", errors.New("session store full")
	}
	ss.m[id] = session{key: key, exp: now.Add(ss.ttl)}
	return id, nil
}

func (ss *sessionStore) get(id string) (store.APIKey, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s, ok := ss.m[id]
	if !ok {
		return store.APIKey{}, false
	}
	if time.Now().After(s.exp) {
		delete(ss.m, id)
		return store.APIKey{}, false
	}
	return s.key, true
}

func (ss *sessionStore) delete(id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.m, id)
}

// --- handlers ---

type createSessionRequest struct {
	APIKey string `json:"api_key"`
}

// handleCreateSession is the panel login: verify the API key, set the
// session cookie, return the granted scopes for the UI.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	key, err := s.st.AuthAPIKey(r.Context(), req.APIKey)
	if errors.Is(err, store.ErrBadAPIKey) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid API key")
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	id, err := s.auth.sessions.create(key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	http.SetCookie(w, sessionCookieFor(id, int(sessionTTL.Seconds()), requestIsHTTPS(r)))
	writeJSON(w, http.StatusOK, map[string]any{"scopes": key.Scopes, "name": key.Name})
}

// handleGetSession reports the caller's scopes (cookie or bearer) — the
// panel probes it on load to skip the login screen.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.auth.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no active session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopes": key.Scopes, "name": key.Name})
}

// handleDeleteSession is logout: idempotent, always clears the cookie.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if r.Header.Get(csrfHeader) == "" {
			writeError(w, http.StatusForbidden, "csrf_required", csrfHeader+" header is required")
			return
		}
		s.auth.sessions.delete(c.Value)
	}
	http.SetCookie(w, sessionCookieFor("", -1, requestIsHTTPS(r)))
	w.WriteHeader(http.StatusNoContent)
}

// requestIsHTTPS reports whether the request effectively arrived over TLS —
// directly (r.TLS) or via a TLS-terminating proxy (X-Forwarded-Proto=https).
// Drives the Secure cookie flag: over plain HTTP (dev via SSH tunnel) Secure
// must be off, or Safari/others silently drop the cookie and login loops.
func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sessionCookieFor builds the session cookie (maxAge < 0 deletes it).
// Secure follows the connection: HTTPS in prod, off for the plain-HTTP dev
// tunnel so the cookie actually persists in the browser.
func sessionCookieFor(value string, maxAge int, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}
