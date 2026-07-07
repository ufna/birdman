package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type Project struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	MatchSize int32     `json:"match_size"`
	CreatedAt time.Time `json:"created_at"`
}

// GetProject returns a project by slug (ErrNotFound when missing).
func (s *Store) GetProject(ctx context.Context, slug string) (Project, error) {
	var p Project
	err := s.Pool.QueryRow(ctx, `
		select id::text, slug, match_size, created_at
		from projects where slug = $1`, slug).
		Scan(&p.ID, &p.Slug, &p.MatchSize, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, fmt.Errorf("project %q: %w", slug, ErrNotFound)
	}
	return p, err
}

// SetProjectMatchSize upserts the project (implicit creation on first
// reference, v0 convention) and sets its match_size
// (PUT /v1/projects/{slug}, docs/specs/master.md §4).
func (s *Store) SetProjectMatchSize(ctx context.Context, slug string, matchSize int32) (Project, error) {
	if matchSize < 1 {
		return Project{}, fmt.Errorf("match_size must be >= 1")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback(ctx)
	projectID, err := ensureProject(ctx, tx, slug)
	if err != nil {
		return Project{}, err
	}
	var p Project
	err = tx.QueryRow(ctx, `
		update projects set match_size = $2 where id = $1::uuid
		returning id::text, slug, match_size, created_at`,
		projectID, matchSize).
		Scan(&p.ID, &p.Slug, &p.MatchSize, &p.CreatedAt)
	if err != nil {
		return Project{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Project{}, err
	}
	return p, nil
}

// SoleProjectSlug returns the slug of the only existing project — the v0
// default when a matchmaking ticket does not name one. ErrNotFound when no
// project exists yet, ErrConflict when several do (the ticket must then name
// its project explicitly).
func (s *Store) SoleProjectSlug(ctx context.Context) (string, error) {
	rows, err := s.Pool.Query(ctx, `select slug from projects order by created_at limit 2`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return "", err
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(slugs) {
	case 0:
		return "", fmt.Errorf("no projects: %w", ErrNotFound)
	case 1:
		return slugs[0], nil
	default:
		return "", fmt.Errorf("several projects exist, ticket must name one: %w", ErrConflict)
	}
}
