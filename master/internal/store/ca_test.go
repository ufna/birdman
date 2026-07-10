package store_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestEnsureInternalCARoundtrip(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	certPEM, keyPEM, err := st.EnsureInternalCA(ctx)
	if err != nil {
		t.Fatalf("EnsureInternalCA: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("empty CA material")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("cert PEM decode failed")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	if !ca.IsCA {
		t.Fatal("stored CA cert is not a CA")
	}

	// Idempotent: a second call returns the SAME persisted CA, not a new one.
	cert2, key2, err := st.EnsureInternalCA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(cert2) != string(certPEM) || string(key2) != string(keyPEM) {
		t.Fatal("EnsureInternalCA not idempotent: second call produced a different CA")
	}

	active, err := st.ActiveCAs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("ActiveCAs = %d, want 1", len(active))
	}
	if string(active[0]) != string(certPEM) {
		t.Fatal("ActiveCAs cert does not match EnsureInternalCA")
	}
	// Certs-only by construction: the signing key must never ride along — every
	// planned consumer (ClientCAs pool, ca_bundle_pem, GET /v1/ca) needs certs
	// only, and /v1/ca must have nowhere to read the key from (design §5).
	if strings.Contains(string(active[0]), string(keyPEM)) {
		t.Fatal("ActiveCAs leaked the CA private key alongside the cert")
	}
}

// Two masters starting at once must create exactly one CA (PG advisory lock).
func TestEnsureInternalCAConcurrent(t *testing.T) {
	st := testdb.New(t)
	const n = 8

	var wg sync.WaitGroup
	certs := make([][]byte, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			certs[i], _, errs[i] = st.EnsureInternalCA(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if string(certs[i]) != string(certs[0]) {
			t.Fatalf("goroutine %d got a different CA — advisory lock failed to serialize generation", i)
		}
	}
	var count int
	if err := st.Pool.QueryRow(context.Background(), `select count(*) from internal_ca`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("internal_ca rows = %d, want exactly 1", count)
	}
}

func TestSetNodeCert(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)

	// Unknown node → ErrNotFound; the whole tx rolls back, so no event either.
	if err := st.SetNodeCert(ctx, uuid.NewString(), "deadbeef", time.Now(),
		store.EventNodeEnrolled, "0.2.0"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown node: want ErrNotFound, got %v", err)
	}
	if n, err := st.CountEvents(ctx, store.EventNodeEnrolled); err != nil || n != 0 {
		t.Fatalf("failed SetNodeCert must not leave an event behind: n=%d err=%v", n, err)
	}

	serial := "0a1b2c3d4e5f"
	notAfter := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.SetNodeCert(ctx, f.NodeID, serial, notAfter, store.EventNodeEnrolled, "0.2.0"); err != nil {
		t.Fatalf("SetNodeCert: %v", err)
	}

	n, err := st.GetNode(ctx, f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n.CertSerial == nil || *n.CertSerial != serial {
		t.Fatalf("cert_serial = %v, want %s", n.CertSerial, serial)
	}
	if n.CertNotAfter == nil || !n.CertNotAfter.Equal(notAfter) {
		t.Fatalf("cert_not_after = %v, want %v", n.CertNotAfter, notAfter)
	}
	if n.EnrolledAt == nil {
		t.Fatal("enrolled_at was not set")
	}
	first := *n.EnrolledAt

	// The cert write and its audit event are one transaction (design §3): a
	// node_enrolled event exists, references the node, and its payload carries
	// exactly {serial, not_after, agent_version} — no token, no key material.
	evs, err := st.ListEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var enrolled *store.Event
	for i := range evs {
		if evs[i].Kind == store.EventNodeEnrolled {
			enrolled = &evs[i]
		}
	}
	if enrolled == nil {
		t.Fatal("SetNodeCert wrote no node_enrolled event")
	}
	if enrolled.NodeID == nil || *enrolled.NodeID != f.NodeID {
		t.Fatalf("event node_id = %v, want %s", enrolled.NodeID, f.NodeID)
	}
	if got := enrolled.Payload["serial"]; got != serial {
		t.Fatalf("event payload serial = %v, want %s", got, serial)
	}
	if got := enrolled.Payload["not_after"]; got != notAfter.UTC().Format(time.RFC3339) {
		t.Fatalf("event payload not_after = %v, want %s", got, notAfter.UTC().Format(time.RFC3339))
	}
	if got := enrolled.Payload["agent_version"]; got != "0.2.0" {
		t.Fatalf("event payload agent_version = %v, want 0.2.0", got)
	}
	if len(enrolled.Payload) != 3 {
		t.Fatalf("event payload must carry exactly {serial, not_after, agent_version}, got %v", enrolled.Payload)
	}

	// A renewal updates serial/expiry, preserves the original enrolled_at and
	// writes a node_cert_renewed event.
	serial2 := "ffee0011"
	if err := st.SetNodeCert(ctx, f.NodeID, serial2, notAfter.Add(30*24*time.Hour),
		store.EventNodeCertRenewed, "0.3.0"); err != nil {
		t.Fatalf("SetNodeCert renewal: %v", err)
	}
	n2, err := st.GetNode(ctx, f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n2.CertSerial == nil || *n2.CertSerial != serial2 {
		t.Fatalf("renewed cert_serial = %v, want %s", n2.CertSerial, serial2)
	}
	if n2.EnrolledAt == nil || !n2.EnrolledAt.Equal(first) {
		t.Fatalf("enrolled_at changed on renewal: was %v, now %v", first, n2.EnrolledAt)
	}
	if cnt, err := st.CountEvents(ctx, store.EventNodeCertRenewed); err != nil || cnt != 1 {
		t.Fatalf("node_cert_renewed events = %d (err=%v), want 1", cnt, err)
	}
	if cnt, err := st.CountEvents(ctx, store.EventNodeEnrolled); err != nil || cnt != 1 {
		t.Fatalf("node_enrolled events after renewal = %d (err=%v), want still 1", cnt, err)
	}
}
