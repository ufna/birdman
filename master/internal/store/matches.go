package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Match is one matches row joined with its version, server and node — the
// read model of GET /v1/matches (docs/specs/master.md §6). ServerPlayers is
// the live players count from the backing server.
//
// Lifecycle (итерация 2): pending (RecordMatch on allocation) → running
// (agent ServerEvent match_start, started_at) → finished|aborted (match_end,
// ended_at; a failing server aborts its match). players_peak is the maximum
// heartbeat players count observed while the match was open.
type Match struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	// Env — окружение матча (environments v1). Колонка была в схеме с самого
	// начала, но в API не выходила; привязка ключа (project, env) без неё
	// непроверяема, а именно этого требует #974.
	Env           string     `json:"env"`
	Region        string     `json:"region"`
	State         string     `json:"state"`
	VersionID     string     `json:"version_id"`
	Semver        string     `json:"semver"`
	ServerID      string     `json:"server_id"`
	Host          string     `json:"host"`
	Port          int32      `json:"port"`
	ServerPlayers int32      `json:"server_players"`
	PlayersPeak   int32      `json:"players_peak"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

type MatchFilter struct {
	Project string
	Region  string
	State   string
	Limit   int
	Offset  int
}

const matchSelect = `
	select m.id::text, p.slug, m.env, m.region, m.state, m.version_id::text, v.semver,
	       m.server_id::text, host(n.public_ip), s.port, s.players,
	       m.players_peak, m.started_at, m.ended_at, m.created_at
	from matches m
	join projects p on p.id = m.project_id
	join versions v on v.id = m.version_id
	join servers  s on s.id = m.server_id
	join nodes    n on n.id = s.node_id`

func scanMatch(row pgx.Row, m *Match) error {
	return row.Scan(&m.ID, &m.Project, &m.Env, &m.Region, &m.State, &m.VersionID, &m.Semver,
		&m.ServerID, &m.Host, &m.Port, &m.ServerPlayers,
		&m.PlayersPeak, &m.StartedAt, &m.EndedAt, &m.CreatedAt)
}

// ListMatches returns matches, newest first, with optional filters and
// limit/offset pagination.
func (s *Store) ListMatches(ctx context.Context, f MatchFilter) ([]Match, error) {
	q := matchSelect
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.Project != "" {
		add("p.slug = $%d", f.Project)
	}
	if f.Region != "" {
		add("m.region = $%d", f.Region)
	}
	if f.State != "" {
		add("m.state = $%d", f.State)
	}
	if len(conds) > 0 {
		q += " where " + strings.Join(conds, " and ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := max(f.Offset, 0)
	args = append(args, limit, offset)
	q += fmt.Sprintf(" order by m.created_at desc, m.id limit $%d offset $%d", len(args)-1, len(args))

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Match
	for rows.Next() {
		var m Match
		if err := scanMatch(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMatch returns one match by id.
func (s *Store) GetMatch(ctx context.Context, id string) (Match, error) {
	var m Match
	err := scanMatch(s.Pool.QueryRow(ctx, matchSelect+" where m.id = $1::uuid", id), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return Match{}, fmt.Errorf("match %s: %w", id, ErrNotFound)
	}
	return m, err
}
