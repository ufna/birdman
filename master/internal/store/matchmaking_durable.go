package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// TicketRecord keeps its opaque body encrypted; the store never interprets
// application payloads or depends on the matchmaker package.
type TicketRecord struct {
	ID, Project, Env, RequestID, PartyID, Status string
	PartyRevision                                int64
	CreatedAt                                    time.Time
	Body                                         []byte
}

type MatchmakingTx struct {
	st *Store
	tx pgx.Tx
}

// One scheduler is active, but the transaction lock also fences concurrent
// API/tick instances during restart handover. A claim and all ticket results
// commit with the allocation command in the same transaction.
func (s *Store) BeginMatchmaking(ctx context.Context) (*MatchmakingTx, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `select pg_advisory_xact_lock(7066344377805153)`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return &MatchmakingTx{st: s, tx: tx}, nil
}

func (t *MatchmakingTx) Commit(ctx context.Context) error { return t.tx.Commit(ctx) }
func (t *MatchmakingTx) Rollback(ctx context.Context)     { _ = t.tx.Rollback(ctx) }

const ticketColumns = `t.id::text,p.slug,t.env,coalesce(t.request_id::text,''),coalesce(t.party_id::text,''),t.party_revision,t.status,t.created_at,t.body`

func (t *MatchmakingTx) tickets(ctx context.Context, predicate string, args ...any) ([]TicketRecord, error) {
	rows, err := t.tx.Query(ctx, `select `+ticketColumns+` from matchmaking_tickets t join projects p on p.id=t.project_id where `+predicate, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TicketRecord
	for rows.Next() {
		var r TicketRecord
		var envelope string
		if err := rows.Scan(&r.ID, &r.Project, &r.Env, &r.RequestID, &r.PartyID, &r.PartyRevision, &r.Status, &r.CreatedAt, &envelope); err != nil {
			return nil, err
		}
		r.Body, err = t.st.codec.Decrypt(envelope, "matchmaking_tickets.body:"+r.ID)
		if err != nil {
			return nil, fmt.Errorf("read encrypted matchmaking ticket: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (t *MatchmakingTx) Load(ctx context.Context, retention time.Duration) ([]TicketRecord, error) {
	return t.tickets(ctx, `t.status='queued' or t.updated_at>now()-$1::interval`, fmt.Sprintf("%f seconds", retention.Seconds()))
}

func (t *MatchmakingTx) Find(ctx context.Context, id string) ([]TicketRecord, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, nil
	}
	return t.tickets(ctx, `t.id=$1::uuid`, id)
}

func (t *MatchmakingTx) FindRequest(ctx context.Context, project, env, request string) ([]TicketRecord, error) {
	return t.tickets(ctx, `p.slug=$1 and t.env=$2 and t.request_id=$3::uuid`, project, env, request)
}

func (t *MatchmakingTx) LatestParty(ctx context.Context, project, env, party string) ([]TicketRecord, error) {
	return t.tickets(ctx, `p.slug=$1 and t.env=$2 and t.party_id=$3::uuid order by t.party_revision desc limit 1`, project, env, party)
}

func (t *MatchmakingTx) Save(ctx context.Context, records []TicketRecord) error {
	for _, r := range records {
		body, err := t.st.codec.Encrypt(r.Body, "matchmaking_tickets.body:"+r.ID)
		if err != nil {
			return err
		}
		_, err = t.tx.Exec(ctx, `insert into matchmaking_tickets(id,project_id,env,request_id,party_id,party_revision,status,body,created_at)
		 select $1::uuid,p.id,$3,nullif($4,'')::uuid,nullif($5,'')::uuid,$6,$7,$8,$9 from projects p where p.slug=$2
		 on conflict(id) do update set status=excluded.status,body=excluded.body,updated_at=now()`, r.ID, r.Project, r.Env, r.RequestID, r.PartyID, r.PartyRevision, r.Status, body, r.CreatedAt)
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *MatchmakingTx) Allocate(ctx context.Context, project, env, region, versionID, matchID, mode string, players, capacity int32, metadata map[string]string, playerKeys []string) (Allocation, error) {
	var projectID string
	if err := t.tx.QueryRow(ctx, `select id::text from projects where slug=$1`, project).Scan(&projectID); err != nil {
		return Allocation{}, err
	}
	var serverID, nodeID string
	var a Allocation
	err := t.tx.QueryRow(ctx, claimSQL, projectID, versionID, region, matchID, env).Scan(&serverID, &nodeID, &a.Port)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, ErrNoCapacity
	}
	if err != nil {
		return Allocation{}, err
	}
	a.ServerID = serverID
	if err = t.tx.QueryRow(ctx, `select host(public_ip) from nodes where id=$1::uuid`, nodeID).Scan(&a.Host); err != nil {
		return Allocation{}, err
	}
	if _, err = t.tx.Exec(ctx, `insert into matches(id,project_id,server_id,version_id,region,env,game_mode,reserved_size,max_size) values($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5,$6,$7,$8,$9)`, matchID, projectID, serverID, versionID, region, env, mode, players, capacity); err != nil {
		return Allocation{}, err
	}
	if err = t.saveRoster(ctx, matchID, playerKeys); err != nil {
		return Allocation{}, err
	}
	return a, t.queueAllocation(ctx, nodeID, serverID, matchID, players, metadata)
}

func (t *MatchmakingTx) queueAllocation(ctx context.Context, nodeID, serverID, matchID string, players int32, metadata map[string]string) error {
	cmdID := uuid.NewString()
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	envelope, err := t.st.codec.Encrypt(raw, "allocation_outbox.metadata:"+cmdID)
	if err != nil {
		return err
	}
	// One cumulative snapshot per server. A new command ID fences old ACKs;
	// stale in-flight envelopes cannot remove members from the game's roster.
	_, err = t.tx.Exec(ctx, `insert into allocation_outbox(cmd_id,node_id,server_id,match_id,players_expected,metadata) values($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5,$6)
	 on conflict(server_id) do update set cmd_id=excluded.cmd_id,match_id=excluded.match_id,players_expected=excluded.players_expected,metadata=excluded.metadata,last_sent_at=null,acked_at=null`, cmdID, nodeID, serverID, matchID, players, envelope)
	return err
}

type durableCommandSender interface {
	SendDurable(nodeID, cmdID string, msg *agentlinkv1.MasterMsg) string
}

// DispatchAllocations retries the same command id until an authenticated
// agent ACKs it. The hub and agent both deduplicate this id; a master restart
// can lose the hub without losing the command or its private metadata.
func (s *Store) DispatchAllocations(ctx context.Context) error {
	if s.sender == nil {
		return nil
	}
	rows, err := s.Pool.Query(ctx, `update allocation_outbox o set last_sent_at=now() from servers s
	 where o.server_id=s.id and o.acked_at is null and s.state in ('allocated','draining')
	 and (o.last_sent_at is null or o.last_sent_at<now()-interval '5 seconds')
	 returning o.cmd_id::text,o.node_id::text,o.server_id::text,o.match_id::text,o.players_expected,o.metadata`)
	if err != nil {
		return err
	}
	type command struct {
		cmdID, nodeID, serverID, matchID, envelope string
		players                                    int32
	}
	var commands []command
	for rows.Next() {
		var c command
		if err = rows.Scan(&c.cmdID, &c.nodeID, &c.serverID, &c.matchID, &c.players, &c.envelope); err != nil {
			rows.Close()
			return err
		}
		commands = append(commands, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range commands {
		raw, err := s.codec.Decrypt(c.envelope, "allocation_outbox.metadata:"+c.cmdID)
		if err != nil {
			return err
		}
		var metadata map[string]string
		if err = json.Unmarshal(raw, &metadata); err != nil {
			return errors.New("invalid encrypted allocation metadata")
		}
		msg := &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Allocate{Allocate: &agentlinkv1.AllocateServer{CmdId: c.cmdID, ServerId: c.serverID, MatchId: c.matchID, PlayersExpected: c.players, Metadata: metadata}}}
		if sender, ok := s.sender.(durableCommandSender); ok {
			sender.SendDurable(c.nodeID, c.cmdID, msg)
		} else {
			s.sender.Send(c.nodeID, msg)
		}
	}
	return nil
}

func (s *Store) AckAllocation(ctx context.Context, nodeID, cmdID string) error {
	_, err := s.Pool.Exec(ctx, `update allocation_outbox set acked_at=coalesce(acked_at,now()) where cmd_id=$1::uuid and node_id=$2::uuid`, cmdID, nodeID)
	return err
}

// An ACK only proves the agent accepted the command. Its cached UDS frame can
// still be lost if the agent restarts before the game attaches; re-arm active
// allocations on every authenticated Hello so that window is recoverable too.
func (s *Store) RearmAllocations(ctx context.Context, nodeID string) error {
	_, err := s.Pool.Exec(ctx, `update allocation_outbox o set acked_at=null,last_sent_at=null from servers s where o.server_id=s.id and o.node_id=$1::uuid and s.state in ('allocated','draining')`, nodeID)
	return err
}
