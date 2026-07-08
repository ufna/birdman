package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Deploy manager persistence (итерация 3, docs/specs/master.md §5).
// Version state machine: registered → prepulling → active → deprecated →
// disabled. Version states are project-global; per-region targeting lives in
// fleet_configs.active_version (уточнено в v0: одна deprecated на проект —
// при флипе более старые deprecated уходят в disabled).

var (
	// ErrVersionState — the version is in a state that forbids the operation
	// (deploy of a disabled version, rollback without a deprecated one, ...).
	ErrVersionState = errors.New("version_state")
	// ErrDeployInProgress — another version of the project is prepulling.
	ErrDeployInProgress = errors.New("deploy_in_progress")
	// ErrNoFleet — the project has no fleet_configs rows, nothing to deploy to.
	ErrNoFleet = errors.New("no_fleet")
)

// BeginDeployResult reports what BeginDeploy did.
type BeginDeployResult struct {
	Version Version
	// AlreadyActive: the version is active — nothing to do (idempotency).
	AlreadyActive bool
	// AlreadyPrepulling: a deploy of this version is in flight (idempotency);
	// the caller must NOT start a second prepull job.
	AlreadyPrepulling bool
}

// BeginDeploy moves a version to `prepulling` (step 1 of POST /v1/deploy).
// Idempotent: repeated calls for a prepulling/active version report the fact
// instead of failing. Only one version per project may be prepulling.
func (s *Store) BeginDeploy(ctx context.Context, versionID string) (BeginDeployResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BeginDeployResult{}, err
	}
	defer tx.Rollback(ctx)

	var v Version
	err = tx.QueryRow(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.channel, v.state, v.created_at, v.deprecated_at
		from versions v join projects p on p.id = v.project_id
		where v.id = $1::uuid
		for update of v`, versionID).
		Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Channel, &v.State, &v.CreatedAt, &v.DeprecatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BeginDeployResult{}, fmt.Errorf("version %s: %w", versionID, ErrNotFound)
	}
	if err != nil {
		return BeginDeployResult{}, err
	}

	switch v.State {
	case "active":
		return BeginDeployResult{Version: v, AlreadyActive: true}, nil
	case "prepulling":
		return BeginDeployResult{Version: v, AlreadyPrepulling: true}, nil
	case "disabled":
		return BeginDeployResult{}, fmt.Errorf("version %s is disabled: %w", v.Semver, ErrVersionState)
	}

	// registered | deprecated → start a deploy, if nothing else is in flight
	// and the project has at least one fleet to deploy to.
	var busy bool
	if err := tx.QueryRow(ctx, `
		select exists(select 1 from versions
		              where project_id = $1::uuid and state = 'prepulling')`,
		v.ProjectID).Scan(&busy); err != nil {
		return BeginDeployResult{}, err
	}
	if busy {
		return BeginDeployResult{}, fmt.Errorf("another deploy of project %s is prepulling: %w", v.Project, ErrDeployInProgress)
	}
	var hasFleet bool
	if err := tx.QueryRow(ctx,
		`select exists(select 1 from fleet_configs where project_id = $1::uuid)`,
		v.ProjectID).Scan(&hasFleet); err != nil {
		return BeginDeployResult{}, err
	}
	if !hasFleet {
		return BeginDeployResult{}, fmt.Errorf("project %s has no fleets: %w", v.Project, ErrNoFleet)
	}

	if _, err := tx.Exec(ctx,
		`update versions set state = 'prepulling' where id = $1::uuid`, v.ID); err != nil {
		return BeginDeployResult{}, err
	}
	if err := insertEvent(ctx, tx, EventDeployStarted, EventRef{VersionID: &v.ID},
		map[string]any{"project": v.Project, "semver": v.Semver, "image_ref": v.ImageRef}); err != nil {
		return BeginDeployResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BeginDeployResult{}, err
	}
	v.State = "prepulling"
	return BeginDeployResult{Version: v}, nil
}

// PrePullNode is one PrePull target.
type PrePullNode struct {
	ID     string
	Region string
}

// PrePullTargets returns the nodes a deploy must warm: active nodes with a
// fresh heartbeat (<30s) in every region the project has a fleet_config for
// (master.md §5 step 1: «всем активным тачкам региона(ов) флита»).
func (s *Store) PrePullTargets(ctx context.Context, projectID string) ([]PrePullNode, error) {
	rows, err := s.Pool.Query(ctx, `
		select n.id::text, n.region
		from nodes n
		where n.project_id = $1::uuid and n.state = 'active'
		  and n.last_heartbeat_at > now() - interval '30 seconds'
		  and n.region in (select region from fleet_configs where project_id = $1::uuid)
		order by n.created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PrePullNode
	for rows.Next() {
		var n PrePullNode
		if err := rows.Scan(&n.ID, &n.Region); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AbortDeploy rolls a prepulling version back to `registered`
// (prepull timeout or a node reported a failed pull) + event deploy_failed.
// No-op if the version is not prepulling anymore (race with activation).
func (s *Store) AbortDeploy(ctx context.Context, versionID, reason string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx, `
		update versions set state = 'registered'
		where id = $1::uuid and state = 'prepulling'`, versionID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return nil
	}
	if err := insertEvent(ctx, tx, EventDeployFailed, EventRef{VersionID: &versionID},
		map[string]any{"reason": reason}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ActivateResult describes an ActivateVersion flip.
type ActivateResult struct {
	Version    Version  // the new active version
	OldActive  *Version // the demoted version (nil on the very first deploy)
	Regions    []string // fleet regions whose active_version now points at Version
	Disabled   []string // older deprecated version ids pushed to disabled
	PrevSemver string   // semver of OldActive ("" if none)
}

// ActivateVersion is the atomic flip (master.md §5 step 3) shared by deploy
// (fromState "prepulling", event deploy_activated) and rollback (fromState
// "deprecated", event deploy_rolled_back), in ONE transaction:
//
//	old active → deprecated (deprecated_at = now);
//	older deprecated versions → disabled (одна deprecated на проект);
//	the new version → active (deprecated_at cleared);
//	fleet_configs.active_version → the new version: every project region,
//	or only `regions` when non-empty (region-scoped rollback; version
//	states remain project-global — уточнено в v0).
func (s *Store) ActivateVersion(ctx context.Context, versionID, fromState, eventKind string, regions []string) (ActivateResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ActivateResult{}, err
	}
	defer tx.Rollback(ctx)

	var v Version
	err = tx.QueryRow(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.channel, v.state, v.created_at, v.deprecated_at
		from versions v join projects p on p.id = v.project_id
		where v.id = $1::uuid
		for update of v`, versionID).
		Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Channel, &v.State, &v.CreatedAt, &v.DeprecatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActivateResult{}, fmt.Errorf("version %s: %w", versionID, ErrNotFound)
	}
	if err != nil {
		return ActivateResult{}, err
	}
	if v.State != fromState {
		return ActivateResult{}, fmt.Errorf("version %s is %s, want %s: %w", v.Semver, v.State, fromState, ErrVersionState)
	}

	res := ActivateResult{}

	// Demote what the project runs today: the `active` version AND any
	// version the fleets still point at while its state is `registered`
	// (bootstrap: active_version was assigned via PUT /v1/fleets before the
	// deploy manager existed) — without this the old build would fall out of
	// the multi-version window and its live matches would be drained at once.
	demoteRows, err := tx.Query(ctx, `
		update versions set state = 'deprecated', deprecated_at = now()
		where project_id = $1::uuid and id <> $2::uuid
		  and (state = 'active'
		       or (state = 'registered' and id in (
		             select active_version from fleet_configs
		             where project_id = $1::uuid and active_version is not null)))
		returning id::text, semver, image_ref, channel, created_at`,
		v.ProjectID, v.ID)
	if err != nil {
		return ActivateResult{}, err
	}
	keep := []string{v.ID}
	for demoteRows.Next() {
		var old Version
		if err := demoteRows.Scan(&old.ID, &old.Semver, &old.ImageRef, &old.Channel, &old.CreatedAt); err != nil {
			demoteRows.Close()
			return ActivateResult{}, err
		}
		old.ProjectID, old.Project, old.State = v.ProjectID, v.Project, "deprecated"
		keep = append(keep, old.ID)
		if res.OldActive == nil {
			o := old
			res.OldActive = &o
			res.PrevSemver = old.Semver
		}
	}
	demoteRows.Close()
	if err := demoteRows.Err(); err != nil {
		return ActivateResult{}, err
	}

	// Older deprecated versions leave the window: only one deprecated per
	// project (уточнено в v0). The freshly demoted ones (and the rollback
	// target itself, still `deprecated` at this point) are kept.
	rows, err := tx.Query(ctx, `
		update versions set state = 'disabled'
		where project_id = $1::uuid and state = 'deprecated'
		  and not (id::text = any($2::text[]))
		returning id::text`, v.ProjectID, keep)
	if err != nil {
		return ActivateResult{}, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return ActivateResult{}, err
		}
		res.Disabled = append(res.Disabled, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ActivateResult{}, err
	}
	for _, id := range res.Disabled {
		id := id
		if err := insertEvent(ctx, tx, EventVersionDisabled, EventRef{VersionID: &id},
			map[string]any{"project": v.Project, "reason": "superseded"}); err != nil {
			return ActivateResult{}, err
		}
	}

	if _, err := tx.Exec(ctx, `
		update versions set state = 'active', deprecated_at = null
		where id = $1::uuid`, v.ID); err != nil {
		return ActivateResult{}, err
	}
	v.State, v.DeprecatedAt = "active", nil

	if regions == nil {
		regions = []string{} // nil would become SQL NULL and poison ANY()
	}
	regRows, err := tx.Query(ctx, `
		update fleet_configs set active_version = $2::uuid
		where project_id = $1::uuid
		  and (cardinality($3::text[]) = 0 or region = any($3::text[]))
		returning region`, v.ProjectID, v.ID, regions)
	if err != nil {
		return ActivateResult{}, err
	}
	for regRows.Next() {
		var r string
		if err := regRows.Scan(&r); err != nil {
			regRows.Close()
			return ActivateResult{}, err
		}
		res.Regions = append(res.Regions, r)
	}
	regRows.Close()
	if err := regRows.Err(); err != nil {
		return ActivateResult{}, err
	}
	if len(regions) > 0 && len(res.Regions) == 0 {
		// Region-scoped flip hit no fleet — abort the whole transaction so
		// the version states stay untouched too.
		return ActivateResult{}, fmt.Errorf("no fleet of project %s in regions %v: %w", v.Project, regions, ErrNotFound)
	}

	payload := map[string]any{
		"project": v.Project, "semver": v.Semver, "regions": res.Regions,
	}
	if res.OldActive != nil {
		payload["old_version_id"] = res.OldActive.ID
		payload["old_semver"] = res.OldActive.Semver
	}
	if err := insertEvent(ctx, tx, eventKind, EventRef{VersionID: &v.ID}, payload); err != nil {
		return ActivateResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ActivateResult{}, err
	}
	res.Version = v
	return res, nil
}

// RollbackTarget finds the version a rollback would activate: the project's
// (single by construction) deprecated version, newest deprecated_at first.
func (s *Store) RollbackTarget(ctx context.Context, project string) (Version, error) {
	var v Version
	err := s.Pool.QueryRow(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.channel, v.state, v.created_at, v.deprecated_at
		from versions v join projects p on p.id = v.project_id
		where p.slug = $1 and v.state = 'deprecated'
		order by v.deprecated_at desc nulls last
		limit 1`, project).
		Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Channel, &v.State, &v.CreatedAt, &v.DeprecatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, fmt.Errorf("project %s has no deprecated version to roll back to: %w", project, ErrVersionState)
	}
	return v, err
}

// PrepullingVersions lists versions stuck in `prepulling` — deploy jobs to
// resume after a master restart (the in-memory prepull tracking is lost).
func (s *Store) PrepullingVersions(ctx context.Context) ([]Version, error) {
	rows, err := s.Pool.Query(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.channel, v.state, v.created_at, v.deprecated_at
		from versions v join projects p on p.id = v.project_id
		where v.state = 'prepulling'
		order by v.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Channel, &v.State, &v.CreatedAt, &v.DeprecatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DisableExpiredDeprecated closes the multi-version window by TTL (уточнено
// в v0, master.md §5): a deprecated version older than the LONGEST
// reap_ttl_min across the project's fleets goes to `disabled` — the
// matchmaker stops offering it (old clients start getting update_required)
// and reconcile reaps its ready buffer / drains its live matches.
func (s *Store) DisableExpiredDeprecated(ctx context.Context) ([]Version, error) {
	rows, err := s.Pool.Query(ctx, `
		update versions v set state = 'disabled'
		where v.state = 'deprecated' and v.deprecated_at is not null
		  and v.deprecated_at + make_interval(mins => coalesce((
		        select max(f.reap_ttl_min) from fleet_configs f
		        where f.project_id = v.project_id), 0)) < now()
		returning v.id::text, v.project_id::text, v.semver, v.image_ref, v.channel, v.created_at, v.deprecated_at`)
	if err != nil {
		return nil, err
	}
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.Semver, &v.ImageRef, &v.Channel, &v.CreatedAt, &v.DeprecatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		v.State = "disabled"
		out = append(out, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, v := range out {
		id := v.ID
		if err := insertEvent(ctx, s.Pool, EventVersionDisabled, EventRef{VersionID: &id},
			map[string]any{"semver": v.Semver, "reason": "reap_ttl"}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// VersionStateCounts returns state → count for the birdman_versions metric.
func (s *Store) VersionStateCounts(ctx context.Context) (map[string]map[string]int, error) {
	rows, err := s.Pool.Query(ctx, `
		select p.slug, v.state, count(*)::int
		from versions v join projects p on p.id = v.project_id
		group by 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var project, state string
		var n int
		if err := rows.Scan(&project, &state, &n); err != nil {
			return nil, err
		}
		if out[project] == nil {
			out[project] = map[string]int{}
		}
		out[project][state] = n
	}
	return out, rows.Err()
}

// SetVersionDeprecatedAt pins deprecated_at (tests: fast-forward the TTL).
func (s *Store) SetVersionDeprecatedAt(ctx context.Context, versionID string, at time.Time) error {
	_, err := s.Pool.Exec(ctx,
		`update versions set deprecated_at = $2 where id = $1::uuid`, versionID, at)
	return err
}
