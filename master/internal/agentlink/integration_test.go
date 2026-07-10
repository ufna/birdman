package agentlink_test

// Integration test (mTLS agentlink v1,
// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §Тестирование
// "integration", §Rollout Фаза D): the FULL token→Enroll→mTLS Session→
// SetRegistries cycle at the master boundary, over a REAL TLS handshake
// against a REAL Postgres whose internal CA is PERSISTED (store.EnsureInternalCA),
// plus the master-restart rehearsal that doubles as the ops.md §5 restore-runbook
// proof: a brand-new master process against the SAME PG keeps the SAME CA, so an
// already-enrolled node reconnects by mTLS with NO re-enroll.
//
// What makes this an integration test and not a slice of the unit suite:
// session_auth_test.go and enroll_test.go each mint the node's client leaf
// LOCALLY via clientLeaf() (calling tlsutil directly) and exercise one slice.
// Here the leaf is obtained THROUGH the real Enroll RPC — private key generated
// on the "agent" and never sent, only the CSR crosses the wire — and then that
// exact leaf completes a real mTLS handshake against a listener whose ClientCAs
// is the SAME PG-backed CA the Enroll handler signed with. That is the end-to-end
// path a production agent walks: token → Enroll → mTLS → registries.

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
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	"github.com/ufna/birdman/master/internal/tlsutil"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// masterInstance is a running master-side agentlink stack wired exactly the
// way cmd/birdman-master/main.go wires production (mTLS agentlink v1, design
// §1/§3): the internal CA is read from Postgres (EnsureInternalCA), the gRPC
// listener's server leaf is signed by it, and ClientCAs is the active-CA pool
// (ActiveCAs) — all read from the SAME store, so any client leaf the Enroll
// handler mints chains to this listener. It runs on a real loopback-TCP
// socket; tearing one instance down and starting another against the same
// store models a master restart / PG-dump restore.
type masterInstance struct {
	addr      string
	hub       *agentlink.Hub
	stop      func() // idempotent (sync.OnceFunc): safe to call early and again at cleanup
	caCertPEM []byte // the bootstrap trust root (agent's tls_ca_file), read from the store
}

func startMasterInstance(t *testing.T, st *store.Store, mode agentlink.AuthMode) *masterInstance {
	t.Helper()
	ctx := context.Background()

	// Production reads the CA and the ClientCAs pool from the store — never
	// from a throwaway in-memory CA. Reading them here is what makes the
	// restart rehearsal meaningful: a second instance against the same PG must
	// observe the very same CA.
	caCertPEM, caKeyPEM, err := st.EnsureInternalCA(ctx)
	if err != nil {
		t.Fatalf("EnsureInternalCA: %v", err)
	}
	activeCAs, err := st.ActiveCAs(ctx)
	if err != nil {
		t.Fatalf("ActiveCAs: %v", err)
	}
	clientCAs, err := tlsutil.CAPool(activeCAs)
	if err != nil {
		t.Fatalf("CAPool: %v", err)
	}
	serverCertPEM, serverKeyPEM, err := tlsutil.IssueServerLeaf(caCertPEM, caKeyPEM, "", nil)
	if err != nil {
		t.Fatalf("IssueServerLeaf: %v", err)
	}
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("server X509KeyPair: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := agentlink.NewHub(log)
	svc := agentlink.NewService(st, hub, nil, nil, mode, log)

	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
	})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp loopback: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(creds))
	agentlinkv1.RegisterAgentLinkServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()

	stop := sync.OnceFunc(srv.Stop)
	t.Cleanup(stop)
	return &masterInstance{addr: lis.Addr().String(), hub: hub, stop: stop, caCertPEM: caCertPEM}
}

