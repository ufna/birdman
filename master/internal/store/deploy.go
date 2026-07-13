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
// disabled. Version states are scoped per (project, env) (environments v1 §3);
// per-region targeting lives in fleet_configs.active_version (одна active и одно
// deprecated-окно на (project, env) — при флипе более старые deprecated уходят в
// disabled).

var (
	// ErrVersionState — the version is in a state that forbids the operation
	// (deploy of a disabled version, rollback without a deprecated one, ...).
	ErrVersionState = errors.New("version_state")
	// ErrDeployInProgress — another version of the (project, env) is prepulling.
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

// BeginDeployOpts annotates the deploy_started event. The zero value is the
// manual path (POST /v1/deploy, /v1/promote): the event carries no auto/skipped
// keys. The auto-deploy chain passes Auto=true and Skipped=N (how many
// intermediate registered builds this jump passes over) so the payload records
// «только вперёд» provenance (environments v1 §4). Decision (task-2): manual
// deploys omit the keys entirely rather than writing auto:false — event
// consumers treat a missing "auto" as manual, and the panel/history stay
// unchanged for the pre-env manual flow.
type BeginDeployOpts struct {
	Auto    bool
	Skipped int
}

// BeginDeploy moves a version to `prepulling` (step 1 of POST /v1/deploy).
// Idempotent: repeated calls for a prepulling/active version report the fact
// instead of failing. Only one version per (project, env) may be prepulling.
func (s *Store) BeginDeploy(ctx context.Context, versionID string, opts BeginDeployOpts) (BeginDeployResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BeginDeployResult{}, err
	}
	defer tx.Rollback(ctx)

	var v Version
	err = tx.QueryRow(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.env, v.state, v.created_at, v.deprecated_at
		from versions v join projects p on p.id = v.project_id
		where v.id = $1::uuid
		for update of v`, versionID).
		Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt, &v.DeprecatedAt)
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
	// and the environment has at least one fleet to deploy to. Both checks are
	// scoped to the version's (project, env): a dev prepull must not block a prod
	// deploy of the same project, and hasFleet asks about THIS env's fleets
	// (environments v1 §3).
	var busy bool
	if err := tx.QueryRow(ctx, `
		select exists(select 1 from versions
		              where project_id = $1::uuid and env = $2 and state = 'prepulling')`,
		v.ProjectID, v.Env).Scan(&busy); err != nil {
		return BeginDeployResult{}, err
	}
	if busy {
		return BeginDeployResult{}, fmt.Errorf("another deploy of project %s (env %s) is prepulling: %w", v.Project, v.Env, ErrDeployInProgress)
	}
	var hasFleet bool
	if err := tx.QueryRow(ctx,
		`select exists(select 1 from fleet_configs where project_id = $1::uuid and env = $2)`,
		v.ProjectID, v.Env).Scan(&hasFleet); err != nil {
		return BeginDeployResult{}, err
	}
	if !hasFleet {
		return BeginDeployResult{}, fmt.Errorf("project %s has no fleets in env %s: %w", v.Project, v.Env, ErrNoFleet)
	}

	if _, err := tx.Exec(ctx,
		`update versions set state = 'prepulling' where id = $1::uuid`, v.ID); err != nil {
		return BeginDeployResult{}, err
	}
	// env в payload (environments v1 §3): без него deploy_started невидим в
	// пер-env фильтре событий (W4) — событие несёт окружение прямо в теле.
	payload := map[string]any{"project": v.Project, "env": v.Env, "semver": v.Semver, "image_ref": v.ImageRef}
	if opts.Auto {
		// Авто-путь «только вперёд» (environments v1 §4): помечаем событие и
		// сколько промежуточных registered-билдов этот прыжок пропустил.
		payload["auto"] = true
		payload["skipped"] = opts.Skipped
	}
	if err := insertEvent(ctx, tx, EventDeployStarted, EventRef{VersionID: &v.ID}, payload); err != nil {
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

// PrePullTargets returns the nodes a deploy must warm: active nodes of the
// version's environment with a fresh heartbeat (<30s) in every region that env
// has a fleet_config for (master.md §5 step 1: «всем активным тачкам региона(ов)
// флита»). env-скоуп (environments v1 §3): деплой греет только ноды своего env —
// prod-деплой не трогает dev-ноды того же региона и наоборот.
func (s *Store) PrePullTargets(ctx context.Context, projectID, env string) ([]PrePullNode, error) {
	rows, err := s.Pool.Query(ctx, `
		select n.id::text, n.region
		from nodes n
		where n.project_id = $1::uuid and n.env = $2 and n.state = 'active'
		  and n.last_heartbeat_at > now() - interval '30 seconds'
		  and n.region in (select region from fleet_configs where project_id = $1::uuid and env = $2)
		order by n.created_at`, projectID, env)
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
//	fleet_configs.active_version → the new version: every region of this env,
//	or only `regions` when non-empty (region-scoped rollback; version states
//	are scoped per (project, env) — environments v1 §3).
func (s *Store) ActivateVersion(ctx context.Context, versionID, fromState, eventKind string, regions []string) (ActivateResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ActivateResult{}, err
	}
	defer tx.Rollback(ctx)

	var v Version
	err = tx.QueryRow(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.env, v.state, v.created_at, v.deprecated_at
		from versions v join projects p on p.id = v.project_id
		where v.id = $1::uuid
		for update of v`, versionID).
		Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt, &v.DeprecatedAt)
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

	// Demote what the environment runs today: the `active` version AND any
	// version the fleets still point at while its state is `registered`
	// (bootstrap: active_version was assigned via PUT /v1/fleets before the
	// deploy manager existed) — without this the old build would fall out of
	// the multi-version window and its live matches would be drained at once.
	// env-скоуп (environments v1 §3): флип живёт строго внутри (project, env) —
	// активация dev-версии не демоутит prod-active того же проекта.
	demoteRows, err := tx.Query(ctx, `
		update versions set state = 'deprecated', deprecated_at = now()
		where project_id = $1::uuid and env = $3 and id <> $2::uuid
		  and (state = 'active'
		       or (state = 'registered' and id in (
		             select active_version from fleet_configs
		             where project_id = $1::uuid and env = $3 and active_version is not null)))
		returning id::text, semver, image_ref, env, created_at`,
		v.ProjectID, v.ID, v.Env)
	if err != nil {
		return ActivateResult{}, err
	}
	keep := []string{v.ID}
	for demoteRows.Next() {
		var old Version
		if err := demoteRows.Scan(&old.ID, &old.Semver, &old.ImageRef, &old.Env, &old.CreatedAt); err != nil {
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
	// (project, env) — environments v1 §3 (уточнено в v0). The freshly demoted
	// ones (and the rollback target itself, still `deprecated` at this point)
	// are kept.
	rows, err := tx.Query(ctx, `
		update versions set state = 'disabled'
		where project_id = $1::uuid and env = $3 and state = 'deprecated'
		  and not (id::text = any($2::text[]))
		returning id::text`, v.ProjectID, keep, v.Env)
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
	// env-скоуп репойнта (environments v1 §3, C3): перепойнчиваем ТОЛЬКО флоты
	// этого env — иначе UPDATE сажает версию одного env в active_version флота
	// чужого env того же региона и нарушает составной FK fleet_active_version_env_fk.
	regRows, err := tx.Query(ctx, `
		update fleet_configs set active_version = $2::uuid
		where project_id = $1::uuid and env = $4
		  and (cardinality($3::text[]) = 0 or region = any($3::text[]))
		returning region`, v.ProjectID, v.ID, regions, v.Env)
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
		return ActivateResult{}, fmt.Errorf("no fleet of project %s (env %s) in regions %v: %w", v.Project, v.Env, regions, ErrNotFound)
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

// RollbackTarget finds the version a rollback would activate in an environment:
// that (project, env)'s (single by construction) deprecated version, newest
// deprecated_at first. env-скоуп (environments v1 §3): откат живёт внутри env.
func (s *Store) RollbackTarget(ctx context.Context, project, env string) (Version, error) {
	var v Version
	err := s.Pool.QueryRow(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.env, v.state, v.created_at, v.deprecated_at
		from versions v join projects p on p.id = v.project_id
		where p.slug = $1 and v.env = $2 and v.state = 'deprecated'
		order by v.deprecated_at desc nulls last
		limit 1`, project, env).
		Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt, &v.DeprecatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, fmt.Errorf("project %s (env %s) has no deprecated version to roll back to: %w", project, env, ErrVersionState)
	}
	return v, err
}

// EnvsWithDeprecated returns the environments of a project that currently hold a
// deprecated version (a warm rollback window), sorted by name. The rollback API
// resolves env with it when the request names none: exactly one → that env;
// zero → nothing to roll back; several → env is required (environments v1 §3, I3).
func (s *Store) EnvsWithDeprecated(ctx context.Context, project string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		select distinct v.env
		from versions v join projects p on p.id = v.project_id
		where p.slug = $1 and v.state = 'deprecated'
		order by v.env`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var env string
		if err := rows.Scan(&env); err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

// PrepullingVersions lists versions stuck in `prepulling` — deploy jobs to
// resume after a master restart (the in-memory prepull tracking is lost).
func (s *Store) PrepullingVersions(ctx context.Context) ([]Version, error) {
	rows, err := s.Pool.Query(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.env, v.state, v.created_at, v.deprecated_at
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
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt, &v.DeprecatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DisableExpiredDeprecated closes the multi-version window by TTL (уточнено
// в v0, master.md §5): a deprecated version older than the LONGEST
// reap_ttl_min across ITS ENVIRONMENT's fleets goes to `disabled` — the
// matchmaker stops offering it (old clients start getting update_required)
// and reconcile reaps its ready buffer / drains its live matches. env-скоуп
// (environments v1 §3, M4): TTL берётся от флотов env версии, не всего проекта —
// иначе длинный prod-TTL держал бы dev-версию в окне сверх её dev-TTL.
func (s *Store) DisableExpiredDeprecated(ctx context.Context) ([]Version, error) {
	rows, err := s.Pool.Query(ctx, `
		update versions v set state = 'disabled'
		where v.state = 'deprecated' and v.deprecated_at is not null
		  and v.deprecated_at + make_interval(mins => coalesce((
		        select max(f.reap_ttl_min) from fleet_configs f
		        where f.project_id = v.project_id and f.env = v.env), 0)) < now()
		returning v.id::text, v.project_id::text, v.semver, v.image_ref, v.env, v.created_at, v.deprecated_at`)
	if err != nil {
		return nil, err
	}
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.Semver, &v.ImageRef, &v.Env, &v.CreatedAt, &v.DeprecatedAt); err != nil {
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
