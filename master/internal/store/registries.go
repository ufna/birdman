package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Registry is one row of `registries` WITHOUT the token — the admin-facing
// read (docs/superpowers/specs/2026-07-09-registries-design.md §1, extended by
// the Реестры v2 design §1 with Type). The secret never lives on this struct;
// ListRegistryCreds is the only read that carries it, and that one is for
// agentlink dispatch only, never HTTP.
type Registry struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Type      string    `json:"type"`
	Username  string    `json:"username"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RegistryCred is one private-registry credential WITH the token — used only
// to build the agentlink SetRegistries snapshot (proto/agentlink/v1, field
// 11). Never serialize this to an HTTP response or a log line. It is
// deliberately type-agnostic: the master has already normalized every type
// into docker-basic-auth (username, secret) on write (Реестры v2 §1), so the
// distribution path needs no type.
type RegistryCred struct {
	Host     string
	Username string
	Token    string
}

// Registry credential types (Реестры v2 §1). The type drives the panel form,
// the per-type validation, and the server-side credential normalization; the
// distribution columns (host/username/token) are always docker-basic-auth.
const (
	RegistryTypeGHCR    = "ghcr"    // GitHub Container Registry: user + PAT (read:packages)
	RegistryTypeGAR     = "gar"     // Google Artifact Registry / legacy GCR: _json_key + SA-JSON key
	RegistryTypeGeneric = "generic" // any docker registry: user + password
)

const registryCols = `id::text, host, type, username, note, created_at, updated_at`

// normalizeHostShape lowercases/trims a host and rejects the structural
// invalids shared by every registry type: empty, a scheme (`://`), or a path
// (a slash). It does NOT apply the generic-only docker.io policy — that lives
// in NormalizeRegistryHost. Returns the normalized host.
func normalizeHostShape(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	if h == "" {
		return "", errors.New("host is required")
	}
	if strings.Contains(h, "://") {
		return "", fmt.Errorf("host must not include a scheme (got %q)", raw)
	}
	if strings.Contains(h, "/") {
		return "", fmt.Errorf("host must not include a path (got %q)", raw)
	}
	return h, nil
}

// NormalizeRegistryHost validates and lowercases a registry host
// (docs/superpowers/specs/2026-07-09-registries-design.md §1). It rejects an
// empty host, a host carrying a scheme (`http://`/`https://`/anything else
// with `://`), a host carrying a path (a slash), and docker.io/
// index.docker.io — v1 does not support docker.io: containerd resolves it to
// registry-1.docker.io, so an exact host-match against image_ref would
// silently never fire (§3). This is the `generic`-type host rule; ghcr/gar
// have their own shape checks in ValidateRegistry.
func NormalizeRegistryHost(raw string) (string, error) {
	h, err := normalizeHostShape(raw)
	if err != nil {
		return "", err
	}
	if h == "docker.io" || h == "index.docker.io" {
		return "", fmt.Errorf("docker.io is not supported in v1 (host-match cannot follow its registry-1.docker.io resolution) — got %q", raw)
	}
	return h, nil
}

// isGARHost reports whether h is a Google container-registry host: an Artifact
// Registry host `REGION-docker.pkg.dev` (or the bare `docker.pkg.dev`), or a
// legacy GCR host `gcr.io` / `REGION.gcr.io` (Реестры v2 §3). h must already be
// shape-normalized (lowercase, no scheme/slash).
func isGARHost(h string) bool {
	if h == "docker.pkg.dev" || strings.HasSuffix(h, "-docker.pkg.dev") {
		return true
	}
	if h == "gcr.io" || strings.HasSuffix(h, ".gcr.io") {
		return true
	}
	return false
}

// validateGARKey checks that a gar secret is a service-account JSON key — a
// JSON object with "type":"service_account" and a non-empty "private_key"
// (Реестры v2 §3). A malformed key fails here, at add/patch time, not at the
// first pull. The error is static and never echoes the secret bytes.
func validateGARKey(token string) error {
	const want = "GAR credential must be a service-account JSON key"
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(token), &obj); err != nil {
		return errors.New(want)
	}
	var typ string
	if err := json.Unmarshal(obj["type"], &typ); err != nil || typ != "service_account" {
		return errors.New(want)
	}
	var pk string
	if err := json.Unmarshal(obj["private_key"], &pk); err != nil || pk == "" {
		return errors.New(want)
	}
	return nil
}

