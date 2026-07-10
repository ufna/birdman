package link

// Agent-side mTLS enrollment/renewal against a bufconn fake master that speaks
// REAL TLS (mTLS agentlink v1, design §4 + §Тестирование "unit agent"). The
// listener runs VerifyClientCertIfGiven with a CA-signed server leaf (SAN DNS
// birdman-master) and ClientCAs = the test CA, so every case exercises an
// actual TLS handshake and real VerifiedChains — not an insecure stand-in.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// --- test CA + leaf issuance (stdlib only; the agent module can't import
// master/internal/tlsutil) ---

type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "birdman internal CA (test)"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// signPub signs a public key into a leaf (CN=cn) — pure (no *testing.T), so the
// enroll handler can call it from the server goroutine.
func (ca *testCA) signPub(pub any, cn string, notAfter time.Time, eku x509.ExtKeyUsage) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	notBefore := time.Now().Add(-2 * time.Hour)
	if !notBefore.Before(notAfter) { // expired-leaf case: keep NotBefore < NotAfter
		notBefore = notAfter.Add(-time.Hour)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{eku},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// signCSR verifies proof-of-possession and signs the CSR's key as a client leaf
// (CN forced to nodeID, exactly like the real master).
func (ca *testCA) signCSR(csrPEM []byte, nodeID string, notAfter time.Time) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, status.Error(codes.InvalidArgument, "bad csr pem")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "parse csr")
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, status.Error(codes.InvalidArgument, "csr signature")
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, status.Error(codes.InvalidArgument, "csr must be ECDSA P-256")
	}
	return ca.signPub(csr.PublicKey, nodeID, notAfter, x509.ExtKeyUsageClientAuth)
}

// serverLeaf issues the master's server leaf (SAN DNS birdman-master).
func (ca *testCA) serverLeaf(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "birdman-master"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 90),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"birdman-master", "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// clientLeafPEM pre-seeds a client cert+key on disk (valid or expired).
func (ca *testCA) clientLeafPEM(t *testing.T, nodeID string, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err = ca.signPub(&key.PublicKey, nodeID, notAfter, x509.ExtKeyUsageClientAuth)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	return certPEM, keyPEM
}

// --- fake mTLS master ---

func verifiedPeerLeaf(ctx context.Context) *x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}
	if len(ti.State.VerifiedChains) == 0 || len(ti.State.VerifiedChains[0]) == 0 {
		return nil
	}
	return ti.State.VerifiedChains[0][0]
}

type enrollMaster struct {
	agentlinkv1.UnimplementedAgentLinkServer
	ca            *testCA
	nodeID        string
	token         string
	unimplemented bool
	leafTTL       time.Duration // issued-leaf lifetime; 0 → 90d (real master default)

	mu                  sync.Mutex
	enrolls             []*agentlinkv1.EnrollRequest
	enrollByCert        []bool
	hellos              []*agentlinkv1.Hello
	sessionLeafNotAfter []int64 // per session: client-leaf NotAfter unix, 0 if none
}

func (m *enrollMaster) Enroll(ctx context.Context, req *agentlinkv1.EnrollRequest) (*agentlinkv1.EnrollResponse, error) {
	if m.unimplemented {
		return nil, status.Error(codes.Unimplemented, "method Enroll not implemented")
	}
	leaf := verifiedPeerLeaf(ctx)
	nodeID := m.nodeID
	byCert := leaf != nil
	if byCert {
		nodeID = leaf.Subject.CommonName // renewal: identity from the live cert
	} else if req.GetNodeToken() != m.token {
		return nil, status.Error(codes.PermissionDenied, "bad node token")
	}
	ttl := 90 * 24 * time.Hour
	if m.leafTTL > 0 {
		ttl = m.leafTTL
	}
	notAfter := time.Now().Add(ttl)
	certPEM, err := m.ca.signCSR(req.GetCsrPem(), nodeID, notAfter)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.enrolls = append(m.enrolls, req)
	m.enrollByCert = append(m.enrollByCert, byCert)
	m.mu.Unlock()
	return &agentlinkv1.EnrollResponse{
		CertPem:      certPEM,
		CaBundlePem:  m.ca.certPEM,
		NotAfterUnix: notAfter.Unix(),
	}, nil
}

func (m *enrollMaster) Session(stream agentlinkv1.AgentLink_SessionServer) error {
	var naUnix int64
	if leaf := verifiedPeerLeaf(stream.Context()); leaf != nil {
		naUnix = leaf.NotAfter.Unix()
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be Hello")
	}
	m.mu.Lock()
	m.hellos = append(m.hellos, hello)
	m.sessionLeafNotAfter = append(m.sessionLeafNotAfter, naUnix)
	m.mu.Unlock()
	for {
		if _, err := stream.Recv(); err != nil {
			return err
		}
	}
}

