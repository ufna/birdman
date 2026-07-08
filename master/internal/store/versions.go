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
	Channel  string
}

// CreateVersion registers a build (POST /v1/versions, CI entry point).
func (s *Store) CreateVersion(ctx context.Context, p CreateVersionParams) (Version, error) {
	if p.Semver == "" || p.ImageRef == "" {
		return Version{}, fmt.Errorf("semver and image_ref are required")
	}
	if p.Channel != "staging" && p.Channel != "prod" {
		return Version{}, fmt.Errorf("channel must be 'staging' or 'prod'")
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
	var v Version
	err = tx.QueryRow(ctx, `
		insert into versions (project_id, semver, image_ref, channel)
		values ($1::uuid, $2, $3, $4)
		returning id::text, project_id::text, semver, image_ref, channel, state, created_at`,
		projectID, p.Semver, p.ImageRef, p.Channel).
		Scan(&v.ID, &v.ProjectID, &v.Semver, &v.ImageRef, &v.Channel, &v.State, &v.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Version{}, fmt.Errorf("version %s/%s (%s): %w", p.Project, p.Semver, p.Channel, ErrConflict)
	}
	if err != nil {
		return Version{}, err
	}
	v.Project = p.Project
	if err := insertEvent(ctx, tx, EventVersionRegistered, EventRef{VersionID: &v.ID},
		map[string]any{"project": p.Project, "semver": p.Semver, "channel": p.Channel, "image_ref": p.ImageRef}); err != nil {
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
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.channel, v.state, v.created_at, v.deprecated_at
		from versions v join projects p on p.id = v.project_id
		order by v.created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Channel, &v.State, &v.CreatedAt, &v.DeprecatedAt); err != nil {
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
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.channel, v.state, v.created_at, v.deprecated_at
		from versions v join projects p on p.id = v.project_id
		where v.id = $1::uuid`, id).
		Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Channel, &v.State, &v.CreatedAt, &v.DeprecatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, fmt.Errorf("version %s: %w", id, ErrNotFound)
	}
	return v, err
}
