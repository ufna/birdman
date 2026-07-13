package store

import (
	"context"
)

// RegionVersion is one allocatable (region, server version) candidate the
// matchmaker checks client compatibility against (docs/specs/master.md §4,
// docs/specs/ops.md §3). Candidates are scoped to the ticket's environment
// (environments v1 §3). A region's candidate set, preference-ordered:
//   - rank 0 — the region's fleet_configs.active_version for this env: what the
//     fleet actually runs;
//   - rank 1 — versions with state='deprecated' in this env: the multi-version
//     window (итерация 3, master.md §5) — old clients keep matching onto them
//     until the version is disabled, but a client covered by an active version
//     never lands here (the matchmaker takes the first compatible candidate).
//
// The old rank-1 rule (state='active' with the prod label) died together with
// the retired version label: the fleet active_version (rank 0) already covers
// what the env runs. Disabled versions are never candidates.
type RegionVersion struct {
	Region    string
	VersionID string
	Semver    string
	Rank      int // 0 fleet-active, 1 deprecated (window)
}

// Deprecated reports whether the candidate is a multi-version-window one.
func (rv RegionVersion) Deprecated() bool { return rv.Rank >= 1 }

// ActiveRegionVersions returns matchmaking version candidates per region for a
// project's environment, preference-ordered (rank asc, newest first within a
// rank). Both the fleet-active and the deprecated-window candidates are scoped
// to env — a dev ticket never sees prod versions and vice versa.
func (s *Store) ActiveRegionVersions(ctx context.Context, project, env string) ([]RegionVersion, error) {
	rows, err := s.Pool.Query(ctx, `
		select region, version_id, semver, min(rank)
		from (
			select f.region, v.id::text as version_id, v.semver,
			       0 as rank, v.created_at
			from fleet_configs f
			join projects p on p.id = f.project_id
			join versions v on v.id = f.active_version
			where p.slug = $1 and f.env = $2
			union all
			select f.region, v.id::text, v.semver, 1, v.created_at
			from fleet_configs f
			join projects p on p.id = f.project_id
			join versions v on v.project_id = p.id
			     and v.state = 'deprecated' and v.env = $2
			where p.slug = $1 and f.env = $2
		) t
		group by region, version_id, semver
		order by region, min(rank), max(created_at) desc`,
		project, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegionVersion
	for rows.Next() {
		var rv RegionVersion
		if err := rows.Scan(&rv.Region, &rv.VersionID, &rv.Semver, &rv.Rank); err != nil {
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
