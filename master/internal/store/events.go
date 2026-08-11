package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgconn"
)

// execer is satisfied by *pgxpool.Pool and pgx.Tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type EventRef struct {
	NodeID    *string
	ServerID  *string
	MatchID   *string
	VersionID *string
}

func insertEvent(ctx context.Context, db execer, kind string, ref EventRef, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// project_id выводится ЗДЕСЬ, а не передаётся вызывающими (эпик #968 шаг 2).
	// Точка записи одна, вызывающих — два десятка, и любой новый вид события
	// иначе молча приезжал бы платформенным: забыть поле в структуре легко,
	// забыть его в общем INSERT'е невозможно.
	//
	// Источники те же и в том же порядке, что у бэкфилла миграции 000019:
	// ссылки на сущности (факт — project_id лежит в их собственной колонке),
	// затем слаг в payload (его несут события без ссылок вовсе: project_created,
	// environment_deleted, deploy_*, fleet_updated). Ничего не нашлось — NULL,
	// и это законное значение: платформенному событию проект не приписывается.
	_, err = db.Exec(ctx, `
		insert into events (kind, node_id, server_id, match_id, version_id, payload, project_id)
		values ($1, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6::jsonb, coalesce(
		  (select n.project_id from nodes    n where n.id = $2::uuid),
		  (select s.project_id from servers  s where s.id = $3::uuid),
		  (select v.project_id from versions v where v.id = $5::uuid),
		  (select m.project_id from matches  m where m.id = $4::uuid),
		  (select p.id from projects p where p.slug = ($6::jsonb->>'project'))
		))`,
		kind, ref.NodeID, ref.ServerID, ref.MatchID, ref.VersionID, string(raw))
	return err
}

// InsertEvent writes one audit/feed event (docs/specs/master.md §1 events).
func (s *Store) InsertEvent(ctx context.Context, kind string, ref EventRef, payload map[string]any) error {
	return insertEvent(ctx, s.Pool, kind, ref, payload)
}

// ListEvents returns the newest events, up to limit.
// ListEvents returns the newest events, up to limit. Пустой project — вся
// платформа (поведение до мультипроекта).
//
// Сужение НЕ СКРЫВАЮЩЕЕ, как у алертов (#955): уходит только событие ЯВНО
// чужого проекта, а платформенное (project_id is null — бекапы, CA, сессии)
// видно под любым фильтром. Спрятать его значило бы утверждать, что при
// выбранном проекте на платформе ничего не происходит.
func (s *Store) ListEvents(ctx context.Context, limit int, project string) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.Pool.Query(ctx, `
		select e.id, e.ts, e.kind, e.node_id::text, e.server_id::text, e.match_id::text,
		       e.version_id::text, e.payload
		from events e
		where $2 = '' or e.project_id is null
		   or e.project_id = (select p.id from projects p where p.slug = $2)
		order by e.ts desc, e.id desc limit $1`, limit, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.NodeID, &e.ServerID, &e.MatchID, &e.VersionID, &payload); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &e.Payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEventsAfter returns events with id > afterID, oldest first, up to
// limit — the SSE stream cursor read (docs/specs/master.md §6).
func (s *Store) ListEventsAfter(ctx context.Context, afterID int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.Pool.Query(ctx, `
		select id, ts, kind, node_id::text, server_id::text, match_id::text, version_id::text, payload
		from events where id > $1 order by id limit $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.NodeID, &e.ServerID, &e.MatchID, &e.VersionID, &payload); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &e.Payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxEventID returns the current top of the events feed (0 when empty) —
// the default SSE cursor: stream only what happens after connect.
func (s *Store) MaxEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&id)
	return id, err
}

// CountEvents is a small helper for tests and sanity checks.
func (s *Store) CountEvents(ctx context.Context, kind string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `select count(*) from events where kind = $1`, kind).Scan(&n)
	return n, err
}
