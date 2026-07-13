package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// ErrNoCapacity — no ready server matched the allocation request
// (docs/specs/master.md §3 → HTTP 409 {"error":"no_capacity"}).
var ErrNoCapacity = errors.New("no_capacity")

type Allocation struct {
	ServerID string `json:"server_id"`
	Host     string `json:"host"`
	Port     int32  `json:"port"`
}

// claimSQL is the claim statement from docs/specs/master.md §3, scoped to the
// environment (environments v1 §3 — a global allocate key must not claim a
// server of a random env). The env is taken from the server row itself
// (s.env, invariant I6), never joined from the node.
// $1 project_id, $2 version_id (nullable), $3 region, $4 match_id, $5 env.
const claimSQL = `
with c as (
  select s.id from servers s
  join nodes n on n.id = s.node_id
  where s.project_id = $1 and s.state = 'ready' and s.env = $5
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
// Idempotent by match_id WITHIN an environment (environments v1 §3, M-5): a
// repeated request in the same env returns the same server (partial unique index
// on servers(match_id)); the same match_id aimed at ANOTHER env misses the
// fast-path and takes the normal claim, so a global match_id can never hand back
// a server of the wrong env.
//
// After a fresh claim the node's agent receives AllocateServer (итерация 2,
// protocol.md §1): the agent forwards `allocated{match_id, players_expected}`
// to liba. Delivery guarantees match the other commands (cmd_id/Ack, replay
// on reconnect); the idempotent repeat does not re-send — the pending command
// is already tracked by the hub. playersExpected 0 = unknown (external
// matchmaker via REST does not report it).
func (s *Store) Allocate(ctx context.Context, project, env, region string, versionID *string, matchID string, playersExpected int32) (Allocation, error) {
	var projectID string
	err := s.Pool.QueryRow(ctx, `select id::text from projects where slug = $1`, project).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, fmt.Errorf("project %q: %w", project, ErrNotFound)
	}
	if err != nil {
		return Allocation{}, err
	}

	// Idempotency fast path: the match already holds a server IN THIS ENV.
	if a, ok, err := s.findByMatch(ctx, projectID, env, matchID); err != nil {
		return Allocation{}, err
	} else if ok {
		return a, nil
	}

	var serverID, nodeID uuid.UUID
	var port int32
	err = s.Pool.QueryRow(ctx, claimSQL, projectID, versionID, region, matchID, env).
		Scan(&serverID, &nodeID, &port)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, ErrNoCapacity
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Lost the idempotency race: another request with the same match_id
		// claimed a server between our fast path and the claim.
		if a, ok, err := s.findByMatch(ctx, projectID, env, matchID); err != nil {
			return Allocation{}, err
		} else if ok {
			return a, nil
		}
		return Allocation{}, fmt.Errorf("match_id %s already used", matchID)
	}
	if err != nil {
		return Allocation{}, err
	}

	// The claim is committed — the dedik must learn about its match now,
	// even if the host lookup below fails.
	s.notifyAllocated(nodeID.String(), serverID.String(), matchID, playersExpected)

	var host string
	if err := s.Pool.QueryRow(ctx,
		`select host(public_ip) from nodes where id = $1::uuid`, nodeID.String()).Scan(&host); err != nil {
		return Allocation{}, err
	}
	return Allocation{ServerID: serverID.String(), Host: host, Port: port}, nil
}

// notifyAllocated sends AllocateServer to the node's agent via the hub. A nil
// sender (not wired in some tests) is a no-op — production wiring installs it
// at startup (SetCommandSender in cmd/birdman-master).
func (s *Store) notifyAllocated(nodeID, serverID, matchID string, playersExpected int32) {
	if s.sender == nil {
		return
	}
	s.sender.Send(nodeID, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Allocate{
		Allocate: &agentlinkv1.AllocateServer{
			ServerId:        serverID,
			MatchId:         matchID,
			PlayersExpected: playersExpected,
		},
	}})
}

// SoleEnvWithReady returns the single environment of a project that currently
// has a ready server on a live node in the region — the /v1/allocate env
// fallback when the request names none (environments v1 §3, I4). The env is the
// server's own (s.env, invariant I6). ErrNoCapacity when ZERO envs qualify (an
// empty pool — the caller returns 409 no_capacity, as before the env wave, not
// env_required); ErrConflict when SEVERAL qualify (the request must then name
// env explicitly → 409 env_required). Node liveness mirrors claimSQL so a
// resolved env is actually claimable.
func (s *Store) SoleEnvWithReady(ctx context.Context, project, region string) (string, error) {
	rows, err := s.Pool.Query(ctx, `
		select distinct s.env
		from servers s
		join nodes n on n.id = s.node_id
		join projects p on p.id = s.project_id
		where p.slug = $1 and s.state = 'ready'
		  and n.region = $2 and n.state = 'active'
		  and n.last_heartbeat_at > now() - interval '10 seconds'
		order by s.env
		limit 2`, project, region)
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
		// Пустой пул, НЕ двусмысленность: вызывающий отдаёт no_capacity (как до
		// волны env), а не env_required. Различение 0 и >1 живёт здесь.
		return "", fmt.Errorf("no environment of project %s has ready servers in %s: %w", project, region, ErrNoCapacity)
	case 1:
		return envs[0], nil
	default:
		return "", fmt.Errorf("several environments of project %s have ready servers in %s, env is required: %w", project, region, ErrConflict)
	}
}

// findByMatch resolves the server a match already holds, scoped to (project,
// env) — the env filter (environments v1 §3, M-5) keeps the globally-unique
// match_id from returning a server of a different environment on the idempotency
// fast-path; s.env is the server's own env (invariant I6).
func (s *Store) findByMatch(ctx context.Context, projectID, env, matchID string) (Allocation, bool, error) {
	var a Allocation
	err := s.Pool.QueryRow(ctx, `
		select s.id::text, host(n.public_ip), s.port
		from servers s join nodes n on n.id = s.node_id
		where s.project_id = $1::uuid and s.env = $2 and s.match_id = $3::uuid`,
		projectID, env, matchID).Scan(&a.ServerID, &a.Host, &a.Port)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, false, nil
	}
	if err != nil {
		return Allocation{}, false, err
	}
	return a, true, nil
}
