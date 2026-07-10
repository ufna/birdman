package daemon

import (
	"strings"
	"sync"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// registryCred is one master-supplied private-registry credential held in
// memory only (docs/superpowers/specs/2026-07-09-registries-design.md §2/§3).
type registryCred struct {
	username string
	token    string
}

// registryStore is the agent's in-memory snapshot of master-supplied
// registry credentials. SetRegistries always carries the FULL set (never a
// diff): Set replaces the whole map, it never merges. Nothing here is ever
// written to disk — a restarted agent gets the snapshot again on the next
// Hello (the master resends it at attach, before replaying any pending
// commands).
type registryStore struct {
	mu     sync.Mutex
	byHost map[string]registryCred
}

func newRegistryStore() *registryStore {
	return &registryStore{byHost: map[string]registryCred{}}
}

// Set replaces the entire credential set. A nil/empty creds wipes the store
// (e.g. the last registry was removed on the master).
func (s *registryStore) Set(creds []*agentlinkv1.RegistryCred) {
	m := make(map[string]registryCred, len(creds))
	for _, c := range creds {
		host := strings.ToLower(c.GetHost())
		if host == "" {
			continue
		}
		m[host] = registryCred{username: c.GetUsername(), token: c.GetToken()}
	}
	s.mu.Lock()
	s.byHost = m
	s.mu.Unlock()
}

// Lookup returns the credential for host (already normalized by the caller —
// lowercase, no scheme/path), if any.
func (s *registryStore) Lookup(host string) (username, token string, ok bool) {
	s.mu.Lock()
	c, ok := s.byHost[host]
	s.mu.Unlock()
	if !ok {
		return "", "", false
	}
	return c.username, c.token, true
}
