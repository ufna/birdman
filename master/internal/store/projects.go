package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

type Project struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	MatchSize int32     `json:"match_size"`
	CreatedAt time.Time `json:"created_at"`
}

// projectSlugRe — тот же класс имён, что у окружений: слаг попадает в URL,
// в тела матчмейкинг-запросов и в CI, поэтому регистр и пробелы исключены.
var projectSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validateProjectSlug(slug string) error {
	if !projectSlugRe.MatchString(slug) {
		return fmt.Errorf("project slug %q must match ^[a-z0-9][a-z0-9-]{0,31}$ (lowercase letters, digits and dashes, 1–32 chars, not starting with a dash)", slug)
	}
	return nil
}

// ProjectUsage — что стоит за проектом. Показывается ДО удаления: оператор
// должен видеть, что именно исчезнет, а не подтверждать вслепую.
type ProjectUsage struct {
	Environments int `json:"environments"`
	Versions     int `json:"versions"`
	Fleets       int `json:"fleets"`
	// Nodes — ЖИВЫЕ ноды: они блокируют удаление (осиротевший агент недопустим).
	Nodes int `json:"nodes"`
	// RetiredNodes — выведенные (dead). Удалению НЕ мешают: их бокса уже нет и
	// они уезжают каскадом. Но проект с ними не «пустой»: вместе с ним исчезнет
	// история этих нод, и подтверждение вводом слага обязано спрашиваться.
	RetiredNodes int `json:"retired_nodes"`
	Matches      int `json:"matches"`
	Servers      int `json:"servers"`
	APIKeys      int `json:"api_keys"`
}

func (u ProjectUsage) Empty() bool {
	return u.Versions == 0 && u.Fleets == 0 && u.Nodes == 0 && u.RetiredNodes == 0 &&
		u.Matches == 0 && u.Servers == 0 && u.APIKeys == 0
}

// DeleteProjectResult — состав снесённого, для события и тоста панели.
type DeleteProjectResult struct {
	Slug           string   `json:"slug"`
	WasEmpty       bool     `json:"was_empty"`
	Environments   int      `json:"environments"`
	Versions       int      `json:"versions"`
	Fleets         int      `json:"fleets"`
	Matches        int      `json:"matches"`
	Servers        int      `json:"servers"`
	APIKeysRevoked int      `json:"api_keys_revoked"`
	RevokedKeyIDs  []string `json:"revoked_key_ids,omitempty"`
}

// CreateProject заводит проект ЯВНО (POST /v1/projects) — в отличие от
// ensureProject, который создаёт его молча как побочный эффект регистрации ноды
// или версии. Разница принципиальна для UI: `PUT /v1/projects/{slug}`
// идемпотентен и на опечатке в имени тихо перезаписал бы match_size ЧУЖОГО
// проекта, а форма «создать» обязана сказать «такой уже есть» (ErrConflict).
//
// Окружения dev+prod засеиваются тем же ensureProject — новый проект сразу
// пригоден к работе, как и при неявном создании.
func (s *Store) CreateProject(ctx context.Context, slug string, matchSize int32) (Project, error) {
	if err := validateProjectSlug(slug); err != nil {
		return Project{}, err
	}
	if matchSize < 1 {
		return Project{}, fmt.Errorf("match_size must be >= 1")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx,
		`select exists(select 1 from projects where slug = $1)`, slug).Scan(&exists); err != nil {
		return Project{}, err
	}
	if exists {
		return Project{}, fmt.Errorf("project %q already exists: %w", slug, ErrConflict)
	}

	projectID, err := ensureProject(ctx, tx, slug)
	if err != nil {
		return Project{}, err
	}
	var p Project
	if err := tx.QueryRow(ctx, `
		update projects set match_size = $2 where id = $1::uuid
		returning id::text, slug, match_size, created_at`, projectID, matchSize).
		Scan(&p.ID, &p.Slug, &p.MatchSize, &p.CreatedAt); err != nil {
		return Project{}, err
	}
	if err := insertEvent(ctx, tx, EventProjectCreated, EventRef{},
		map[string]any{"project": slug, "match_size": matchSize}); err != nil {
		return Project{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Project{}, err
	}
	return p, nil
}

// ProjectUsage считает состав проекта — ровно то, что снесёт DeleteProject
// (плюс ноды, которые он снести откажется).
func (s *Store) ProjectUsage(ctx context.Context, slug string) (ProjectUsage, error) {
	var u ProjectUsage
	var projectID string
	err := s.Pool.QueryRow(ctx, `select id::text from projects where slug = $1`, slug).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, fmt.Errorf("project %s: %w", slug, ErrNotFound)
	}
	if err != nil {
		return u, err
	}
	return projectUsage(ctx, s.Pool, projectID)
}

