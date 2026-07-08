package store

import (
	"context"
)

// RegionVersion is one allocatable (region, server version) candidate the
// matchmaker checks client compatibility against (docs/specs/master.md §4,
// docs/specs/ops.md §3).
//
// (Уточнено в v0.) The deploy manager does not exist yet, so nothing flips
// versions.state to 'active'. A region's candidate set is therefore:
//   - the region's fleet_configs.active_version — what the fleet actually
//     runs (any channel: dev setups deploy staging builds);
//   - plus every project version with state='active' and channel='prod'
//     (the spec'd rule; becomes meaningful with the deploy manager).
type RegionVersion struct {
	Region      string
	VersionID   string
	Semver      string
	FleetActive bool // true — the region's fleet_configs.active_version
}

// ActiveRegionVersions returns matchmaking version candidates per region for
// a project, fleet-active versions first, newest first within a rank.
func (s *Store) ActiveRegionVersions(ctx context.Context, project string) ([]RegionVersion, error) {
	rows, err := s.Pool.Query(ctx, `
		select region, version_id, semver, bool_or(fleet_active)
		from (
			select f.region, v.id::text as version_id, v.semver,
			       true as fleet_active, v.created_at
			from fleet_configs f
			join projects p on p.id = f.project_id
			join versions v on v.id = f.active_version
			where p.slug = $1
			union all
			select f.region, v.id::text, v.semver, false, v.created_at
			from fleet_configs f
			join projects p on p.id = f.project_id
			join versions v on v.project_id = p.id
			     and v.channel = 'prod' and v.state = 'active'
			where p.slug = $1
		) t
		group by region, version_id, semver
		order by region, bool_or(fleet_active) desc, max(created_at) desc`,
		project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegionVersion
	for rows.Next() {
		var rv RegionVersion
		if err := rows.Scan(&rv.Region, &rv.VersionID, &rv.Semver, &rv.FleetActive); err != nil {
			return nil, err
		}
		out = append(out, rv)
	}
	return out, rows.Err()
}

// QoSUDPPort is the UDP echo port of the agent on every node
// (docs/specs/master.md §7; the echo responder itself ships in iteration 4).
const QoSUDPPort = 19999

type QoSEndpoint struct {
	Region  string `json:"region"`
	Host    string `json:"host"`
	UDPPort int32  `json:"udp_port"`
}

// ListQoSEndpoints returns ping targets for GET /v1/qos: active nodes with a
// fresh heartbeat (<30s — the NodeDown alert threshold, docs/specs/ops.md §1).
func (s *Store) ListQoSEndpoints(ctx context.Context) ([]QoSEndpoint, error) {
	rows, err := s.Pool.Query(ctx, `
		select distinct n.region, host(n.public_ip)
		from nodes n
		where n.state = 'active'
		  and n.last_heartbeat_at > now() - interval '30 seconds'
		order by 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QoSEndpoint
	for rows.Next() {
		e := QoSEndpoint{UDPPort: QoSUDPPort}
		if err := rows.Scan(&e.Region, &e.Host); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordMatch writes the matches row for a successful internal allocation
// (docs/specs/master.md §1). Idempotent by match id — and it never clobbers a
// row the agent's match_start already upserted to 'running' (the dedik can be
// faster than this insert); state starts 'pending' otherwise, the lifecycle
// is driven by ApplyServerEvent (итерация 2).
func (s *Store) RecordMatch(ctx context.Context, matchID, project, region, serverID, versionID string) error {
	_, err := s.Pool.Exec(ctx, `
		insert into matches (id, project_id, server_id, version_id, region)
		select $1::uuid, p.id, $4::uuid, $5::uuid, $3
		from projects p where p.slug = $2
		on conflict (id) do nothing`,
		matchID, project, region, serverID, versionID)
	return err
}
