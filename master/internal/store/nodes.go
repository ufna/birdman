package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// ErrBadToken is returned when node token authentication fails.
var ErrBadToken = errors.New("bad_token")

// ensureProject returns the project id for slug, creating the project on
// first reference (v0 clarification in docs/specs/master.md §6: no dedicated
// projects endpoint yet).
//
// Сев окружений — ТОЛЬКО при фактическом INSERT проекта (seed-on-insert, w2).
// Раньше dev+prod сеялись при КАЖДОМ касании проекта (`on conflict do nothing`
// в безусловном insert'е), поэтому удалённое оператором окружение dev/prod молча
// воскресало на первом же CreateVersion/UpsertFleet/CreateNode — DELETE
// /v1/environments выглядел работающим, а окружение возвращалось из ниоткуда.
// Детект вставки — `xmax = 0` в RETURNING: у строки, вставленной ЭТИМ
// оператором, xmax нулевой; у обновлённой веткой `do update` — нет.
func ensureProject(ctx context.Context, tx pgx.Tx, slug string) (string, error) {
	if slug == "" {
		return "", fmt.Errorf("project slug is required")
	}
	var id string
	var inserted bool
	err := tx.QueryRow(ctx, `
		insert into projects (slug) values ($1)
		on conflict (slug) do update set slug = excluded.slug
		returning id::text, (xmax = 0) as inserted`, slug).Scan(&id, &inserted)
	if err != nil {
		return "", err
	}
	if !inserted {
		return id, nil // проект уже существовал — его окружения не наши, не трогаем
	}
	// Environments v1: НОВЫЙ проект получает dev+prod (design §1). Это load-bearing —
	// nodes/versions/fleets ссылаются на environments по FK (project_id, env), а новая
	// нода/версия/флот входят как dev, поэтому строка environments dev обязана
	// существовать в этой же tx до их вставки. dev: auto_deploy, keep=20; prod:
	// production, keep=∞. `do nothing` оставлен как страховка от гонки двух
	// параллельных первых касаний проекта.
	if _, err := tx.Exec(ctx, `
		insert into environments (project_id, name, production, auto_deploy, retention_keep)
		values ($1::uuid, 'dev', false, true, 20), ($1::uuid, 'prod', true, false, 0)
		on conflict (project_id, name) do nothing`, id); err != nil {
		return "", err
	}
	return id, nil
}

type CreateNodeParams struct {
	Project       string
	Region        string
	Hostname      string
	PublicIP      string
	CapacitySlots int32
	Labels        map[string]any
	Env           string // окружение регистрации; пусто → "dev" (дефолт колонки)
}