func serveEnrollMaster(t *testing.T, m *enrollMaster) *bufconn.Listener {
	t.Helper()
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(m.ca.certPEM) {
		t.Fatal("append client CA")
	}
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{m.ca.serverLeaf(t)},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
	})
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.Creds(creds))
	agentlinkv1.RegisterAgentLinkServer(srv, m)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis
}

func mtlsConfig(lis *bufconn.Listener, caFile, certDir, token string) Config {
	return Config{
		MasterAddr:      "passthrough:///bufconn",
		NodeToken:       token,
		Hostname:        "n1",
		Region:          "dev",
		CapacitySlots:   4,
		AgentVersion:    "test-agent",
		TLSCAFile:       caFile,
		TLSCertDir:      certDir,
		TLSServerName:   "birdman-master",
		HeartbeatEvery:  30 * time.Millisecond,
		BackoffMin:      20 * time.Millisecond,
		BackoffMax:      80 * time.Millisecond,
		RenewBefore:     14 * 24 * time.Hour,
		RenewCheckEvery: time.Hour, // renewal in tests is driven by the on-connect check
		DialContext: func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		},
	}
}

func startLinkClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c := New(cfg, &fakeHandler{}, &fakeSource{}, NewOutbox(t.Logf), t.Logf)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return c
}

func writeSeed(t *testing.T, dir string, files map[string][]byte, perms map[string]os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		perm := os.FileMode(0o644)
		if p, ok := perms[name]; ok {
			perm = p
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, perm); err != nil {
			t.Fatal(err)
		}
	}
}

func onDiskNotAfter(path string) time.Time {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return time.Time{}
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return time.Time{}
	}
	return c.NotAfter
}

// --- tests ---

// TestEnrollmentByTokenThenMTLS: a fresh agent (no cert, only tls_ca_file +
// node_token) enrolls by token, writes client.key(0600)/client.crt/ca.pem
// atomically, then dials mTLS and sends a Hello with NO token (the cert is the
// identity).
func TestEnrollmentByTokenThenMTLS(t *testing.T) {
	ca := newTestCA(t)
	m := &enrollMaster{ca: ca, nodeID: "node-uuid-1", token: goodToken}
	lis := serveEnrollMaster(t, m)

	root := t.TempDir()
	certDir := filepath.Join(root, "tls")
	caFile := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caFile, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	c := startLinkClient(t, mtlsConfig(lis, caFile, certDir, goodToken))

	eventually(t, "mTLS session presenting a client cert", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, na := range m.sessionLeafNotAfter {
			if na > 0 {
				return true
			}
		}
		return false
	})

	// client.key is 0600; all three files exist; no leftover tmp files.
	fi, err := os.Stat(filepath.Join(certDir, "client.key"))
	if err != nil {
		t.Fatalf("client.key: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("client.key perm = %v, want 0600", fi.Mode().Perm())
	}
	for _, f := range []string{"client.crt", "ca.pem"} {
		if _, err := os.Stat(filepath.Join(certDir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	ents, _ := os.ReadDir(certDir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("atomic write left a tmp file behind: %s", e.Name())
		}
	}

	// Enroll was by token (bootstrap), carried the CSR and agent_version.
	m.mu.Lock()
	if len(m.enrolls) == 0 {
		m.mu.Unlock()
		t.Fatal("no Enroll recorded")
	}
	e0, byCert0 := m.enrolls[0], m.enrollByCert[0]
	m.mu.Unlock()
	if e0.GetNodeToken() != goodToken || byCert0 {
		t.Fatalf("bootstrap enroll must be by token, got token=%q byCert=%v", e0.GetNodeToken(), byCert0)
	}
	if len(e0.GetCsrPem()) == 0 || e0.GetAgentVersion() != "test-agent" {
		t.Fatalf("enroll req: csr_len=%d agent_version=%q", len(e0.GetCsrPem()), e0.GetAgentVersion())
	}

	// Hello on a cert session carries no token.
	m.mu.Lock()
	lastHello := m.hellos[len(m.hellos)-1]
	m.mu.Unlock()
	if lastHello.GetNodeToken() != "" {
		t.Fatalf("cert-session Hello must not carry the node token, got %q", lastHello.GetNodeToken())
	}

	if c.CertExpiryUnix() <= time.Now().Unix() {
		t.Fatalf("cert-expiry metric not set to a future time: %d", c.CertExpiryUnix())
	}
}

// TestEnrollUnimplementedFallsBackToToken: an old master (no Enroll RPC) makes
// the agent WARN and fall back to today's token-auth Hello over the verified
// server-auth connection — no cert is written.
func TestEnrollUnimplementedFallsBackToToken(t *testing.T) {
	ca := newTestCA(t)
	m := &enrollMaster{ca: ca, nodeID: "node-2", token: goodToken, unimplemented: true}
	lis := serveEnrollMaster(t, m)

	root := t.TempDir()
	certDir := filepath.Join(root, "tls")
	caFile := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caFile, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	startLinkClient(t, mtlsConfig(lis, caFile, certDir, goodToken))

	eventually(t, "token-fallback Hello without a client cert", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(m.hellos) == 0 {
			return false
		}
		last := len(m.hellos) - 1
		return m.sessionLeafNotAfter[last] == 0 && m.hellos[last].GetNodeToken() == goodToken
	})

	if _, err := os.Stat(filepath.Join(certDir, "client.crt")); !os.IsNotExist(err) {
		t.Fatalf("no cert must be written on Unimplemented fallback (stat err=%v)", err)
	}
}

// TestRenewalSwapAndReconnect: a cert within the 14-day window is renewed over
// the live mTLS connection (Enroll with EMPTY token, authorized by the cert),
// the files are swapped atomically, and the link softly reconnects with the
// fresh (~90d) leaf.
func TestRenewalSwapAndReconnect(t *testing.T) {
	ca := newTestCA(t)
	m := &enrollMaster{ca: ca, nodeID: "node-3", token: goodToken}
	lis := serveEnrollMaster(t, m)

	root := t.TempDir()
	certDir := filepath.Join(root, "tls")
	caFile := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caFile, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := ca.clientLeafPEM(t, "node-3", time.Now().Add(7*24*time.Hour)) // within window
	writeSeed(t, certDir, map[string][]byte{
		"client.crt": certPEM, "client.key": keyPEM, "ca.pem": ca.certPEM,
	}, map[string]os.FileMode{"client.key": 0o600})

	c := startLinkClient(t, mtlsConfig(lis, caFile, certDir, goodToken))

	eventually(t, "renewal Enroll by cert with empty token", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i, e := range m.enrolls {
			if e.GetNodeToken() == "" && m.enrollByCert[i] {
				return true
			}
		}
		return false
	})

	fresh := time.Now().Add(80 * 24 * time.Hour)
	eventually(t, "on-disk cert swapped to the fresh leaf", func() bool {
		return onDiskNotAfter(filepath.Join(certDir, "client.crt")).After(fresh)
	})
	eventually(t, "reconnected session presents the renewed leaf", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, na := range m.sessionLeafNotAfter {
			if na > fresh.Unix() {
				return true
			}
		}
		return false
	})
	if c.CertExpiryUnix() < fresh.Unix() {
		t.Fatalf("cert-expiry metric not advanced after renewal: %d", c.CertExpiryUnix())
	}
}

