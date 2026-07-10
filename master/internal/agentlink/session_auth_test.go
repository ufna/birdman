package agentlink_test

// Session auth over a REAL TLS handshake (mTLS agentlink v1,
// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §3,
// §Тестирование "unit Session-auth"). A test CA issues the master's server
// leaf and the nodes' client leaves via tlsutil; the bufconn listener runs
// with tls.VerifyClientCertIfGiven + ClientCAs, so the handshake populates
// VerifiedChains exactly as the production listener does. The matrix:
//   mixed → cert-session ok (identity from CN, empty token) AND token-session ok;
//   mtls  → token-only denied, cert ok, cert+mismatching-token denied, dead-node cert denied;
//   token → client certs ignored (byte-identical regression);
//   dead-node → refused on BOTH the cert AND the token Session path, in every mode
//               (revocation, design §Безопасность).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	"github.com/ufna/birdman/master/internal/tlsutil"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// testCA generates a throwaway internal CA (the same tlsutil path master uses).
func testCA(t *testing.T) (caCertPEM, caKeyPEM []byte) {
	t.Helper()
	caCertPEM, caKeyPEM, err := tlsutil.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	return caCertPEM, caKeyPEM
}

func caPool(t *testing.T, caCertPEM []byte) *x509.CertPool {
	t.Helper()
	pool, err := tlsutil.CAPool([][]byte{caCertPEM})
	if err != nil {
		t.Fatalf("CAPool: %v", err)
	}
	return pool
}

// serverLeaf builds the master's server tls.Certificate from the test CA.
func serverLeaf(t *testing.T, caCertPEM, caKeyPEM []byte) tls.Certificate {
	t.Helper()
	certPEM, keyPEM, err := tlsutil.IssueServerLeaf(caCertPEM, caKeyPEM, "", nil)
	if err != nil {
		t.Fatalf("IssueServerLeaf: %v", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("server X509KeyPair: %v", err)
	}
	return cert
}

// clientLeaf mints a node client cert with CN=nodeID: it generates an ECDSA
// P-256 key + CSR (a bogus subject, deliberately ignored by the issuer) and
// signs it with the test CA, exactly as the Enroll handler will.
func clientLeaf(t *testing.T, caCertPEM, caKeyPEM []byte, nodeID string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: "ignored-by-issuer"}}, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	certPEM, _, err := tlsutil.IssueClientLeafFromCSR(caCertPEM, caKeyPEM, nodeID, csrPEM)
	if err != nil {
		t.Fatalf("IssueClientLeafFromCSR: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("client X509KeyPair: %v", err)
	}
	return cert
}

// startTLSServer runs the AgentLink service behind a gRPC server on REAL
// loopback TCP whose transport creds mirror the production listener: server
// leaf from the CA, ClientCAs = the CA, ClientAuth = VerifyClientCertIfGiven
// (so a client cert is optional but, when presented, is verified into
// VerifiedChains). Loopback TCP (not bufconn) because since the SetRegistries
// gate (design §3) the registries preface — requireAuthed's success signal —
// only reaches trusted sessions, and this file's token-session cases model
// the trusted dev box: token over 127.0.0.1. Returns the address to dial.
func startTLSServer(t *testing.T, st *store.Store, mode agentlink.AuthMode, caCertPEM, caKeyPEM []byte) string {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverLeaf(t, caCertPEM, caKeyPEM)},
		ClientCAs:    caPool(t, caCertPEM),
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
	})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp loopback: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(creds))
	agentlinkv1.RegisterAgentLinkServer(srv, agentlink.NewService(st, agentlink.NewHub(log), nil, nil, mode, log))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// tlsClient dials addr with the CA as its RootCAs and ServerName
// "birdman-master" (the leaf's DNS SAN — overrides the IP-literal target the
// same way the agent overrides its master_addr). A nil clientCert connects
// without one; otherwise it is offered for VerifyClientCertIfGiven.
func tlsClient(t *testing.T, addr string, caCertPEM []byte, clientCert *tls.Certificate) agentlinkv1.AgentLinkClient {
	t.Helper()
	cfg := &tls.Config{
		RootCAs:    caPool(t, caCertPEM),
		ServerName: "birdman-master",
		MinVersion: tls.VersionTLS12,
	}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentlinkv1.NewAgentLinkClient(conn)
}

// openSession opens a Session, sends Hello(token) and returns the first server
// message (the registries preface on success) or the auth error. A successful
// auth always prefaces the stream with a SetRegistries snapshot (T3), so a
// non-nil SetRegistries first message is the success signal and a status error
// is the rejection.
func openSession(t *testing.T, client agentlinkv1.AgentLinkClient, token string) (*agentlinkv1.MasterMsg, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	stream, err := client.Session(ctx)
	if err != nil {
		t.Fatalf("open Session: %v", err)
	}
	if err := stream.Send(hello(token)); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	return stream.Recv()
}

func requireAuthed(t *testing.T, client agentlinkv1.AgentLinkClient, token string) {
	t.Helper()
	msg, err := openSession(t, client, token)
	if err != nil {
		t.Fatalf("want authenticated session, got error: %v", err)
	}
	if msg.GetSetRegistries() == nil {
		t.Fatalf("want a registries preface on a successful attach, got %+v", msg)
	}
}

