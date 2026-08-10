package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Environments v1 (docs/superpowers/specs/2026-07-13-environments-v1-design.md
// §1–2): окружение — полноценное измерение платформы per-project. Поведение
// ведёт флаг production (не имя): production=true запрещает auto_deploy
// (guardrail в БД CHECK + здесь), ретеншн-дефолт безлимитный. CRUD явный. Сиды
// dev+prod каждому проекту делает ensureProject (nodes.go).
//
// Удаление НЕПУСТОГО окружения (снимает тупик I10 «использованный env неудаляем»):
// DeleteEnvironment каскадит содержимое одной транзакцией — при жёстком
// предусловии «ноль нод» и подтверждении вводом имени. Состав считает
// EnvironmentUsage (его же показывает панель перед удалением).

type Environment struct {
	ProjectID     string    `json:"project_id"`
	Project       string    `json:"project"`
	Name          string    `json:"name"`
	Production    bool      `json:"production"`
	AutoDeploy    bool      `json:"auto_deploy"`
	RetentionKeep int       `json:"retention_keep"`
	CreatedAt     time.Time `json:"created_at"`
}

type CreateEnvironmentParams struct {
	Project       string
	Name          string
	Production    bool
	AutoDeploy    bool
	RetentionKeep int
}

// EnvironmentPatch — частичный апдейт (PATCH); nil-поле = не трогать. Имя
// иммутабельно (нет поля).
type EnvironmentPatch struct {
	Production    *bool
	AutoDeploy    *bool
	RetentionKeep *int
}

// envNameRe дублирует БД-CHECK name ~ '^[a-z0-9][a-z0-9-]{0,31}$' — чтобы
// отдавать понятный 400, а не FK/CHECK-500. Зарезервированные all/global
// проверяются отдельно (тоже CHECK в БД).
var envNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validateEnvName(name string) error {
	if !envNameRe.MatchString(name) {
		return fmt.Errorf("environment name %q must match ^[a-z0-9][a-z0-9-]{0,31}$ (lowercase letters, digits and dashes, 1–32 chars, not starting with a dash)", name)
	}
	if name == "all" || name == "global" {
		return fmt.Errorf("environment name %q is reserved", name)
	}
	return nil
}

// ListEnvironments returns a project's environments, non-production first
// (panel convention, §8), then by name.
func (s *Store) ListEnvironments(ctx context.Context, projectSlug string) ([]Environment, error) {
	rows, err := s.Pool.Query(ctx, `
		select e.project_id::text, p.slug, e.name, e.production, e.auto_deploy, e.retention_keep, e.created_at
		from environments e join projects p on p.id = e.project_id
		where p.slug = $1
		order by e.production, e.name`, projectSlug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		var e Environment
		if err := rows.Scan(&e.ProjectID, &e.Project, &e.Name, &e.Production,
			&e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnvironmentNameExists reports whether ANY project has an environment with
// this name — the typo guard for `?env=` без выбранного проекта (мультипроект
// W3, httpapi.statsScope). Пары (project, env) тут нет по условию, поэтому
// проверяется только имя: этого хватает, чтобы опечатка дала понятный 400, а
// не молча пустой ряд, и не требует sole-project допущения.
func (s *Store) EnvironmentNameExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx,
		`select exists(select 1 from environments where name = $1)`, name).Scan(&exists)
	return exists, err
}

// GetEnvironment returns one environment (ErrBadEnv when missing). Every caller
// is an existence check of an env NAMED BY A REQUEST — the matchmaker ticket, the
// ?env= stats filter, the rollback body, the auto-deploy resolve — so a missing
// env is bad client input (400), not a missing resource (v3, ErrBadEnv). The
// env-as-addressed-resource routes (PATCH/DELETE /v1/environments/{p}/{name})
// have their own queries and keep ErrNotFound → 404.
func (s *Store) GetEnvironment(ctx context.Context, project, name string) (Environment, error) {
	var e Environment
	err := s.Pool.QueryRow(ctx, `
		select e.project_id::text, p.slug, e.name, e.production, e.auto_deploy, e.retention_keep, e.created_at
		from environments e join projects p on p.id = e.project_id
		where p.slug = $1 and e.name = $2`, project, name).
		Scan(&e.ProjectID, &e.Project, &e.Name, &e.Production, &e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Environment{}, badEnvErr(project, name)
	}
	return e, err
}

// AutoDeployEnvironments lists every environment (across all projects) with
// auto_deploy set — the set the deploy manager walks on Resume to restart each
// forward-only chain after a master restart (environments v1 §4).
func (s *Store) AutoDeployEnvironments(ctx context.Context) ([]Environment, error) {
	rows, err := s.Pool.Query(ctx, `
		select e.project_id::text, p.slug, e.name, e.production, e.auto_deploy, e.retention_keep, e.created_at
		from environments e join projects p on p.id = e.project_id
		where e.auto_deploy
		order by p.slug, e.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		var e Environment
		if err := rows.Scan(&e.ProjectID, &e.Project, &e.Name, &e.Production,
			&e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CreateEnvironment adds an environment (POST /v1/environments). Guardrails:
// name shape/reserved (§1 CHECK), production⇒!auto_deploy (§2). Duplicate →
// ErrConflict. The project is created on first reference (ensureProject seeds
// dev+prod), so an explicit dev/prod create collides with the seed → ErrConflict.
func (s *Store) CreateEnvironment(ctx context.Context, p CreateEnvironmentParams) (Environment, error) {
	if err := validateEnvName(p.Name); err != nil {
		return Environment{}, err
	}
	if p.Production && p.AutoDeploy {
		return Environment{}, fmt.Errorf("auto_deploy is not allowed on a production environment")
	}
	if p.RetentionKeep < 0 {
		return Environment{}, fmt.Errorf("retention_keep must be >= 0")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Environment{}, err
	}
	defer tx.Rollback(ctx)
	projectID, err := ensureProject(ctx, tx, p.Project)
	if err != nil {
		return Environment{}, err
	}
	var e Environment
	err = tx.QueryRow(ctx, `
		insert into environments (project_id, name, production, auto_deploy, retention_keep)
		values ($1::uuid, $2, $3, $4, $5)
		returning project_id::text, name, production, auto_deploy, retention_keep, created_at`,
		projectID, p.Name, p.Production, p.AutoDeploy, p.RetentionKeep).
		Scan(&e.ProjectID, &e.Name, &e.Production, &e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Environment{}, fmt.Errorf("environment %s/%s already exists: %w", p.Project, p.Name, ErrConflict)
	}
	if err != nil {
		return Environment{}, err
	}
	e.Project = p.Project
	if err := tx.Commit(ctx); err != nil {
		return Environment{}, err
	}
	return e, nil
}

// PatchEnvironment updates flags (PATCH /v1/environments/{project}/{name}).
// The guardrail production⇒!auto_deploy is re-checked on the RESULTING state
// (so enabling auto_deploy on a production env fails in any field order, §2).
// Name is immutable (not patchable). ErrNotFound for an unknown env.
func (s *Store) PatchEnvironment(ctx context.Context, project, name string, p EnvironmentPatch) (Environment, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Environment{}, err
	}
	defer tx.Rollback(ctx)

	// Текущее состояние в tx (против гонки двух PATCH).
	var e Environment
	err = tx.QueryRow(ctx, `
		select e.project_id::text, e.name, e.production, e.auto_deploy, e.retention_keep, e.created_at
		from environments e join projects p on p.id = e.project_id
		where p.slug = $1 and e.name = $2 for update of e`, project, name).
		Scan(&e.ProjectID, &e.Name, &e.Production, &e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Environment{}, fmt.Errorf("environment %s/%s: %w", project, name, ErrNotFound)
	}
	if err != nil {
		return Environment{}, err
	}

	if p.Production != nil {
		e.Production = *p.Production
	}
	if p.AutoDeploy != nil {
		e.AutoDeploy = *p.AutoDeploy
	}
	if p.RetentionKeep != nil {
		e.RetentionKeep = *p.RetentionKeep
	}
	if e.Production && e.AutoDeploy {
		return Environment{}, fmt.Errorf("auto_deploy is not allowed on a production environment")
	}
	if e.RetentionKeep < 0 {
		return Environment{}, fmt.Errorf("retention_keep must be >= 0")
	}

	err = tx.QueryRow(ctx, `
		update environments set production = $3, auto_deploy = $4, retention_keep = $5
		where project_id = $1::uuid and name = $2
		returning project_id::text, name, production, auto_deploy, retention_keep, created_at`,
		e.ProjectID, e.Name, e.Production, e.AutoDeploy, e.RetentionKeep).
		Scan(&e.ProjectID, &e.Name, &e.Production, &e.AutoDeploy, &e.RetentionKeep, &e.CreatedAt)
	if err != nil {
		return Environment{}, err
	}
	e.Project = project
	if err := tx.Commit(ctx); err != nil {
		return Environment{}, err
	}
	return e, nil
}

// ErrConfirmRequired — удаление НЕПУСТОГО окружения без корректного confirm
// (тело DELETE {"confirm":"<name>"} обязано ТОЧНО совпасть с именем). Отдельный
// sentinel, а не ErrConflict: это плохой ввод (400), а не состояние ресурса.
var ErrConfirmRequired = errors.New("confirm_required")

// queryRower — общий знаменатель *pgxpool.Pool и pgx.Tx: состав окружения
// считается и снаружи (GET .../usage), и внутри транзакции удаления (там он
// авторитетный — под блокировкой строки env).
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// EnvironmentUsage — состав окружения (GET /v1/environments/{project}/{name}/usage):
// сколько всего на него ссылается. Панель показывает это в диалоге удаления
// («будут удалены: N версий, N флотов…»), а API решает по нему, требовать ли
// подтверждение вводом имени. APIKeys — только ЖИВЫЕ (не отозванные) привязанные
// ключи: ровно те, что каскад отзовёт.
type EnvironmentUsage struct {
	Versions int `json:"versions"`
	Fleets   int `json:"fleets"`
	Nodes    int `json:"nodes"`
	Servers  int `json:"servers"`
	Matches  int `json:"matches"`
	APIKeys  int `json:"api_keys"`
}

// Empty — окружением никто не пользовался: удаляется как раньше, без confirm
// (204). Роллапы match_stats_daily сюда НЕ входят: это производные строки (их
// нельзя завести без матчей), и держать из-за них окружение «непустым» — шум.
func (u EnvironmentUsage) Empty() bool {
	return u.Versions == 0 && u.Fleets == 0 && u.Nodes == 0 &&
		u.Servers == 0 && u.Matches == 0 && u.APIKeys == 0
}

// DeleteEnvironmentResult — состав удалённого окружения: тело ответа 200
// («что снесли») и payload события environment_deleted.
type DeleteEnvironmentResult struct {
	Name           string `json:"name"`
	Production     bool   `json:"production"`
	Versions       int    `json:"versions"`
	Fleets         int    `json:"fleets"`
	Matches        int    `json:"matches"`
	Servers        int    `json:"servers"`
	APIKeysRevoked int    `json:"api_keys_revoked"`

	// RevokedKeyIDs — id отозванных ключей: httpapi гасит по ним auth-кэш, чтобы
	// ключ умер сразу, а не через authCacheTTL. Не часть тела ответа.
	RevokedKeyIDs []string `json:"-"`
	// WasEmpty — окружение было никогда не использованным (каскад ничего не
	// тронул): API отвечает 204 и не требует confirm (совместимость с v1).
	WasEmpty bool `json:"-"`
}

// environmentUsage считает состав окружения (project уже разрешён в projectID).
func environmentUsage(ctx context.Context, db queryRower, projectID, name string) (EnvironmentUsage, error) {
	var u EnvironmentUsage
	err := db.QueryRow(ctx, `
		select
			(select count(*) from versions      where project_id = $1::uuid and env = $2),
			(select count(*) from fleet_configs where project_id = $1::uuid and env = $2),
			(select count(*) from nodes         where project_id = $1::uuid and env = $2),
			(select count(*) from servers       where project_id = $1::uuid and env = $2),
			(select count(*) from matches       where project_id = $1::uuid and env = $2),
			(select count(*) from api_keys      where project_id = $1::uuid and env = $2 and revoked_at is null)`,
		projectID, name).
		Scan(&u.Versions, &u.Fleets, &u.Nodes, &u.Servers, &u.Matches, &u.APIKeys)
	return u, err
}

// EnvironmentUsage returns what the environment holds (GET .../usage, readonly):
// the panel renders it in the delete dialog and blocks deletion while nodes > 0.
// ErrNotFound for an unknown env (адресуемый ресурс, как PATCH/DELETE — не ссылка).
func (s *Store) EnvironmentUsage(ctx context.Context, project, name string) (EnvironmentUsage, error) {
	var projectID string
	err := s.Pool.QueryRow(ctx, `
		select e.project_id::text from environments e
		join projects p on p.id = e.project_id
		where p.slug = $1 and e.name = $2`, project, name).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentUsage{}, fmt.Errorf("environment %s/%s: %w", project, name, ErrNotFound)
	}
	if err != nil {
		return EnvironmentUsage{}, err
	}
	return environmentUsage(ctx, s.Pool, projectID, name)
}

// DeleteEnvironment removes an environment WITH its content — одной транзакцией
// (DELETE /v1/environments/{project}/{name}; «удаление непустого окружения»).
//
// Предусловие (жёсткое): в окружении НОЛЬ нод. Ноду нельзя молча осиротить —
// живой агент остался бы стучаться с node_id из несуществующего env; nodes > 0 →
// ErrConflict «move them to another environment first» (перевод ноды — PATCH
// /v1/nodes/{id}). Побочно это гарантирует отсутствие живых серверов: guard
// перевода ноды их не пускает, а без нод их неоткуда взять.
//
// Подтверждение: НЕПУСТОЕ окружение (EnvironmentUsage.Empty() == false) удаляется
// только с confirm, ТОЧНО равным имени (без trim — лишний пробел не совпал), иначе
// ErrConfirmRequired (→400). Пустое удаляется как раньше — без confirm (204).
// Проверка и каскад живут в ОДНОЙ транзакции под `for update` строки env, поэтому
// «пустое» не может стать «непустым» между решением и удалением.
//
// Каскад (порядок продиктован FK):
//  1. matches (FK на servers/versions);
//  2. servers (FK на versions; их ноды могли уехать в другой env — I6, история
//     не переписывается, поэтому чистим по КОЛОНКЕ env, а не по нодам);
//  3. fleet_configs: active_version = null, затем delete (составной FK на versions);
//  4. versions: сначала обнуляем promoted_from у версий ДРУГИХ окружений, которые
//     промоутились из этого (self-FK versions.promoted_from иначе не даст удалить
//     строку-источник; сама версия-потомок остаётся жива, теряется лишь provenance);
//  5. match_stats_daily — производные роллапы этого env;
//  6. api_keys, привязанные к (project, env): revoked_at = now() (ключ умирает —
//     AuthAPIKey фильтрует revoked_at is null, «раз-отзыва» в API нет);
//  7. строка environments — последней; событие environment_deleted пишется до неё.
//
// ВАЖНО (расхождение с дизайном, вынужденное схемой): привязку (project_id, env)
// у ключей приходится СНИМАТЬ (оба поля в null) — FK api_keys_env_fk не даст
// удалить строку environments, пока хоть один ключ на неё ссылается (revoked_at
// его не волнует). Дыры в безопасности нет: ключ в той же транзакции ОТОЗВАН, а
// отозванный ключ не аутентифицируется и не воскресает (эндпоинта «раз-отзыв»
// нет; PurgeAPIKey только удаляет строку). Утраченная привязка сохраняется в
// аудите — событие apikey_revoked {project, env, reason: environment_deleted}.
//
// ErrNotFound для неизвестного окружения. dev/prod — обычные строки: ensureProject
// пересевает их только при ВСТАВКЕ проекта, воскрешения не будет.
func (s *Store) DeleteEnvironment(ctx context.Context, project, name, confirm string) (DeleteEnvironmentResult, error) {
	var res DeleteEnvironmentResult
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	var projectID string
	var production bool
	err = tx.QueryRow(ctx, `
		select e.project_id::text, e.production from environments e
		join projects p on p.id = e.project_id
		where p.slug = $1 and e.name = $2 for update of e`, project, name).Scan(&projectID, &production)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("environment %s/%s: %w", project, name, ErrNotFound)
	}
	if err != nil {
		return res, err
	}
	res.Name, res.Production = name, production

	usage, err := environmentUsage(ctx, tx, projectID, name)
	if err != nil {
		return res, err
	}
	// Предусловие: ноды — единственное, что не каскадится. Живой агент с
	// node_id из удалённого окружения — молчаливо осиротевшая машина.
	if usage.Nodes > 0 {
		return res, fmt.Errorf("environment %s/%s has %d node(s) — move them to another environment first: %w",
			project, name, usage.Nodes, ErrConflict)
	}
	res.WasEmpty = usage.Empty()
	if !res.WasEmpty && confirm != name {
		return res, fmt.Errorf("confirm must equal the environment name %q: %w", name, ErrConfirmRequired)
	}

	// (1) матчи → (2) серверы → (3) флоты → (4) версии → (5) роллапы.
	n, err := tx.Exec(ctx, `delete from matches where project_id = $1::uuid and env = $2`, projectID, name)
	if err != nil {
		return res, err
	}
	res.Matches = int(n.RowsAffected())

	n, err = tx.Exec(ctx, `delete from servers where project_id = $1::uuid and env = $2`, projectID, name)
	if err != nil {
		return res, err
	}
	res.Servers = int(n.RowsAffected())

	if _, err := tx.Exec(ctx,
		`update fleet_configs set active_version = null where project_id = $1::uuid and env = $2`,
		projectID, name); err != nil {
		return res, err
	}
	n, err = tx.Exec(ctx, `delete from fleet_configs where project_id = $1::uuid and env = $2`, projectID, name)
	if err != nil {
		return res, err
	}
	res.Fleets = int(n.RowsAffected())

	// Provenance промоутов ИЗ этого окружения (self-FK): версия-потомок в другом
	// env остаётся, но её ссылка на удаляемый источник обнуляется.
	if _, err := tx.Exec(ctx, `
		update versions set promoted_from = null
		where project_id = $1::uuid
		  and promoted_from in (select id from versions where project_id = $1::uuid and env = $2)`,
		projectID, name); err != nil {
		return res, err
	}
	n, err = tx.Exec(ctx, `delete from versions where project_id = $1::uuid and env = $2`, projectID, name)
	if err != nil {
		return res, err
	}
	res.Versions = int(n.RowsAffected())

	// match_stats_daily — платформенная таблица БЕЗ project_id (I5): строки
	// адресуются (day, region, semver, env). Поэтому чистим её только когда имя
	// окружения не занято ДРУГИМ проектом — иначе снесли бы чужие роллапы.
	if _, err := tx.Exec(ctx, `
		delete from match_stats_daily
		where env = $2
		  and not exists (select 1 from environments where name = $2 and project_id <> $1::uuid)`,
		projectID, name); err != nil {
		return res, err
	}

	// (6) Ключи (project, env): отзыв + вынужденное снятие привязки (см. док выше).
	rows, err := tx.Query(ctx, `
		update api_keys set revoked_at = now()
		where project_id = $1::uuid and env = $2 and revoked_at is null
		returning id::text, name`, projectID, name)
	if err != nil {
		return res, err
	}
	type revoked struct{ id, name string }
	var keys []revoked
	for rows.Next() {
		var k revoked
		if err := rows.Scan(&k.id, &k.name); err != nil {
			rows.Close()
			return res, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, k := range keys {
		res.RevokedKeyIDs = append(res.RevokedKeyIDs, k.id)
		if err := insertEvent(ctx, tx, EventAPIKeyRevoked, EventRef{}, map[string]any{
			"key_id": k.id, "name": k.name,
			"project": project, "env": name, "reason": EventEnvironmentDeleted,
		}); err != nil {
			return res, err
		}
	}
	res.APIKeysRevoked = len(keys)
	if _, err := tx.Exec(ctx,
		`update api_keys set project_id = null, env = null where project_id = $1::uuid and env = $2`,
		projectID, name); err != nil {
		return res, err
	}

	// (7) Событие — ДО удаления строки env (одна транзакция, порядок для читателя).
	if err := insertEvent(ctx, tx, EventEnvironmentDeleted, EventRef{}, map[string]any{
		"project": project, "name": res.Name, "production": res.Production,
		"versions": res.Versions, "fleets": res.Fleets, "matches": res.Matches,
		"servers": res.Servers, "api_keys_revoked": res.APIKeysRevoked,
	}); err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx,
		`delete from environments where project_id = $1::uuid and name = $2`, projectID, name); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// SoleEnvWithActiveNodes returns the single environment of a project that has
// active nodes with a fresh heartbeat (<30s) — the matchmaker/allocate env
// fallback when a ticket names none (§3). ErrConflict when zero or several
// environments have live nodes (the request must then name env explicitly).
func (s *Store) SoleEnvWithActiveNodes(ctx context.Context, projectSlug string) (string, error) {
	rows, err := s.Pool.Query(ctx, `
		select distinct n.env
		from nodes n join projects p on p.id = n.project_id
		where p.slug = $1 and n.state = 'active'
		  and n.last_heartbeat_at > now() - interval '30 seconds'
		order by n.env
		limit 2`, projectSlug)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var envs []string
	for rows.Next() {
		var env string
		if err := rows.Scan(&env); err != nil {
			return "", err
		}
		envs = append(envs, env)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(envs) {
	case 0:
		return "", fmt.Errorf("no environment of project %s has active nodes: %w", projectSlug, ErrConflict)
	case 1:
		return envs[0], nil
	default:
		return "", fmt.Errorf("several environments of project %s have active nodes, env is required: %w", projectSlug, ErrConflict)
	}
}
