package store

import (
	"context"
	"errors"
	"fmt"

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

// touchNode refreshes the lease and lifts quarantine on heartbeat return
// (docs/specs/protocol.md §1 Lease). Returns true when the node recovered.
func touchNode(ctx context.Context, tx pgx.Tx, nodeID string) (recovered bool, err error) {
	var oldState, newState string
	err = tx.QueryRow(ctx, `
		with old as (select state from nodes where id = $1::uuid)
		update nodes n set
			last_heartbeat_at = now(),
			state = case when n.state = 'quarantine' then 'active' else n.state end
		where n.id = $1::uuid
		returning n.state, (select state from old)`, nodeID).Scan(&newState, &oldState)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("node %s: %w", nodeID, ErrNotFound)
	}
	if err != nil {
		return false, err
	}
	return oldState == "quarantine" && newState == "active", nil
}

// applyReports upserts per-server state from an agent Hello/Heartbeat
// (docs/specs/master.md: переход creating→ready делает только heartbeat).
// Reports for servers that do not belong to this node are ignored.
func applyReports(ctx context.Context, tx pgx.Tx, nodeID string, reports []ServerReport) error {
	reported := make([]string, 0, len(reports))
	for _, r := range reports {
		reported = append(reported, r.ServerID)
		var cur string
		err := tx.QueryRow(ctx, `
			select state from servers
			where id = $1::uuid and node_id = $2::uuid
			for update`, r.ServerID, nodeID).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown or foreign server id — ignore (agent restart races,
			// hostile agent). Reconcile owns the desired state.
			continue
		}
		if err != nil {
			return err
		}
		next := cur
		if mapped := mapAgentState(r.State); mapped != "" && transitionAllowed(cur, mapped) {
			next = mapped
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
		if next == "failed" && cur != "failed" {
			id := r.ServerID
			if err := insertEvent(ctx, tx, EventServerFailed,
				EventRef{ServerID: &id, NodeID: &nodeID},
				map[string]any{"reason": "agent_report", "agent_state": r.State}); err != nil {
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
	if err := applyReports(ctx, tx, nodeID, reports); err != nil {
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
	if err := applyReports(ctx, tx, nodeID, reports); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ApplyServerEvent handles an agent ServerEvent (failed|oom|ready|match_*).
func (s *Store) ApplyServerEvent(ctx context.Context, nodeID, serverID, kind, detail string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var cur string
	err = tx.QueryRow(ctx, `
		select state from servers
		where id = $1::uuid and node_id = $2::uuid
		for update`, serverID, nodeID).Scan(&cur)
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
				EventRef{ServerID: &serverID, NodeID: &nodeID},
				map[string]any{"reason": kind, "detail": detail}); err != nil {
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
	default:
		// match_start / match_end / custom — audit trail only in v0.
		if err := insertEvent(ctx, tx, kind,
			EventRef{ServerID: &serverID, NodeID: &nodeID},
			map[string]any{"detail": detail}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
