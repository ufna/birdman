// Package secrets encrypts the platform's reversible at-rest secrets
// (registries.token, internal_ca.key_pem) with an AEAD envelope so pg_dump
// output and on-disk DB files carry only ciphertext and the decryption key
// never rides along in a dump
// (docs/superpowers/specs/2026-07-10-secrets-encryption-design.md §1–§2).
//
// Envelope: "birdman:v1:<key_id>:<base64std(nonce||ciphertext)>". "v1" names
// the construction — AES-256-GCM with a fresh 12-byte random nonce prepended to
// the sealed ciphertext (GCM appends its auth tag). key_id = hex(sha256(key)[:4])
// fingerprints the loaded key so a restore against the wrong key fails with an
// exact, both-fingerprints diagnostic (design §6, the DR crux) instead of
// silent corruption. The AAD passed to Encrypt/Decrypt is the owning column
// name ("registries.token" | "internal_ca.key_pem"): ciphertext moved to a
// different column will not authenticate.
//
// This package is a stdlib-only leaf — it never imports config — so it cannot
// take part in an import cycle. Key material and plaintext are never placed in
// an error string, log line, or %v (same discipline as tlsutil): errors carry
// only key_id fingerprints, byte lengths, and static text.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// envPrefix is the strict, self-describing envelope prefix. base64
	// StdEncoding's alphabet contains no ':', so splitting an envelope on ':'
	// is unambiguous — the base64 body is always the fourth field.
	envPrefix = "birdman:v1:"
	// keyLen is the AES-256 key size; keyIDBytes is how many leading bytes of
	// sha256(key) form the key_id fingerprint (8 hex chars).
	keyLen     = 32
	keyIDBytes = 4
)

// Codec encrypts and decrypts reversible secrets at-rest with AES-256-GCM. One
// key per process; the envelope is self-describing and carries the key's
// fingerprint. A Codec is safe for concurrent use (cipher.AEAD is).
type Codec struct {
	aead  cipher.AEAD
	keyID string
}

// New builds a Codec from a 32-byte key (AES-256). A key of any other length is
// an error whose text carries only the length, never the key bytes.
func New(key []byte) (*Codec, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("secrets: key must be %d bytes, got %d", keyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new gcm: %w", err)
	}
	sum := sha256.Sum256(key)
	return &Codec{aead: aead, keyID: hex.EncodeToString(sum[:keyIDBytes])}, nil
}

// KeyID returns the loaded key's fingerprint, hex(sha256(key)[:4]).
func (c *Codec) KeyID() string { return c.keyID }

// Encrypt seals plaintext under a fresh random nonce and returns the envelope
// "birdman:v1:<key_id>:<base64std(nonce||ciphertext)>". aad is the owning
// column name; it is authenticated but not encrypted, so a value replayed into
// a different column will not open.
func (c *Codec) Encrypt(plaintext []byte, aad string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secrets: read nonce: %w", err)
	}
	// Seal appends ciphertext+tag to its first argument, so the result is
	// nonce||ciphertext||tag in one contiguous slice.
	sealed := c.aead.Seal(nonce, nonce, plaintext, []byte(aad))
	return envPrefix + c.keyID + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens an envelope produced by Encrypt under the same aad. It is
// strict: a value without the "birdman:v1:" prefix → "not an encrypted
// envelope"; a key_id that differs from the loaded key → an error naming BOTH
// fingerprints (the DR diagnostic); malformed base64, a body shorter than the
// nonce, tampering, or a wrong aad → error. No error ever carries plaintext or
// key bytes.
func (c *Codec) Decrypt(envelope string, aad string) ([]byte, error) {
	if !IsEncrypted(envelope) {
		return nil, errors.New("secrets: not an encrypted envelope")
	}
	// "birdman" : "v1" : "<key_id>" : "<base64>" — exactly four ':'-separated
	// fields; the base64 body has no ':' so SplitN(…, 4) isolates it cleanly.
	parts := strings.SplitN(envelope, ":", 4)
	if len(parts) != 4 || parts[2] == "" || parts[3] == "" {
		return nil, errors.New("secrets: malformed envelope")
	}
	envKeyID := parts[2]
	if envKeyID != c.keyID {
		// The DR crux: name both fingerprints so an operator sees exactly which
		// key the ciphertext needs versus which key is loaded (design §6). key_id
		// is a hash prefix, not key material.
		return nil, fmt.Errorf("secrets: secret encrypted with key %s, loaded key is %s", envKeyID, c.keyID)
	}
	raw, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return nil, fmt.Errorf("secrets: malformed envelope: invalid base64: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("secrets: malformed envelope: body shorter than nonce")
	}
	nonce, ciphertext := raw[:ns], raw[ns:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, []byte(aad))
	if err != nil {
		// Deliberately generic: covers tampering, a wrong aad, and any body that
		// does not authenticate. Never echo ciphertext or key material.
		return nil, errors.New("secrets: decrypt failed: envelope does not authenticate")
	}
	return plaintext, nil
}

// IsEncrypted reports whether s is a birdman v1 envelope (strict prefix). Read
// paths use it to tell an already-encrypted value from a legacy plaintext one.
func IsEncrypted(s string) bool {
	return strings.HasPrefix(s, envPrefix)
}

// LoadKey resolves the 32-byte master key from exactly one source and returns
// the raw key bytes (design §2). Callers pass primitives — the resolved file
// path (from config secrets_key_file / env BIRDMAN_SECRETS_KEY_FILE) and the
// dev/test env value (BIRDMAN_SECRETS_KEY) — which keeps this package a
// stdlib-only leaf and rules out an import cycle with config; the thin wiring
// that maps config.Config → these two strings lives in the main/config layer.
//
// Fail-loud, actionable errors: no source, both sources (ambiguous), a missing
// or unreadable file, a value that is not base64, or a key that is not exactly
// 32 bytes — each is a clear error. No error ever carries the key bytes; the
// file's permission bits are NOT enforced here (an over-permissive mode is a
// WARN in the caller, design §2), only the key's validity.
func LoadKey(path string, envKeyValue string) ([]byte, error) {
	switch {
	case path != "" && envKeyValue != "":
		return nil, fmt.Errorf("secrets key is ambiguous: both a key file (%s) and BIRDMAN_SECRETS_KEY are set; provide exactly one", path)
	case path != "":
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("secrets key required: cannot read key file: %w", err)
		}
		key, err := decodeKey(string(raw))
		if err != nil {
			return nil, fmt.Errorf("secrets key file %s: %w", path, err)
		}
		return key, nil
	case envKeyValue != "":
		key, err := decodeKey(envKeyValue)
		if err != nil {
			return nil, fmt.Errorf("BIRDMAN_SECRETS_KEY: %w", err)
		}
		return key, nil
	default:
		return nil, errors.New("secrets key required: set secrets_key_file (or BIRDMAN_SECRETS_KEY_FILE), or BIRDMAN_SECRETS_KEY for dev/test — master cannot decrypt registries.token / internal_ca.key_pem without it")
	}
}

// decodeKey base64-decodes a whitespace-trimmed key value (the box file is
// written by `openssl rand -base64 32`, so it ends in a newline) and enforces
// the 32-byte length. Errors carry only the decoded length or a base64 byte
// offset, never the bytes themselves.
func decodeKey(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	if len(raw) != keyLen {
		return nil, fmt.Errorf("decoded key is %d bytes, want %d", len(raw), keyLen)
	}
	return raw, nil
}
