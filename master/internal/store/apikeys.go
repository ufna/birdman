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
// which is fine — that path only reads ID/Name/Scopes and the binding.
//
// Binding (environments v1 §5): the (ProjectID, Project, Env) triple is the
// optional key binding. All nil → a global key: the pre-env default, allowed on
// every surface (existing keys keep working). All set → a key scoped to exactly
// one (project, env) on the deploy/matchmaking/allocate surfaces. Project is the
// slug (for display and enforcement), ProjectID the uuid. Parity is a DB CHECK
// (api_keys_binding_all_or_nothing) and revalidated on create; an admin key is
// never bound (rejected on create). AuthAPIKey and ListAPIKeys fill the triple;
// Revoke/Purge leave it nil (their callers only audit id/name).
type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ProjectID *string    `json:"project_id,omitempty"`
	Project   *string    `json:"project,omitempty"`
	Env       *string    `json:"env,omitempty"`
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

// CreateAPIKeyParams is the input to CreateAPIKey. Project/Env are the optional
// binding (environments v1 §5): both nil → a global key (the pre-env default);
// both set → a key bound to that (project, env) pair. A half-set pair, a binding
// on the admin scope, or a binding to a non-existent project/env is rejected
// with a clear error (the API maps every CreateAPIKey error to 400).
type CreateAPIKeyParams struct {
	Name    string
	Scopes  []string
	Project *string // project slug of the binding; nil → global
	Env     *string // environment name of the binding; nil → global
}

// CreateAPIKey mints a key with the given scopes (and optional (project, env)
// binding) and returns the bearer secret — shown exactly once.
func (s *Store) CreateAPIKey(ctx context.Context, p CreateAPIKeyParams) (APIKey, string, error) {
	if p.Name == "" || len(p.Scopes) == 0 {
		return APIKey{}, "", fmt.Errorf("name and scopes are required")
	}
	// Binding is all-or-nothing (mirrors api_keys_binding_all_or_nothing) —
	// validate in code so the operator gets a clear message, not a raw CHECK.
	if (p.Project == nil) != (p.Env == nil) {
		return APIKey{}, "", fmt.Errorf("project and env must be set together: a key is either bound to a (project, env) pair or global")
	}
	bound := p.Project != nil
	// A binding is incompatible with the admin scope (environments v1 §5, I8):
	// admin is platform-wide, scoping it to a single env is contradictory.
	if bound && slices.Contains(p.Scopes, "admin") {
		return APIKey{}, "", fmt.Errorf("an admin-scoped key cannot be bound to a project/env")
	}
	var projectID *string
	if bound {
		// Resolve the slug to an id explicitly — NOT ensureProject: a binding to a
		// project that does not exist is a 400, never an implicit project creation.
		var pid string
		err := s.Pool.QueryRow(ctx,
			`select id::text from projects where slug = $1`, *p.Project).Scan(&pid)
		if errors.Is(err, pgx.ErrNoRows) {
			return APIKey{}, "", fmt.Errorf("no such project %q: %w", *p.Project, ErrNotFound)
		}
		if err != nil {
			return APIKey{}, "", err
		}
		// The env must exist for the project (clean error instead of the FK's 23503).
		var envExists bool
		if err := s.Pool.QueryRow(ctx,
			`select exists(select 1 from environments where project_id = $1::uuid and name = $2)`,
			pid, *p.Env).Scan(&envExists); err != nil {
			return APIKey{}, "", err
		}
		if !envExists {
			return APIKey{}, "", fmt.Errorf("no such environment %s/%s", *p.Project, *p.Env)
		}
		projectID = &pid
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
		insert into api_keys (name, hash, scopes, project_id, env)
		values ($1, $2, $3, $4::uuid, $5)
		returning id::text, name, scopes, project_id::text, env, created_at, revoked_at`,
		p.Name, string(hash), p.Scopes, projectID, p.Env).
		Scan(&k.ID, &k.Name, &k.Scopes, &k.ProjectID, &k.Env, &k.CreatedAt, &k.RevokedAt)
	// Гонка с DELETE env между пре-чеком и insert'ом (только для bound-ключа —
	// у глобального project_id/env суть NULL, api_keys_env_fk на них молчит):
	// 23503 → внятный «no such environment» (400), а не сырой 500 (w5).
	if bound {
		if mapped := mapEnvFKViolation(err, *p.Project, *p.Env); mapped != nil {
			return APIKey{}, "", mapped
		}
	}
	if err != nil {
		return APIKey{}, "", err
	}
	k.Project = p.Project // slug for display/enforcement (nil for a global key)
	return k, composeToken(apiKeyPrefix, k.ID, secret), nil
}

// ListAPIKeys returns all keys (active and revoked), newest first, without any
// secret — the /v1/apikeys admin read. Active keys carry revoked_at = null.
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.Pool.Query(ctx, `
		select k.id::text, k.name, k.scopes, p.slug, k.project_id::text, k.env, k.created_at, k.revoked_at
		from api_keys k left join projects p on p.id = k.project_id
		order by k.created_at desc, k.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Scopes, &k.Project, &k.ProjectID, &k.Env, &k.CreatedAt, &k.RevokedAt); err != nil {
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

// PurgeAPIKey hard-deletes an already-revoked key row (registries v1 design
// §6): unlike RevokeAPIKey it never revokes anything — it is a cleanup step
// on top of a key that is already stopped, so revoked keys don't pile up in
// the admin list forever. The three-way result distinguishes: purged=true
// (row deleted, the returned APIKey carries its last known fields for the
// audit event); notRevoked=true (the key exists but is still active — purge
// refuses, caller answers 409 not_revoked); both false (no such key — 404,
// including on a retried purge of an id already deleted). Like RevokeAPIKey,
// the read and the delete share one transaction (row locked FOR UPDATE) so a
// concurrent revoke/purge on the same id can't race the active/gone check
// against the delete.
func (s *Store) PurgeAPIKey(ctx context.Context, id string) (APIKey, bool, bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return APIKey{}, false, false, err
	}
	defer tx.Rollback(ctx)

	var k APIKey
	err = tx.QueryRow(ctx, `
		select id::text, name, scopes, created_at, revoked_at
		from api_keys where id = $1::uuid for update`, id).
		Scan(&k.ID, &k.Name, &k.Scopes, &k.CreatedAt, &k.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, false, false, nil // no such key
	}
	if err != nil {
		return APIKey{}, false, false, err
	}
	if k.RevokedAt == nil {
		return k, false, true, nil // active — purge never revokes
	}
	if _, err := tx.Exec(ctx, `delete from api_keys where id = $1::uuid`, id); err != nil {
		return APIKey{}, false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return APIKey{}, false, false, err
	}
	return k, true, false, nil
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
		select k.id::text, k.name, k.scopes, k.hash, p.slug, k.project_id::text, k.env
		from api_keys k left join projects p on p.id = k.project_id
		where k.id = $1::uuid and k.revoked_at is null`, id).
		Scan(&k.ID, &k.Name, &k.Scopes, &hash, &k.Project, &k.ProjectID, &k.Env)
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
