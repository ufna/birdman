package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// garKey is a minimal but well-formed service-account JSON key — a JSON object
// with "type":"service_account" and a non-empty "private_key" (the two fields
// store.ValidateRegistry insists on). Used across the gar tests.
const garKey = `{"type":"service_account","project_id":"p","private_key_id":"k","private_key":"-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----\n","client_email":"sa@p.iam.gserviceaccount.com"}`

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

// TestValidateRegistry is a pure-function table test of the per-type
// host+credential validation (Реестры v2 §3): ghcr must be ghcr.io; gar must
// match a *-docker.pkg.dev / gcr.io host AND carry a service-account JSON key,
// and it forces the docker-login user to _json_key regardless of the passed
// username; generic keeps the v1 host rules and a passed username. Unknown
// type is an error.
func TestValidateRegistry(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		typ      string
		username string
		token    string
		wantHost string
		wantUser string
		wantErr  bool
	}{
		// ghcr
		{name: "ghcr ok", host: "ghcr.io", typ: "ghcr", username: "alice", token: "ghp_x", wantHost: "ghcr.io", wantUser: "alice"},
		{name: "ghcr lowercases host", host: "GHCR.io", typ: "ghcr", username: "alice", token: "ghp_x", wantHost: "ghcr.io", wantUser: "alice"},
		{name: "ghcr wrong host", host: "example.com", typ: "ghcr", username: "alice", token: "ghp_x", wantErr: true},
		{name: "ghcr scheme host", host: "https://ghcr.io", typ: "ghcr", username: "alice", token: "ghp_x", wantErr: true},
		{name: "ghcr empty token", host: "ghcr.io", typ: "ghcr", username: "alice", token: "", wantErr: true},
		{name: "ghcr empty username", host: "ghcr.io", typ: "ghcr", username: "", token: "ghp_x", wantErr: true},
		// gar — username forced to _json_key, host patterns, JSON secret
		{name: "gar artifact-registry", host: "europe-docker.pkg.dev", typ: "gar", username: "ignored", token: garKey, wantHost: "europe-docker.pkg.dev", wantUser: "_json_key"},
		{name: "gar artifact-registry uppercased", host: "US-Docker.PKG.dev", typ: "gar", username: "", token: garKey, wantHost: "us-docker.pkg.dev", wantUser: "_json_key"},
		{name: "gar bare pkg.dev", host: "docker.pkg.dev", typ: "gar", token: garKey, wantHost: "docker.pkg.dev", wantUser: "_json_key"},
		{name: "gar legacy gcr.io", host: "gcr.io", typ: "gar", token: garKey, wantHost: "gcr.io", wantUser: "_json_key"},
		{name: "gar legacy regional gcr.io", host: "eu.gcr.io", typ: "gar", token: garKey, wantHost: "eu.gcr.io", wantUser: "_json_key"},
		{name: "gar wrong host", host: "example.com", typ: "gar", token: garKey, wantErr: true},
		{name: "gar near-miss pkg.dev without dash", host: "xdocker.pkg.dev", typ: "gar", token: garKey, wantErr: true},
		{name: "gar near-miss gcr.io suffix", host: "notgcr.io", typ: "gar", token: garKey, wantErr: true},
		{name: "gar scheme host", host: "https://europe-docker.pkg.dev", typ: "gar", token: garKey, wantErr: true},
		{name: "gar not json", host: "gcr.io", typ: "gar", token: "not-json", wantErr: true},
		{name: "gar json but no private_key", host: "gcr.io", typ: "gar", token: `{"type":"service_account"}`, wantErr: true},
		{name: "gar json empty private_key", host: "gcr.io", typ: "gar", token: `{"type":"service_account","private_key":""}`, wantErr: true},
		{name: "gar json wrong type", host: "gcr.io", typ: "gar", token: `{"type":"authorized_user","private_key":"x"}`, wantErr: true},
		{name: "gar json array", host: "gcr.io", typ: "gar", token: `[1,2,3]`, wantErr: true},
		{name: "gar empty token", host: "gcr.io", typ: "gar", token: "", wantErr: true},
		// generic — v1 host rules + passed username
		{name: "generic ok", host: "registry.example.com:5000", typ: "generic", username: "bob", token: "pw", wantHost: "registry.example.com:5000", wantUser: "bob"},
		{name: "generic lowercases", host: "Reg.Example.COM", typ: "generic", username: "bob", token: "pw", wantHost: "reg.example.com", wantUser: "bob"},
		{name: "generic docker.io rejected", host: "docker.io", typ: "generic", username: "bob", token: "pw", wantErr: true},
		{name: "generic scheme rejected", host: "https://x", typ: "generic", username: "bob", token: "pw", wantErr: true},
		{name: "generic empty username", host: "example.com", typ: "generic", username: "", token: "pw", wantErr: true},
		{name: "generic empty token", host: "example.com", typ: "generic", username: "bob", token: "", wantErr: true},
		// unknown type — distinctive token so the leak-check (below) is not
		// tripped by a coincidental 1-char substring of the error text.
		{name: "unknown type", host: "example.com", typ: "bogus", username: "a", token: "ghp_sentinel", wantErr: true},
		{name: "empty type", host: "example.com", typ: "", username: "a", token: "ghp_sentinel", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, user, err := store.ValidateRegistry(c.host, c.typ, c.username, c.token)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ValidateRegistry(%q,%q,...) = (%q,%q), want error", c.host, c.typ, host, user)
				}
				// No error must ever echo the token bytes.
				if c.token != "" && strings.Contains(err.Error(), c.token) {
					t.Fatalf("error leaked the token: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateRegistry(%q,%q,...): unexpected error: %v", c.host, c.typ, err)
			}
			if host != c.wantHost || user != c.wantUser {
				t.Fatalf("ValidateRegistry(%q,%q,%q) = (%q,%q), want (%q,%q)", c.host, c.typ, c.username, host, user, c.wantHost, c.wantUser)
			}
		})
	}
}