// projectUsage — тот же счёт, но по переданному исполнителю: DeleteProject
// обязан читать состав ВНУТРИ своей транзакции, под уже взятым локом строки
// проекта. Иначе между проверкой «нод ноль» и каскадом успела бы вклиниться
// регистрация ноды, и она осиротела бы вместе с проектом.
func projectUsage(ctx context.Context, db queryRower, projectID string) (ProjectUsage, error) {
	var u ProjectUsage
	err := db.QueryRow(ctx, `
		select (select count(*) from environments where project_id = $1::uuid),
		       (select count(*) from versions     where project_id = $1::uuid),
		       (select count(*) from fleet_configs where project_id = $1::uuid),
		       (select count(*) from nodes        where project_id = $1::uuid and state <> 'dead'),
		       (select count(*) from nodes        where project_id = $1::uuid and state =  'dead'),
		       (select count(*) from matches      where project_id = $1::uuid),
		       (select count(*) from servers      where project_id = $1::uuid),
		       (select count(*) from api_keys     where project_id = $1::uuid and revoked_at is null)`,
		projectID).Scan(&u.Environments, &u.Versions, &u.Fleets, &u.Nodes, &u.RetiredNodes,
		&u.Matches, &u.Servers, &u.APIKeys)
	return u, err
}

// DeleteProject сносит проект вместе с содержимым (DELETE /v1/projects/{slug}).
//
// Предусловие — НОЛЬ живых нод во всём проекте (как у окружения, уровнем выше):
// живой агент с node_id несуществующего проекта — молчаливо осиротевшая машина.
// Выведенные (`dead`) ноды предусловию не мешают: их бокса уже нет, и держать
// из-за них проект вечно неудаляемым нет причин — они уезжают каскадом.
//
// Непустой проект требует подтверждения вводом слага (ErrConfirmRequired):
// удаление необратимо и уносит ключи, которыми ходит CI.
func (s *Store) DeleteProject(ctx context.Context, slug, confirm string) (DeleteProjectResult, error) {
	var res DeleteProjectResult
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	var projectID string
	err = tx.QueryRow(ctx, `select id::text from projects where slug = $1 for update`, slug).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("project %s: %w", slug, ErrNotFound)
	}
	if err != nil {
		return res, err
	}
	res.Slug = slug

	usage, err := projectUsage(ctx, tx, projectID)
	if err != nil {
		return res, err
	}
	if usage.Nodes > 0 {
		return res, fmt.Errorf("project %s has %d live node(s) — retire or move them first: %w",
			slug, usage.Nodes, ErrConflict)
	}
	res.WasEmpty = usage.Empty()
	if !res.WasEmpty && confirm != slug {
		return res, fmt.Errorf("confirm must equal the project slug %q: %w", slug, ErrConfirmRequired)
	}

	// Порядок как в каскаде окружения, только по project_id: матчи → серверы →
	// флоты → версии → роллапы → ключи → ноды(dead) → окружения → сам проект.
	n, err := tx.Exec(ctx, `delete from matches where project_id = $1::uuid`, projectID)
	if err != nil {
		return res, err
	}
	res.Matches = int(n.RowsAffected())

	n, err = tx.Exec(ctx, `delete from servers where project_id = $1::uuid`, projectID)
	if err != nil {
		return res, err
	}
	res.Servers = int(n.RowsAffected())

	if _, err := tx.Exec(ctx,
		`update fleet_configs set active_version = null where project_id = $1::uuid`, projectID); err != nil {
		return res, err
	}
	n, err = tx.Exec(ctx, `delete from fleet_configs where project_id = $1::uuid`, projectID)
	if err != nil {
		return res, err
	}
	res.Fleets = int(n.RowsAffected())

	// Provenance промоутов ИЗ этого проекта (self-FK) — версии других проектов
	// остаются, но ссылка на исчезающий источник обнуляется.
	if _, err := tx.Exec(ctx, `
		update versions set promoted_from = null
		where promoted_from in (select id from versions where project_id = $1::uuid)`, projectID); err != nil {
		return res, err
	}
	n, err = tx.Exec(ctx, `delete from versions where project_id = $1::uuid`, projectID)
	if err != nil {
		return res, err
	}
	res.Versions = int(n.RowsAffected())

	// match_stats_daily не несёт project_id (инвариант I5): строки адресуются
	// (day, region, semver, env). Чистим только те имена окружений, которых нет
	// у ДРУГИХ проектов — иначе снесли бы чужие роллапы.
	if _, err := tx.Exec(ctx, `
		delete from match_stats_daily
		where env in (select name from environments where project_id = $1::uuid)
		  and not exists (
		    select 1 from environments e2
		    where e2.name = match_stats_daily.env and e2.project_id <> $1::uuid)`, projectID); err != nil {
		return res, err
	}

	rows, err := tx.Query(ctx, `
		update api_keys set revoked_at = now()
		where project_id = $1::uuid and revoked_at is null
		returning id::text, name`, projectID)
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
			"key_id": k.id, "name": k.name, "project": slug, "reason": EventProjectDeleted,
		}); err != nil {
			return res, err
		}
	}
	res.APIKeysRevoked = len(keys)
	if _, err := tx.Exec(ctx,
		`update api_keys set project_id = null, env = null where project_id = $1::uuid`, projectID); err != nil {
		return res, err
	}

	// Выведенные ноды: живых здесь уже нет (предусловие выше), а dead уезжают с
	// проектом — иначе строка ноды пережила бы свой проект осиротевшей.
	if _, err := tx.Exec(ctx, `delete from nodes where project_id = $1::uuid`, projectID); err != nil {
		return res, err
	}

	n, err = tx.Exec(ctx, `delete from environments where project_id = $1::uuid`, projectID)
	if err != nil {
		return res, err
	}
	res.Environments = int(n.RowsAffected())

	// Событие — ДО удаления строки проекта: у события нет FK на projects, но
	// порядок читается как «сначала объяснили, потом сделали».
	if err := insertEvent(ctx, tx, EventProjectDeleted, EventRef{}, map[string]any{
		"project": slug, "environments": res.Environments, "versions": res.Versions,
		"fleets": res.Fleets, "matches": res.Matches, "servers": res.Servers,
		"api_keys_revoked": res.APIKeysRevoked,
	}); err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx, `delete from projects where id = $1::uuid`, projectID); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// ListProjects returns every project, oldest first — what the panel's project