// enrollOverTLS drives the agent's real enrollment: it generates an ECDSA
// P-256 identity key that NEVER leaves (only its public half rides out inside
// the CSR), calls Enroll{node_token, csr} over the server-auth `bootstrap`
// connection, and assembles the mTLS client certificate from the returned leaf
// PEM + the local key. Returns the assembled tls.Certificate (ready to offer
// on a Session handshake), the parsed leaf, and the raw response.
func enrollOverTLS(t *testing.T, bootstrap agentlinkv1.AgentLinkClient, token, agentVersion string) (tls.Certificate, *x509.Certificate, *agentlinkv1.EnrollResponse) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("agent identity key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: "csr-subject-ignored"}}, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	resp, err := bootstrap.Enroll(enrollCtx(t), &agentlinkv1.EnrollRequest{
		NodeToken:    token,
		CsrPem:       csrPEM,
		AgentVersion: agentVersion,
	})
	if err != nil {
		t.Fatalf("Enroll over TLS: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal agent key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(resp.GetCertPem(), keyPEM)
	if err != nil {
		t.Fatalf("assemble client cert from enroll response: %v", err)
	}
	return cert, parseCertPEM(t, resp.GetCertPem()), resp
}

// TestIntegrationTokenEnrollMTLSRegistriesCycle drives the whole cycle in the
// STRICT mtls end-state (Фаза D step 3): a new node has only a node_token, yet
//  1. enrolls by token over a certless server-auth connection (Enroll stays
//     open in mtls — otherwise a fresh node could never join, design §3),
//  2. reconnects a Session with the issued leaf over a real mTLS handshake and
//     is authenticated BY THE CERT (it sends an EMPTY node_token — in mtls a
//     token-only Hello is refused, so a successful empty-token session can
//     only have been carried by the client cert; identity = CN = node_id),
//  3. receives the registries snapshot (the SetRegistries transport gate opens
//     for a cert-authenticated session), and
//  4. a token-only client (no cert) is refused — the negative that Фаза D
//     step 3 rehearses on the box.
func TestIntegrationTokenEnrollMTLSRegistriesCycle(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	const regToken = "tok-integration-mtls"
	seedRegistry(t, st, "ghcr.io", regToken)

	m := startMasterInstance(t, st, agentlink.AuthMTLS)

	// (1) Bootstrap: server-auth TLS, no client cert yet — the agent trusts
	// the CA delivered out of band (here: the CA cert from the store, which
	// ansible ships as master-ca.pem). Enroll by node_token mints the leaf.
	bootstrap := tlsClient(t, m.addr, m.caCertPEM, nil)
	clientCert, leaf, resp := enrollOverTLS(t, bootstrap, f.NodeToken, "0.2.0")

	if leaf.Subject.CommonName != f.NodeID {
		t.Fatalf("issued leaf CN = %q, want node id %q", leaf.Subject.CommonName, f.NodeID)
	}
	if got := resp.GetNotAfterUnix(); got != leaf.NotAfter.Unix() {
		t.Fatalf("not_after_unix = %d, want %d", got, leaf.NotAfter.Unix())
	}
	// The response bundle is a usable trust anchor and matches the store's
	// active-CA set (it is what the agent persists as ca.pem).
	if len(resp.GetCaBundlePem()) == 0 {
		t.Fatal("enroll response carried no ca_bundle_pem")
	}
	cas, err := st.ActiveCAs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cas) != 1 || string(resp.GetCaBundlePem()) != string(cas[0]) {
		t.Fatalf("ca_bundle_pem does not match the store's ActiveCAs")
	}
	// The enrollment was recorded atomically: cert fields + exactly one
	// node_enrolled event, no renewal event on a first exchange.
	n, err := st.GetNode(context.Background(), f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n.CertSerial == nil || *n.CertSerial != leaf.SerialNumber.Text(16) {
		t.Fatalf("nodes.cert_serial = %v, want %s", n.CertSerial, leaf.SerialNumber.Text(16))
	}
	if n.EnrolledAt == nil {
		t.Fatal("nodes.enrolled_at not set by Enroll")
	}
	if got := countEvents(t, st, store.EventNodeEnrolled); got != 1 {
		t.Fatalf("node_enrolled events = %d, want 1", got)
	}
	if got := countEvents(t, st, store.EventNodeCertRenewed); got != 0 {
		t.Fatalf("node_cert_renewed events = %d, want 0 on first enrollment", got)
	}

	// (2)+(3) mTLS Session with the enrolled leaf and an EMPTY token: the
	// handshake presents the cert (RootCAs from the response bundle), the CN
	// authorizes the node, and the registries preface is delivered.
	agent := tlsClient(t, m.addr, resp.GetCaBundlePem(), &clientCert)
	stream, cancel := openStream(t, agent, "") // empty token → cert is the only credential
	defer cancel()

	in, err := stream.Recv()
	if err != nil {
		t.Fatalf("mTLS session preface recv: %v", err)
	}
	snap := in.GetSetRegistries()
	if snap == nil {
		t.Fatalf("cert-authenticated session must receive the registries preface, got %+v", in)
	}
	if regs := snap.GetRegistries(); len(regs) != 1 || regs[0].GetToken() != regToken {
		t.Fatalf("registries snapshot mismatch over mTLS: %+v", regs)
	}

	// The session was recorded as cert-authenticated (identity from the cert,
	// not a token) — this is the sessions{auth="mtls"} the operator watches
	// before flipping to strict mtls (design §3).
	waitConnected(t, m.hub, f.NodeID)
	eventually(t, "session counted as mtls", func() bool {
		mtls, token := m.hub.SessionAuthCounts()
		return mtls == 1 && token == 0
	})

	// (4) Negative (Фаза D step 3): a token-only client — a valid token but no
	// client cert — is refused in mtls. Only the cert opens a session now.
	requireDenied(t, tlsClient(t, m.addr, m.caCertPEM, nil), f.NodeToken)
}

// TestIntegrationMasterRestartReconnectsMTLSWithoutReenroll is the ops.md §5
// restore-runbook rehearsal (design §1 "Где ключ CA", §Тестирование
// "integration"): the internal CA lives in Postgres, so a master that is torn
// down and rebuilt against the SAME store keeps the SAME CA — and a node that
// enrolled before the restart reconnects by mTLS with its existing leaf, no
// re-enrollment, because that leaf still chains to the (unchanged) ClientCAs.
// If the CA were regenerated on restart, the old leaf would fail the handshake
// and this test would fail at the Session — which is exactly the failure the
// restore runbook must never hit.
func TestIntegrationMasterRestartReconnectsMTLSWithoutReenroll(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	seedRegistry(t, st, "ghcr.io", "tok-restore")

	// Instance A: enroll the node and confirm mTLS works against it.
	instanceA := startMasterInstance(t, st, agentlink.AuthMTLS)
	clientCert, leaf, resp := enrollOverTLS(t,
		tlsClient(t, instanceA.addr, instanceA.caCertPEM, nil), f.NodeToken, "0.2.0")
	caBundle := resp.GetCaBundlePem()

	streamA, cancelA := openStream(t, tlsClient(t, instanceA.addr, caBundle, &clientCert), "")
	if _, err := streamA.Recv(); err != nil {
		t.Fatalf("mTLS session against instance A failed: %v", err)
	}
	cancelA()

	caBefore, err := st.ActiveCAs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	serialBefore := leaf.SerialNumber.Text(16)
	if got := countEvents(t, st, store.EventNodeEnrolled); got != 1 {
		t.Fatalf("node_enrolled events before restart = %d, want 1", got)
	}

	// Restart: stop instance A entirely, then build a fresh master stack
	// (new Hub, new Service, new listener, freshly-read CA) against the SAME
	// store — a master process restart / PG-dump restore.
	instanceA.stop()
	instanceB := startMasterInstance(t, st, agentlink.AuthMTLS)

	// The CA survived unchanged (persisted in PG, not regenerated) — both the
	// active-CA bundle and the server-facing CA cert are byte-identical.
	caAfter, err := st.ActiveCAs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(caBefore) != 1 || len(caAfter) != 1 || string(caBefore[0]) != string(caAfter[0]) {
		t.Fatalf("internal CA changed across restart: the restore runbook would break the fleet")
	}
	if string(instanceA.caCertPEM) != string(instanceB.caCertPEM) {
		t.Fatalf("EnsureInternalCA returned a different CA on the restarted master")
	}

	// The pre-restart leaf still authenticates a FRESH Session against the new
	// master — no re-enroll — carrying an empty token, so only the cert can
	// have opened it.
	streamB, cancelB := openStream(t, tlsClient(t, instanceB.addr, caBundle, &clientCert), "")
	defer cancelB()
	in, err := streamB.Recv()
	if err != nil {
		t.Fatalf("pre-restart leaf must still authenticate against the restarted master: %v", err)
	}
	if snap := in.GetSetRegistries(); snap == nil || len(snap.GetRegistries()) != 1 || snap.GetRegistries()[0].GetToken() != "tok-restore" {
		t.Fatalf("restored master must deliver the registries preface to the reconnected node, got %+v", in)
	}
	waitConnected(t, instanceB.hub, f.NodeID)

	// No re-enrollment happened: no new node_enrolled/renewed event, and the
	// stored cert serial is untouched — the node simply reused its leaf.
	if got := countEvents(t, st, store.EventNodeEnrolled); got != 1 {
		t.Fatalf("node_enrolled events after restart = %d, want still 1 (no re-enroll)", got)
	}
	if got := countEvents(t, st, store.EventNodeCertRenewed); got != 0 {
		t.Fatalf("node_cert_renewed events after restart = %d, want 0 (no re-enroll)", got)
	}
	n, err := st.GetNode(context.Background(), f.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n.CertSerial == nil || *n.CertSerial != serialBefore {
		t.Fatalf("cert_serial changed across restart (%v, want %s) — a silent re-enroll", n.CertSerial, serialBefore)
	}
}