// validateRegistryHostUser normalizes the host per type and returns the
// docker-login username the type implies — gar forces `_json_key` (the passed
// username is ignored), ghcr/generic keep the passed username (required). It
// does NOT look at the secret: it is the shared core of both ValidateRegistry
// (add / rotate, which then also checks the secret) and PatchRegistry's
// keep-secret path (where only ciphertext is on hand, so the JSON shape cannot
// be re-checked). An unknown type is an error.
func validateRegistryHostUser(rawHost, typ, username string) (host, user string, err error) {
	switch typ {
	case RegistryTypeGHCR:
		h, err := normalizeHostShape(rawHost)
		if err != nil {
			return "", "", err
		}
		if h != "ghcr.io" {
			return "", "", fmt.Errorf("ghcr type expects host ghcr.io (got %q)", rawHost)
		}
		if username == "" {
			return "", "", errors.New("username is required")
		}
		return h, username, nil
	case RegistryTypeGAR:
		h, err := normalizeHostShape(rawHost)
		if err != nil {
			return "", "", err
		}
		if !isGARHost(h) {
			return "", "", fmt.Errorf("gar type expects a REGION-docker.pkg.dev (Artifact Registry) or gcr.io (legacy GCR) host (got %q)", rawHost)
		}
		// The master forces the docker-login user; the panel never sends one.
		return h, "_json_key", nil
	case RegistryTypeGeneric:
		h, err := NormalizeRegistryHost(rawHost)
		if err != nil {
			return "", "", err
		}
		if username == "" {
			return "", "", errors.New("username is required")
		}
		return h, username, nil
	default:
		return "", "", fmt.Errorf("unknown registry type %q (want ghcr, gar or generic)", typ)
	}
}

// ValidateRegistry normalizes the host and username for a registry write and
// validates the secret per type (Реестры v2 §3). It returns the normalized
// (host, docker-login-username): for gar the username is forced to `_json_key`
// regardless of the input, and the token must be a service-account JSON key;
// for ghcr the host must be ghcr.io; for generic the v1 host rules apply. The
// token must be non-empty for every type (ghcr PAT / generic password are only
// checked for presence — they cannot be verified without contacting the
// registry). No error ever carries the token bytes.
func ValidateRegistry(rawHost, typ, username, token string) (host, user string, err error) {
	host, user, err = validateRegistryHostUser(rawHost, typ, username)
	if err != nil {
		return "", "", err
	}
	if token == "" {
		return "", "", errors.New("token is required")
	}
	if typ == RegistryTypeGAR {
		if err := validateGARKey(token); err != nil {
			return "", "", err
		}
	}
	return host, user, nil
}

// UpsertRegistry creates a registry or — when the (normalized) host already
// has one — replaces its type/username/token/note in place (`on conflict
// (host)`) and bumps updated_at. Host and username are validated/normalized by
// ValidateRegistry (the gar `_json_key` forcing and per-type host/secret
// checks); the raw token is the value to store (the whole SA-JSON key for gar,
// the PAT/password otherwise). token is required on every call — there is no
// "edit note only" path here; PatchRegistry is the partial-update door.
func (s *Store) UpsertRegistry(ctx context.Context, host, typ, username, token, note string) (Registry, error) {
	h, user, err := ValidateRegistry(host, typ, username, token)
	if err != nil {
		return Registry{}, err
	}
	// Encrypt the token before it ever reaches SQL: only the AEAD envelope is
	// written, so pg_dump/DB files carry ciphertext (secrets-v1 §4). AAD binds
	// it to this column — a value replayed into internal_ca.key_pem would not
	// open.
	encToken, err := s.codec.Encrypt([]byte(token), "registries.token")
	if err != nil {
		return Registry{}, err
	}
	var r Registry
	err = s.Pool.QueryRow(ctx, `
		insert into registries (host, type, username, token, note)
		values ($1, $2, $3, $4, $5)
		on conflict (host) do update set
			type = excluded.type,
			username = excluded.username,
			token = excluded.token,
			note = excluded.note,
			updated_at = now()
		returning `+registryCols,
		h, typ, user, encToken, note).
		Scan(&r.ID, &r.Host, &r.Type, &r.Username, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Registry{}, err
	}
	return r, nil
}