func requireDenied(t *testing.T, client agentlinkv1.AgentLinkClient, token string) {
	t.Helper()
	if _, err := openSession(t, client, token); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

func secondNode(t *testing.T, st *store.Store, f *testdb.Fixture) (nodeID, token string) {
	t.Helper()
	n, tok, err := st.CreateNode(context.Background(), store.CreateNodeParams{
		Project: f.Project, Region: f.Region, Hostname: "node-2", PublicIP: "203.0.113.11", CapacitySlots: 10,
	})
	if err != nil {
		t.Fatalf("create second node: %v", err)
	}
	return n.ID, tok
}

func markDead(t *testing.T, st *store.Store, nodeID string) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`update nodes set state = 'dead' where id = $1::uuid`, nodeID); err != nil {
		t.Fatalf("mark node dead: %v", err)
	}
}

// --- mixed ---

func TestSessionCertAuthMixedOK(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	caCert, caKey := testCA(t)
	lis := startTLSServer(t, st, agentlink.AuthMixed, caCert, caKey)
	cert := clientLeaf(t, caCert, caKey, f.NodeID)
	// Identity from the cert CN, node_token empty.
	requireAuthed(t, tlsClient(t, lis, caCert, &cert), "")
}

func TestSessionTokenAuthMixedOK(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	caCert, caKey := testCA(t)
	lis := startTLSServer(t, st, agentlink.AuthMixed, caCert, caKey)
	// No client cert; a valid node_token still authenticates in mixed mode.
	requireAuthed(t, tlsClient(t, lis, caCert, nil), f.NodeToken)
}

func TestSessionCertPlusMatchingTokenMixedOK(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	caCert, caKey := testCA(t)
	lis := startTLSServer(t, st, agentlink.AuthMixed, caCert, caKey)
	cert := clientLeaf(t, caCert, caKey, f.NodeID)
	// A token that names the SAME node as the cert is accepted (no confused deputy).
	requireAuthed(t, tlsClient(t, lis, caCert, &cert), f.NodeToken)
}

// --- mtls ---

func TestSessionTokenOnlyMTLSDenied(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	caCert, caKey := testCA(t)
	lis := startTLSServer(t, st, agentlink.AuthMTLS, caCert, caKey)
	// A perfectly valid token with no client cert is rejected under mtls.
	requireDenied(t, tlsClient(t, lis, caCert, nil), f.NodeToken)
}

func TestSessionCertAuthMTLSOK(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	caCert, caKey := testCA(t)
	lis := startTLSServer(t, st, agentlink.AuthMTLS, caCert, caKey)
	cert := clientLeaf(t, caCert, caKey, f.NodeID)
	requireAuthed(t, tlsClient(t, lis, caCert, &cert), "")
}

func TestSessionCertPlusMismatchingTokenMTLSDenied(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	_, token2 := secondNode(t, st, f)
	caCert, caKey := testCA(t)
	lis := startTLSServer(t, st, agentlink.AuthMTLS, caCert, caKey)
	// Cert says node-1, token says node-2 → confused-deputy guard rejects.
	cert := clientLeaf(t, caCert, caKey, f.NodeID)
	requireDenied(t, tlsClient(t, lis, caCert, &cert), token2)
}

func TestSessionCertDeadNodeMTLSDenied(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	markDead(t, st, f.NodeID)
	caCert, caKey := testCA(t)
	lis := startTLSServer(t, st, agentlink.AuthMTLS, caCert, caKey)
	// A validly-signed cert for a dead node: identity ok, authorization denied.
	cert := clientLeaf(t, caCert, caKey, f.NodeID)
	requireDenied(t, tlsClient(t, lis, caCert, &cert), "")
}

// --- token (regression) ---

// In token mode a client cert must be ignored entirely: a valid cert does NOT
// rescue a bad token, and a valid token still authenticates regardless of the
// cert — byte-identical to the pre-mTLS behaviour.
func TestSessionTokenModeIgnoresClientCert(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	caCert, caKey := testCA(t)
	lis := startTLSServer(t, st, agentlink.AuthToken, caCert, caKey)
	cert := clientLeaf(t, caCert, caKey, f.NodeID)

	// A valid cert + a bad token: the cert is ignored, so the bad token denies.
	requireDenied(t, tlsClient(t, lis, caCert, &cert), "bnt_bogus")

	// A valid cert + a valid token: authenticates via the token path, unchanged.
	cert2 := clientLeaf(t, caCert, caKey, f.NodeID)
	requireAuthed(t, tlsClient(t, lis, caCert, &cert2), f.NodeToken)
}

// --- revocation on the token Session path (dead-node) ---

// The cert Session path already refuses a dead node (TestSessionCertDeadNode*),
// but bcrypt token auth (store.AuthNodeToken) carries no not-dead check, so the
// token Session path used to let a revoked node back in. Revocation must reject
// the token path too (design §Безопасность: "серт И токен отвергаются" — holds
// in all modes for Session). A dead node with a perfectly valid node_token is
// now refused, in mixed AND in token mode. A live node with the same token is
// unaffected (byte-identical regression, covered by the *OK / token-mode tests).

func TestSessionTokenDeadNodeMixedDenied(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	markDead(t, st, f.NodeID)
	caCert, caKey := testCA(t)
	lis := startTLSServer(t, st, agentlink.AuthMixed, caCert, caKey)
	// No client cert; a valid token for a DEAD node must be refused on the
	// token Session path, exactly as the cert path already refuses one.
	requireDenied(t, tlsClient(t, lis, caCert, nil), f.NodeToken)
}

func TestSessionTokenDeadNodeTokenModeDenied(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	markDead(t, st, f.NodeID)
	caCert, caKey := testCA(t)
	lis := startTLSServer(t, st, agentlink.AuthToken, caCert, caKey)
	// Even in the emergency token-mode rollback a dead node must never link.
	requireDenied(t, tlsClient(t, lis, caCert, nil), f.NodeToken)
}
