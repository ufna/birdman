package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// ErrBadToken is returned when node token authentication fails.
var ErrBadToken = errors.New("bad_token")

// ensureProject returns the project id for slug, creating the project on
// first reference (v0 clarification in docs/specs/master.md §6: no dedicated
// projects endpoint yet).
func ensureProject(ctx context.Context, tx pgx.Tx, slug string) (string, error) {
	if slug == "" {
		return "", fmt.Errorf("project slug is required")
	}
	var id string
	err := tx.QueryRow(ctx, `
		insert into projects (slug) values ($1)
		on conflict (slug) do update set slug = excluded.slug
		returning id::text`, slug).Scan(&id)
	return id, err
}

type CreateNodeParams struct {
	Project       string
	Region        string
	Hostname      string
	PublicIP      string
	CapacitySlots int32
	Labels        map[string]any
}

// CreateNode registers a node ahead of agent connection and returns the
// bootstrap node_token — shown exactly once (docs/specs/protocol.md §Auth).
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
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
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
	var n Node
	err = tx.QueryRow(ctx, `
		insert into nodes (project_id, region, hostname, public_ip, capacity_slots, labels, token_hash)
		values ($1::uuid, $2, $3, $4::inet, $5, $6::jsonb, $7)
		returning id::text, project_id::text, region, hostname, host(public_ip),
		          capacity_slots, agent_version, state, created_at`,
		projectID, p.Region, p.Hostname, p.PublicIP, p.CapacitySlots, string(labels), string(hash)).
		Scan(&n.ID, &n.ProjectID, &n.Region, &n.Hostname, &n.PublicIP,
			&n.CapacitySlots, &n.AgentVersion, &n.State, &n.CreatedAt)
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

// ListNodes returns all nodes with project slugs.
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.Pool.Query(ctx, `
		select n.id::text, n.project_id::text, p.slug, n.region, n.hostname, host(n.public_ip),
		       n.capacity_slots, n.agent_version, n.state, n.last_heartbeat_at, n.labels, n.created_at
		from nodes n join projects p on p.id = n.project_id
		order by n.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var labels []byte
		if err := rows.Scan(&n.ID, &n.ProjectID, &n.Project, &n.Region, &n.Hostname, &n.PublicIP,
			&n.CapacitySlots, &n.AgentVersion, &n.State, &n.LastHeartbeatAt, &labels, &n.CreatedAt); err != nil {
			return nil, err
		}
		if len(labels) > 0 {
			_ = json.Unmarshal(labels, &n.Labels)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
