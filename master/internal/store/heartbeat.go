package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// mapAgentState translates an agent-side server state (docs/specs/agent.md
// §3) into the master-side servers.state. Empty result → only stats are
// updated, state is left untouched.
func mapAgentState(agent string) string {
	switch agent {
	case "pulling", "starting":
		return "creating"
	case "ready":
		return "ready"
	case "allocated":
		return "allocated"
	case "draining":
		return "draining"
	case "stopped":
		return "reaped" // graceful stop requested by master
	case "failed":
		return "failed"
	default:
		return ""
	}
}

// liveState reports whether a master-side state means the server is alive
// on its node (resurrection targets on Hello map reconciliation).
func liveState(s string) bool {
	return s == "ready" || s == "allocated" || s == "draining"
}

// transitionAllowed guards heartbeat-driven state changes: terminal states
// stick, allocation is never downgraded by a stale agent report.
func transitionAllowed(cur, next string) bool {
	if cur == next {
		return false
	}
	if cur == "failed" || cur == "reaped" {
		return false
	}
	switch next {
	case "failed", "reaped":
		return true
	case "ready":
		return cur == "creating"
	case "allocated":
		return cur == "creating" || cur == "ready"
	case "draining":
		return cur == "creating" || cur == "ready" || cur == "allocated"
	default:
		return false
	}
}

// touchNode refreshes the lease and lifts quarantine or down on heartbeat
// return (docs/specs/protocol.md §1 Lease; down is the итерация 5 follow-up
// auto state). A heartbeat only arrives inside a live mTLS session, so the
// lift is safe — the link genuinely came back. 'dead' is deliberately NOT
// lifted: it is the manual revocation terminal (agentlink refuses a dead node
// a session in every auth mode, so this path is unreachable for it anyway).
// Returns true when the node recovered.
func touchNode(ctx context.Context, tx pgx.Tx, nodeID string) (recovered bool, err error) {
	var oldState, newState string
	err = tx.QueryRow(ctx, `
		with old as (select state from nodes where id = $1::uuid)
		update nodes n set
			last_heartbeat_at = now(),
			state = case when n.state in ('quarantine','down') then 'active' else n.state end
		where n.id = $1::uuid
		returning n.state, (select state from old)`, nodeID).Scan(&newState, &oldState)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("node %s: %w", nodeID, ErrNotFound)
	}
	if err != nil {
		return false, err
	}
	return (oldState == "quarantine" || oldState == "down") && newState == "active", nil
}