// CreateNode registers a node ahead of agent connection and returns the
// bootstrap node_token — shown exactly once (docs/specs/protocol.md §Auth).
//
// Env необязателен и по умолчанию "dev" — но существование окружения проверяется
// ЯВНО (w2). После seed-on-insert в ensureProject окружение dev у старого проекта
// может быть удалено оператором, и «нода входит как dev» перестало быть
// самоочевидным: без проверки insert падал бы сырым 23503 (nodes_env_fk → 500).
// Теперь это внятный ErrBadEnv → 400 «no such environment <project>/<env>».
func (s *Store) CreateNode(ctx context.Context, p CreateNodeParams) (Node, string, error) {
	if p.Region == "" || p.Hostname == "" || p.PublicIP == "" {
		return Node{}, "", fmt.Errorf("region, hostname and public_ip are required")
	}
	if p.CapacitySlots <= 0 {
		return Node{}, "", fmt.Errorf("capacity_slots must be > 0")
	}
	if p.Labels == nil {
		p.Labels = map[string]any{}
	}
	labels, err := json.Marshal(p.Labels)
	if err != nil {
		return Node{}, "", err
	}
	secret, err := newSecret()
	if err != nil {
		return Node{}, "", err
	}
	hash, err := s.hashSecret(secret)
	if err != nil {
		return Node{}, "", err
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Node{}, "", err
	}
	defer tx.Rollback(ctx)

	projectID, err := ensureProject(ctx, tx, p.Project)
	if err != nil {
		return Node{}, "", err
	}
	env := p.Env
	if env == "" {
		env = "dev"
	}
	// Env обязан существовать у проекта. Проверка внутри tx: для НОВОГО проекта
	// ensureProject засеял dev/prod прямо сейчас (ещё до коммита) — тот же паттерн,
	// что в CreateVersion/UpsertFleet.
	var envExists bool
	if err := tx.QueryRow(ctx,
		`select exists(select 1 from environments where project_id = $1::uuid and name = $2)`,
		projectID, env).Scan(&envExists); err != nil {
		return Node{}, "", err
	}
	if !envExists {
		return Node{}, "", badEnvErr(p.Project, env)
	}
	var n Node
	err = tx.QueryRow(ctx, `
		insert into nodes (project_id, region, hostname, public_ip, capacity_slots, labels, token_hash, env)
		values ($1::uuid, $2, $3, $4::inet, $5, $6::jsonb, $7, $8)
		returning id::text, project_id::text, region, env, hostname, host(public_ip),
		          capacity_slots, agent_version, state, created_at`,
		projectID, p.Region, p.Hostname, p.PublicIP, p.CapacitySlots, string(labels), hash, env).
		Scan(&n.ID, &n.ProjectID, &n.Region, &n.Env, &n.Hostname, &n.PublicIP,
			&n.CapacitySlots, &n.AgentVersion, &n.State, &n.CreatedAt)
	// Гонка с DELETE env между in-tx пре-чеком и insert'ом: nodes_env_fk (23503) →
	// внятный ErrBadEnv (400), а не сырой 500 (паритет CreateVersion, w5).
	if mapped := mapEnvFKViolation(err, p.Project, env); mapped != nil {
		return Node{}, "", mapped
	}
	if err != nil {
		return Node{}, "", err
	}
	n.Project = p.Project
	n.Labels = p.Labels
	if err := insertEvent(ctx, tx, EventNodeCreated, EventRef{NodeID: &n.ID},
		map[string]any{"region": n.Region, "hostname": n.Hostname}); err != nil {
		return Node{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, "", err
	}
	return n, composeToken(nodeTokenPrefix, n.ID, secret), nil
}

// AuthNodeToken authenticates an agent Hello (v0: node_token on every
// connect; token→mTLS cert exchange is a later-iteration TODO).
func (s *Store) AuthNodeToken(ctx context.Context, token string) (Node, error) {
	id, secret, err := parseToken(nodeTokenPrefix, token)
	if err != nil {
		return Node{}, ErrBadToken
	}
	var n Node
	var hash string
	err = s.Pool.QueryRow(ctx, `
		select id::text, project_id::text, region, hostname, host(public_ip),
		       capacity_slots, agent_version, state, token_hash
		from nodes where id = $1::uuid`, id).
		Scan(&n.ID, &n.ProjectID, &n.Region, &n.Hostname, &n.PublicIP,
			&n.CapacitySlots, &n.AgentVersion, &n.State, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, ErrBadToken
	}
	if err != nil {
		return Node{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)) != nil {
		return Node{}, ErrBadToken
	}
	return n, nil
}

// GetNode returns one node with its project slug.
func (s *Store) GetNode(ctx context.Context, id string) (Node, error) {
	var n Node
	var labels []byte
	err := s.Pool.QueryRow(ctx, `
		select n.id::text, n.project_id::text, p.slug, n.region, n.env, n.hostname, host(n.public_ip),
		       n.capacity_slots, n.agent_version, n.state, n.last_heartbeat_at, n.labels, n.created_at,
		       n.cert_serial, n.cert_not_after, n.enrolled_at
		from nodes n join projects p on p.id = n.project_id
		where n.id = $1::uuid`, id).
		Scan(&n.ID, &n.ProjectID, &n.Project, &n.Region, &n.Env, &n.Hostname, &n.PublicIP,
			&n.CapacitySlots, &n.AgentVersion, &n.State, &n.LastHeartbeatAt, &labels, &n.CreatedAt,
			&n.CertSerial, &n.CertNotAfter, &n.EnrolledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, err
	}
	if len(labels) > 0 {
		_ = json.Unmarshal(labels, &n.Labels)
	}
	return n, nil
}

// RevokeNode retires a node for good (POST /v1/nodes/{id}/revoke): state → `dead`.
//
// Это ЕДИНСТВЕННЫЙ путь в `dead` — автоматика туда не переводит никогда, она
// доводит молчащую ноду только до `down` (MarkDownNodes). Ревокация означает
// «бокса больше нет, перестань за ним следить», и у неё три следствия, ради
// которых она и нужна:
//   - окружение перестаёт блокировать sweep снятия образов: его выборка смотрит
//     `n.state = 'down'` (versions.go), `dead` туда не попадает, и живые ноды
//     окружения снова чистят диски;
//   - метрики мёртвой ноды больше не эмитятся (`state <> 'dead'` в выборках
//     heartbeat-age и cert-expiry), поэтому NodeDown/CertExpiry перестают гореть
//     вечно и не маскируют настоящий отказ;
//   - панель убирает ноду из списка по умолчанию.
//
// Строку НЕ удаляем: `servers.node_id references nodes(id)`, каскад унёс бы
// историю матчей вместе с нодой.
//
// Предусловие — нет живых серверов (creating/ready/allocated/draining), иначе
// ErrConflict: ревокация ноды с живым матчем оборвала бы игру, для вывода живой
// ноды есть drain. Идемпотентна: повтор на `dead` возвращает ноду без второго
// события (панель может отправить дважды, ретрай не должен плодить историю).
//
// Воскрешения бояться не нужно: Hello поднимает состояние только из
// `quarantine|down` (heartbeat.go), `dead` он не трогает.
func (s *Store) RevokeNode(ctx context.Context, nodeID string) (Node, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Node{}, err
	}
	defer tx.Rollback(ctx)

	var cur, hostname, region, env string
	err = tx.QueryRow(ctx, `
		select state, hostname, region, env from nodes where id = $1::uuid for update`,
		nodeID).Scan(&cur, &hostname, &region, &env)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, fmt.Errorf("node %s: %w", nodeID, ErrNotFound)
	}
	if err != nil {
		return Node{}, err
	}
	if cur == "dead" {
		return s.GetNode(ctx, nodeID)
	}

	var live bool
	if err := tx.QueryRow(ctx, `
		select exists(select 1 from servers
		              where node_id = $1::uuid
		                and state in ('creating','ready','allocated','draining'))`,
		nodeID).Scan(&live); err != nil {
		return Node{}, err
	}
	if live {
		return Node{}, fmt.Errorf("node %s has live servers, drain it first: %w", nodeID, ErrConflict)
	}

	if _, err := tx.Exec(ctx, `update nodes set state = 'dead' where id = $1::uuid`, nodeID); err != nil {
		return Node{}, err
	}
	if err := insertEvent(ctx, tx, EventNodeRevoked, EventRef{NodeID: &nodeID},
		map[string]any{"from": cur, "hostname": hostname, "region": region, "env": env}); err != nil {
		return Node{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, err
	}
	return s.GetNode(ctx, nodeID)
}

