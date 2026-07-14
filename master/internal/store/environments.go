package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Environments v1 (docs/superpowers/specs/2026-07-13-environments-v1-design.md
// §1–2): окружение — полноценное измерение платформы per-project. Поведение
// ведёт флаг production (не имя): production=true запрещает auto_deploy
// (guardrail в БД CHECK + здесь), ретеншн-дефолт безлимитный. CRUD явный;
// использованный env неудаляем (versions не удаляются — I10). Сиды dev+prod
// каждому проекту делает ensureProject (nodes.go).

type Environment struct {
	ProjectID     string    `json:"project_id"`
	Project       string    `json:"project"`
	Name          string    `json:"name"`
	Production    bool      `json:"production"`
	AutoDeploy    bool      `json:"auto_deploy"`
	RetentionKeep int       `json:"retention_keep"`
	CreatedAt     time.Time `json:"created_at"`
}

type CreateEnvironmentParams struct {
	Project       string
	Name          string
	Production    bool
	AutoDeploy    bool
	RetentionKeep int
}

// EnvironmentPatch — частичный апдейт (PATCH); nil-поле = не трогать. Имя
// иммутабельно (нет поля).
type EnvironmentPatch struct {
	Production    *bool
	AutoDeploy    *bool
	RetentionKeep *int
}

// envNameRe дублирует БД-CHECK name ~ '^[a-z0-9][a-z0-9-]{0,31}$' — чтобы
// отдавать понятный 400, а не FK/CHECK-500. Зарезервированные all/global
// проверяются отдельно (тоже CHECK в БД).
var envNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validateEnvName(name string) error {
	if !envNameRe.MatchString(name) {
		return fmt.Errorf("environment name %q must match ^[a-z0-9][a-z0-9-]{0,31}$ (lowercase letters, digits and dashes, 1–32 chars, not starting with a dash)", name)
	}
	if name == "all" || name == "global" {
		return fmt.Errorf("environment name %q is reserved", name)
	}
	return nil
}

// ListEnvironments returns a project's environments, non-production first
// (panel convention, §8), then by name.
func (s *Store) ListEnvironments(ctx context.Context, projectSlug string) ([]Environment, error) {
	rows, err := s.Pool.Query(ctx, `
		select e.project_id::text, p.slug, e.name, e.production, e.auto_deploy, e.retention_keep, e.created_at
		from environments e join projects p on p.id = e.project_id
		where p.slug = $1
		order by e.production, e.name`, projectSlug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		var e Environment
		if err := rows.Scan(&e.ProjectID, &e.Project, &e.Name, &e.Production,
			&e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEnvironment returns one environment (ErrBadEnv when missing). Every caller
// is an existence check of an env NAMED BY A REQUEST — the matchmaker ticket, the
// ?env= stats filter, the rollback body, the auto-deploy resolve — so a missing
// env is bad client input (400), not a missing resource (v3, ErrBadEnv). The
// env-as-addressed-resource routes (PATCH/DELETE /v1/environments/{p}/{name})
// have their own queries and keep ErrNotFound → 404.
func (s *Store) GetEnvironment(ctx context.Context, project, name string) (Environment, error) {
	var e Environment
	err := s.Pool.QueryRow(ctx, `
		select e.project_id::text, p.slug, e.name, e.production, e.auto_deploy, e.retention_keep, e.created_at
		from environments e join projects p on p.id = e.project_id
		where p.slug = $1 and e.name = $2`, project, name).
		Scan(&e.ProjectID, &e.Project, &e.Name, &e.Production, &e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Environment{}, badEnvErr(project, name)
	}
	return e, err
}

// AutoDeployEnvironments lists every environment (across all projects) with
// auto_deploy set — the set the deploy manager walks on Resume to restart each
// forward-only chain after a master restart (environments v1 §4).
func (s *Store) AutoDeployEnvironments(ctx context.Context) ([]Environment, error) {
	rows, err := s.Pool.Query(ctx, `
		select e.project_id::text, p.slug, e.name, e.production, e.auto_deploy, e.retention_keep, e.created_at
		from environments e join projects p on p.id = e.project_id
		where e.auto_deploy
		order by p.slug, e.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		var e Environment
		if err := rows.Scan(&e.ProjectID, &e.Project, &e.Name, &e.Production,
			&e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CreateEnvironment adds an environment (POST /v1/environments). Guardrails:
// name shape/reserved (§1 CHECK), production⇒!auto_deploy (§2). Duplicate →
// ErrConflict. The project is created on first reference (ensureProject seeds
// dev+prod), so an explicit dev/prod create collides with the seed → ErrConflict.
func (s *Store) CreateEnvironment(ctx context.Context, p CreateEnvironmentParams) (Environment, error) {
	if err := validateEnvName(p.Name); err != nil {
		return Environment{}, err
	}
	if p.Production && p.AutoDeploy {
		return Environment{}, fmt.Errorf("auto_deploy is not allowed on a production environment")
	}
	if p.RetentionKeep < 0 {
		return Environment{}, fmt.Errorf("retention_keep must be >= 0")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Environment{}, err
	}
	defer tx.Rollback(ctx)
	projectID, err := ensureProject(ctx, tx, p.Project)
	if err != nil {
		return Environment{}, err
	}
	var e Environment
	err = tx.QueryRow(ctx, `
		insert into environments (project_id, name, production, auto_deploy, retention_keep)
		values ($1::uuid, $2, $3, $4, $5)
		returning project_id::text, name, production, auto_deploy, retention_keep, created_at`,
		projectID, p.Name, p.Production, p.AutoDeploy, p.RetentionKeep).
		Scan(&e.ProjectID, &e.Name, &e.Production, &e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Environment{}, fmt.Errorf("environment %s/%s already exists: %w", p.Project, p.Name, ErrConflict)
	}
	if err != nil {
		return Environment{}, err
	}
	e.Project = p.Project
	if err := tx.Commit(ctx); err != nil {
		return Environment{}, err
	}
	return e, nil
}

// PatchEnvironment updates flags (PATCH /v1/environments/{project}/{name}).
// The guardrail production⇒!auto_deploy is re-checked on the RESULTING state
// (so enabling auto_deploy on a production env fails in any field order, §2).
// Name is immutable (not patchable). ErrNotFound for an unknown env.
func (s *Store) PatchEnvironment(ctx context.Context, project, name string, p EnvironmentPatch) (Environment, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Environment{}, err
	}
	defer tx.Rollback(ctx)

	// Текущее состояние в tx (против гонки двух PATCH).
	var e Environment
	err = tx.QueryRow(ctx, `
		select e.project_id::text, e.name, e.production, e.auto_deploy, e.retention_keep, e.created_at
		from environments e join projects p on p.id = e.project_id
		where p.slug = $1 and e.name = $2 for update of e`, project, name).
		Scan(&e.ProjectID, &e.Name, &e.Production, &e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Environment{}, fmt.Errorf("environment %s/%s: %w", project, name, ErrNotFound)
	}
	if err != nil {
		return Environment{}, err
	}

	if p.Production != nil {
		e.Production = *p.Production
	}
	if p.AutoDeploy != nil {
		e.AutoDeploy = *p.AutoDeploy
	}
	if p.RetentionKeep != nil {
		e.RetentionKeep = *p.RetentionKeep
	}
	if e.Production && e.AutoDeploy {
		return Environment{}, fmt.Errorf("auto_deploy is not allowed on a production environment")
	}
	if e.RetentionKeep < 0 {
		return Environment{}, fmt.Errorf("retention_keep must be >= 0")
	}

	err = tx.QueryRow(ctx, `
		update environments set production = $3, auto_deploy = $4, retention_keep = $5
		where project_id = $1::uuid and name = $2
		returning project_id::text, name, production, auto_deploy, retention_keep, created_at`,
		e.ProjectID, e.Name, e.Production, e.AutoDeploy, e.RetentionKeep).
		Scan(&e.ProjectID, &e.Name, &e.Production, &e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt)
	if err != nil {
		return Environment{}, err
	}
	e.Project = project
	if err := tx.Commit(ctx); err != nil {
		return Environment{}, err
	}
	return e, nil
}

// DeleteEnvironment removes a never-used environment (DELETE, 204). An env
// referenced by any versions/fleets/nodes/keys (including disabled/dead) is
// not empty → ErrConflict listing the offenders (§2, honest I10: used envs are
// effectively undeletable in v1 since versions rows are never removed).
// ErrNotFound for an unknown env.
//
// dev и prod — платформенно-гарантированные окружения: ensureProject пересевает
// их при следующем касании проекта; DELETE действует до этого момента.
func (s *Store) DeleteEnvironment(ctx context.Context, project, name string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var projectID string
	err = tx.QueryRow(ctx, `
		select e.project_id::text from environments e
		join projects p on p.id = e.project_id
		where p.slug = $1 and e.name = $2 for update of e`, project, name).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("environment %s/%s: %w", project, name, ErrNotFound)
	}
	if err != nil {
		return err
	}

	var used []string
	for _, c := range []struct{ table, label string }{
		{"versions", "versions"},
		{"fleet_configs", "fleets"},
		{"nodes", "nodes"},
		{"api_keys", "keys"},
	} {
		var exists bool
		if err := tx.QueryRow(ctx,
			`select exists(select 1 from `+c.table+` where project_id = $1::uuid and env = $2)`,
			projectID, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			used = append(used, c.label)
		}
	}
	if len(used) > 0 {
		return fmt.Errorf("environment %s/%s is not empty (%s): %w", project, name, strings.Join(used, ", "), ErrConflict)
	}

	if _, err := tx.Exec(ctx,
		`delete from environments where project_id = $1::uuid and name = $2`, projectID, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SoleEnvWithActiveNodes returns the single environment of a project that has
// active nodes with a fresh heartbeat (<30s) — the matchmaker/allocate env
// fallback when a ticket names none (§3). ErrConflict when zero or several
// environments have live nodes (the request must then name env explicitly).
func (s *Store) SoleEnvWithActiveNodes(ctx context.Context, projectSlug string) (string, error) {
	rows, err := s.Pool.Query(ctx, `
		select distinct n.env
		from nodes n join projects p on p.id = n.project_id
		where p.slug = $1 and n.state = 'active'
		  and n.last_heartbeat_at > now() - interval '30 seconds'
		order by n.env
		limit 2`, projectSlug)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var envs []string
	for rows.Next() {
		var env string
		if err := rows.Scan(&env); err != nil {
			return "", err
		}
		envs = append(envs, env)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(envs) {
	case 0:
		return "", fmt.Errorf("no environment of project %s has active nodes: %w", projectSlug, ErrConflict)
	case 1:
		return envs[0], nil
	default:
		return "", fmt.Errorf("several environments of project %s have active nodes, env is required: %w", projectSlug, ErrConflict)
	}
}
