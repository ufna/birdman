package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestRegistryHostNormalization covers store.NormalizeRegistryHost: lowercase
// normalization, and the rejected shapes (docs/superpowers/specs/2026-07-09-registries-design.md
// §1) — empty, a scheme, a path/slash, and docker.io/index.docker.io (v1
// does not host-match docker.io's registry-1.docker.io resolution).
func TestRegistryHostNormalization(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "lowercases", in: "Host.IO", want: "host.io"},
		{name: "lowercases host colon port", in: "Registry.Example.COM:5000", want: "registry.example.com:5000"},
		{name: "plain host unchanged", in: "ghcr.io", want: "ghcr.io"},
		{name: "empty", in: "", wantErr: true},
		{name: "blank", in: "   ", wantErr: true},
		{name: "https scheme", in: "https://x", wantErr: true},
		{name: "http scheme", in: "http://x", wantErr: true},
		{name: "path slash", in: "x/y", wantErr: true},
		{name: "docker.io rejected", in: "docker.io", wantErr: true},
		{name: "index.docker.io rejected", in: "index.docker.io", wantErr: true},
		{name: "docker.io rejected case-insensitive", in: "Docker.IO", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := store.NormalizeRegistryHost(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("NormalizeRegistryHost(%q) = %q, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeRegistryHost(%q): unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("NormalizeRegistryHost(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestRegistriesStoreUpsertAndList covers UpsertRegistry (new row, then a
// same-host upsert that replaces the token/username/note in place — no
// duplicate row), ListRegistries (no token field, host ascending), and
// ListRegistryCreds (the only read that carries the token, for agentlink).
func TestRegistriesStoreUpsertAndList(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	r1, err := st.UpsertRegistry(ctx, "GHCR.io", "alice", "tok-1", "primary")
	if err != nil {
		t.Fatalf("upsert new: %v", err)
	}
	if r1.ID == "" || r1.Host != "ghcr.io" || r1.Username != "alice" || r1.Note != "primary" {
		t.Fatalf("unexpected registry: %+v", r1)
	}
	if r1.CreatedAt.IsZero() || r1.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", r1)
	}

	if _, err := st.UpsertRegistry(ctx, "registry.example.com:5000", "bob", "tok-2", ""); err != nil {
		t.Fatalf("upsert second host: %v", err)
	}

	// Re-upsert the SAME host (different case) — replaces token/username/note
	// on the existing row (on conflict (host) do update), not a new row.
	r1b, err := st.UpsertRegistry(ctx, "ghcr.io", "alice2", "tok-1-new", "updated note")
	if err != nil {
		t.Fatalf("upsert replace: %v", err)
	}
	if r1b.ID != r1.ID {
		t.Fatalf("re-upsert of the same host should reuse the row: %s != %s", r1b.ID, r1.ID)
	}
	if r1b.Username != "alice2" || r1b.Note != "updated note" {
		t.Fatalf("re-upsert should replace username/note: %+v", r1b)
	}
	if r1b.UpdatedAt.Before(r1.UpdatedAt) {
		t.Fatalf("re-upsert should bump updated_at: before=%v after=%v", r1.UpdatedAt, r1b.UpdatedAt)
	}

	// List: both hosts, host ascending, no duplicate for the re-upserted host.
	list, err := st.ListRegistries(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 registries, got %d: %+v", len(list), list)
	}
	if list[0].Host != "ghcr.io" || list[1].Host != "registry.example.com:5000" {
		t.Fatalf("list not host-ascending: %+v", list)
	}

	// Creds read carries the replaced token — the ONLY read that does.
	creds, err := st.ListRegistryCreds(ctx)
	if err != nil {
		t.Fatalf("list creds: %v", err)
	}
	byHost := map[string]store.RegistryCred{}
	for _, c := range creds {
		byHost[c.Host] = c
	}
	if byHost["ghcr.io"].Token != "tok-1-new" || byHost["ghcr.io"].Username != "alice2" {
		t.Fatalf("creds for ghcr.io wrong: %+v", byHost["ghcr.io"])
	}
	if byHost["registry.example.com:5000"].Token != "tok-2" {
		t.Fatalf("creds for second host wrong: %+v", byHost["registry.example.com:5000"])
	}
}

// TestRegistriesStoreDelete covers DeleteRegistry's found/absent signal.
func TestRegistriesStoreDelete(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	r, err := st.UpsertRegistry(ctx, "ghcr.io", "alice", "tok-1", "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	del, deleted, err := st.DeleteRegistry(ctx, r.ID)
	if err != nil || !deleted || del.ID != r.ID {
		t.Fatalf("delete: deleted=%v id=%s err=%v", deleted, del.ID, err)
	}
	list, err := st.ListRegistries(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("want empty list after delete, got %+v", list)
	}

	// Deleting again (same id) is a no-op signal, not an error.
	if _, deleted, err := st.DeleteRegistry(ctx, r.ID); err != nil || deleted {
		t.Fatalf("delete again: deleted=%v err=%v", deleted, err)
	}
	// An unknown (but valid) uuid is also a no-op.
	if _, deleted, err := st.DeleteRegistry(ctx, uuid.NewString()); err != nil || deleted {
		t.Fatalf("delete unknown id: deleted=%v err=%v", deleted, err)
	}
}

// TestRegistriesStoreEmpty: ListRegistries/ListRegistryCreds are [] (never
// nil) when there are no registries.
func TestRegistriesStoreEmpty(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	list, err := st.ListRegistries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("want [], got %+v", list)
	}
	creds, err := st.ListRegistryCreds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if creds == nil || len(creds) != 0 {
		t.Fatalf("want [], got %+v", creds)
	}
}