// DrainNode marks a node draining (итерация 4, docs/specs/master.md §6): the
// reconcile loop stops placing new servers on it and reaps its ready buffer,
// while allocated servers play their matches out. Idempotent; a node_drain
// event is emitted only on the active→draining transition. Dead nodes cannot
// be drained.
func (s *Store) DrainNode(ctx context.Context, id string) (Node, error) {
	return s.setNodeDrain(ctx, id, true)
}

// UndrainNode lifts a node drain (draining→active); idempotent, node_undrain
// event only on an actual transition.
func (s *Store) UndrainNode(ctx context.Context, id string) (Node, error) {
	return s.setNodeDrain(ctx, id, false)
}

func (s *Store) setNodeDrain(ctx context.Context, id string, drain bool) (Node, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Node{}, err
	}
	defer tx.Rollback(ctx)

	var cur string
	err = tx.QueryRow(ctx, `select state from nodes where id = $1::uuid for update`, id).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, err
	}

	target, kind := cur, ""
	if drain {
		if cur == "dead" {
			return Node{}, fmt.Errorf("cannot drain a dead node")
		}
		target, kind = "draining", EventNodeDrain
	} else if cur == "draining" {
		target, kind = "active", EventNodeUndrain
	}

	if target != cur {
		if _, err := tx.Exec(ctx, `update nodes set state = $2 where id = $1::uuid`, id, target); err != nil {
			return Node{}, err
		}
		if err := insertEvent(ctx, tx, kind, EventRef{NodeID: &id},
			map[string]any{"from": cur, "to": target}); err != nil {
			return Node{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, err
	}
	return s.GetNode(ctx, id)
}

// SetNodeEnv moves a node to another environment (PATCH /v1/nodes/{id} {env},
// environments v1 §2). Allowed in ANY state except `dead`, and only when the
// node carries no live servers (state in creating/ready/allocated/draining) —
// otherwise ErrConflict. The target env must exist for the node's project.
// Emits node_env_changed {from, to} on an actual change; idempotent when the
// env is unchanged. A moved node does NOT rewrite history — servers/matches keep
// their own env column (invariant I6).
func (s *Store) SetNodeEnv(ctx context.Context, nodeID, env string) (Node, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Node{}, err
	}
	defer tx.Rollback(ctx)

	var projectID, project, cur, state string
	err = tx.QueryRow(ctx, `
		select n.project_id::text, p.slug, n.env, n.state
		from nodes n join projects p on p.id = n.project_id
		where n.id = $1::uuid for update of n`, nodeID).
		Scan(&projectID, &project, &cur, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, fmt.Errorf("node %s: %w", nodeID, ErrNotFound)
	}
	if err != nil {
		return Node{}, err
	}
	if state == "dead" {
		return Node{}, fmt.Errorf("cannot change env of a dead node %s: %w", nodeID, ErrConflict)
	}

	// Target env must exist for this project (nodes_env_fk would also reject the
	// write, but this gives a clean 400 instead of a 500). ErrBadEnv, не ErrNotFound
	// (v3): целевой env — это ССЫЛКА в теле PATCH'а, опечатка в ней — плохой ввод
	// (400), тогда как несуществующая НОДА (адресуемый ресурс) остаётся 404 выше.
	var envExists bool
	if err := tx.QueryRow(ctx,
		`select exists(select 1 from environments where project_id = $1::uuid and name = $2)`,
		projectID, env).Scan(&envExists); err != nil {
		return Node{}, err
	}
	if !envExists {
		return Node{}, badEnvErr(project, env)
	}

	// A node with live servers cannot be moved — its running dedics belong to
	// the current env (drain first). reaped/failed servers do not block.
	var live bool
	if err := tx.QueryRow(ctx, `
		select exists(select 1 from servers
		              where node_id = $1::uuid
		                and state in ('creating','ready','allocated','draining'))`,
		nodeID).Scan(&live); err != nil {
		return Node{}, err
	}
	if live {
		return Node{}, fmt.Errorf("node %s has live servers, drain it first: %w", nodeID, ErrConflict)
	}

	if env != cur {
		if _, err := tx.Exec(ctx, `update nodes set env = $2 where id = $1::uuid`, nodeID, env); err != nil {
			// Гонка с DELETE целевого env между in-tx пре-чеком и UPDATE'ом:
			// nodes_env_fk (23503) → внятный «no such environment» (ErrBadEnv →
			// 400), а не сырой 500 (w5, паритет CreateVersion/PromoteVersion).
			if mapped := mapEnvFKViolation(err, project, env); mapped != nil {
				return Node{}, mapped
			}
			return Node{}, err
		}
		if err := insertEvent(ctx, tx, EventNodeEnvChanged, EventRef{NodeID: &nodeID},
			map[string]any{"from": cur, "to": env}); err != nil {
			return Node{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, err
	}
	return s.GetNode(ctx, nodeID)
}

// EnvNodeIDs returns the ids of every not-`dead` node of (projectID, env) — the
// RemoveImage dispatch targets (environments v1 §6б). All non-dead states are
// included (creating/ready/allocated/draining/quarantine/down and offline ones):
// the at-least-once agentlink queue replays the command when the node reconnects,
// so an offline node still gets its image retired. A `dead` node's agent is
// revoked and will never reconnect, so it is skipped (its queue would never
// drain). Env-скоуп: ноды принадлежат ровно одному env — ноды чужого env не
// трогаются.
func (s *Store) EnvNodeIDs(ctx context.Context, projectID, env string) ([]string, error) {
	rows, err := s.Pool.Query(ctx,
		`select id::text from nodes
		 where project_id = $1::uuid and env = $2 and state <> 'dead'
		 order by created_at`, projectID, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// NodeFilter narrows ListNodes; a zero value means "everything" (пустое поле —
// отсутствие условия, как в ServerFilter).
type NodeFilter struct {
	Project string // слаг проекта (мультипроект W2)
	Env     string // окружение ноды (environments v1)
}

// ListNodes returns nodes with project slugs, oldest first, optionally
// narrowed by NodeFilter.
func (s *Store) ListNodes(ctx context.Context, f NodeFilter) ([]Node, error) {
	q := `
		select n.id::text, n.project_id::text, p.slug, n.region, n.env, n.hostname, host(n.public_ip),
		       n.capacity_slots, n.agent_version, n.state, n.last_heartbeat_at, n.labels, n.created_at,
		       n.cert_serial, n.cert_not_after, n.enrolled_at
		from nodes n join projects p on p.id = n.project_id`
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.Project != "" {
		add("p.slug = $%d", f.Project)
	}
	if f.Env != "" {
		add("n.env = $%d", f.Env)
	}
	if len(conds) > 0 {
		q += " where " + strings.Join(conds, " and ")
	}
	q += " order by n.created_at"

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var labels []byte
		if err := rows.Scan(&n.ID, &n.ProjectID, &n.Project, &n.Region, &n.Env, &n.Hostname, &n.PublicIP,
			&n.CapacitySlots, &n.AgentVersion, &n.State, &n.LastHeartbeatAt, &labels, &n.CreatedAt,
			&n.CertSerial, &n.CertNotAfter, &n.EnrolledAt); err != nil {
			return nil, err
		}
		if len(labels) > 0 {
			_ = json.Unmarshal(labels, &n.Labels)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
