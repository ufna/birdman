package store_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/secrets"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// secretEnvPrefix is the at-rest envelope prefix, asserted from outside the
// secrets package (raw psql-visible form).
const secretEnvPrefix = "birdman:v1:"

// codecFilled builds a codec from a 32-byte key filled with b. Distinct fill
// bytes yield distinct key_ids, which the wrong-key rehearsal relies on.
func codecFilled(t *testing.T, b byte) *secrets.Codec {
	t.Helper()
	c, err := secrets.New(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}
	return c
}

// TestSecretsAtRestHygiene: after a normal write through the store, the raw
// columns hold only ciphertext — an envelope, never the plaintext. This is the
// property the whole workstream exists for: pg_dump carries no plaintext.
func TestSecretsAtRestHygiene(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	const plainToken = "ghp_hygieneToken_do_not_dump_0123456789"
	if _, err := st.UpsertRegistry(ctx, "ghcr.io", "alice", plainToken, "note"); err != nil {
		t.Fatalf("UpsertRegistry: %v", err)
	}
	if _, _, err := st.EnsureInternalCA(ctx); err != nil {
		t.Fatalf("EnsureInternalCA: %v", err)
	}

	var rawToken string
	if err := st.Pool.QueryRow(ctx, `select token from registries where host = 'ghcr.io'`).Scan(&rawToken); err != nil {
		t.Fatalf("raw select token: %v", err)
	}
	if !strings.HasPrefix(rawToken, secretEnvPrefix) {
		t.Fatalf("registries.token at rest is not an envelope: %q", rawToken)
	}
	if strings.Contains(rawToken, plainToken) {
		t.Fatal("registries.token at rest still contains the plaintext token")
	}

	var rawKey string
	if err := st.Pool.QueryRow(ctx, `select key_pem from internal_ca where active order by created_at desc limit 1`).Scan(&rawKey); err != nil {
		t.Fatalf("raw select key_pem: %v", err)
	}
	if !strings.HasPrefix(rawKey, secretEnvPrefix) {
		t.Fatalf("internal_ca.key_pem at rest is not an envelope: %q", rawKey)
	}
	if strings.Contains(rawKey, "PRIVATE KEY") {
		t.Fatal("internal_ca.key_pem at rest still contains a plaintext PEM private key")
	}
}

// seedPlaintextSecrets writes a legacy plaintext registries.token and
// internal_ca.key_pem straight through raw SQL, bypassing the codec — the
// simulation of a database written before this release.
func seedPlaintextSecrets(t *testing.T, st *store.Store, host, token, keyPEM string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Pool.Exec(ctx,
		`insert into registries (host, username, token, note) values ($1, 'u', $2, '')`, host, token); err != nil {
		t.Fatalf("seed plaintext registry: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`insert into internal_ca (cert_pem, key_pem, not_after) values ('fake-cert-pem', $1, $2)`,
		keyPEM, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("seed plaintext internal_ca: %v", err)
	}
}

// TestEncryptExistingSecrets: the startup pass encrypts legacy plaintext rows
// in place, is reversible through the store reads, and is idempotent (a second
// pass touches nothing). This is the data migration §3.
func TestEncryptExistingSecrets(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	const plainToken = "ghp_legacyPlaintextToken_9876543210"
	const plainKey = "-----BEGIN EC PRIVATE KEY-----\nLEGACY-PLAINTEXT-KEY\n-----END EC PRIVATE KEY-----\n"
	seedPlaintextSecrets(t, st, "legacy.example.com", plainToken, plainKey)

	n, err := st.EncryptExistingSecrets(ctx)
	if err != nil {
		t.Fatalf("EncryptExistingSecrets: %v", err)
	}
	if n != 2 {
		t.Fatalf("first pass encrypted %d rows, want 2 (one token + one key_pem)", n)
	}

	// At rest: both are now envelopes, neither carries its plaintext.
	var rawToken, rawKey string
	if err := st.Pool.QueryRow(ctx, `select token from registries where host = 'legacy.example.com'`).Scan(&rawToken); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `select key_pem from internal_ca where active order by created_at desc limit 1`).Scan(&rawKey); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rawToken, secretEnvPrefix) || strings.Contains(rawToken, plainToken) {
		t.Fatalf("token not encrypted in place: %q", rawToken)
	}
	if !strings.HasPrefix(rawKey, secretEnvPrefix) || strings.Contains(rawKey, "PRIVATE KEY") {
		t.Fatalf("key_pem not encrypted in place: %q", rawKey)
	}

	// Reversible through the real read paths.
	creds, err := st.ListRegistryCreds(ctx)
	if err != nil {
		t.Fatalf("ListRegistryCreds after pass: %v", err)
	}
	var got string
	for _, c := range creds {
		if c.Host == "legacy.example.com" {
			got = c.Token
		}
	}
	if got != plainToken {
		t.Fatalf("decrypted token = %q, want original plaintext", got)
	}
	_, keyPEM, err := st.EnsureInternalCA(ctx)
	if err != nil {
		t.Fatalf("EnsureInternalCA after pass: %v", err)
	}
	if string(keyPEM) != plainKey {
		t.Fatalf("decrypted key_pem = %q, want original plaintext", keyPEM)
	}

	// Idempotent: a second pass finds nothing to do and does not re-wrap.
	if n2, err := st.EncryptExistingSecrets(ctx); err != nil || n2 != 0 {
		t.Fatalf("second pass = (%d, %v), want (0, nil)", n2, err)
	}
	var rawToken2 string
	if err := st.Pool.QueryRow(ctx, `select token from registries where host = 'legacy.example.com'`).Scan(&rawToken2); err != nil {
		t.Fatal(err)
	}
	if rawToken2 != rawToken {
		t.Fatal("second pass re-wrapped an already-encrypted token (not idempotent)")
	}
}

