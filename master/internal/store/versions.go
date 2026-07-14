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

// imageCleanupSweepBatch — потолок одного прохода VersionsPendingImageCleanup
// (M-2). Держит взрыв кандидатов вне in-memory очередей хаба; остаток забирает
// следующий 60с-субтик.
const imageCleanupSweepBatch = 200

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
		return Version{}, badEnvErr(p.Project, p.Env)
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
		return Version{}, badEnvErr(src.Project, toEnv)
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
	// (23503) → тот же ErrBadEnv, что и пре-чек выше (v3): promoteError отдаёт его как
	// 400 (опечатка/гонка to_env — ввод клиента), а не 404/500.
	if mapped := mapEnvFKViolation(err, src.Project, toEnv); mapped != nil {
		return Version{}, mapped
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

// RetireVersions applies per-environment version retention (environments v1
// §6а, M5): a ~60s reconcile subtick. For every env with retention_keep > 0, it
// ranks that env's registered|disabled versions by created_at desc (id desc
// tie-break, consistent with the auto-deploy ordering) and moves the `registered`
// ones beyond position keep — AND older than 1 hour — to `disabled`. This is the
// ONLY path registered → disabled; active/prepulling/deprecated are never ranked
// nor touched. The 1h floor guards the race with the auto-deploy queue (a
// just-registered build in an auto_deploy burst must not be retired before it is
// even picked). Each retired version emits version_retired {semver, env}.
//
// Returns the retired versions (as DisabledVersion) so the caller dispatches
// RemoveImage to the env's nodes — the same disabled-transition contract as
// ActivateVersion/DisableExpiredDeprecated.
func (s *Store) RetireVersions(ctx context.Context) ([]DisabledVersion, error) {
	rows, err := s.Pool.Query(ctx, `
		with ranked as (
			select v.id, v.project_id, v.semver, v.image_ref, v.env, v.state, v.created_at,
			       e.retention_keep,
			       row_number() over (partition by v.project_id, v.env
			                          order by v.created_at desc, v.id desc) as rn
			from versions v
			join environments e on e.project_id = v.project_id and e.name = v.env
			where e.retention_keep > 0
			  and v.state in ('registered', 'disabled')
		)
		update versions u set state = 'disabled'
		from ranked r
		where u.id = r.id
		  and r.state = 'registered'
		  and r.rn > r.retention_keep
		  and r.created_at < now() - interval '1 hour'
		returning u.id::text, u.project_id::text, u.semver, u.image_ref, u.env`)
	if err != nil {
		return nil, err
	}
	var out []DisabledVersion
	for rows.Next() {
		var d DisabledVersion
		if err := rows.Scan(&d.VersionID, &d.ProjectID, &d.Semver, &d.ImageRef, &d.Env); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range out {
		id := d.VersionID
		if err := insertEvent(ctx, s.Pool, EventVersionRetired, EventRef{VersionID: &id},
			map[string]any{"semver": d.Semver, "env": d.Env}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ImageRefInUse reports whether image_ref still belongs to a NON-disabled version
// of (projectID, env) — the shared-ref guard for RemoveImage (environments v1
// §6б). Exact-string match (tag ≠ digest): a live containerd ref must not be
// deleted, so the dispatcher withholds RemoveImage while any active/prepulling/
// deprecated/registered version of the same (project, env) still names that ref
// (e.g. a promote that registered the same ref, or two versions sharing a build).
func (s *Store) ImageRefInUse(ctx context.Context, projectID, env, imageRef string) (bool, error) {
	var inUse bool
	err := s.Pool.QueryRow(ctx, `
		select exists(select 1 from versions
		              where project_id = $1::uuid and env = $2
		                and image_ref = $3 and state <> 'disabled')`,
		projectID, env, imageRef).Scan(&inUse)
	return inUse, err
}

// VersionsPendingImageCleanup returns the disabled versions whose image is still
// on the fleet's nodes and can now be removed — the CONVERGING sweep behind the
// immediate RemoveImage dispatch (Фаза D, дефект стенда).
//
// Гонка disabled×drain: немедленная отправка происходит В МОМЕНТ перехода версии
// в disabled — но ровно тогда серверы этой версии ещё дренятся (реконсайлер
// только что выгнал их из окна, grace 30с). Агент видит образ занятым живым
// контейнером и скипает команду; через 40-60с контейнеры уходят, а повторить
// команду некому — образ остаётся до watermark-GC. Sweep догоняет этот случай.
//
// A version qualifies when ALL of:
//
//	state = 'disabled'          — образ флоту больше не нужен;
//	image_cleanup_at is null    — sweep по ней ещё не отправлял (ровно одна
//	                              догоняющая команда, без спама каждые 60с);
//	no live server of it        — creating/ready/allocated/draining ещё держат ref
//	                              контейнером на ноде (failed/reaped — уже нет);
//	ref not held by a non-disabled version of the same (project, env) — shared-ref
//	                              guard, ровно семантика ImageRefInUse (§6б).
//
// Дедуп ref'ов и рассылка нодам окружения — на стороне ImageCleaner.CleanupImages;
// отправив, вызывающий штампует MarkImageCleanupSent.
//
// Выборка ограничена imageCleanupSweepBatch (M-2): разовый всплеск кандидатов —
// после миграции, на большом парке или после долгого простоя sweep'а — не должен
// раздувать in-memory pending-очереди хаба (RemoveImage уходит КАЖДОЙ ноде
// окружения, офлайн-ноды копят команды в памяти мастера). Остаток доберётся
// следующим субтиком: выборка сходящаяся (order by created_at + маркер), поэтому
// хвост не голодает.
func (s *Store) VersionsPendingImageCleanup(ctx context.Context) ([]DisabledVersion, error) {
	rows, err := s.Pool.Query(ctx, `
		select v.id::text, v.project_id::text, v.semver, v.image_ref, v.env
		from versions v
		where v.state = 'disabled'
		  and v.image_cleanup_at is null
		  and v.image_ref <> ''
		  and not exists (
		        select 1 from servers s
		        where s.version_id = v.id
		          and s.state in ('creating', 'ready', 'allocated', 'draining'))
		  and not exists (
		        select 1 from versions o
		        where o.project_id = v.project_id and o.env = v.env
		          and o.image_ref = v.image_ref and o.state <> 'disabled')
		order by v.created_at
		limit $1`, imageCleanupSweepBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DisabledVersion
	for rows.Next() {
		var d DisabledVersion
		if err := rows.Scan(&d.VersionID, &d.ProjectID, &d.Semver, &d.ImageRef, &d.Env); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkImageCleanupSent stamps image_cleanup_at on the versions the sweep has just
// dispatched RemoveImage for, so each disabled version gets AT MOST ONE catch-up
// command — the ~60s subtick must not re-send it forever. The `is null` guard keeps
// the FIRST stamp (idempotent re-runs); an empty batch is a no-op. Stamped only
// after a successful dispatch: a failed CleanupImages leaves the marker null, so
// the next subtick retries (at-least-once, как и вся доставка agentlink).
func (s *Store) MarkImageCleanupSent(ctx context.Context, versionIDs []string) error {
	if len(versionIDs) == 0 {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `
		update versions set image_cleanup_at = now()
		where id = any($1::uuid[]) and image_cleanup_at is null`, versionIDs)
	return err
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
