package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNoCapacity — no ready server matched the allocation request
// (docs/specs/master.md §3 → HTTP 409 {"error":"no_capacity"}).
var ErrNoCapacity = errors.New("no_capacity")

type Allocation struct {
	ServerID string `json:"server_id"`
	Host     string `json:"host"`
	Port     int32  `json:"port"`
}

// claimSQL is the exact claim statement from docs/specs/master.md §3.
// $1 project_id, $2 version_id (nullable), $3 region, $4 match_id.
const claimSQL = `
with c as (
  select s.id from servers s
  join nodes n on n.id = s.node_id
  where s.project_id = $1 and s.state = 'ready'
    and s.version_id = coalesce($2, s.version_id)
    and n.region = $3 and n.state = 'active'
    and n.last_heartbeat_at > now() - interval '10 seconds'
  order by s.created_at
  limit 1
  for update of s skip locked
)
update servers set state='allocated', match_id=$4, updated_at=now()
where id = (select id from c)
returning id, node_id, port`

// Allocate atomically claims one ready server (FOR UPDATE SKIP LOCKED).
// Idempotent by match_id: a repeated request returns the same server, backed
// by the partial unique index on servers(match_id).
func (s *Store) Allocate(ctx context.Context, project, region string, versionID *string, matchID string) (Allocation, error) {
	var projectID string
	err := s.Pool.QueryRow(ctx, `select id::text from projects where slug = $1`, project).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, fmt.Errorf("project %q: %w", project, ErrNotFound)
	}
	if err != nil {
		return Allocation{}, err
	}

	// Idempotency fast path: the match already holds a server.
	if a, ok, err := s.findByMatch(ctx, projectID, matchID); err != nil {
		return Allocation{}, err
	} else if ok {
		return a, nil
	}

	var serverID, nodeID uuid.UUID
	var port int32
	err = s.Pool.QueryRow(ctx, claimSQL, projectID, versionID, region, matchID).
		Scan(&serverID, &nodeID, &port)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, ErrNoCapacity
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Lost the idempotency race: another request with the same match_id
		// claimed a server between our fast path and the claim.
		if a, ok, err := s.findByMatch(ctx, projectID, matchID); err != nil {
			return Allocation{}, err
		} else if ok {
			return a, nil
		}
		return Allocation{}, fmt.Errorf("match_id %s already used", matchID)
	}
	if err != nil {
		return Allocation{}, err
	}

	var host string
	if err := s.Pool.QueryRow(ctx,
		`select host(public_ip) from nodes where id = $1::uuid`, nodeID.String()).Scan(&host); err != nil {
		return Allocation{}, err
	}
	return Allocation{ServerID: serverID.String(), Host: host, Port: port}, nil
}

func (s *Store) findByMatch(ctx context.Context, projectID, matchID string) (Allocation, bool, error) {
	var a Allocation
	err := s.Pool.QueryRow(ctx, `
		select s.id::text, host(n.public_ip), s.port
		from servers s join nodes n on n.id = s.node_id
		where s.project_id = $1::uuid and s.match_id = $2::uuid`,
		projectID, matchID).Scan(&a.ServerID, &a.Host, &a.Port)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, false, nil
	}
	if err != nil {
		return Allocation{}, false, err
	}
	return a, true, nil
}
