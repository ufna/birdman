package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/jackc/pgx/v5"
)

// NodeAddressChange is an operator-confirmed replacement, never a claim from Hello.
type NodeAddressChange struct {
	Project, Env, Region, Hostname string
	ExpectedPublicIP, PublicIP     string
}

// ReconcileNodeAddress changes only an idle, non-serving node. The fleet lock
// fences warm-server creation, while the node lock fences identity/env changes.
// Existing server and match history is never rewritten to look like a new world.
func (s *Store) ReconcileNodeAddress(ctx context.Context, id string, p NodeAddressChange) (Node, error) {
	oldIP, e1 := netip.ParseAddr(p.ExpectedPublicIP)
	newIP, e2 := netip.ParseAddr(p.PublicIP)
	if e1 != nil || e2 != nil || !newIP.IsGlobalUnicast() || newIP.IsLoopback() || p.Project == "" || p.Env == "" || p.Region == "" || p.Hostname == "" {
		return Node{}, fmt.Errorf("explicit node identity and unicast addresses required: %w", ErrBadNodeAddress)
	}
	oldIP, newIP = oldIP.Unmap(), newIP.Unmap()
	before, err := s.GetNode(ctx, id)
	if err != nil {
		return Node{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Node{}, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, `select pg_try_advisory_xact_lock(hashtextextended($1,42))`, before.ProjectID+":"+before.Env+":"+before.Region).Scan(&locked); err != nil {
		return Node{}, err
	}
	if !locked {
		return Node{}, fmt.Errorf("fleet reconciliation is busy: %w", ErrConflict)
	}
	var project, env, region, hostname, address, state string
	err = tx.QueryRow(ctx, `select p.slug,n.env,n.region,n.hostname,host(n.public_ip),n.state from nodes n join projects p on p.id=n.project_id where n.id=$1::uuid for update of n`, id).Scan(&project, &env, &region, &hostname, &address, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, err
	}
	if project != p.Project || env != p.Env || region != p.Region || hostname != p.Hostname || env != before.Env || region != before.Region || state == "dead" {
		return Node{}, fmt.Errorf("node identity, scope or lifecycle changed: %w", ErrConflict)
	}
	if address == newIP.String() {
		if err = tx.Commit(ctx); err != nil {
			return Node{}, err
		}
		return s.GetNode(ctx, id)
	}
	if address != oldIP.String() {
		return Node{}, fmt.Errorf("registered address changed since review: %w", ErrConflict)
	}
	if state != "down" && state != "draining" && state != "quarantine" {
		return Node{}, fmt.Errorf("drain the node before changing its address: %w", ErrConflict)
	}
	var live bool
	if err = tx.QueryRow(ctx, `select exists(select from servers where node_id=$1::uuid and state in ('creating','ready','allocated','draining'))`, id).Scan(&live); err != nil {
		return Node{}, err
	}
	if live {
		return Node{}, fmt.Errorf("node still has live servers: %w", ErrConflict)
	}
	if _, err = tx.Exec(ctx, `update nodes set public_ip=$2::inet where id=$1::uuid`, id, newIP.String()); err != nil {
		return Node{}, err
	}
	if err = insertEvent(ctx, tx, "node_address_changed", EventRef{NodeID: &id}, map[string]any{"from": address, "to": newIP.String(), "hostname": hostname, "region": region, "env": env}); err != nil {
		return Node{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Node{}, err
	}
	return s.GetNode(ctx, id)
}

var ErrBadNodeAddress = errors.New("bad_node_address")
