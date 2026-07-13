package store

import (
	"context"
	"fmt"
)

type UpsertFleetParams struct {
	Project       string
	Region        string
	ActiveVersion *string // version id; nil → keep current (deploy manager owns flips)
	BufferReady   *int32  // nil → default 2 on insert / keep on update
	MaxServers    *int32  // nil → default 50 on insert / keep on update
	ReapTTLMin    *int32  // nil → default 180 on insert / keep on update
}

// UpsertFleet sets the desired state for (project, region)
// (PUT /v1/fleets/{region}). active_version is normally flipped by the
// deploy manager (итерация 3); setting it here directly remains as the
// bootstrap/ops override. A nil ActiveVersion keeps the current one — a
// PUT tweaking only buffers must not clear the deployed version.
func (s *Store) UpsertFleet(ctx context.Context, p UpsertFleetParams) (FleetConfig, error) {
	if p.Region == "" {
		return FleetConfig{}, fmt.Errorf("region is required")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return FleetConfig{}, err
	}
	defer tx.Rollback(ctx)
	projectID, err := ensureProject(ctx, tx, p.Project)
	if err != nil {
		return FleetConfig{}, err
	}
	if p.ActiveVersion != nil {
		var ok bool
		err := tx.QueryRow(ctx,
			`select exists(select 1 from versions where id = $1::uuid and project_id = $2::uuid)`,
			*p.ActiveVersion, projectID).Scan(&ok)
		if err != nil {
			return FleetConfig{}, err
		}
		if !ok {
			return FleetConfig{}, fmt.Errorf("active_version %s: %w", *p.ActiveVersion, ErrNotFound)
		}
	}
	// Environments v1: fleet_configs.env — NOT NULL, PK теперь (project, env,
	// region). W1 пинует env='dev' (поведение стенда не меняется, всё живёт в
	// dev); env-параметризованный PUT /v1/fleets (I3) приходит в W2. active_version
	// обязан принадлежать (project, 'dev') — БД-FK fleet_active_version_env_fk (C3).
	var f FleetConfig
	err = tx.QueryRow(ctx, `
		insert into fleet_configs (project_id, env, region, active_version, buffer_ready, max_servers, reap_ttl_min)
		values ($1::uuid, 'dev', $2, $3::uuid, coalesce($4, 2), coalesce($5, 50), coalesce($6, 180))
		on conflict (project_id, env, region) do update set
			active_version = coalesce($3::uuid, fleet_configs.active_version),
			buffer_ready   = coalesce($4, fleet_configs.buffer_ready),
			max_servers    = coalesce($5, fleet_configs.max_servers),
			reap_ttl_min   = coalesce($6, fleet_configs.reap_ttl_min)
		returning project_id::text, env, region, active_version::text, buffer_ready, max_servers, reap_ttl_min`,
		projectID, p.Region, p.ActiveVersion, p.BufferReady, p.MaxServers, p.ReapTTLMin).
		Scan(&f.ProjectID, &f.Env, &f.Region, &f.ActiveVersion, &f.BufferReady, &f.MaxServers, &f.ReapTTLMin)
	if err != nil {
		return FleetConfig{}, err
	}
	f.Project = p.Project
	if err := insertEvent(ctx, tx, EventFleetUpdated, EventRef{VersionID: f.ActiveVersion},
		map[string]any{"project": p.Project, "region": f.Region,
			"buffer_ready": f.BufferReady, "max_servers": f.MaxServers}); err != nil {
		return FleetConfig{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FleetConfig{}, err
	}
	return f, nil
}

// ListFleetConfigs returns all fleet configs joined with the active version
// image (reconcile input).
func (s *Store) ListFleetConfigs(ctx context.Context) ([]FleetConfig, error) {
	rows, err := s.Pool.Query(ctx, `
		select f.project_id::text, p.slug, f.env, f.region, f.active_version::text,
		       coalesce(v.image_ref, ''), f.buffer_ready, f.max_servers, f.reap_ttl_min
		from fleet_configs f
		join projects p on p.id = f.project_id
		left join versions v on v.id = f.active_version
		order by p.slug, f.env, f.region`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FleetConfig
	for rows.Next() {
		var f FleetConfig
		if err := rows.Scan(&f.ProjectID, &f.Project, &f.Env, &f.Region, &f.ActiveVersion,
			&f.ActiveImageRef, &f.BufferReady, &f.MaxServers, &f.ReapTTLMin); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
