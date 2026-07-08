package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PlannedStart is a StartServer command produced by a reconcile pass.
type PlannedStart struct {
	ServerID string
	NodeID   string
	ImageRef string
}

// PlannedStop is a StopServer command produced by a reconcile pass.
type PlannedStop struct {
	ServerID string
	NodeID   string
}

// PlanFleet performs the DB half of one reconcile pass for a fleet
// (docs/specs/master.md §2) inside a single transaction guarded by a PG
// advisory lock (protects against two masters running by mistake):
//
//	deficit  = buffer - count(creating+ready of active version)  → insert
//	           servers(creating) on first-fit nodes (densest packing);
//	surplus  = ready of active version beyond buffer              → draining;
//	stale    = ready of non-active versions in the region         → draining.
//
// pausedNodes are excluded from placement (crash-loop pairs, §2).
// The caller sends StartServer/StopServer to agents after commit.
func (s *Store) PlanFleet(ctx context.Context, f FleetConfig, pausedNodes []string) (starts []PlannedStart, stops []PlannedStop, locked bool, err error) {
	if f.ActiveVersion == nil {
		return nil, nil, true, nil
	}
	if pausedNodes == nil {
		pausedNodes = []string{} // nil would become SQL NULL and poison ANY()
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer tx.Rollback(ctx)

	if err := tx.QueryRow(ctx,
		`select pg_try_advisory_xact_lock(hashtextextended($1, 42))`,
		f.ProjectID+":"+f.Region).Scan(&locked); err != nil {
		return nil, nil, false, err
	}
	if !locked {
		return nil, nil, false, nil
	}

	var cur, ready, total int32
	err = tx.QueryRow(ctx, `
		select
			count(*) filter (where s.state in ('creating','ready') and s.version_id = $3::uuid),
			count(*) filter (where s.state = 'ready' and s.version_id = $3::uuid),
			count(*) filter (where s.state in ('creating','ready','allocated','draining'))
		from servers s join nodes n on n.id = s.node_id
		where s.project_id = $1::uuid and n.region = $2`,
		f.ProjectID, f.Region, *f.ActiveVersion).Scan(&cur, &ready, &total)
	if err != nil {
		return nil, nil, true, err
	}

	deficit := f.BufferReady - cur
	if room := f.MaxServers - total; deficit > room {
		deficit = room
	}
	for range deficit {
		var nodeID string
		err := tx.QueryRow(ctx, `
			select n.id::text
			from nodes n
			left join lateral (
				select count(*)::int as used from servers s
				where s.node_id = n.id
				  and s.state in ('creating','ready','allocated','draining')
			) u on true
			where n.project_id = $1::uuid and n.region = $2 and n.state = 'active'
			  and n.last_heartbeat_at > now() - interval '10 seconds'
			  and u.used < n.capacity_slots
			  and not (n.id::text = any($3::text[]))
			order by u.used desc, n.created_at
			limit 1`,
			f.ProjectID, f.Region, pausedNodes).Scan(&nodeID)
		if errors.Is(err, pgx.ErrNoRows) {
			break // no capacity in the region right now
		}
		if err != nil {
			return nil, nil, true, err
		}
		var serverID string
		if err := tx.QueryRow(ctx, `
			insert into servers (project_id, node_id, version_id, state, port)
			values ($1::uuid, $2::uuid, $3::uuid, 'creating', 0)
			returning id::text`,
			f.ProjectID, nodeID, *f.ActiveVersion).Scan(&serverID); err != nil {
			return nil, nil, true, err
		}
		starts = append(starts, PlannedStart{ServerID: serverID, NodeID: nodeID, ImageRef: f.ActiveImageRef})
	}

	if surplus := ready - f.BufferReady; surplus > 0 {
		more, err := drainServers(ctx, tx, `
			update servers set state = 'draining', updated_at = now()
			where id in (
				select s.id from servers s join nodes n on n.id = s.node_id
				where s.project_id = $1::uuid and n.region = $2
				  and s.version_id = $3::uuid and s.state = 'ready'
				order by s.created_at
				limit $4
			)
			returning id::text, node_id::text`,
			f.ProjectID, f.Region, *f.ActiveVersion, surplus)
		if err != nil {
			return nil, nil, true, err
		}
		stops = append(stops, more...)
	}

	// Ready servers of non-active versions get reaped (v0: no multi-version
	// window yet — deploy manager is a later iteration).
	more, err := drainServers(ctx, tx, `
		update servers set state = 'draining', updated_at = now()
		where id in (
			select s.id from servers s join nodes n on n.id = s.node_id
			where s.project_id = $1::uuid and n.region = $2
			  and s.version_id <> $3::uuid and s.state = 'ready'
		)
		returning id::text, node_id::text`,
		f.ProjectID, f.Region, *f.ActiveVersion)
	if err != nil {
		return nil, nil, true, err
	}
	stops = append(stops, more...)

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, true, err
	}
	return starts, stops, true, nil
}

func drainServers(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]PlannedStop, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlannedStop
	for rows.Next() {
		var p PlannedStop
		if err := rows.Scan(&p.ServerID, &p.NodeID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecentFailedTimes returns failure timestamps per node for (version, region)
// within the lookback window — crash-loop detection input (§2). Derived from
// server_failed events (they survive master restarts and carry the reason),
// NOT the servers table: failures with reason node_lost — mass-fails of a
// quarantined node's servers — say nothing about the (version, node) pair and
// must not trip the crash-loop pause (ложнопозитив acceptance ит. 1).
func (s *Store) RecentFailedTimes(ctx context.Context, versionID, region string, lookback time.Duration) (map[string][]time.Time, error) {
	rows, err := s.Pool.Query(ctx, `
		select e.node_id::text, e.ts
		from events e
		join servers s on s.id = e.server_id
		join nodes n on n.id = e.node_id
		where e.kind = 'server_failed'
		  and s.version_id = $1::uuid and n.region = $2
		  and coalesce(e.payload->>'reason', '') <> 'node_lost'
		  and e.ts > now() - $3::interval
		order by e.node_id, e.ts`,
		versionID, region, fmt.Sprintf("%d milliseconds", lookback.Milliseconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]time.Time{}
	for rows.Next() {
		var nodeID string
		var at time.Time
		if err := rows.Scan(&nodeID, &at); err != nil {
			return nil, err
		}
		out[nodeID] = append(out[nodeID], at)
	}
	return out, rows.Err()
}
