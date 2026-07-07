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
	_, err = db.Exec(ctx, `
		insert into events (kind, node_id, server_id, match_id, version_id, payload)
		values ($1, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6::jsonb)`,
		kind, ref.NodeID, ref.ServerID, ref.MatchID, ref.VersionID, string(raw))
	return err
}

// InsertEvent writes one audit/feed event (docs/specs/master.md §1 events).
func (s *Store) InsertEvent(ctx context.Context, kind string, ref EventRef, payload map[string]any) error {
	return insertEvent(ctx, s.Pool, kind, ref, payload)
}

// ListEvents returns the newest events, up to limit.
func (s *Store) ListEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.Pool.Query(ctx, `
		select id, ts, kind, node_id::text, server_id::text, match_id::text, version_id::text, payload
		from events order by ts desc, id desc limit $1`, limit)
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

// CountEvents is a small helper for tests and sanity checks.
func (s *Store) CountEvents(ctx context.Context, kind string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `select count(*) from events where kind = $1`, kind).Scan(&n)
	return n, err
}
