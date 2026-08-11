package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type ServerFilter struct {
	Project string
	// Env — окружение сервера (денормализованная колонка `servers.env`,
	// миграция 000013). Появилось как носитель арендаторской границы: `GET
	// /v1/servers` сам `?env=` не принимает, значение приходит из привязки
	// ключа (httpapi.tenantScope, tracker #993). Пусто = не сужать.
	Env    string
	Region string
	State  string
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
	if f.Env != "" {
		add("s.env = $%d", f.Env)
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
// ServerNodeID returns the node a server lives on — the logs proxy target
// (итерация 4, GET /v1/servers/{id}/logs). Works for dead servers too: their
// row stays (state reaped/failed) with the node_id, so logs of a finished
// match remain reachable. ErrNotFound when the server id is unknown.
func (s *Store) ServerNodeID(ctx context.Context, id string) (string, error) {
	t, err := s.ServerLogTarget(ctx, id)
	return t.NodeID, err
}

// ServerLogTarget — всё, что нужно проксии логов про сервер: куда слать Tail и
// к какому (project, env) относится вывод. Скоуп нужен не для маршрутизации, а
// для энфорса привязки ключа (#988): ручка readonly и адресуется по uuid,
// поэтому без него привязанный ключ проекта А читал бы логи дедика проекта Б.
// Env берётся из КОЛОНКИ servers, а не join'ом к nodes — денормализация
// намеренная (инвариант I6: перевод ноды в другое окружение не переписывает
// историю уже запущенных серверов).
type ServerLogTarget struct {
	NodeID  string
	Project string
	Env     string
}

func (s *Store) ServerLogTarget(ctx context.Context, id string) (ServerLogTarget, error) {
	var t ServerLogTarget
	err := s.Pool.QueryRow(ctx, `
		select s.node_id::text, p.slug, s.env
		from servers s join projects p on p.id = s.project_id
		where s.id = $1::uuid`, id).Scan(&t.NodeID, &t.Project, &t.Env)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerLogTarget{}, ErrNotFound
	}
	return t, err
}

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
		// The dedik may still be playing behind the dead link, but from the
		// master's view the match is unobservable — close it.
		if err := abortServerMatch(ctx, s.Pool, r.id); err != nil {
			return 0, err
		}
	}
	return len(failed), nil
}

// MarkDownNodes moves long-silent quarantined nodes to 'down'
// (node_down_after_min, спека followups §2 РЕВИЗИЯ): оператор отличает
// «моргнула» (quarantine) от «лежит давно» (down). Deliberately NOT 'dead':
// dead — ручная терминальная ревокация (agentlink отказывает dead-ноде во
// всех auth-режимах), авто-dead запер бы ноду навсегда после любого аутажа
// дольше порога. down self-heals — heartbeat живой mTLS-сессии поднимает
// down → active (touchNode) с node_recovered.
func (s *Store) MarkDownNodes(ctx context.Context, silentFor time.Duration) (int, error) {
	rows, err := s.Pool.Query(ctx, `
		update nodes n set state = 'down'
		where n.state = 'quarantine'
		  and n.last_heartbeat_at < now() - $1::interval
		returning n.id::text, n.hostname,
		          extract(epoch from now() - n.last_heartbeat_at)::int`,
		fmt.Sprintf("%d milliseconds", silentFor.Milliseconds()))
	if err != nil {
		return 0, err
	}
	type ref struct {
		id, hostname string
		silentS      int
	}
	var downs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.id, &r.hostname, &r.silentS); err != nil {
			rows.Close()
			return 0, err
		}
		downs = append(downs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range downs {
		if err := insertEvent(ctx, s.Pool, EventNodeDown,
			EventRef{NodeID: &r.id},
			map[string]any{"hostname": r.hostname, "silent_for_s": r.silentS}); err != nil {
			return 0, err
		}
	}
	return len(downs), nil
}