// TestRenewedCertStillInWindowDoesNotSpin: a MISCONFIGURED master that issues
// leaves already inside the renewal window (TTL ≤ RenewBefore — impossible with
// the real master's hard-coded 90d, but a config bug could) must not send the
// agent into a tight errRenew→reconnect→renew loop hammering Enroll. The guard
// in maybeRenew keeps the fresh leaf, logs loudly, and defers to the daily
// ticker — so Enroll is called a BOUNDED number of times, not unboundedly.
func TestRenewedCertStillInWindowDoesNotSpin(t *testing.T) {
	ca := newTestCA(t)
	// 7d issued leaf < 14d RenewBefore → every renewal lands back in-window.
	m := &enrollMaster{ca: ca, nodeID: "node-6", token: goodToken, leafTTL: 7 * 24 * time.Hour}
	lis := serveEnrollMaster(t, m)

	root := t.TempDir()
	certDir := filepath.Join(root, "tls")
	caFile := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caFile, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := ca.clientLeafPEM(t, "node-6", time.Now().Add(7*24*time.Hour)) // seeded in-window → triggers renewal
	writeSeed(t, certDir, map[string][]byte{
		"client.crt": certPEM, "client.key": keyPEM, "ca.pem": ca.certPEM,
	}, map[string]os.FileMode{"client.key": 0o600})

	startLinkClient(t, mtlsConfig(lis, caFile, certDir, goodToken))

	// The on-connect renewal fires once; the guard then stops the reconnect
	// spin (a spin would rack up Enroll calls with no sleep between them).
	eventually(t, "the in-window renewal happened at least once", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.enrolls) >= 1
	})
	// Give a would-be spin ample time to blow the count up (RenewCheckEvery is
	// 1h in tests, so no scheduled renewal fires in this window).
	time.Sleep(300 * time.Millisecond)
	m.mu.Lock()
	n := len(m.enrolls)
	m.mu.Unlock()
	if n > 3 {
		t.Fatalf("Enroll called %d times — the renewal is spinning (misconfig guard failed)", n)
	}
}