// applyReports upserts per-server state from an agent Hello/Heartbeat
// (docs/specs/master.md: переход creating→ready делает только heartbeat).
// Reports for servers that do not belong to this node are ignored.
//
// resurrect is the Hello-only map reconciliation (protocol.md §1 Lease:
// «возвращение heartbeat → сверка карты серверов»): a server the lease
// checker failed while the node was silent, but which the returning agent
// reports alive (containers survive agent/node-link outages), goes back to
// its live state instead of being wastefully re-created. Regular heartbeats
// keep the terminal guard: failed stays failed there.
func applyReports(ctx context.Context, tx pgx.Tx, nodeID string, reports []ServerReport, resurrect bool) error {
	reported := make([]string, 0, len(reports))
	for _, r := range reports {
		reported = append(reported, r.ServerID)
		var cur string
		var matchID *string
		err := tx.QueryRow(ctx, `
			select state, match_id::text from servers
			where id = $1::uuid and node_id = $2::uuid
			for update`, r.ServerID, nodeID).Scan(&cur, &matchID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown or foreign server id — ignore (agent restart races,
			// hostile agent). Reconcile owns the desired state.
			continue
		}
		if err != nil {
			return err
		}
		next := cur
		if mapped := mapAgentState(r.State); mapped != "" {
			switch {
			case transitionAllowed(cur, mapped):
				next = mapped
			case resurrect && cur == "failed" && liveState(mapped):
				next = mapped
				id := r.ServerID
				if err := insertEvent(ctx, tx, EventServerRecovered,
					EventRef{ServerID: &id, NodeID: &nodeID},
					map[string]any{"state": mapped}); err != nil {
					return err
				}
				// The lease checker aborted the match together with failing
				// the server (node_lost); the dedik survived the outage and
				// still plays it — symmetric resurrection for the match.
				if mapped == "allocated" {
					if _, err := tx.Exec(ctx, `
						update matches m set state = 'running', ended_at = null
						from servers s
						where s.id = $1::uuid and m.id = s.match_id
						  and m.state = 'aborted'`, r.ServerID); err != nil {
						return err
					}
				}
			}
		}
		if _, err := tx.Exec(ctx, `
			update servers set
				state = $2,
				players = $3,
				tick_ms = case when $4 > 0 then $4 else tick_ms end,
				port = case when $5 > 0 then $5 else port end,
				updated_at = now()
			where id = $1::uuid`,
			r.ServerID, next, r.Players, r.TickMS, r.Port); err != nil {
			return err
		}
		// players_peak = максимум players из heartbeat за матч (master.md §1).
		if r.Players > 0 {
			if _, err := tx.Exec(ctx, `
				update matches m set players_peak = greatest(m.players_peak, $2)
				from servers s
				where s.id = $1::uuid and m.id = s.match_id
				  and m.state in ('pending','running')`,
				r.ServerID, r.Players); err != nil {
				return err
			}
		}
		if next == "failed" && cur != "failed" {
			id := r.ServerID
			// MatchID included: an allocated one-shot that dies in its lobby
			// (nobody joined, lobby-timeout exit) fails through THIS path, and
			// downstream matchmakers correlate server_failed by match_id -- an
			// event without it left the match listed forever (khl-3jb.14).
			if err := insertEvent(ctx, tx, EventServerFailed,
				EventRef{ServerID: &id, NodeID: &nodeID, MatchID: matchID},
				map[string]any{"reason": "agent_report", "agent_state": r.State}); err != nil {
				return err
			}
			if err := abortServerMatch(ctx, tx, r.ServerID); err != nil {
				return err
			}
		}
	}

	// Draining servers the agent no longer reports were stopped and removed
	// on the node → reaped.
	_, err := tx.Exec(ctx, `
		update servers set state = 'reaped', updated_at = now()
		where node_id = $1::uuid and state = 'draining'
		  and not (id = any($2::uuid[]))`, nodeID, reported)
	return err
}

// ApplyHeartbeat handles one agent Heartbeat: lease refresh + servers upsert.
func (s *Store) ApplyHeartbeat(ctx context.Context, nodeID string, reports []ServerReport) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	recovered, err := touchNode(ctx, tx, nodeID)
	if err != nil {
		return err
	}
	if recovered {
		if err := insertEvent(ctx, tx, EventNodeRecovered, EventRef{NodeID: &nodeID}, nil); err != nil {
			return err
		}
	}
	if err := applyReports(ctx, tx, nodeID, reports, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// HelloSync applies an agent Hello: node facts + recovered server map.
func (s *Store) HelloSync(ctx context.Context, nodeID, hostname string, capacitySlots int32, agentVersion string, reports []ServerReport) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		update nodes set
			hostname = coalesce(nullif($2, ''), hostname),
			capacity_slots = case when $3 > 0 then $3 else capacity_slots end,
			agent_version = coalesce(nullif($4, ''), agent_version)
		where id = $1::uuid`, nodeID, hostname, capacitySlots, agentVersion); err != nil {
		return err
	}
	recovered, err := touchNode(ctx, tx, nodeID)
	if err != nil {
		return err
	}
	if recovered {
		if err := insertEvent(ctx, tx, EventNodeRecovered, EventRef{NodeID: &nodeID}, nil); err != nil {
			return err
		}
	}
	// Hello = map reconciliation: the agent's recovered map may resurrect
	// servers the lease checker failed while the node was silent.
	if err := applyReports(ctx, tx, nodeID, reports, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ApplyServerEvent handles an agent ServerEvent (failed|oom|ready|match_*).
//
// Match lifecycle (итерация 2, master.md §1): match_start moves the server's
// match to running (started_at), match_end finishes it (finished|aborted,
// ended_at). The match row is identified via servers.match_id — the
// allocation source of truth; a row missing at match_start (external
// matchmaker allocated via REST and never wrote one) is created on the fly.
func (s *Store) ApplyServerEvent(ctx context.Context, nodeID, serverID, kind, detail string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var cur string
	var matchID *string
	err = tx.QueryRow(ctx, `
		select state, match_id::text from servers
		where id = $1::uuid and node_id = $2::uuid
		for update`, serverID, nodeID).Scan(&cur, &matchID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // unknown/foreign server — ignore
	}
	if err != nil {
		return err
	}

	switch kind {
	case "failed", "oom":
		if transitionAllowed(cur, "failed") {
			if _, err := tx.Exec(ctx,
				`update servers set state = 'failed', updated_at = now() where id = $1::uuid`,
				serverID); err != nil {
				return err
			}
			if err := insertEvent(ctx, tx, EventServerFailed,
				EventRef{ServerID: &serverID, NodeID: &nodeID, MatchID: matchID},
				map[string]any{"reason": kind, "detail": detail}); err != nil {
				return err
			}
			// A dedik dying mid-match never sends match_end — close the match.
			if err := abortServerMatch(ctx, tx, serverID); err != nil {
				return err
			}
		}
	case "ready":
		if transitionAllowed(cur, "ready") {
			if _, err := tx.Exec(ctx,
				`update servers set state = 'ready', updated_at = now() where id = $1::uuid`,
				serverID); err != nil {
				return err
			}
		}
	case "match_start":
		// Upsert keeps whoever came first consistent: the matchmaker's
		// RecordMatch (pending) or this event (REST-allocate path has no row).
		// env — из строки СЕРВЕРА (s.env, инвариант I6), а не из migration-default
		// 'dev': REST-путь без RecordMatch иначе записал бы prod-матч как dev (W-I1).
		if _, err := tx.Exec(ctx, `
			insert into matches (id, project_id, server_id, version_id, region, env, state, started_at)
			select s.match_id, s.project_id, s.id, s.version_id, n.region, s.env, 'running', now()
			from servers s join nodes n on n.id = s.node_id
			where s.id = $1::uuid and s.match_id is not null
			on conflict (id) do update
				set state = 'running', started_at = coalesce(matches.started_at, now())
				where matches.state = 'pending'`, serverID); err != nil {
			return err
		}
		if err := insertEvent(ctx, tx, kind,
			EventRef{ServerID: &serverID, NodeID: &nodeID, MatchID: matchID},
			map[string]any{"detail": detail}); err != nil {
			return err
		}
	case "match_end":
		target := "finished"
		if matchResult(detail) == "aborted" {
			target = "aborted"
		}
		if _, err := tx.Exec(ctx, `
			update matches m set state = $2, ended_at = coalesce(m.ended_at, now())
			from servers s
			where s.id = $1::uuid and m.id = s.match_id
			  and m.state in ('pending','running')`, serverID, target); err != nil {
			return err
		}
		if err := insertEvent(ctx, tx, kind,
			EventRef{ServerID: &serverID, NodeID: &nodeID, MatchID: matchID},
			map[string]any{"detail": detail, "result": matchResult(detail)}); err != nil {
			return err
		}
	default:
		// Custom kinds — audit trail only in v0.
		if err := insertEvent(ctx, tx, kind,
			EventRef{ServerID: &serverID, NodeID: &nodeID, MatchID: matchID},
			map[string]any{"detail": detail}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// matchResult extracts the liba result out of a match_end event detail
// ("<match_id> completed|aborted", protocol.md §2 via agent). Anything not
// explicitly aborted counts as completed — a one-shot dedik that exited
// cleanly played its match out.
func matchResult(detail string) string {
	fields := strings.Fields(detail)
	if len(fields) > 0 && fields[len(fields)-1] == "aborted" {
		return "aborted"
	}
	return "completed"
}

// abortServerMatch closes the still-open match of a failed server: without a
// live dedik no match_end will ever arrive.
func abortServerMatch(ctx context.Context, db execer, serverID string) error {
	_, err := db.Exec(ctx, `
		update matches m set state = 'aborted', ended_at = coalesce(m.ended_at, now())
		from servers s
		where s.id = $1::uuid and m.id = s.match_id
		  and m.state in ('pending','running')`, serverID)
	return err
}
