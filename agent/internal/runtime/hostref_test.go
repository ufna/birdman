package runtime

import "testing"

// TestHostFromRef covers the reference-parser host extraction used to gate
// registry credentials by host (docs/superpowers/specs/2026-07-09-registries-design.md
// §3): a real reference parser, not a naive string split, so host:port,
// mixed-case domains and bare (Docker Hub) refs normalize the same way the
// master's host validation does (store.NormalizeRegistryHost). No containerd
// daemon involved — pure function.
func TestHostFromRef(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		host string
		ok   bool
	}{
		{"qualified host with tag", "ghcr.io/x/y:1", "ghcr.io", true},
		{"mixed-case host lowercased", "Host.IO/x", "host.io", true},
		{"host with port", "registry:5000/x:3", "registry:5000", true},
		{"bare ref normalizes to docker.io", "ubuntu:22.04", "docker.io", true},
		{"unparseable ref yields no host", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, ok := HostFromRef(c.ref)
			if ok != c.ok {
				t.Fatalf("HostFromRef(%q) ok = %v, want %v", c.ref, ok, c.ok)
			}
			if host != c.host {
				t.Fatalf("HostFromRef(%q) host = %q, want %q", c.ref, host, c.host)
			}
		})
	}
}