// TestExpiredCertEnrollsByToken: a fully expired cert (node down > TTL) cannot
// renew over mTLS, so the agent self-heals via Enroll-by-token and returns on a
// fresh mTLS session.
func TestExpiredCertEnrollsByToken(t *testing.T) {
	ca := newTestCA(t)
	m := &enrollMaster{ca: ca, nodeID: "node-4", token: goodToken}
	lis := serveEnrollMaster(t, m)

	root := t.TempDir()
	certDir := filepath.Join(root, "tls")
	caFile := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caFile, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := ca.clientLeafPEM(t, "node-4", time.Now().Add(-time.Hour)) // expired
	writeSeed(t, certDir, map[string][]byte{
		"client.crt": certPEM, "client.key": keyPEM, "ca.pem": ca.certPEM,
	}, map[string]os.FileMode{"client.key": 0o600})

	startLinkClient(t, mtlsConfig(lis, caFile, certDir, goodToken))

	eventually(t, "Enroll-by-token after expiry", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i, e := range m.enrolls {
			if e.GetNodeToken() == goodToken && !m.enrollByCert[i] {
				return true
			}
		}
		return false
	})
	eventually(t, "back on a fresh mTLS session", func() bool {
		cutoff := time.Now().Add(80 * 24 * time.Hour).Unix()
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, na := range m.sessionLeafNotAfter {
			if na > cutoff {
				return true
			}
		}
		return false
	})
}

// TestExpiredLeafRejectedByServer proves WHY an expired cert forces
// Enroll-by-token: offered directly to the strict listener, an expired client
// leaf fails the TLS handshake (VerifyClientCertIfGiven still verifies a
// presented cert), so renewal-over-mTLS is impossible.
func TestExpiredLeafRejectedByServer(t *testing.T) {
	ca := newTestCA(t)
	m := &enrollMaster{ca: ca, nodeID: "node-5", token: goodToken}
	lis := serveEnrollMaster(t, m)

	certPEM, keyPEM := ca.clientLeafPEM(t, "node-5", time.Now().Add(-time.Hour))
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.certPEM)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{pair},
			RootCAs:      pool,
			ServerName:   "birdman-master",
			MinVersion:   tls.VersionTLS12,
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = agentlinkv1.NewAgentLinkClient(conn).Enroll(ctx, &agentlinkv1.EnrollRequest{CsrPem: []byte("x")})
	if err == nil {
		t.Fatal("an expired client leaf must be rejected by the mTLS handshake")
	}
	// err != nil alone is too weak: the handler would ALSO reject this garbage
	// CSR with InvalidArgument — which would mean the handshake SUCCEEDED and
	// the expired leaf was accepted. Assert the TRANSPORT rejected it: a failed
	// mTLS handshake surfaces as codes.Unavailable carrying a TLS-layer message
	// ("handshake failed: remote error: tls: ... certificate"), never as the
	// handler's InvalidArgument.
	if code := status.Code(err); code != codes.Unavailable &&
		!strings.Contains(err.Error(), "handshake") &&
		!strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expired leaf must fail at the TLS handshake (want Unavailable / TLS error), got code=%v err=%v", code, err)
	}
}

// TestTrustPoolUnion: the effective trust pool is the UNION of tls_ca_file and
// {tls_cert_dir}/ca.pem — a server leaf signed by EITHER CA verifies, a leaf
// from an unrelated CA does not.
func TestTrustPoolUnion(t *testing.T) {
	caA, caB, caC := newTestCA(t), newTestCA(t), newTestCA(t)
	root := t.TempDir()
	certDir := filepath.Join(root, "tls")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(root, "caA.pem")
	if err := os.WriteFile(caFile, caA.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.pem"), caB.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	c := New(Config{TLSCAFile: caFile, TLSCertDir: certDir}, &fakeHandler{}, &fakeSource{}, NewOutbox(t.Logf), t.Logf)
	pool, err := c.trustPool()
	if err != nil {
		t.Fatal(err)
	}

	verifyAgainst := func(ca *testCA) error {
		leafPEM, err := ca.signPub(&mustKey(t).PublicKey, "srv", time.Now().Add(24*time.Hour), x509.ExtKeyUsageServerAuth)
		if err != nil {
			return err
		}
		blk, _ := pem.Decode(leafPEM)
		leaf, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return err
		}
		_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		return err
	}
	if err := verifyAgainst(caA); err != nil {
		t.Fatalf("leaf signed by tls_ca_file CA must verify against the union pool: %v", err)
	}
	if err := verifyAgainst(caB); err != nil {
		t.Fatalf("leaf signed by ca.pem CA must verify against the union pool: %v", err)
	}
	if err := verifyAgainst(caC); err == nil {
		t.Fatal("leaf from an untrusted CA must NOT verify against the union pool")
	}
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
