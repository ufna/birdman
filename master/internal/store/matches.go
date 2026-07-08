package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Match is one matches row (docs/specs/master.md §1). Lifecycle (итерация 2):
// pending (RecordMatch on allocation) → running (agent ServerEvent
// match_start) → finished|aborted (match_end, or abort when the server
// fails). players_peak is maintained from agent heartbeats while the match is
// open.
type Match struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	ServerID    string     `json:"server_id"`
	VersionID   string     `json:"version_id"`
	Region      string     `json:"region"`
	State       string     `json:"state"`
	PlayersPeak int32      `json:"players_peak"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type MatchFilter struct {
	Project string
	State   string
}

const matchColumns = `
	m.id::text, m.project_id::text, m.server_id::text, m.version_id::text,
	m.region, m.state, m.players_peak, m.started_at, m.ended_at, m.created_at`

// ListMatches returns matches, newest first, with optional filters.
func (s *Store) ListMatches(ctx context.Context, f MatchFilter) ([]Match, error) {
	q := `select ` + matchColumns + `
		from matches m join projects p on p.id = m.project_id`
	var conds []string
	var args []any
	if f.Project != "" {
		args = append(args, f.Project)
		conds = append(conds, fmt.Sprintf("p.slug = $%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("m.state = $%d", len(args)))
	}
	if len(conds) > 0 {
		q += " where " + strings.Join(conds, " and ")
	}
	q += " order by m.created_at desc limit 1000"

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Match
	for rows.Next() {
		var m Match
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.ServerID, &m.VersionID,
			&m.Region, &m.State, &m.PlayersPeak, &m.StartedAt, &m.EndedAt, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMatch returns one match by id.
func (s *Store) GetMatch(ctx context.Context, id string) (Match, error) {
	var m Match
	err := s.Pool.QueryRow(ctx, `select `+matchColumns+`
		from matches m where m.id = $1::uuid`, id).
		Scan(&m.ID, &m.ProjectID, &m.ServerID, &m.VersionID,
			&m.Region, &m.State, &m.PlayersPeak, &m.StartedAt, &m.EndedAt, &m.CreatedAt)
	return m, err
}
