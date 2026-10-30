package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

type BackfillMatch struct {
	ID, Region, VersionID string
	Free                  int
	PlayerKeys            []string
}

// Called inside the same scheduler transaction as the eventual capacity CAS.
// Draining, terminal, unresponsive and old/manual allocations cannot admit
// new players. Existing players and their reconnect rights are unaffected.
func (t *MatchmakingTx) BackfillMatches(ctx context.Context, project, env, mode string) ([]BackfillMatch, error) {
	rows, err := t.tx.Query(ctx, `select m.id::text,m.region,m.version_id::text,m.max_size-m.reserved_size,
     array(select player_key from matchmaking_roster r where r.match_id=m.id)
	 from matches m join projects p on p.id=m.project_id join servers s on s.id=m.server_id
	 join nodes n on n.id=s.node_id join versions v on v.id=m.version_id
	 where p.slug=$1 and m.env=$2 and m.game_mode=$3 and m.state in ('pending','running')
	 and s.match_id=m.id and s.state='allocated' and n.state='active'
	 and n.last_heartbeat_at>now()-interval '10 seconds' and v.state<>'disabled'
	 and m.max_size>m.reserved_size order by m.created_at,m.id`, project, env, mode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackfillMatch
	for rows.Next() {
		var m BackfillMatch
		if err := rows.Scan(&m.ID, &m.Region, &m.VersionID, &m.Free, &m.PlayerKeys); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (t *MatchmakingTx) Backfill(ctx context.Context, matchID string, players int32, additions map[string]string, playerKeys []string) (Allocation, error) {
	var nodeID, cmdID, envelope string
	var expected int32
	var a Allocation
	err := t.tx.QueryRow(ctx, `select s.id::text,s.node_id::text,host(n.public_ip),s.port,m.reserved_size,o.cmd_id::text,o.metadata
	 from matches m join servers s on s.id=m.server_id join nodes n on n.id=s.node_id join versions v on v.id=m.version_id
	 join allocation_outbox o on o.server_id=s.id and o.match_id=m.id
	 where m.id=$1::uuid and m.state in ('pending','running') and s.match_id=m.id and s.state='allocated'
	 and n.state='active' and n.last_heartbeat_at>now()-interval '10 seconds' and v.state<>'disabled'
	 and $2>0 and m.reserved_size+$2<=m.max_size
     and not exists(select from matchmaking_roster r where r.match_id=m.id and r.player_key=any($3::text[]))
     for update of m,s`, matchID, players, playerKeys).
		Scan(&a.ServerID, &nodeID, &a.Host, &a.Port, &expected, &cmdID, &envelope)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, ErrNoCapacity
	}
	if err != nil {
		return Allocation{}, err
	}
	raw, err := t.st.codec.Decrypt(envelope, "allocation_outbox.metadata:"+cmdID)
	if err != nil {
		return Allocation{}, err
	}
	var metadata map[string]string
	if err = json.Unmarshal(raw, &metadata); err != nil {
		return Allocation{}, errors.New("invalid encrypted allocation metadata")
	}
	for key, value := range additions {
		if old, ok := metadata[key]; ok && old != value {
			return Allocation{}, errors.New("immutable allocation metadata conflict")
		}
		metadata[key] = value
	}
	if len(metadata) > 16 {
		return Allocation{}, ErrNoCapacity
	}
	if _, err = t.tx.Exec(ctx, `update matches set reserved_size=reserved_size+$2 where id=$1::uuid`, matchID, players); err != nil {
		return Allocation{}, err
	}
	if err = t.saveRoster(ctx, matchID, playerKeys); err != nil {
		return Allocation{}, err
	}
	return a, t.queueAllocation(ctx, nodeID, a.ServerID, matchID, expected+players, metadata)
}

func (t *MatchmakingTx) saveRoster(ctx context.Context, matchID string, keys []string) error {
	_, err := t.tx.Exec(ctx, `insert into matchmaking_roster(match_id,player_key) select $1::uuid,unnest($2::text[])`, matchID, keys)
	return err
}
