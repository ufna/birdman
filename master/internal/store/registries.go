package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Registry is one row of `registries` WITHOUT the token — the admin-facing
// read (docs/superpowers/specs/2026-07-09-registries-design.md §1). The
// secret never lives on this struct; ListRegistryCreds is the only read that
// carries it, and that one is for agentlink dispatch only, never HTTP.
type Registry struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RegistryCred is one private-registry credential WITH the token — used only
// to build the agentlink SetRegistries snapshot (proto/agentlink/v1, field
// 11). Never serialize this to an HTTP response or a log line.
type RegistryCred struct {
	Host     string
	Username string
	Token    string
}

const registryCols = `id::text, host, username, note, created_at, updated_at`

// NormalizeRegistryHost validates and lowercases a registry host
// (docs/superpowers/specs/2026-07-09-registries-design.md §1). It rejects an
// empty host, a host carrying a scheme (`http://`/`https://`/anything else
// with `://`), a host carrying a path (a slash), and docker.io/
// index.docker.io — v1 does not support docker.io: containerd resolves it to
// registry-1.docker.io, so an exact host-match against image_ref would
// silently never fire (§3).
func NormalizeRegistryHost(raw string) (string, error) {
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
	if h == "docker.io" || h == "index.docker.io" {
		return "", fmt.Errorf("docker.io is not supported in v1 (host-match cannot follow its registry-1.docker.io resolution) — got %q", raw)
	}
	return h, nil
}

// UpsertRegistry creates a registry or — when the (normalized) host already
// has one — replaces its username/token/note in place (`on conflict (host)`)
// and bumps updated_at. host is validated/normalized here too (defense in
// depth: the httpapi layer also calls NormalizeRegistryHost for a friendlier
// 400, but this is the actual gate the unique constraint relies on). token is
// required on every call — there is no "edit note only" path; a fresh token
// accompanies every write, by design (no panel form omits it).
func (s *Store) UpsertRegistry(ctx context.Context, host, username, token, note string) (Registry, error) {
	h, err := NormalizeRegistryHost(host)
	if err != nil {
		return Registry{}, err
	}
	if username == "" {
		return Registry{}, errors.New("username is required")
	}
	if token == "" {
		return Registry{}, errors.New("token is required")
	}
	// Encrypt the token before it ever reaches SQL: only the AEAD envelope is
	// written, so pg_dump/DB files carry ciphertext (design §4). AAD binds it to
	// this column — a value replayed into internal_ca.key_pem would not open.
	encToken, err := s.codec.Encrypt([]byte(token), "registries.token")
	if err != nil {
		return Registry{}, err
	}
	var r Registry
	err = s.Pool.QueryRow(ctx, `
		insert into registries (host, username, token, note)
		values ($1, $2, $3, $4)
		on conflict (host) do update set
			username = excluded.username,
			token = excluded.token,
			note = excluded.note,
			updated_at = now()
		returning `+registryCols,
		h, username, encToken, note).
		Scan(&r.ID, &r.Host, &r.Username, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Registry{}, err
	}
	return r, nil
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
		if err := rows.Scan(&r.ID, &r.Host, &r.Username, &r.Note, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRegistryCreds returns every registry WITH its token, host ascending —
// for agentlink's SetRegistries snapshot ONLY (T3). Never expose this read
// over HTTP or log its result.
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
		// Strict read (design §4): after the startup encrypt-existing pass every
		// stored token is an envelope, so a non-envelope value is an error, not a
		// silent passthrough. The error carries no token bytes; the agentlink
		// caller logs it without the value.
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
		Scan(&r.ID, &r.Host, &r.Username, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Registry{}, false, nil
	}
	if err != nil {
		return Registry{}, false, err
	}
	return r, true, nil
}