// TestEncryptExistingSecretsConcurrent: two masters starting at once run the
// pass under the advisory lock; exactly one does the work, the state stays
// consistent, and no row is double-wrapped.
func TestEncryptExistingSecretsConcurrent(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	const plainToken = "ghp_concurrentPlaintext_0000"
	const plainKey = "-----BEGIN EC PRIVATE KEY-----\nCONCURRENT\n-----END EC PRIVATE KEY-----\n"
	seedPlaintextSecrets(t, st, "concurrent.example.com", plainToken, plainKey)

	const g = 8
	var wg sync.WaitGroup
	counts := make([]int, g)
	errs := make([]error, g)
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			counts[i], errs[i] = st.EncryptExistingSecrets(context.Background())
		}(i)
	}
	wg.Wait()

	total := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		total += counts[i]
	}
	// The seeded rows are encrypted exactly once across all concurrent passes.
	if total != 2 {
		t.Fatalf("sum of encrypted counts = %d, want exactly 2 (no double-wrap under the lock)", total)
	}

	creds, err := st.ListRegistryCreds(ctx)
	if err != nil {
		t.Fatalf("ListRegistryCreds: %v", err)
	}
	if len(creds) != 1 || creds[0].Token != plainToken {
		t.Fatalf("post-concurrency creds inconsistent: %+v", creds)
	}
	if n, err := st.EncryptExistingSecrets(ctx); err != nil || n != 0 {
		t.Fatalf("final pass = (%d, %v), want (0, nil)", n, err)
	}
}

// TestSecretsStrictReadRegistries: after the startup pass, a plaintext value
// that appears on a read path (raw-SQL injected here) is a hard error, never a
// silent passthrough (§4 strict reads).
func TestSecretsStrictReadRegistries(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	// A normal encrypted row plus a rogue plaintext row inserted straight to SQL.
	if _, err := st.UpsertRegistry(ctx, "ok.example.com", "alice", "ghp_ok", ""); err != nil {
		t.Fatalf("UpsertRegistry: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`insert into registries (host, username, token, note) values ('rogue.example.com', 'u', 'ghp_rawPlaintext', '')`); err != nil {
		t.Fatalf("seed rogue plaintext: %v", err)
	}

	_, err := st.ListRegistryCreds(ctx)
	if err == nil {
		t.Fatal("ListRegistryCreds must reject a plaintext token, not pass it through")
	}
	if !strings.Contains(err.Error(), "not an encrypted envelope") {
		t.Fatalf("want 'not an encrypted envelope', got: %v", err)
	}
}

// TestSecretsStrictReadCA: same strict-read invariant on the CA key path.
func TestSecretsStrictReadCA(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	if _, err := st.Pool.Exec(ctx,
		`insert into internal_ca (cert_pem, key_pem, not_after) values ('fake-cert', 'raw-plaintext-key', $1)`,
		time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("seed rogue plaintext CA: %v", err)
	}

	_, _, err := st.EnsureInternalCA(ctx)
	if err == nil {
		t.Fatal("EnsureInternalCA must reject a plaintext key_pem, not pass it through")
	}
	if !strings.Contains(err.Error(), "not an encrypted envelope") {
		t.Fatalf("want 'not an encrypted envelope', got: %v", err)
	}
}

// TestSecretsWrongKey rehearses the DR failure: a master opened against the
// same database with the WRONG key cannot read either secret, and the error
// names both key fingerprints so an operator sees exactly which key is missing.
func TestSecretsWrongKey(t *testing.T) {
	right := codecFilled(t, 0x11)
	wrong := codecFilled(t, 0x22)

	st, dsn := testdb.NewWithCodec(t, right)
	ctx := context.Background()

	if _, err := st.UpsertRegistry(ctx, "ghcr.io", "alice", "ghp_secret", ""); err != nil {
		t.Fatalf("UpsertRegistry: %v", err)
	}
	if _, _, err := st.EnsureInternalCA(ctx); err != nil {
		t.Fatalf("EnsureInternalCA: %v", err)
	}

	// A second master on the SAME database but loaded with a different key.
	st2, err := store.Open(ctx, dsn, wrong)
	if err != nil {
		t.Fatalf("open wrong-key store: %v", err)
	}
	t.Cleanup(st2.Close)

	namesBothKeys := func(msg string) bool {
		return strings.Contains(msg, right.KeyID()) && strings.Contains(msg, wrong.KeyID())
	}

	if _, err := st2.ListRegistryCreds(ctx); err == nil {
		t.Fatal("ListRegistryCreds with the wrong key must fail")
	} else if !namesBothKeys(err.Error()) {
		t.Fatalf("wrong-key registries error must name both key_ids (want=%s loaded=%s): %v", right.KeyID(), wrong.KeyID(), err)
	}

	if _, _, err := st2.EnsureInternalCA(ctx); err == nil {
		t.Fatal("EnsureInternalCA with the wrong key must fail")
	} else if !namesBothKeys(err.Error()) {
		t.Fatalf("wrong-key CA error must name both key_ids (want=%s loaded=%s): %v", right.KeyID(), wrong.KeyID(), err)
	}
}
