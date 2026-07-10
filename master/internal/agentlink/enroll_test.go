package agentlink_test

// Enroll handler (mTLS agentlink v1,
// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §3,
// §Тестирование "unit Enroll"): the unary RPC swaps a node_token (bootstrap/
// recovery) or a live verified client cert (renewal) for a signed client
// leaf, records nodes.cert_* plus the audit event atomically, and returns
// cert + active-CA bundle + expiry. Token-path cases run over an insecure
// transport (no TLS peer info → exactly the "no verified cert" shape);
// cert-path cases run over a real TLS handshake whose ClientCAs is the SAME
// internal CA the handler signs with (st.EnsureInternalCA), as in production.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// newEnrollCSR builds an ECDSA P-256 PKCS#10 CSR with an arbitrary
// (deliberately attacker-chooseable) subject CN — the issuer must ignore it.
func newEnrollCSR(t *testing.T, subjectCN string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("csr key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: subjectCN}}, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func enrollCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func parseCertPEM(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("response cert is not a CERTIFICATE PEM")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse response cert: %v", err)
	}
	return c
}

func countEvents(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	n, err := st.CountEvents(context.Background(), kind)
	if err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return n
}

// Token → cert happy path: the leaf's CN is the node id embedded in the
// token, nodes.cert_serial/cert_not_after/enrolled_at are recorded, a
// node_enrolled event is written, and the response carries the active-CA
// bundle plus the leaf expiry.
func TestEnrollByTokenIssuesCert(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	_, _, client := startServer(t, st)

	resp, err := client.Enroll(enrollCtx(t), &agentlinkv1.EnrollRequest{
		NodeToken:    f.NodeToken,
		CsrPem:       newEnrollCSR(t, "csr-subject-is-ignored"),
		AgentVersion: "0.2.0",
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	leaf := parseCertPEM(t, resp.GetCertPem())
	if leaf.Subject.CommonName != f.NodeID {
		t.Fatalf("leaf CN = %q, want node id %q", leaf.Subject.CommonName, f.NodeID)
	}
	if got := resp.GetNotAfterUnix(); got != leaf.NotAfter.Unix() {
		t.Fatalf("not_after_unix = %d, want %d", got, leaf.NotAfter.Unix())
	}

	// The leaf chains to the internal CA, and ca_bundle_pem carries exactly
	// the active CA set (the agent's future trust bundle).
	cas, err := st.ActiveCAs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cas) != 1 {
		t.Fatalf("active CAs = %d, want 1", len(cas))
	}
	ca := parseCertPEM(t, cas[0])
	if err := leaf.CheckSignatureFrom(ca); err != nil {
		t.Fatalf("leaf must be signed by the internal CA: %v", err)
	}
	if string(resp.GetCaBundlePem()) != string(cas[0]) {
		t.Fatalf("ca_bundle_pem does not match ActiveCAs")
	}

	// nodes.cert_* + enrolled_at recorded; audit event node_enrolled written.
	n, err := st.GetNode(context.Background(), f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	wantSerial := leaf.SerialNumber.Text(16)
	if n.CertSerial == nil || *n.CertSerial != wantSerial {
		t.Fatalf("cert_serial = %v, want %s", n.CertSerial, wantSerial)
	}
	if n.CertNotAfter == nil || !n.CertNotAfter.Equal(leaf.NotAfter) {
		t.Fatalf("cert_not_after = %v, want %v", n.CertNotAfter, leaf.NotAfter)
	}
	if n.EnrolledAt == nil {
		t.Fatal("enrolled_at not set by Enroll")
	}
	if got := countEvents(t, st, store.EventNodeEnrolled); got != 1 {
		t.Fatalf("node_enrolled events = %d, want 1", got)
	}
	if got := countEvents(t, st, store.EventNodeCertRenewed); got != 0 {
		t.Fatalf("node_cert_renewed events = %d, want 0 on first enrollment", got)
	}
}

func TestEnrollBadTokenDenied(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10)
	_, _, client := startServer(t, st)

	_, err := client.Enroll(enrollCtx(t), &agentlinkv1.EnrollRequest{
		NodeToken: "bnt_bogus",
		CsrPem:    newEnrollCSR(t, "x"),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

// Neither a token nor a verified client cert → nothing to authenticate.
func TestEnrollNoCredentialsDenied(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10)
	_, _, client := startServer(t, st)

	_, err := client.Enroll(enrollCtx(t), &agentlinkv1.EnrollRequest{
		CsrPem: newEnrollCSR(t, "x"),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

// A valid token for a dead node must not mint a cert (identity from the
// credential, authorization from the DB — design §3).
func TestEnrollDeadNodeDenied(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	markDead(t, st, f.NodeID)
	_, _, client := startServer(t, st)

	_, err := client.Enroll(enrollCtx(t), &agentlinkv1.EnrollRequest{
		NodeToken: f.NodeToken,
		CsrPem:    newEnrollCSR(t, "x"),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for a dead node, got %v", err)
	}
	if got := countEvents(t, st, store.EventNodeEnrolled); got != 0 {
		t.Fatalf("dead node must not produce a node_enrolled event, got %d", got)
	}
}

// Renewal: a live verified client cert alone (empty token) re-issues the
// leaf; the audit event is node_cert_renewed, not node_enrolled.
func TestEnrollRenewalByCert(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	caCert, caKey, err := st.EnsureInternalCA(context.Background())
	if err != nil {
		t.Fatalf("EnsureInternalCA: %v", err)
	}
	lis := startTLSServer(t, st, agentlink.AuthMixed, caCert, caKey)
	cert := clientLeaf(t, caCert, caKey, f.NodeID)
	client := tlsClient(t, lis, caCert, &cert)

	resp, err := client.Enroll(enrollCtx(t), &agentlinkv1.EnrollRequest{
		NodeToken:    "", // renewal: authorization comes from the live cert
		CsrPem:       newEnrollCSR(t, "renewal"),
		AgentVersion: "0.3.0",
	})
	if err != nil {
		t.Fatalf("Enroll renewal: %v", err)
	}
	leaf := parseCertPEM(t, resp.GetCertPem())
	if leaf.Subject.CommonName != f.NodeID {
		t.Fatalf("renewed leaf CN = %q, want %q", leaf.Subject.CommonName, f.NodeID)
	}
	n, err := st.GetNode(context.Background(), f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n.CertSerial == nil || *n.CertSerial != leaf.SerialNumber.Text(16) {
		t.Fatalf("cert_serial = %v, want the renewed leaf serial %s", n.CertSerial, leaf.SerialNumber.Text(16))
	}
	if got := countEvents(t, st, store.EventNodeCertRenewed); got != 1 {
		t.Fatalf("node_cert_renewed events = %d, want 1", got)
	}
	if got := countEvents(t, st, store.EventNodeEnrolled); got != 0 {
		t.Fatalf("node_enrolled events = %d, want 0 for a cert renewal", got)
	}
}

// A cert for node-1 combined with node-2's token must be rejected (confused
// deputy): both credentials must resolve to the same node.
func TestEnrollCertPlusForeignTokenDenied(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	_, token2 := secondNode(t, st, f)
	caCert, caKey, err := st.EnsureInternalCA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lis := startTLSServer(t, st, agentlink.AuthMixed, caCert, caKey)
	cert := clientLeaf(t, caCert, caKey, f.NodeID)
	client := tlsClient(t, lis, caCert, &cert)

	_, err = client.Enroll(enrollCtx(t), &agentlinkv1.EnrollRequest{
		NodeToken: token2,
		CsrPem:    newEnrollCSR(t, "x"),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for cert+foreign-token, got %v", err)
	}
}

// The CSR subject is transport only: even a CSR naming ANOTHER real node's id
// as its CN mints a leaf whose CN is the AUTHENTICATED node id.
func TestEnrollCSRForeignSubjectIgnored(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	otherID, _ := secondNode(t, st, f)
	_, _, client := startServer(t, st)

	resp, err := client.Enroll(enrollCtx(t), &agentlinkv1.EnrollRequest{
		NodeToken: f.NodeToken,
		CsrPem:    newEnrollCSR(t, otherID), // tries to impersonate node-2
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	leaf := parseCertPEM(t, resp.GetCertPem())
	if leaf.Subject.CommonName != f.NodeID {
		t.Fatalf("leaf CN = %q, want the authenticated node %q (CSR subject must be ignored)",
			leaf.Subject.CommonName, f.NodeID)
	}
}

func TestEnrollMalformedCSRInvalidArgument(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	_, _, client := startServer(t, st)

	_, err := client.Enroll(enrollCtx(t), &agentlinkv1.EnrollRequest{
		NodeToken: f.NodeToken,
		CsrPem:    []byte("this is not a csr"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for a malformed CSR, got %v", err)
	}
	if got := countEvents(t, st, store.EventNodeEnrolled); got != 0 {
		t.Fatalf("malformed CSR must not produce an event, got %d", got)
	}
}

// In mtls mode Session requires a client cert, but Enroll by token MUST keep
// working on a certless connection — otherwise a new node could never join
// (design §3: "Enroll по token остаётся").
func TestEnrollByTokenWorksInMTLSMode(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	caCert, caKey, err := st.EnsureInternalCA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lis := startTLSServer(t, st, agentlink.AuthMTLS, caCert, caKey)
	client := tlsClient(t, lis, caCert, nil) // no client cert

	resp, err := client.Enroll(enrollCtx(t), &agentlinkv1.EnrollRequest{
		NodeToken: f.NodeToken,
		CsrPem:    newEnrollCSR(t, "bootstrap"),
	})
	if err != nil {
		t.Fatalf("Enroll by token in mtls mode: %v", err)
	}
	if leaf := parseCertPEM(t, resp.GetCertPem()); leaf.Subject.CommonName != f.NodeID {
		t.Fatalf("leaf CN = %q, want %q", leaf.Subject.CommonName, f.NodeID)
	}
}
