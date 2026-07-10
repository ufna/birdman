package daemon

import (
	"testing"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// TestRegistryStoreLookupAndReplace covers the agent's in-memory
// master-registry credential snapshot (registries v1,
// docs/superpowers/specs/2026-07-09-registries-design.md §2/§3): host lookup
// is exact and normalized (lowercase), and Set is a FULL replace — a second,
// smaller snapshot must make the first snapshot's hosts unreachable, not
// merge with them.
func TestRegistryStoreLookupAndReplace(t *testing.T) {
	s := newRegistryStore()

	if _, _, ok := s.Lookup("ghcr.io"); ok {
		t.Fatal("empty store must not match anything")
	}

	s.Set([]*agentlinkv1.RegistryCred{
		{Host: "ghcr.io", Username: "u1", Token: "t1"},
		{Host: "Registry.Example.com:5000", Username: "u2", Token: "t2"},
	})

	u, tok, ok := s.Lookup("ghcr.io")
	if !ok || u != "u1" || tok != "t1" {
		t.Fatalf("ghcr.io lookup = (%q, %q, %v), want (u1, t1, true)", u, tok, ok)
	}
	// The stored host is normalized (lowercase) regardless of the case the
	// snapshot arrived in (master already normalizes, but the agent must not
	// silently rely on that).
	u, tok, ok = s.Lookup("registry.example.com:5000")
	if !ok || u != "u2" || tok != "t2" {
		t.Fatalf("registry.example.com:5000 lookup = (%q, %q, %v), want (u2, t2, true)", u, tok, ok)
	}
	if _, _, ok := s.Lookup("evil.example.com"); ok {
		t.Fatal("unrelated host must not match")
	}

	// A second, smaller snapshot REPLACES the first wholesale: ghcr.io must
	// no longer be found.
	s.Set([]*agentlinkv1.RegistryCred{
		{Host: "registry.example.com:5000", Username: "u3", Token: "t3"},
	})
	if _, _, ok := s.Lookup("ghcr.io"); ok {
		t.Fatal("second snapshot must wipe the first, not merge with it")
	}
	u, tok, ok = s.Lookup("registry.example.com:5000")
	if !ok || u != "u3" || tok != "t3" {
		t.Fatalf("post-replace lookup = (%q, %q, %v), want (u3, t3, true)", u, tok, ok)
	}

	// An empty snapshot (all registries removed) wipes everything.
	s.Set(nil)
	if _, _, ok := s.Lookup("registry.example.com:5000"); ok {
		t.Fatal("empty snapshot must wipe every host")
	}
}
