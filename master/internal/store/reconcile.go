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

// PlannedDrain is a per-server DrainServer command produced by a reconcile
// pass (итерация 3: live matches on versions outside the multi-version
// window play out and exit — docs/specs/master.md §5 step 5).
type PlannedDrain struct {
	ServerID  string
	NodeID    string
	DeadlineS int32
	Reason    string
}

// DrainDeadlineS is the in-game deadline handed to liba with a reap drain.
const DrainDeadlineS = 300

// DeprecatedBuffer is the ready buffer kept for the deprecated version in
// the multi-version window: min(2, buffer_ready) (master.md §5).
func DeprecatedBuffer(bufferReady int32) int32 {
	if bufferReady < 2 {
		return bufferReady
	}
	return 2
}

// DeprecatedWindowVersion returns the project's deprecated version still in
// the multi-version window (there is at most one by construction — older
// ones are disabled on flip), or nil. The window is closed by
// DisableExpiredDeprecated (reap_ttl_min).
func (s *Store) DeprecatedWindowVersion(ctx context.Context, projectID string) (*Version, error) {
	var v Version
	err := s.Pool.QueryRow(ctx, `
		select v.id::text, v.project_id::text, v.semver, v.image_ref, v.channel, v.state, v.created_at, v.deprecated_at
		from versions v
		where v.project_id = $1::uuid and v.state = 'deprecated'
		order by v.deprecated_at desc nulls last
		limit 1`, projectID).
		Scan(&v.ID, &v.ProjectID, &v.Semver, &v.ImageRef, &v.Channel, &v.State, &v.CreatedAt, &v.DeprecatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// PlanFleet performs the DB half of one reconcile pass for a fleet
// (docs/specs/master.md §2, §5) inside a single transaction guarded by a PG
// advisory lock (protects against two masters running by mistake).
//
// The multi-version window (итерация 3): buffers are kept for the active
// version (full buffer_ready) AND the deprecated one (min(2, buffer_ready)),
// per version:
//
//	deficit  = target - count(creating+ready of version)  → insert
//	           servers(creating) spread onto the least-busy nodes first
//	           (anti-affinity, follow-ups итерации 5 §1);
//	surplus  = ready of version beyond target             → draining + stop.
//
// Versions outside the window (registered/disabled/older): ready → draining +
// stop; allocated (live matches) → draining + per-server Drain — the dedik
// finishes the match and exits on its own (`доигрывают`).
//
// dep is the project's deprecated window version (nil — no window open);
// pausedByVersion excludes crash-looping (version, node) pairs from
// placement (§2). The caller sends the commands to agents after commit.
func (s *Store) PlanFleet(ctx context.Context, f FleetConfig, dep *Version, pausedByVersion map[string][]string) (starts []PlannedStart, stops []PlannedStop, drains []PlannedDrain, locked bool, err error) {
	if f.ActiveVersion == nil {
		return nil, nil, nil, true, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, false, err
	}
	defer tx.Rollback(ctx)

	if err := tx.QueryRow(ctx,
		`select pg_try_advisory_xact_lock(hashtextextended($1, 42))`,
		f.ProjectID+":"+f.Region).Scan(&locked); err != nil {
		return nil, nil, nil, false, err
	}
	if !locked {
		return nil, nil, nil, false, nil
	}

	// Node drain (итерация 4, docs/specs/master.md §6): ready servers on nodes
	// leaving the fleet (draining/quarantine/dead) are reaped so the warm pool
	// moves to active nodes; allocated servers are left to play their match
	// out. Marked draining first so they drop out of the per-version buffer
	// counts below and the deficit is placed on active nodes.
	drainedReady, err := drainServers(ctx, tx, `
		update servers set state = 'draining', updated_at = now()
		where id in (
			select s.id from servers s join nodes n on n.id = s.node_id
			where s.project_id = $1::uuid and n.region = $2
			  and n.state <> 'active' and s.state = 'ready'
		)
		returning id::text, node_id::text`,
		f.ProjectID, f.Region)
	if err != nil {
		return nil, nil, nil, true, err
	}
	stops = append(stops, drainedReady...)

	type target struct {
		versionID string
		imageRef  string
		buffer    int32
	}
	window := []target{{*f.ActiveVersion, f.ActiveImageRef, f.BufferReady}}
	windowIDs := []string{*f.ActiveVersion}
	if dep != nil && dep.ID != *f.ActiveVersion {
		window = append(window, target{dep.ID, dep.ImageRef, DeprecatedBuffer(f.BufferReady)})
		windowIDs = append(windowIDs, dep.ID)
	}

	var total int32
	if err := tx.QueryRow(ctx, `
		select count(*) from servers s join nodes n on n.id = s.node_id
		where s.project_id = $1::uuid and n.region = $2
		  and s.state in ('creating','ready','allocated','draining')`,
		f.ProjectID, f.Region).Scan(&total); err != nil {
		return nil, nil, nil, true, err
	}

	for _, w := range window {
		paused := pausedByVersion[w.versionID]
		if paused == nil {
			paused = []string{} // nil would become SQL NULL and poison ANY()
		}
		var cur, ready int32
		err = tx.QueryRow(ctx, `
			select
				count(*) filter (where s.state in ('creating','ready')),
				count(*) filter (where s.state = 'ready')
			from servers s join nodes n on n.id = s.node_id
			where s.project_id = $1::uuid and n.region = $2 and s.version_id = $3::uuid`,
			f.ProjectID, f.Region, w.versionID).Scan(&cur, &ready)
		if err != nil {
			return nil, nil, nil, true, err
		}

		deficit := w.buffer - cur
		if room := f.MaxServers - total; deficit > room {
			deficit = room
		}
		for range deficit {
			var nodeID string
			// Наименее занятая нода первой — спред буфера по региону
			// (анти-аффинити): смерть любой ноды теряет минимум ready.
			// Bin-pack (used desc) отвергнут учением 5.2 D5 — смерть «полной»
			// ноды мигрировала весь буфер разом.
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
				order by u.used asc, n.created_at
				limit 1`,
				f.ProjectID, f.Region, paused).Scan(&nodeID)
			if errors.Is(err, pgx.ErrNoRows) {
				break // no capacity in the region right now
			}
			if err != nil {
				return nil, nil, nil, true, err
			}
			var serverID string
			if err := tx.QueryRow(ctx, `
				insert into servers (project_id, node_id, version_id, state, port)
				values ($1::uuid, $2::uuid, $3::uuid, 'creating', 0)
				returning id::text`,
				f.ProjectID, nodeID, w.versionID).Scan(&serverID); err != nil {
				return nil, nil, nil, true, err
			}
			starts = append(starts, PlannedStart{ServerID: serverID, NodeID: nodeID, ImageRef: w.imageRef})
			total++
		}

		if surplus := ready - w.buffer; surplus > 0 {
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
				f.ProjectID, f.Region, w.versionID, surplus)
			if err != nil {
				return nil, nil, nil, true, err
			}
			stops = append(stops, more...)
		}
	}

	// Ready servers of versions outside the window get reaped.
	more, err := drainServers(ctx, tx, `
		update servers set state = 'draining', updated_at = now()
		where id in (
			select s.id from servers s join nodes n on n.id = s.node_id
			where s.project_id = $1::uuid and n.region = $2
			  and not (s.version_id::text = any($3::text[])) and s.state = 'ready'
		)
		returning id::text, node_id::text`,
		f.ProjectID, f.Region, windowIDs)
	if err != nil {
		return nil, nil, nil, true, err
	}
	stops = append(stops, more...)

	// Live matches on versions outside the window: per-server drain — liba
	// gets `drain{deadline_s}`, the match plays out, the dedik exits itself
	// (master.md §5: «по reap_ttl_min — Drain с дедлайном»). The
	// allocated→draining mark makes this fire exactly once per server.
	rows, err := tx.Query(ctx, `
		update servers s set state = 'draining', updated_at = now()
		from nodes n, versions v
		where n.id = s.node_id and v.id = s.version_id
		  and s.project_id = $1::uuid and n.region = $2
		  and not (s.version_id::text = any($3::text[])) and s.state = 'allocated'
		returning s.id::text, s.node_id::text, s.version_id::text, s.match_id::text, v.semver`,
		f.ProjectID, f.Region, windowIDs)
	if err != nil {
		return nil, nil, nil, true, err
	}
	type drained struct {
		id, node, version, semver string
		matchID                   *string
	}
	var ds []drained
	for rows.Next() {
		var d drained
		if err := rows.Scan(&d.id, &d.node, &d.version, &d.matchID, &d.semver); err != nil {
			rows.Close()
			return nil, nil, nil, true, err
		}
		ds = append(ds, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, nil, true, err
	}
	for _, d := range ds {
		reason := fmt.Sprintf("version %s reaped (reap_ttl)", d.semver)
		drains = append(drains, PlannedDrain{
			ServerID: d.id, NodeID: d.node, DeadlineS: DrainDeadlineS, Reason: reason,
		})
		id, node, version := d.id, d.node, d.version
		if err := insertEvent(ctx, tx, EventServerDrain,
			EventRef{ServerID: &id, NodeID: &node, VersionID: &version, MatchID: d.matchID},
			map[string]any{"reason": reason, "deadline_s": DrainDeadlineS}); err != nil {
			return nil, nil, nil, true, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, nil, true, err
	}
	return starts, stops, drains, true, nil
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
