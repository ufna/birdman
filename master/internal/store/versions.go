package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrConflict is returned on unique-constraint conflicts (duplicate version).
var ErrConflict = errors.New("conflict")

// CreatedID orders versions for the auto-deploy «только вперёд» rule
// (environments v1 §4): primary key created_at, tie-broken by id — a CI burst
// can stamp several versions in the same second. The zero value (empty ID)
// sorts before every real version, so it is the «nothing attempted yet» marker.
type CreatedID struct {
	CreatedAt time.Time
	ID        string
}

// afterBound turns a CreatedID lower bound into query params: a nil timestamp
// means «no lower bound» (the zero marker), so the SQL below matches every row.
func afterBound(after CreatedID) (*time.Time, string) {
	if after.ID == "" {
		return nil, ""
	}
	t := after.CreatedAt
	return &t, after.ID
}

type CreateVersionParams struct {
	Project  string
	Semver   string
	ImageRef string
	Env      string // окружение регистрации (environments v1); заменило прежний лейбл
}

// CreateVersion registers a build (POST /v1/versions, CI entry point) in an
// environment. env must exist for the project (versions_env_fk enforces it; the
// explicit in-tx check gives a clean 400 instead of a 500, and sees the dev/prod
// seeded by ensureProject in this very transaction for a brand-new project).
func (s *Store) CreateVersion(ctx context.Context, p CreateVersionParams) (Version, error) {
	if p.Semver == "" || p.ImageRef == "" {
		return Version{}, fmt.Errorf("semver and image_ref are required")
	}
	if p.Env == "" {
		return Version{}, fmt.Errorf("env is required")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Version{}, err
	}
	defer tx.Rollback(ctx)
	projectID, err := ensureProject(ctx, tx, p.Project)
	if err != nil {
		return Version{}, err
	}
	var envExists bool
	if err := tx.QueryRow(ctx,
		`select exists(select 1 from environments where project_id = $1::uuid and name = $2)`,
		projectID, p.Env).Scan(&envExists); err != nil {
		return Version{}, err
	}
	if !envExists {
		return Version{}, fmt.Errorf("no such environment %s/%s", p.Project, p.Env)
	}
	var v Version
	err = tx.QueryRow(ctx, `
		insert into versions (project_id, semver, image_ref, env)
		values ($1::uuid, $2, $3, $4)
		returning id::text, project_id::text, semver, image_ref, env, state, created_at`,
		projectID, p.Semver, p.ImageRef, p.Env).
		Scan(&v.ID, &v.ProjectID, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Version{}, fmt.Errorf("version %s/%s (%s): %w", p.Project, p.Semver, p.Env, ErrConflict)
	}
	// Гонка с DELETE env между in-tx пре-чеком и insert'ом: versions_env_fk (23503)
	// → внятный «no such environment» (400), а не сырой 500 (w5).
	if mapped := mapEnvFKViolation(err, p.Project, p.Env); mapped != nil {
		return Version{}, mapped
	}
	if err != nil {
		return Version{}, err
	}
	v.Project = p.Project
	if err := insertEvent(ctx, tx, EventVersionRegistered, EventRef{VersionID: &v.ID},
		map[string]any{"project": p.Project, "semver": p.Semver, "env": p.Env, "image_ref": p.ImageRef}); err != nil {
		return Version{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Version{}, err
	}
	return v, nil
}

// ListVersions returns all registered versions.
func (s *Store) ListVersions(ctx context.Context) ([]Version, error) {
	rows, err := s.Pool.Query(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.env, v.state, v.created_at, v.deprecated_at, v.promoted_from::text
		from versions v join projects p on p.id = v.project_id
		order by v.created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt, &v.DeprecatedAt, &v.PromotedFrom); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PromoteVersion promotes a version into another environment (POST /v1/promote,
// environments v1 §4): a NEW version in toEnv carrying the SAME project/semver/
// image_ref and provenance promoted_from = source. Idempotent (I7): if toEnv
// already holds that semver with the SAME image_ref in state `registered`, that
// row is reused (a re-promote after «deploy did not start» must not dead-end the
// operator); a different image_ref → ErrConflict; a non-registered state →
// ErrConflict («already …»). Promote into the source's own env → ErrConflict.
// An unknown toEnv → a plain «no such environment» error (mapped to 400 like
// CreateVersion — a typo'd env is client input, not ErrNotFound/ErrConflict). A
// missing source version → ErrNotFound.
//
// The transaction covers ONLY the version row; the deploy is async — the caller
// runs the normal deploy pipeline (dep.Deploy) afterwards, so the version stays
// registered and retryable even when that deploy later fails (e.g. no fleet yet).
func (s *Store) PromoteVersion(ctx context.Context, versionID, toEnv string) (Version, error) {
	if toEnv == "" {
		return Version{}, fmt.Errorf("to_env is required")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Version{}, err
	}
	defer tx.Rollback(ctx)

	// Источник под замком: сериализует конкурентные промоуты одной версии и не
	// даёт ей измениться, пока считаем provenance (сам источник не мутируем).
	var src Version
	err = tx.QueryRow(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.env, v.state
		from versions v join projects p on p.id = v.project_id
		where v.id = $1::uuid
		for update of v`, versionID).
		Scan(&src.ID, &src.ProjectID, &src.Project, &src.Semver, &src.ImageRef, &src.Env, &src.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, fmt.Errorf("version %s: %w", versionID, ErrNotFound)
	}
	if err != nil {
		return Version{}, err
	}

	// Промоут в тот же env бессмыслен (та же строка) — конфликт.
	if toEnv == src.Env {
		return Version{}, fmt.Errorf("version %s is already in environment %s: %w", src.Semver, toEnv, ErrConflict)
	}
	// Целевой env обязан существовать; несуществующий → понятная ошибка (→400,
	// паритет CreateVersion), НЕ ErrConflict/ErrNotFound. Проверка в tx видит
	// dev/prod, засеянные ensureProject для нового проекта в этой же транзакции.
	var envExists bool
	if err := tx.QueryRow(ctx,
		`select exists(select 1 from environments where project_id = $1::uuid and name = $2)`,
		src.ProjectID, toEnv).Scan(&envExists); err != nil {
		return Version{}, err
	}
	if !envExists {
		return Version{}, fmt.Errorf("no such environment %s/%s", src.Project, toEnv)
	}

	// Идемпотентность (I7): уже есть версия (project, toEnv, semver)? Реюз строго
	// при том же image_ref И state=registered; иначе — конфликт.
	var ex Version
	err = tx.QueryRow(ctx, `
		select v.id::text, v.image_ref, v.state, v.created_at, v.promoted_from::text
		from versions v
		where v.project_id = $1::uuid and v.env = $2 and v.semver = $3
		for update of v`, src.ProjectID, toEnv, src.Semver).
		Scan(&ex.ID, &ex.ImageRef, &ex.State, &ex.CreatedAt, &ex.PromotedFrom)
	switch {
	case err == nil:
		if ex.ImageRef != src.ImageRef {
			return Version{}, fmt.Errorf("version %s/%s already exists in %s with a different image_ref: %w", src.Project, src.Semver, toEnv, ErrConflict)
		}
		if ex.State != "registered" {
			return Version{}, fmt.Errorf("version %s/%s is already %s in %s: %w", src.Project, src.Semver, ex.State, toEnv, ErrConflict)
		}
		// Реюз той же строки — без нового события (idempotent no-op, паритет
		// BeginDeploy); деплой запустит хендлер отдельно.
		if err := tx.Commit(ctx); err != nil {
			return Version{}, err
		}
		ex.ProjectID, ex.Project, ex.Semver, ex.Env = src.ProjectID, src.Project, src.Semver, toEnv
		return ex, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Новой строки нет — создаём с provenance ниже.
	default:
		return Version{}, err
	}

	var v Version
	err = tx.QueryRow(ctx, `
		insert into versions (project_id, semver, image_ref, env, promoted_from)
		values ($1::uuid, $2, $3, $4, $5::uuid)
		returning id::text, state, created_at`,
		src.ProjectID, src.Semver, src.ImageRef, toEnv, src.ID).
		Scan(&v.ID, &v.State, &v.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Гонка: параллельный промоут занял semver между select и insert.
		return Version{}, fmt.Errorf("version %s/%s (%s): %w", src.Project, src.Semver, toEnv, ErrConflict)
	}
	// Гонка с DELETE целевого env между in-tx пре-чеком и insert'ом: versions_env_fk
	// (23503). Детектим тем же mapEnvFKViolation, но возвращаем ПЛОСКУЮ (не-ErrNotFound)
	// «no such environment» — точь-в-точь как пре-чек выше: promoteError отдаёт её как
	// 400 (опечатка/гонка to_env — ввод клиента), а не 404 от завёрнутого ErrNotFound.
	if mapEnvFKViolation(err, src.Project, toEnv) != nil {
		return Version{}, fmt.Errorf("no such environment %s/%s", src.Project, toEnv)
	}
	if err != nil {
		return Version{}, err
	}
	v.ProjectID, v.Project, v.Semver, v.ImageRef, v.Env = src.ProjectID, src.Project, src.Semver, src.ImageRef, toEnv
	v.PromotedFrom = &src.ID
	if err := insertEvent(ctx, tx, EventVersionPromoted, EventRef{VersionID: &v.ID},
		map[string]any{"project": src.Project, "semver": src.Semver, "from_env": src.Env, "to_env": toEnv, "image_ref": src.ImageRef}); err != nil {
		return Version{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Version{}, err
	}
	return v, nil
}

// NewestRegisteredAfter returns the newest `registered` version of (project,
// env) strictly after the `after` marker by (created_at, id) — the auto-deploy
// «только вперёд» target (environments v1 §4). The bool is false when the tail
// is empty (nothing newer to roll). The zero marker matches every registered
// version, so the very first registration is picked.
func (s *Store) NewestRegisteredAfter(ctx context.Context, projectID, env string, after CreatedID) (Version, bool, error) {
	afterAt, afterID := afterBound(after)
	var v Version
	err := s.Pool.QueryRow(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.env, v.state, v.created_at, v.deprecated_at
		from versions v join projects p on p.id = v.project_id
		where v.project_id = $1::uuid and v.env = $2 and v.state = 'registered'
		  and ($3::timestamptz is null
		       or v.created_at > $3
		       or (v.created_at = $3 and v.id::text > $4))
		order by v.created_at desc, v.id::text desc
		limit 1`, projectID, env, afterAt, afterID).
		Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt, &v.DeprecatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, err
	}
	return v, true, nil
}

// CountRegisteredBetween counts `registered` versions of (project, env) strictly
// after `after` and strictly before `before` by (created_at, id) — the number
// of intermediate builds an auto-deploy jump skips (the deploy_started
// "skipped" annotation, environments v1 §4). The zero `after` marker leaves the
// lower bound open.
func (s *Store) CountRegisteredBetween(ctx context.Context, projectID, env string, after, before CreatedID) (int, error) {
	afterAt, afterID := afterBound(after)
	var n int
	err := s.Pool.QueryRow(ctx, `
		select count(*)::int
		from versions v
		where v.project_id = $1::uuid and v.env = $2 and v.state = 'registered'
		  and ($3::timestamptz is null
		       or v.created_at > $3
		       or (v.created_at = $3 and v.id::text > $4))
		  and (v.created_at < $5
		       or (v.created_at = $5 and v.id::text < $6))`,
		projectID, env, afterAt, afterID, before.CreatedAt, before.ID).Scan(&n)
	return n, err
}

// NewestNonRegisteredMarker returns the (created_at, id) of the newest version
// of (project, env) that has left `registered` (prepulling/active/deprecated/
// disabled) — the last version auto-deploy attempted. Resume rebuilds the
// in-memory last_attempted marker from it after a restart so the chain never
// rolls backwards past the version already in flight or active (environments v1
// §4). The bool is false when every version is still registered (nothing
// attempted yet → the zero marker stands).
func (s *Store) NewestNonRegisteredMarker(ctx context.Context, projectID, env string) (CreatedID, bool, error) {
	var mk CreatedID
	err := s.Pool.QueryRow(ctx, `
		select v.created_at, v.id::text
		from versions v
		where v.project_id = $1::uuid and v.env = $2 and v.state <> 'registered'
		order by v.created_at desc, v.id::text desc
		limit 1`, projectID, env).Scan(&mk.CreatedAt, &mk.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreatedID{}, false, nil
	}
	if err != nil {
		return CreatedID{}, false, err
	}
	return mk, true, nil
}

// NewestAttemptedMarker reconstructs the auto-deploy «только вперёд»
// last_attempted marker for (project, env) after a master restart: the
// (created_at, id) of the newest version that was EVER put in flight — i.e.
// carries a deploy_started event (join events by version_id). Unlike
// NewestNonRegisteredMarker, which reads version STATE, this remembers an
// attempt even after an abort rolled the version back to `registered` (prepull
// timeout / failed pull): the deploy_started event survives, so Resume no longer
// forgets the failed build and re-attacks it once per restart (environments v1
// §4, follow-up v5). Manual deploys write deploy_started too, so the marker stays
// consistent with them.
//
// Falls back to NewestNonRegisteredMarker when NO version was ever attempted
// (no deploy_started event) — a version made active/deprecated purely by a
// bootstrap PUT /v1/fleets active_version, or pre-event historical data, still
// anchors the marker. The bool is false only when both are empty (nothing
// attempted and everything registered → the zero marker stands).
func (s *Store) NewestAttemptedMarker(ctx context.Context, projectID, env string) (CreatedID, bool, error) {
	var mk CreatedID
	err := s.Pool.QueryRow(ctx, `
		select v.created_at, v.id::text
		from versions v
		where v.project_id = $1::uuid and v.env = $2
		  and exists (select 1 from events e
		              where e.version_id = v.id and e.kind = $3)
		order by v.created_at desc, v.id::text desc
		limit 1`, projectID, env, EventDeployStarted).Scan(&mk.CreatedAt, &mk.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Никого не деплоили (нет ни одного deploy_started) — падаем на прежний
		// признак «версия покинула registered»: bootstrap-через-флот или
		// доисторические (до-событийные) данные всё ещё держат маркер.
		return s.NewestNonRegisteredMarker(ctx, projectID, env)
	}
	if err != nil {
		return CreatedID{}, false, err
	}
	return mk, true, nil
}

// GetVersion returns one version by id.
func (s *Store) GetVersion(ctx context.Context, id string) (Version, error) {
	var v Version
	err := s.Pool.QueryRow(ctx, `
		select v.id::text, v.project_id::text, p.slug, v.semver, v.image_ref, v.env, v.state, v.created_at, v.deprecated_at, v.promoted_from::text
		from versions v join projects p on p.id = v.project_id
		where v.id = $1::uuid`, id).
		Scan(&v.ID, &v.ProjectID, &v.Project, &v.Semver, &v.ImageRef, &v.Env, &v.State, &v.CreatedAt, &v.DeprecatedAt, &v.PromotedFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, fmt.Errorf("version %s: %w", id, ErrNotFound)
	}
	return v, err
}
