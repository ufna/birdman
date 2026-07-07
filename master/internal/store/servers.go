package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type ServerFilter struct {
	Project string
	Region  string
	State   string
}

// ListServers returns servers, newest first, with optional filters.
func (s *Store) ListServers(ctx context.Context, f ServerFilter) ([]Server, error) {
	q := `
		select s.id::text, s.project_id::text, s.node_id::text, s.version_id::text,
		       n.region, s.state, s.port, s.players, s.tick_ms, s.match_id::text,
		       s.created_at, s.updated_at
		from servers s
		join nodes n on n.id = s.node_id
		join projects p on p.id = s.project_id`
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
		add("n.region = $%d", f.Region)
	}
	if f.State != "" {
		add("s.state = $%d", f.State)
	}
	if len(conds) > 0 {
		q += " where " + strings.Join(conds, " and ")
	}
	q += " order by s.created_at desc limit 1000"

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		var sv Server
		if err := rows.Scan(&sv.ID, &sv.ProjectID, &sv.NodeID, &sv.VersionID,
			&sv.Region, &sv.State, &sv.Port, &sv.Players, &sv.TickMS, &sv.MatchID,
			&sv.CreatedAt, &sv.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

// GetServer returns one server by id (tests / debugging).
func (s *Store) GetServer(ctx context.Context, id string) (Server, error) {
	var sv Server
	err := s.Pool.QueryRow(ctx, `
		select s.id::text, s.project_id::text, s.node_id::text, s.version_id::text,
		       n.region, s.state, s.port, s.players, s.tick_ms, s.match_id::text,
		       s.created_at, s.updated_at
		from servers s join nodes n on n.id = s.node_id
		where s.id = $1::uuid`, id).
		Scan(&sv.ID, &sv.ProjectID, &sv.NodeID, &sv.VersionID,
			&sv.Region, &sv.State, &sv.Port, &sv.Players, &sv.TickMS, &sv.MatchID,
			&sv.CreatedAt, &sv.UpdatedAt)
	return sv, err
}

// FailStuckCreating marks servers stuck in creating (no agent progress for
// timeout) as failed, so reconcile re-creates them. (Уточнено в v0 in
// docs/specs/master.md §2.) Agent pull progress refreshes updated_at via
// heartbeat, so long image pulls do not trip this.
func (s *Store) FailStuckCreating(ctx context.Context, timeout time.Duration) (int, error) {
	rows, err := s.Pool.Query(ctx, `
		update servers set state = 'failed', updated_at = now()
		where state = 'creating' and updated_at < now() - $1::interval
		returning id::text, node_id::text, version_id::text`,
		fmt.Sprintf("%d milliseconds", timeout.Milliseconds()))
	if err != nil {
		return 0, err
	}
	type ref struct{ id, node, version string }
	var failed []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.id, &r.node, &r.version); err != nil {
			rows.Close()
			return 0, err
		}
		failed = append(failed, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range failed {
		if err := insertEvent(ctx, s.Pool, EventServerFailed,
			EventRef{ServerID: &r.id, NodeID: &r.node, VersionID: &r.version},
			map[string]any{"reason": "start_timeout"}); err != nil {
			return 0, err
		}
	}
	return len(failed), nil
}

// QuarantineStaleNodes moves active nodes with heartbeat older than 10s into
// quarantine (docs/specs/protocol.md §1 Lease). Nodes that never sent a
// heartbeat stay active — they are excluded from placement/allocation by the
// freshness predicate anyway.
func (s *Store) QuarantineStaleNodes(ctx context.Context) (int, error) {
	rows, err := s.Pool.Query(ctx, `
		update nodes set state = 'quarantine'
		where state = 'active'
		  and last_heartbeat_at is not null
		  and last_heartbeat_at < now() - interval '10 seconds'
		returning id::text, hostname`)
	if err != nil {
		return 0, err
	}
	type ref struct{ id, hostname string }
	var qs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.id, &r.hostname); err != nil {
			rows.Close()
			return 0, err
		}
		qs = append(qs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range qs {
		if err := insertEvent(ctx, s.Pool, EventNodeQuarantine,
			EventRef{NodeID: &r.id}, map[string]any{"hostname": r.hostname}); err != nil {
			return 0, err
		}
	}
	return len(qs), nil
}

// FailQuarantinedServers fails all live servers on nodes silent for 30s+
// (10s to quarantine + 20s more, docs/specs/protocol.md §1 Lease).
func (s *Store) FailQuarantinedServers(ctx context.Context) (int, error) {
	rows, err := s.Pool.Query(ctx, `
		update servers s set state = 'failed', updated_at = now()
		from nodes n
		where n.id = s.node_id
		  and n.state = 'quarantine'
		  and n.last_heartbeat_at < now() - interval '30 seconds'
		  and s.state in ('creating','ready','allocated','draining')
		returning s.id::text, s.node_id::text, s.version_id::text`)
	if err != nil {
		return 0, err
	}
	type ref struct{ id, node, version string }
	var failed []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.id, &r.node, &r.version); err != nil {
			rows.Close()
			return 0, err
		}
		failed = append(failed, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range failed {
		if err := insertEvent(ctx, s.Pool, EventServerFailed,
			EventRef{ServerID: &r.id, NodeID: &r.node, VersionID: &r.version},
			map[string]any{"reason": "node_lost"}); err != nil {
			return 0, err
		}
	}
	return len(failed), nil
}
