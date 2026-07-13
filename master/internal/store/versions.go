package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrConflict is returned on unique-constraint conflicts (duplicate version).
var ErrConflict = errors.New("conflict")

type CreateVersionParams struct {
	Project  string
	Semver   string
	ImageRef string
	Env      string // окружение регистрации (environments v1); заменило прежний лейбл
}

// CreateVersion registers a build (POST /v1/versions, CI entry point) in an
// environment. env must exist for the project (versions_env_fk enforces it; the
// explicit in-tx check gives a clean 400 instead of a 500, and sees the dev/prod
// seeded by ensureProject in this very transaction for a brand-new project).
func (s *Store) CreateVersion(ctx context.Context, p CreateVersionParams) (Version, error) {
	if p.Semver == "" || p.ImageRef == "" {
		return Version{}, fmt.Errorf("semver and image_ref are required")
	}
	if p.Env == "" {
		return Version{}, fmt.Errorf("env is required")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Version{}, err
	}
	defer tx.Rollback(ctx)
	projectID, err := ensureProject(ctx, tx, p.Project)
	if err != nil {
		return Version{}, err
	}
	var envExists bool
	if err := tx.QueryRow(ctx,
		`select exists(select 1 from environments where project_id = $1::uuid and name = $2)`,
		projectID, p.Env).Scan(&envExists); err != nil {
		return Version{}, err
	}
	if !envExists {
		return Version{}, fmt.Errorf("no such environment %s/%s", p.Project, p.Env)
	}
	var v Version
	err = tx.QueryRow(ctx, `
		insert into versions (project_id, semver, image_ref, env)
		values ($1::uuid, $2, $3, $4)
		returning id::text, project_id::text, semver, image_ref, env, state, created_at`,
		projectID, p.Semver, p.ImageRef, p.Env).
		Scan(&v.ID, &v.ProjectID, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Version{}, fmt.Errorf("version %s/%s (%s): %w", p.Project, p.Semver, p.Env, ErrConflict)
	}
	if err != nil {
		return Version{}, err
	}
	v.Project = p.Project
	if err := insertEvent(ctx, tx, EventVersionRegistered, EventRef{VersionID: &v.ID},
		map[string]any{"project": p.Project, "semver": p.Semver, "env": p.Env, "image_ref": p.ImageRef}); err != nil {
		return Version{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Version{}, err
	}
	return v, nil
}

// ListVersions returns all registered versions.
func (s *Store) ListVersions(ctx context.Context) ([]Version, error) {
	rows, err := s.Pool.Query(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.env, v.state, v.created_at, v.deprecated_at, v.promoted_from::text
		from versions v join projects p on p.id = v.project_id
		order by v.created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt, &v.DeprecatedAt, &v.PromotedFrom); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersion returns one version by id.
func (s *Store) GetVersion(ctx context.Context, id string) (Version, error) {
	var v Version
	err := s.Pool.QueryRow(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.env, v.state, v.created_at, v.deprecated_at, v.promoted_from::text
		from versions v join projects p on p.id = v.project_id
		where v.id = $1::uuid`, id).
		Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt, &v.DeprecatedAt, &v.PromotedFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, fmt.Errorf("version %s: %w", id, ErrNotFound)
	}
	return v, err
}