// selector is built from (мультипроект W1). Sorted by (created_at, slug): the
// timestamp alone is not a total order, and the panel's DEFAULT choice is
// literally "the first row", so a tie would make that default flip between
// reloads. Ties are reachable without any single transaction creating two
// projects (no path does): `now()` is per-transaction, and two projects
// registered concurrently — or restored from a dump — can land on the same
// microsecond. The slug tiebreak makes the order total either way.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.Pool.Query(ctx, `
		select id::text, slug, match_size, created_at
		from projects order by created_at, slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Slug, &p.MatchSize, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProject returns a project by slug (ErrNotFound when missing).
func (s *Store) GetProject(ctx context.Context, slug string) (Project, error) {
	var p Project
	err := s.Pool.QueryRow(ctx, `
		select id::text, slug, match_size, created_at
		from projects where slug = $1`, slug).
		Scan(&p.ID, &p.Slug, &p.MatchSize, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, fmt.Errorf("project %q: %w", slug, ErrNotFound)
	}
	return p, err
}

// SetProjectMatchSize upserts the project (implicit creation on first
// reference, v0 convention) and sets its match_size
// (PUT /v1/projects/{slug}, docs/specs/master.md §4).
func (s *Store) SetProjectMatchSize(ctx context.Context, slug string, matchSize int32) (Project, error) {
	if matchSize < 1 {
		return Project{}, fmt.Errorf("match_size must be >= 1")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback(ctx)
	projectID, err := ensureProject(ctx, tx, slug)
	if err != nil {
		return Project{}, err
	}
	var p Project
	err = tx.QueryRow(ctx, `
		update projects set match_size = $2 where id = $1::uuid
		returning id::text, slug, match_size, created_at`,
		projectID, matchSize).
		Scan(&p.ID, &p.Slug, &p.MatchSize, &p.CreatedAt)
	if err != nil {
		return Project{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Project{}, err
	}
	return p, nil
}

// SoleProjectSlug returns the slug of the only existing project — the v0
// default when a matchmaking ticket does not name one. ErrNotFound when no
// project exists yet, ErrConflict when several do (the ticket must then name
// its project explicitly).
func (s *Store) SoleProjectSlug(ctx context.Context) (string, error) {
	rows, err := s.Pool.Query(ctx, `select slug from projects order by created_at limit 2`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return "", err
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(slugs) {
	case 0:
		return "", fmt.Errorf("no projects: %w", ErrNotFound)
	case 1:
		return slugs[0], nil
	default:
		return "", fmt.Errorf("several projects exist, ticket must name one: %w", ErrConflict)
	}
}