// TestRegistriesStoreUpsertAndList covers UpsertRegistry (new row, then a
// same-host upsert that replaces the token/username/note/type in place — no
// duplicate row), ListRegistries (no token field, host ascending, carries
// type), and ListRegistryCreds (the only read that carries the token).
func TestRegistriesStoreUpsertAndList(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	r1, err := st.UpsertRegistry(ctx, "GHCR.io", "ghcr", "alice", "tok-1", "primary")
	if err != nil {
		t.Fatalf("upsert new: %v", err)
	}
	if r1.ID == "" || r1.Host != "ghcr.io" || r1.Type != "ghcr" || r1.Username != "alice" || r1.Note != "primary" {
		t.Fatalf("unexpected registry: %+v", r1)
	}
	if r1.CreatedAt.IsZero() || r1.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", r1)
	}

	if _, err := st.UpsertRegistry(ctx, "registry.example.com:5000", "generic", "bob", "tok-2", ""); err != nil {
		t.Fatalf("upsert second host: %v", err)
	}

	// Re-upsert the SAME host (different case) — replaces token/username/note/type
	// on the existing row (on conflict (host) do update), not a new row.
	r1b, err := st.UpsertRegistry(ctx, "ghcr.io", "ghcr", "alice2", "tok-1-new", "updated note")
	if err != nil {
		t.Fatalf("upsert replace: %v", err)
	}
	if r1b.ID != r1.ID {
		t.Fatalf("re-upsert of the same host should reuse the row: %s != %s", r1b.ID, r1.ID)
	}
	if r1b.Username != "alice2" || r1b.Note != "updated note" || r1b.Type != "ghcr" {
		t.Fatalf("re-upsert should replace username/note/type: %+v", r1b)
	}
	if r1b.UpdatedAt.Before(r1.UpdatedAt) {
		t.Fatalf("re-upsert should bump updated_at: before=%v after=%v", r1.UpdatedAt, r1b.UpdatedAt)
	}

	// List: both hosts, host ascending, no duplicate for the re-upserted host,
	// type carried.
	list, err := st.ListRegistries(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 registries, got %d: %+v", len(list), list)
	}
	if list[0].Host != "ghcr.io" || list[0].Type != "ghcr" || list[1].Host != "registry.example.com:5000" || list[1].Type != "generic" {
		t.Fatalf("list not host-ascending or missing type: %+v", list)
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

// TestRegistriesStoreGARNormalization: an upsert with type=gar forces the
// stored docker-login username to _json_key regardless of the passed username,
// and keeps the whole JSON key as the (encrypted) token.
func TestRegistriesStoreGARNormalization(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	reg, err := st.UpsertRegistry(ctx, "europe-docker.pkg.dev", "gar", "whatever-ignored", garKey, "gar note")
	if err != nil {
		t.Fatalf("upsert gar: %v", err)
	}
	if reg.Type != "gar" || reg.Username != "_json_key" {
		t.Fatalf("gar upsert should normalize username to _json_key: %+v", reg)
	}
	// The stored credential: user=_json_key, secret=the JSON key.
	creds, err := st.ListRegistryCreds(ctx)
	if err != nil {
		t.Fatalf("list creds: %v", err)
	}
	if len(creds) != 1 || creds[0].Username != "_json_key" || creds[0].Token != garKey {
		t.Fatalf("gar cred should be (_json_key, <json>): %+v", creds)
	}

	// A malformed gar secret is rejected at write time.
	if _, err := st.UpsertRegistry(ctx, "gcr.io", "gar", "", "not-a-json-key", ""); err == nil {
		t.Fatal("gar upsert with a non-JSON secret must fail")
	}
	if _, err := st.UpsertRegistry(ctx, "ghcr.io", "ghcr", "alice", "ghp_x", ""); err != nil {
		t.Fatalf("ghcr upsert: %v", err)
	}
	// A ghcr upsert with a non-ghcr.io host is rejected.
	if _, err := st.UpsertRegistry(ctx, "example.com", "ghcr", "alice", "ghp_x", ""); err == nil {
		t.Fatal("ghcr upsert with a non-ghcr.io host must fail")
	}
}

// TestRegistriesStorePatch covers PatchRegistry: keep-secret (empty token
// leaves the existing ciphertext intact — verified through ListRegistryCreds),
// rotate (non-empty token re-encrypts), note/username edits, host immutability,
// the gar _json_key invariant on patch, the type-change-to-gar-without-token
// guard, and the 404 signal.
func TestRegistriesStorePatch(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	reg, err := st.UpsertRegistry(ctx, "reg.example.com", "generic", "alice", "secret-1", "note-1")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Keep-secret: patch note+username, omit the token → existing secret intact.
	upd, found, err := st.PatchRegistry(ctx, reg.ID, nil, strptr("alice2"), strptr("note-2"), "")
	if err != nil || !found {
		t.Fatalf("patch keep: found=%v err=%v", found, err)
	}
	if upd.Username != "alice2" || upd.Note != "note-2" || upd.Host != "reg.example.com" || upd.Type != "generic" {
		t.Fatalf("patch keep result: %+v", upd)
	}
	if got := credFor(t, ctx, st, "reg.example.com").Token; got != "secret-1" {
		t.Fatalf("keep-secret: token must be unchanged, got %q", got)
	}

	// Rotate: non-empty token re-encrypts to the new value.
	if _, found, err := st.PatchRegistry(ctx, reg.ID, nil, nil, nil, "secret-2"); err != nil || !found {
		t.Fatalf("patch rotate: found=%v err=%v", found, err)
	}
	if got := credFor(t, ctx, st, "reg.example.com").Token; got != "secret-2" {
		t.Fatalf("rotate: token should be secret-2, got %q", got)
	}

	// Host is immutable — the store never reads a host from the patch (there is
	// no host arg); the row's host stays put.
	if upd, _, _ := st.PatchRegistry(ctx, reg.ID, nil, nil, strptr("note-3"), ""); upd.Host != "reg.example.com" {
		t.Fatalf("host must be immutable: %+v", upd)
	}

	// gar invariant on patch: switch type→gar WITH a fresh JSON key on a
	// gar-shaped host works and forces username=_json_key even if a username is
	// also supplied.
	gar, err := st.UpsertRegistry(ctx, "us-docker.pkg.dev", "generic", "u", "pw", "")
	if err != nil {
		t.Fatalf("seed gar-host generic: %v", err)
	}
	garUpd, found, err := st.PatchRegistry(ctx, gar.ID, strptr("gar"), strptr("still-ignored"), nil, garKey)
	if err != nil || !found {
		t.Fatalf("patch to gar: found=%v err=%v", found, err)
	}
	if garUpd.Type != "gar" || garUpd.Username != "_json_key" {
		t.Fatalf("patch to gar should force _json_key: %+v", garUpd)
	}
	if got := credFor(t, ctx, st, "us-docker.pkg.dev").Token; got != garKey {
		t.Fatalf("patch to gar should store the JSON key, got %q", got)
	}
	// And a subsequent keep-secret patch of the gar row keeps _json_key + JSON.
	if _, _, err := st.PatchRegistry(ctx, gar.ID, nil, nil, strptr("gar-note"), ""); err != nil {
		t.Fatalf("keep patch on gar: %v", err)
	}
	if c := credFor(t, ctx, st, "us-docker.pkg.dev"); c.Username != "_json_key" || c.Token != garKey {
		t.Fatalf("gar keep patch broke the invariant: %+v", c)
	}

	// Guard: a type-change TO gar WITHOUT a fresh token cannot retro-fit the
	// _json_key + SA-JSON invariant (only ciphertext is on hand) → error.
	if _, _, err := st.PatchRegistry(ctx, reg.ID, strptr("gar"), nil, nil, ""); err == nil {
		t.Fatal("type→gar without a token must error")
	}
	// The reg row is untouched by the rejected patch (still generic/secret-2).
	if c := credFor(t, ctx, st, "reg.example.com"); c.Token != "secret-2" {
		t.Fatalf("rejected patch must not mutate the row: %+v", c)
	}

	// 404: unknown (valid) uuid → found=false, nil error.
	if _, found, err := st.PatchRegistry(ctx, uuid.NewString(), nil, nil, strptr("x"), ""); err != nil || found {
		t.Fatalf("patch unknown id: found=%v err=%v", found, err)
	}
}

// TestRegistriesStoreDelete covers DeleteRegistry's found/absent signal.
func TestRegistriesStoreDelete(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	r, err := st.UpsertRegistry(ctx, "ghcr.io", "ghcr", "alice", "tok-1", "")
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

// credFor returns the single credential for host (fatal if absent) — the store
// read that carries the decrypted token, used to prove keep-vs-rotate.
func credFor(t *testing.T, ctx context.Context, st *store.Store, host string) store.RegistryCred {
	t.Helper()
	creds, err := st.ListRegistryCreds(ctx)
	if err != nil {
		t.Fatalf("list creds: %v", err)
	}
	for _, c := range creds {
		if c.Host == host {
			return c
		}
	}
	t.Fatalf("no credential for host %q in %+v", host, creds)
	return store.RegistryCred{}
}
