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
