package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

type UpsertFleetParams struct {
	Project       string
	Env           string  // окружение флота (environments v1, I3); обязателен
	Region        string
	ActiveVersion *string // version id; nil → keep current (deploy manager owns flips)
	BufferReady   *int32  // nil → default 2 on insert / keep on update
	MaxServers    *int32  // nil → default 50 on insert / keep on update
	ReapTTLMin    *int32  // nil → default 180 on insert / keep on update
}

// UpsertFleet sets the desired state for (project, env, region)
// (PUT /v1/fleets/{region}, body carries env — I3). active_version is normally
// flipped by the deploy manager (итерация 3); setting it here directly remains
// as the bootstrap/ops override. A nil ActiveVersion keeps the current one — a
// PUT tweaking only buffers must not clear the deployed version.
func (s *Store) UpsertFleet(ctx context.Context, p UpsertFleetParams) (FleetConfig, error) {
	if p.Region == "" {
		return FleetConfig{}, fmt.Errorf("region is required")
	}
	if p.Env == "" {
		return FleetConfig{}, fmt.Errorf("env is required")
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
	// Env обязан существовать у проекта — понятный 400 вместо сырого FK-500 на
	// fleet_env_fk. Проверка внутри tx: для нового проекта ensureProject засидил
	// dev/prod прямо сейчас (ещё до коммита), GetEnvironment по s.Pool их не
	// увидел бы — тот же паттерн, что в CreateVersion.
	var envExists bool
	if err := tx.QueryRow(ctx,
		`select exists(select 1 from environments where project_id = $1::uuid and name = $2)`,
		projectID, p.Env).Scan(&envExists); err != nil {
		return FleetConfig{}, err
	}
	if !envExists {
		return FleetConfig{}, fmt.Errorf("no such environment %s/%s", p.Project, p.Env)
	}
	// Environments v1: fleet_configs.env — NOT NULL, PK (project, env, region).
	// active_version обязан принадлежать ТОМУ ЖЕ (project, env) — это держит
	// составной FK fleet_active_version_env_fk (C3). Ручной пре-чек active_version
	// убран: он не знал env и на prod-версии в dev-флоте давал сырой 500 при
	// нарушении FK. Ловим 23503 по имени констрейнта и мапим в ErrNotFound (400).
	var f FleetConfig
	err = tx.QueryRow(ctx, `
		insert into fleet_configs (project_id, env, region, active_version, buffer_ready, max_servers, reap_ttl_min)
		values ($1::uuid, $2, $3, $4::uuid, coalesce($5, 2), coalesce($6, 50), coalesce($7, 180))
		on conflict (project_id, env, region) do update set
			active_version = coalesce($4::uuid, fleet_configs.active_version),
			buffer_ready   = coalesce($5, fleet_configs.buffer_ready),
			max_servers    = coalesce($6, fleet_configs.max_servers),
			reap_ttl_min   = coalesce($7, fleet_configs.reap_ttl_min)
		returning project_id::text, env, region, active_version::text, buffer_ready, max_servers, reap_ttl_min`,
		projectID, p.Env, p.Region, p.ActiveVersion, p.BufferReady, p.MaxServers, p.ReapTTLMin).
		Scan(&f.ProjectID, &f.Env, &f.Region, &f.ActiveVersion, &f.BufferReady, &f.MaxServers, &f.ReapTTLMin)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == "fleet_active_version_env_fk" {
		av := ""
		if p.ActiveVersion != nil {
			av = *p.ActiveVersion
		}
		return FleetConfig{}, fmt.Errorf("active_version %s is not a version of %s/%s: %w", av, p.Project, p.Env, ErrNotFound)
	}
	// Гонка с DELETE env между in-tx пре-чеком и insert'ом: fleet_env_fk (23503)
	// → внятный «no such environment» (400), а не сырой 500 (w5).
	if mapped := mapEnvFKViolation(err, p.Project, p.Env); mapped != nil {
		return FleetConfig{}, mapped
	}
	if err != nil {
		return FleetConfig{}, err
	}
	f.Project = p.Project
	if err := insertEvent(ctx, tx, EventFleetUpdated, EventRef{VersionID: f.ActiveVersion},
		map[string]any{"project": p.Project, "env": f.Env, "region": f.Region,
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
