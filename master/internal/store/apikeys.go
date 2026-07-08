package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// ErrBadAPIKey is returned when API key authentication fails.
var ErrBadAPIKey = errors.New("bad_api_key")

// ErrLastAdminKey is returned by RevokeAPIKey when revoking the key would
// leave no active admin key — a self-lockout the API refuses (→ 409).
var ErrLastAdminKey = errors.New("last_admin_key")

// APIKey is one row of api_keys. The secret is never stored (only bcrypt of it)
// and never lives on this struct. CreatedAt/RevokedAt are filled by the
// management reads (List/Revoke/Create); they stay zero on the auth hot path,
// which is fine — that path only reads ID/Name/Scopes.
type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

// CountActiveAPIKeys — used by startup bootstrap.
func (s *Store) CountActiveAPIKeys(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx,
		`select count(*) from api_keys where revoked_at is null`).Scan(&n)
	return n, err
}

// CreateAPIKey mints a key with the given scopes and returns the bearer
// secret — shown exactly once.
func (s *Store) CreateAPIKey(ctx context.Context, name string, scopes []string) (APIKey, string, error) {
	if name == "" || len(scopes) == 0 {
		return APIKey{}, "", fmt.Errorf("name and scopes are required")
	}
	secret, err := newSecret()
	if err != nil {
		return APIKey{}, "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return APIKey{}, "", err
	}
	var k APIKey
	err = s.Pool.QueryRow(ctx, `
		insert into api_keys (name, hash, scopes)
		values ($1, $2, $3)
		returning id::text, name, scopes, created_at, revoked_at`, name, string(hash), scopes).
		Scan(&k.ID, &k.Name, &k.Scopes, &k.CreatedAt, &k.RevokedAt)
	if err != nil {
		return APIKey{}, "", err
	}
	return k, composeToken(apiKeyPrefix, k.ID, secret), nil
}

// ListAPIKeys returns all keys (active and revoked), newest first, without any
// secret — the /v1/apikeys admin read. Active keys carry revoked_at = null.
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.Pool.Query(ctx, `
		select id::text, name, scopes, created_at, revoked_at
		from api_keys order by created_at desc, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Scopes, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey stamps revoked_at on a key and returns the row plus whether this
// call actually changed it (false → it was already revoked; the caller then
// skips the audit event and cache flush). It refuses to revoke the last active
// admin key (ErrLastAdminKey → 409) so an operator cannot lock themselves out.
// The check and the write share one transaction (row locked FOR UPDATE) so
// concurrent revokes cannot both pass. A revoked key stops authenticating at
// the DB layer (AuthAPIKey filters revoked_at is null); the caller must also
// flush the in-memory auth cache.
func (s *Store) RevokeAPIKey(ctx context.Context, id string) (APIKey, bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return APIKey{}, false, err
	}
	defer tx.Rollback(ctx)

	var k APIKey
	err = tx.QueryRow(ctx, `
		select id::text, name, scopes, created_at, revoked_at
		from api_keys where id = $1::uuid for update`, id).
		Scan(&k.ID, &k.Name, &k.Scopes, &k.CreatedAt, &k.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, false, fmt.Errorf("api key %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return APIKey{}, false, err
	}
	if k.RevokedAt != nil {
		return k, false, nil // already revoked — idempotent no-op
	}
	if slices.Contains(k.Scopes, "admin") {
		var others int
		if err := tx.QueryRow(ctx, `
			select count(*) from api_keys
			where revoked_at is null and id <> $1::uuid and 'admin' = any(scopes)`, id).
			Scan(&others); err != nil {
			return APIKey{}, false, err
		}
		if others == 0 {
			return APIKey{}, false, ErrLastAdminKey
		}
	}
	var revokedAt time.Time
	if err := tx.QueryRow(ctx,
		`update api_keys set revoked_at = now() where id = $1::uuid returning revoked_at`, id).
		Scan(&revokedAt); err != nil {
		return APIKey{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return APIKey{}, false, err
	}
	k.RevokedAt = &revokedAt
	return k, true, nil
}

// AuthAPIKey verifies a bearer key and returns its scopes.
func (s *Store) AuthAPIKey(ctx context.Context, token string) (APIKey, error) {
	id, secret, err := parseToken(apiKeyPrefix, token)
	if err != nil {
		return APIKey{}, ErrBadAPIKey
	}
	var k APIKey
	var hash string
	err = s.Pool.QueryRow(ctx, `
		select id::text, name, scopes, hash from api_keys
		where id = $1::uuid and revoked_at is null`, id).
		Scan(&k.ID, &k.Name, &k.Scopes, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ErrBadAPIKey
	}
	if err != nil {
		return APIKey{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)) != nil {
		return APIKey{}, ErrBadAPIKey
	}
	return k, nil
}
