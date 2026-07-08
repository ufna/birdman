package store

import (
	"context"
	"time"
)

// Statistics reads for the panel П2 Statistics/Cost-view screens
// (docs/specs/panel.md §3, docs/specs/master.md §6). v0: aggregation is done
// on-the-fly over matches/servers/nodes — no materialized rollups yet (fine at
// our volume; a rollup job comes later). The store returns raw rows; the shape
// the charts consume (day buckets, stacks, percentiles) is built in the httpapi
// layer so it stays pure and unit-testable.

// StatMatch is one started match, the atom of the Statistics/Cost aggregates.
// Only matches that actually ran (started_at is not null) are returned: pending
// tickets that never became a game are noise for product stats. EndedAt is nil
// for a match still running — the caller clamps its interval to "now".
type StatMatch struct {
	Region      string
	Semver      string
	PlayersPeak int
	CreatedAt   time.Time  // match row created = allocation time (time-to-match input)
	StartedAt   time.Time  // match_start report (never nil here)
	EndedAt     *time.Time // match_end report; nil → still running
}

// StatMatches returns every started match with started_at >= since, oldest
// first. One query feeds all overview/cost aggregates (matches/day, CCU,
// duration, version mix, slot-hours).
func (s *Store) StatMatches(ctx context.Context, since time.Time) ([]StatMatch, error) {
	rows, err := s.Pool.Query(ctx, `
		select m.region, v.semver, m.players_peak, m.created_at, m.started_at, m.ended_at
		from matches m
		join versions v on v.id = m.version_id
		where m.started_at is not null and m.started_at >= $1
		order by m.started_at`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatMatch
	for rows.Next() {
		var sm StatMatch
		if err := rows.Scan(&sm.Region, &sm.Semver, &sm.PlayersPeak,
			&sm.CreatedAt, &sm.StartedAt, &sm.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// RegionUtil is a current utilization snapshot for one region: how many slots
// the fleet has (active nodes' capacity) versus how many servers occupy them.
type RegionUtil struct {
	Region        string `json:"region"`
	CapacitySlots int    `json:"capacity_slots"` // sum(capacity_slots) of active nodes
	Allocated     int    `json:"allocated"`      // servers in state=allocated
	Ready         int    `json:"ready"`          // warm-pool servers (state=ready)
	Draining      int    `json:"draining"`       // servers in state=draining
}

// RegionUtilization returns a per-region snapshot of capacity vs. live server
// slots — the Cost-view utilization tile. It is a point-in-time snapshot;
// utilization over time is available to the panel via the metrics proxy
// (birdman_servers / node capacity, query_range). Regions with capacity but no
// live servers still appear (zeros), and vice versa.
func (s *Store) RegionUtilization(ctx context.Context) ([]RegionUtil, error) {
	// Capacity of active nodes, and live server counts by state, merged by
	// region. FULL JOIN so a region shows up whether it has nodes, servers, or
	// both.
	rows, err := s.Pool.Query(ctx, `
		with cap as (
			select region, sum(capacity_slots)::int as capacity
			from nodes where state = 'active' group by region
		),
		srv as (
			select n.region,
			       count(*) filter (where s.state = 'allocated')::int as allocated,
			       count(*) filter (where s.state = 'ready')::int     as ready,
			       count(*) filter (where s.state = 'draining')::int  as draining
			from servers s join nodes n on n.id = s.node_id
			group by n.region
		)
		select coalesce(cap.region, srv.region) as region,
		       coalesce(cap.capacity, 0),
		       coalesce(srv.allocated, 0), coalesce(srv.ready, 0), coalesce(srv.draining, 0)
		from cap full join srv on cap.region = srv.region
		order by region`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegionUtil
	for rows.Next() {
		var u RegionUtil
		if err := rows.Scan(&u.Region, &u.CapacitySlots, &u.Allocated, &u.Ready, &u.Draining); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
