package secrets_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ufna/birdman/master/internal/secrets"
)

// key32 returns a deterministic 32-byte key filled with b. Distinct fill bytes
// yield distinct key_ids, which the wrong-key tests rely on.
func key32(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

const (
	colToken = "registries.token"
	colKey   = "internal_ca.key_pem"
	prefix   = "birdman:v1:" // the wire format, asserted from outside the package
)

func TestSecretsRoundtrip(t *testing.T) {
	c, err := secrets.New(key32(1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := [][]byte{
		[]byte("ghp_exampleRegistryToken1234567890"),
		[]byte("-----BEGIN EC PRIVATE KEY-----\nMHc...\n-----END EC PRIVATE KEY-----\n"),
		{},                            // empty plaintext
		bytes.Repeat([]byte{0x00}, 1), // single NUL byte
		bytes.Repeat([]byte{0xFF}, 4096),
	}
	for i, pt := range cases {
		env, err := c.Encrypt(pt, colToken)
		if err != nil {
			t.Fatalf("case %d Encrypt: %v", i, err)
		}
		if !secrets.IsEncrypted(env) {
			t.Fatalf("case %d Encrypt produced non-envelope: %q", i, env)
		}
		if !strings.HasPrefix(env, prefix+c.KeyID()+":") {
			t.Fatalf("case %d envelope prefix wrong: %q", i, env)
		}
		got, err := c.Decrypt(env, colToken)
		if err != nil {
			t.Fatalf("case %d Decrypt: %v", i, err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("case %d roundtrip mismatch: got %q want %q", i, got, pt)
		}
	}
}

func TestSecretsNonceUnique(t *testing.T) {
	c, err := secrets.New(key32(1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pt := []byte("the same plaintext, encrypted twice")
	a, err := c.Encrypt(pt, colToken)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encrypt(pt, colToken)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two Encrypts of the same plaintext produced identical envelopes — nonce is not random")
	}
	for _, env := range []string{a, b} {
		got, err := c.Decrypt(env, colToken)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("Decrypt: got %q err %v", got, err)
		}
	}
}

func TestSecretsKeyID(t *testing.T) {
	key := key32(1)
	c, err := secrets.New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sum := sha256.Sum256(key)
	want := hex.EncodeToString(sum[:4])
	if c.KeyID() != want {
		t.Fatalf("KeyID = %q, want hex(sha256(key)[:4]) = %q", c.KeyID(), want)
	}
	if len(c.KeyID()) != 8 {
		t.Fatalf("KeyID length = %d, want 8 hex chars", len(c.KeyID()))
	}
	// The envelope's third ':'-field is exactly this key_id.
	env, err := c.Encrypt([]byte("x"), colToken)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(env, ":", 4)
	if len(parts) != 4 || parts[0] != "birdman" || parts[1] != "v1" || parts[2] != want {
		t.Fatalf("envelope key_id field wrong: %q", env)
	}
	// Distinct keys → distinct fingerprints.
	c2, err := secrets.New(key32(2))
	if err != nil {
		t.Fatal(err)
	}
	if c2.KeyID() == c.KeyID() {
		t.Fatal("distinct keys produced the same key_id")
	}
}

func TestSecretsTamper(t *testing.T) {
	c, err := secrets.New(key32(1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env, err := c.Encrypt([]byte("tamper me if you can"), colToken)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(env, ":", 4)
	raw, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	// Flip every byte position in turn (nonce bytes and ciphertext+tag bytes):
	// each single-byte mutation must break decryption.
	for i := range raw {
		mutated := append([]byte(nil), raw...)
		mutated[i] ^= 0xFF
		bad := prefix + c.KeyID() + ":" + base64.StdEncoding.EncodeToString(mutated)
		if _, err := c.Decrypt(bad, colToken); err == nil {
			t.Fatalf("tampering byte %d did not break decryption", i)
		}
	}
}

func TestSecretsWrongAAD(t *testing.T) {
	c, err := secrets.New(key32(1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env, err := c.Encrypt([]byte("secret value"), colToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Decrypt(env, colKey); err == nil {
		t.Fatal("Decrypt with a different aad must fail — AAD binds ciphertext to its owning column")
	}
	if _, err := c.Decrypt(env, colToken); err != nil {
		t.Fatalf("Decrypt with the correct aad: %v", err)
	}
}

func TestSecretsWrongKey(t *testing.T) {
	a, err := secrets.New(key32(1))
	if err != nil {
		t.Fatal(err)
	}
	b, err := secrets.New(key32(2))
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.Encrypt([]byte("secret value"), colToken)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Decrypt(env, colToken)
	if err == nil {
		t.Fatal("Decrypt with the wrong key must fail")
	}
	msg := err.Error()
	// The DR diagnostic: the error must name BOTH the encrypting and the loaded
	// key fingerprints so an operator sees exactly which key is missing.
	if !strings.Contains(msg, a.KeyID()) || !strings.Contains(msg, b.KeyID()) {
		t.Fatalf("wrong-key error must name BOTH key_ids (encrypted=%s loaded=%s); got: %s", a.KeyID(), b.KeyID(), msg)
	}
}

func TestSecretsIsEncrypted(t *testing.T) {
	c, err := secrets.New(key32(1))
	if err != nil {
		t.Fatal(err)
	}
	env, err := c.Encrypt([]byte("x"), colToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{env, "birdman:v1:deadbeef:anything"} {
		if !secrets.IsEncrypted(s) {
			t.Fatalf("IsEncrypted(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"",
		"ghp_plainRegistryToken",
		"-----BEGIN EC PRIVATE KEY-----",
		"birdman:v2:deadbeef:x",  // wrong version
		"birdman:v1",             // partial prefix
		"birdman:",               // partial prefix
		" birdman:v1:deadbeef:x", // leading space defeats strict prefix
	} {
		if secrets.IsEncrypted(s) {
			t.Fatalf("IsEncrypted(%q) = true, want false", s)
		}
	}
}

func TestSecretsMalformedEnvelope(t *testing.T) {
	c, err := secrets.New(key32(1))
	if err != nil {
		t.Fatal(err)
	}
	kid := c.KeyID()

	// No v1 prefix at all → the exact strict-read message.
	for _, s := range []string{"", "plain value", "ghp_token", "birdman:v2:x:y"} {
		_, err := c.Decrypt(s, colToken)
		if err == nil || !strings.Contains(err.Error(), "not an encrypted envelope") {
			t.Fatalf("Decrypt(%q): want 'not an encrypted envelope', got %v", s, err)
		}
	}

	// Prefix present but structurally broken → some error (never a silent pass).
	shortBody := base64.StdEncoding.EncodeToString([]byte("short")) // 5 bytes < 12-byte nonce
	for _, s := range []string{
		"birdman:v1:",                 // no key_id, no body
		"birdman:v1:" + kid,           // missing body field
		"birdman:v1::body",            // empty key_id
		"birdman:v1:" + kid + ":",     // empty body
		"birdman:v1:" + kid + ":@@@@", // body not valid base64
		"birdman:v1:" + kid + ":" + shortBody,
	} {
		if _, err := c.Decrypt(s, colToken); err == nil {
			t.Fatalf("Decrypt(%q): want error, got nil", s)
		}
	}
}

func TestSecretsNewRejectsBadKeyLen(t *testing.T) {
	for _, n := range []int{0, 1, 16, 24, 31, 33, 64} {
		if _, err := secrets.New(make([]byte, n)); err == nil {
			t.Fatalf("New with a %d-byte key: want error, got nil", n)
		}
	}
	if _, err := secrets.New(nil); err == nil {
		t.Fatal("New(nil): want error")
	}
	if _, err := secrets.New(key32(1)); err != nil {
		t.Fatalf("New with a 32-byte key: unexpected error %v", err)
	}
}

func TestSecretsLoadKey(t *testing.T) {
	key := key32(7)
	b64 := base64.StdEncoding.EncodeToString(key)

	// From a file written the way `openssl rand -base64 32 > file` does: a
	// single base64 line with a trailing newline (must be tolerated).
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.key")
	if err := os.WriteFile(path, []byte(b64+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := secrets.LoadKey(path, "")
	if err != nil {
		t.Fatalf("LoadKey(file): %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("LoadKey(file) returned the wrong key")
	}
	// The loaded key must build a working Codec.
	if _, err := secrets.New(got); err != nil {
		t.Fatalf("New(loaded key): %v", err)
	}

	// From the env value (dev/test path).
	got, err = secrets.LoadKey("", b64)
	if err != nil {
		t.Fatalf("LoadKey(env): %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("LoadKey(env) returned the wrong key")
	}
}

func TestSecretsLoadKeyNegatives(t *testing.T) {
	validB64 := base64.StdEncoding.EncodeToString(key32(9))
	dir := t.TempDir()

	// Missing file.
	if _, err := secrets.LoadKey(filepath.Join(dir, "nope.key"), ""); err == nil {
		t.Fatal("LoadKey(missing file): want error")
	}

	// File is not base64.
	badB64 := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(badB64, []byte("@@@ not base64 @@@"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.LoadKey(badB64, ""); err == nil {
		t.Fatal("LoadKey(non-base64 file): want error")
	}

	// File decodes to the wrong length (24 bytes, not 32).
	shortPath := filepath.Join(dir, "short.key")
	if err := os.WriteFile(shortPath, []byte(base64.StdEncoding.EncodeToString(make([]byte, 24))), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.LoadKey(shortPath, ""); err == nil {
		t.Fatal("LoadKey(wrong-length file key): want error")
	}

	// No source at all.
	if _, err := secrets.LoadKey("", ""); err == nil {
		t.Fatal("LoadKey(no source): want error")
	}

	// Both sources present → ambiguous, fail loud.
	okFile := filepath.Join(dir, "ok.key")
	if err := os.WriteFile(okFile, []byte(validB64), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.LoadKey(okFile, validB64); err == nil {
		t.Fatal("LoadKey(both file and env): want ambiguity error")
	}

	// Env value not base64.
	if _, err := secrets.LoadKey("", "@@@ nope @@@"); err == nil {
		t.Fatal("LoadKey(non-base64 env): want error")
	}
	// Env value wrong length.
	if _, err := secrets.LoadKey("", base64.StdEncoding.EncodeToString(make([]byte, 16))); err == nil {
		t.Fatal("LoadKey(wrong-length env key): want error")
	}
}

// TestSecretsErrorHygiene is the secret-hygiene guard (design §Безопасность):
// no error a caller might log may carry raw key bytes or plaintext. Mirrors the
// substring-scan style of store/ca_test.go's "ActiveCAs leaked the CA private
// key" assertion.
func TestSecretsErrorHygiene(t *testing.T) {
	keyA := key32(0xA1)
	keyB := key32(0xB2)
	a, err := secrets.New(keyA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := secrets.New(keyB)
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("TOP-SECRET-ghp_do_not_leak_me_0123456789")
	env, err := a.Encrypt(plaintext, colToken)
	if err != nil {
		t.Fatal(err)
	}

	// Wrong-key Decrypt (key_id mismatch — the DR path).
	if _, err := b.Decrypt(env, colToken); err == nil {
		t.Fatal("wrong-key Decrypt must fail")
	} else {
		assertNoSecret(t, "wrong-key Decrypt", err.Error(), keyA, keyB, plaintext)
	}

	// Wrong-aad Decrypt (same key, GCM authentication failure).
	if _, err := a.Decrypt(env, colKey); err == nil {
		t.Fatal("wrong-aad Decrypt must fail")
	} else {
		assertNoSecret(t, "wrong-aad Decrypt", err.Error(), keyA, keyB, plaintext)
	}

	// New with a bad key length must not echo the key bytes.
	badKey := bytes.Repeat([]byte{0xCC}, 40)
	if _, err := secrets.New(badKey); err == nil {
		t.Fatal("New(40-byte key) must fail")
	} else {
		assertNoSecret(t, "New bad length", err.Error(), badKey)
	}

	// LoadKey with a wrong-length key must not echo the decoded bytes.
	rawKey := bytes.Repeat([]byte{0xD5}, 20)
	if _, err := secrets.LoadKey("", base64.StdEncoding.EncodeToString(rawKey)); err == nil {
		t.Fatal("LoadKey(wrong-length) must fail")
	} else {
		assertNoSecret(t, "LoadKey bad length", err.Error(), rawKey)
	}
}

// assertNoSecret fails if msg contains any of the given secrets as a raw
// substring (key bytes or plaintext must never surface in an error string).
func assertNoSecret(t *testing.T, label, msg string, secretVals ...[]byte) {
	t.Helper()
	for _, s := range secretVals {
		if len(s) > 0 && strings.Contains(msg, string(s)) {
			t.Fatalf("%s error leaked a secret (key or plaintext) in its text: %q", label, msg)
		}
	}
}
