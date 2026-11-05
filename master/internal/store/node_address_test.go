package store_test

import (
	"errors"
	"testing"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func addressIntent() store.NodeAddressChange {
	return store.NodeAddressChange{Project: "game", Env: "dev", Region: "eu", Hostname: "node-1", ExpectedPublicIP: "203.0.113.10", PublicIP: "198.51.100.22"}
}

func TestNodeAddressRecoveryPreservesIdentityAndHistory(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 4)
	p := addressIntent()
	ctx := t.Context()
	if _, err := st.ReconcileNodeAddress(ctx, f.NodeID, p); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("active node must be drained: %v", err)
	}
	if _, err := st.DrainNode(ctx, f.NodeID); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []store.NodeAddressChange{
		{Project: "other", Env: p.Env, Region: p.Region, Hostname: p.Hostname, ExpectedPublicIP: p.ExpectedPublicIP, PublicIP: p.PublicIP},
		{Project: p.Project, Env: "prod", Region: p.Region, Hostname: p.Hostname, ExpectedPublicIP: p.ExpectedPublicIP, PublicIP: p.PublicIP},
		{Project: p.Project, Env: p.Env, Region: p.Region, Hostname: "other", ExpectedPublicIP: p.ExpectedPublicIP, PublicIP: p.PublicIP},
		{Project: p.Project, Env: p.Env, Region: p.Region, Hostname: p.Hostname, ExpectedPublicIP: "203.0.113.99", PublicIP: p.PublicIP},
	} {
		if _, err := st.ReconcileNodeAddress(ctx, f.NodeID, bad); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("scope/precondition changed: %v", err)
		}
	}
	historical := f.InsertServerOn(t, f.NodeID, f.VersionID, "reaped")
	n, err := st.ReconcileNodeAddress(ctx, f.NodeID, p)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != f.NodeID || n.PublicIP != p.PublicIP || n.State != "draining" {
		t.Fatalf("replacement state: %+v", n)
	}
	if _, err = st.ReconcileNodeAddress(ctx, f.NodeID, p); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	count, err := st.CountEvents(ctx, "node_address_changed")
	if err != nil || count != 1 {
		t.Fatalf("audit count=%d err=%v", count, err)
	}
	recovered, err := st.AuthNodeToken(ctx, f.NodeToken)
	if err != nil || recovered.ID != f.NodeID || recovered.PublicIP != p.PublicIP {
		t.Fatalf("restored identity: %v", err)
	}
	server, err := st.GetServer(ctx, historical)
	if err != nil || server.State != "reaped" {
		t.Fatalf("history rewritten: %v", err)
	}
	if err = st.HelloSync(ctx, f.NodeID, "node-1", 4, "test", nil); err != nil {
		t.Fatal(err)
	}
	n, err = st.GetNode(ctx, f.NodeID)
	if err != nil || n.PublicIP != p.PublicIP || n.State != "draining" {
		t.Fatal("Hello changed address or resumed placement")
	}
}

func TestNodeAddressRecoveryRejectsLiveServersAndConcurrentFleetPlan(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 4)
	ctx := t.Context()
	p := addressIntent()
	if _, err := st.DrainNode(ctx, f.NodeID); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"creating", "ready", "allocated", "draining"} {
		id := f.InsertServerOn(t, f.NodeID, f.VersionID, state)
		if _, err := st.ReconcileNodeAddress(ctx, f.NodeID, p); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("live %s accepted: %v", state, err)
		}
		if _, err := st.Pool.Exec(ctx, `update servers set state='reaped' where id=$1::uuid`, id); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.GetNode(ctx, f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := st.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, `select pg_advisory_lock(hashtextextended($1,42))`, n.ProjectID+":"+n.Env+":"+n.Region); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `select pg_advisory_unlock_all()`)
	if _, err = st.ReconcileNodeAddress(ctx, f.NodeID, p); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("racing warm plan accepted: %v", err)
	}
	current, err := st.GetNode(ctx, f.NodeID)
	if err != nil || current.PublicIP != p.ExpectedPublicIP {
		t.Fatal("failed repair altered address")
	}
}
