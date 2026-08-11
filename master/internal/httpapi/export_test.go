package httpapi

// Test-only hooks for the external httpapi_test package.

import (
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

func NewSessionStoreForTest(ttl time.Duration) *sessionStore { return newSessionStore(ttl) }

func (ss *sessionStore) CreateForTest(name string, scopes []string) (string, error) {
	return ss.create(store.APIKey{Name: name, Scopes: scopes})
}

func (ss *sessionStore) GetForTest(id string) (store.APIKey, bool) { return ss.get(id) }

// SessionBindingForTest exposes the binding half of the /v1/session response.
// Needed because the half-pair case (Project set, Env nil) cannot be produced
// through the store at all — CreateAPIKey and a CHECK both reject it — so the
// only way to test how the API describes such a key is to call the mapper.
func SessionBindingForTest(key store.APIKey) *sessionBinding { return sessionResponseFor(key).Binding }