// PatchRegistry partially updates a registry by id (Реестры v2 §2). type,
// username and note are pointers — nil means "leave unchanged". host is NOT a
// parameter: it is immutable (the agent's match key + identity; to change it,
// delete + re-add), and the row's own host is what the per-type validation
// runs against.
//
// The secret is optional: token=="" keeps the existing ciphertext UNTOUCHED
// (no re-encrypt, no read of the plaintext); a non-empty token is encrypted
// and rotated in. Because a kept secret's JSON shape cannot be re-validated
// (only ciphertext is on hand), the keep path validates host+type+username
// only, and a type change TO gar without a fresh token is rejected — the
// `_json_key` + SA-JSON invariant can't be retro-fitted onto an opaque
// ciphertext.
//
// Returns the updated Registry (no token) and a found bool (false, nil error →
// no such id, so the caller answers 404).
func (s *Store) PatchRegistry(ctx context.Context, id string, typ, username, note *string, token string) (Registry, bool, error) {
	var cur Registry
	err := s.Pool.QueryRow(ctx, `select `+registryCols+` from registries where id = $1::uuid`, id).
		Scan(&cur.ID, &cur.Host, &cur.Type, &cur.Username, &cur.Note, &cur.CreatedAt, &cur.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Registry{}, false, nil
	}
	if err != nil {
		return Registry{}, false, err
	}

	effType := derefOr(typ, cur.Type)
	effUser := derefOr(username, cur.Username)
	effNote := derefOr(note, cur.Note)

	var normUser string
	if token == "" {
		// Keep the existing secret. A type change TO gar can't retro-fit the
		// _json_key + SA-JSON invariant without a fresh key.
		if effType == RegistryTypeGAR && cur.Type != RegistryTypeGAR {
			return Registry{}, false, errors.New("changing type to gar requires a new token (a service-account JSON key)")
		}
		_, normUser, err = validateRegistryHostUser(cur.Host, effType, effUser)
		if err != nil {
			return Registry{}, false, err
		}
	} else {
		_, normUser, err = ValidateRegistry(cur.Host, effType, effUser, token)
		if err != nil {
			return Registry{}, false, err
		}
	}

	var r Registry
	if token == "" {
		// Leave the token column (its ciphertext) exactly as-is.
		err = s.Pool.QueryRow(ctx, `
			update registries set type = $2, username = $3, note = $4, updated_at = now()
			where id = $1::uuid
			returning `+registryCols,
			id, effType, normUser, effNote).
			Scan(&r.ID, &r.Host, &r.Type, &r.Username, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	} else {
		encToken, encErr := s.codec.Encrypt([]byte(token), "registries.token")
		if encErr != nil {
			return Registry{}, false, encErr
		}
		err = s.Pool.QueryRow(ctx, `
			update registries set type = $2, username = $3, note = $4, token = $5, updated_at = now()
			where id = $1::uuid
			returning `+registryCols,
			id, effType, normUser, effNote, encToken).
			Scan(&r.ID, &r.Host, &r.Type, &r.Username, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	}
	if err != nil {
		return Registry{}, false, err
	}
	return r, true, nil
}

// derefOr returns *p when p is non-nil, else def — the "nil means unchanged"
// rule for PatchRegistry's optional fields.
func derefOr(p *string, def string) string {
	if p != nil {
		return *p
	}
	return def
}

// ListRegistries returns every registry, host ascending, WITHOUT tokens — the
// GET /v1/registries admin read. Never returns nil, so the JSON is [] not
// null.
func (s *Store) ListRegistries(ctx context.Context) ([]Registry, error) {
	rows, err := s.Pool.Query(ctx, `select `+registryCols+` from registries order by host asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Registry{}
	for rows.Next() {
		var r Registry
		if err := rows.Scan(&r.ID, &r.Host, &r.Type, &r.Username, &r.Note, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRegistryCreds returns every registry WITH its token, host ascending —
// for agentlink's SetRegistries snapshot ONLY (T3). Never expose this read
// over HTTP or log its result. Type-agnostic: the write path already
// normalized every type into (username, secret).
func (s *Store) ListRegistryCreds(ctx context.Context) ([]RegistryCred, error) {
	rows, err := s.Pool.Query(ctx, `select host, username, token from registries order by host asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RegistryCred{}
	for rows.Next() {
		var c RegistryCred
		var encToken string
		if err := rows.Scan(&c.Host, &c.Username, &encToken); err != nil {
			return nil, err
		}
		// Strict read (secrets-v1 §4): after the startup encrypt-existing pass
		// every stored token is an envelope, so a non-envelope value is an error,
		// not a silent passthrough. The error carries no token bytes; the
		// agentlink caller logs it without the value.
		token, err := s.codec.Decrypt(encToken, "registries.token")
		if err != nil {
			return nil, fmt.Errorf("decrypt registries.token for %s: %w", c.Host, err)
		}
		c.Token = string(token)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteRegistry removes a registry by id. It reports whether a row was
// actually deleted (false → no such id, not an error) so the caller (httpapi)
// can answer 404 for an unknown/already-removed id and skip a duplicate audit
// event. id must be a valid uuid (validated by the caller) or the query
// errors.
func (s *Store) DeleteRegistry(ctx context.Context, id string) (Registry, bool, error) {
	var r Registry
	err := s.Pool.QueryRow(ctx, `
		delete from registries where id = $1::uuid
		returning `+registryCols, id).
		Scan(&r.ID, &r.Host, &r.Type, &r.Username, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Registry{}, false, nil
	}
	if err != nil {
		return Registry{}, false, err
	}
	return r, true, nil
}
