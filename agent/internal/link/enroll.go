package link

// Agent-side mTLS enrollment, renewal and trust (mTLS agentlink v1,
// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §4).
//
// The node's private key is generated locally and NEVER leaves the box: it
// reaches the master only as the public key inside a CSR (proof of possession).
// The node_token is a bootstrap/recovery credential — it authenticates the
// FIRST Enroll (or an Enroll after the cert fully expired) and is otherwise off
// the wire (a cert session's Hello carries no token). Renewal re-enrolls over
// the live mTLS connection with an empty token: the master authorizes by the
// presented client certificate. All three files (client.key 0600, client.crt,
// ca.pem) are written tmp+rename so a crash can never leave a torn credential.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// errRenew unwinds the session loop after the client certificate was renewed:
// the connection is torn down and rebuilt with the fresh material (a link
// reconnect is a normal event — master survives, aggregates intact, dedics
// untouched, agent.md §2).
var errRenew = errors.New("link: client certificate renewed, reconnecting")

// transport is one dialled connection plus how the session authenticates on it.
type transport struct {
	conn    *grpc.ClientConn
	client  agentlinkv1.AgentLinkClient
	certSes bool // true → mTLS (Hello without token); false → token-auth Hello
}

func (c *Client) certPaths() (key, crt, ca string) {
	d := c.cfg.TLSCertDir
	return filepath.Join(d, "client.key"), filepath.Join(d, "client.crt"), filepath.Join(d, "ca.pem")
}

func (c *Client) serverName() string {
	if c.cfg.TLSServerName != "" {
		return c.cfg.TLSServerName
	}
	return "birdman-master"
}

// trustPool is the union of the PEM blocks in tls_ca_file (bootstrap trust,
// delivered by ansible) and {tls_cert_dir}/ca.pem (the bundle from the last
// EnrollResponse). Both are public certs, so unioning is monotonic and safe. A
// configured-but-unreadable tls_ca_file is an error; a missing ca.pem (not yet
// enrolled) is skipped.
func (c *Client) trustPool() (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	added := 0
	if c.cfg.TLSCAFile != "" {
		pemBytes, err := os.ReadFile(c.cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read tls_ca_file: %w", err)
		}
		if pool.AppendCertsFromPEM(pemBytes) {
			added++
		}
	}
	_, _, caPath := c.certPaths()
	if pemBytes, err := os.ReadFile(caPath); err == nil {
		if pool.AppendCertsFromPEM(pemBytes) {
			added++
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", caPath, err)
	}
	if added == 0 {
		return nil, fmt.Errorf("empty trust pool: neither tls_ca_file nor %s holds a CA cert", caPath)
	}
	return pool, nil
}

func (c *Client) mtlsCreds(pair tls.Certificate) (credentials.TransportCredentials, error) {
	pool, err := c.trustPool()
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		ServerName:   c.serverName(),
	}), nil
}

func (c *Client) serverAuthCreds() (credentials.TransportCredentials, error) {
	pool, err := c.trustPool()
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: c.serverName(),
	}), nil
}

func (c *Client) dial(creds credentials.TransportCredentials) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if c.cfg.DialContext != nil {
		opts = append(opts, grpc.WithContextDialer(c.cfg.DialContext))
	}
	return grpc.NewClient(c.cfg.MasterAddr, opts...)
}

// loadClientLeaf loads the on-disk client keypair and its parsed leaf. Absent
// or unreadable material returns an error (→ enrollment).
func (c *Client) loadClientLeaf() (tls.Certificate, *x509.Certificate, error) {
	keyPath, crtPath, _ := c.certPaths()
	pair, err := tls.LoadX509KeyPair(crtPath, keyPath)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pair.Leaf = leaf
	return pair, leaf, nil
}

// acquireTransport establishes a connection whose credentials match the current
// TLS material, enrolling first if there is no usable client cert. The gRPC
// connection is rebuilt (not reused) whenever the material changes — that is
// the whole point of returning a fresh transport rather than a shared conn.
func (c *Client) acquireTransport(ctx context.Context) (*transport, error) {
	// Escape hatches that keep today's behaviour byte-for-byte:
	//   - DialOptions override: caller owns the transport (existing tests).
	//   - tls_insecure: dev/loopback debug — token auth over an unverified TLS
	//     conn, no enrollment (validated to loopback-only in config).
	if len(c.cfg.DialOptions) > 0 {
		conn, err := grpc.NewClient(c.cfg.MasterAddr, c.cfg.DialOptions...)
		if err != nil {
			return nil, err
		}
		c.setCertNotAfter(time.Time{})
		return &transport{conn: conn, client: agentlinkv1.NewAgentLinkClient(conn)}, nil
	}
	if c.cfg.TLSInsecure {
		conn, err := c.dial(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true})) //nolint:gosec // dev/loopback only (config-gated)
		if err != nil {
			return nil, err
		}
		c.setCertNotAfter(time.Time{})
		return &transport{conn: conn, client: agentlinkv1.NewAgentLinkClient(conn)}, nil
	}

	// Managed mTLS: a valid, unexpired cert dials mTLS straight away.
	if pair, leaf, err := c.loadClientLeaf(); err == nil && time.Now().Before(leaf.NotAfter) {
		c.setCertNotAfter(leaf.NotAfter)
		creds, err := c.mtlsCreds(pair)
		if err != nil {
			return nil, err
		}
		conn, err := c.dial(creds)
		if err != nil {
			return nil, err
		}
		return &transport{conn: conn, client: agentlinkv1.NewAgentLinkClient(conn), certSes: true}, nil
	}
	// No cert, or expired (node was down > TTL — mTLS handshake is impossible):
	// self-heal by enrolling with the on-disk token.
	return c.enrollByToken(ctx)
}

// enrollByToken opens a server-authenticated connection (master verified, no
// client cert), swaps the node_token for a signed client leaf via Enroll, and
// redials mTLS with it. codes.Unimplemented (old master) → WARN and fall back
// to a token-auth Hello on that same verified connection.
func (c *Client) enrollByToken(ctx context.Context) (*transport, error) {
	creds, err := c.serverAuthCreds()
	if err != nil {
		return nil, err
	}
	conn, err := c.dial(creds)
	if err != nil {
		return nil, err
	}
	client := agentlinkv1.NewAgentLinkClient(conn)
	keyPEM, csrPEM, err := generateKeyAndCSR()
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp, err := client.Enroll(ctx, &agentlinkv1.EnrollRequest{
		NodeToken:    c.cfg.NodeToken,
		CsrPem:       csrPEM,
		AgentVersion: c.cfg.AgentVersion,
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			c.logf("[link] master does not implement Enroll (Unimplemented) — falling back to token-auth Hello over verified TLS")
			c.setCertNotAfter(time.Time{})
			return &transport{conn: conn, client: client}, nil
		}
		conn.Close()
		return nil, err // PermissionDenied → outer max-backoff; others → normal backoff
	}
	if err := c.writeMaterial(keyPEM, resp.GetCertPem(), resp.GetCaBundlePem()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("persist enrolled certificate: %w", err)
	}
	conn.Close() // done with the server-auth conn; redial mTLS with the fresh cert

	pair, leaf, err := c.loadClientLeaf()
	if err != nil {
		return nil, fmt.Errorf("reload enrolled certificate: %w", err)
	}
	c.setCertNotAfter(leaf.NotAfter)
	mtlsCreds, err := c.mtlsCreds(pair)
	if err != nil {
		return nil, err
	}
	mconn, err := c.dial(mtlsCreds)
	if err != nil {
		return nil, err
	}
	c.logf("[link] enrolled: received client certificate (not_after=%s) — switching to mTLS", leaf.NotAfter.UTC().Format(time.RFC3339))
	return &transport{conn: mconn, client: agentlinkv1.NewAgentLinkClient(mconn), certSes: true}, nil
}

// maybeRenew renews the client cert when it enters the renewal window
// (NotAfter-now < RenewBefore), over the CURRENT mTLS connection with an empty
// token (the master authorizes by the live cert). Returns true when the files
// were swapped — the caller then reconnects with the new material. A renewal
// error is non-fatal: the (still valid) current cert keeps the link up.
func (c *Client) maybeRenew(ctx context.Context, client agentlinkv1.AgentLinkClient) (bool, error) {
	na := c.certNotAfter.Load()
	if na == 0 {
		return false, nil
	}
	if time.Until(time.Unix(na, 0)) >= c.cfg.RenewBefore {
		return false, nil
	}
	keyPEM, csrPEM, err := generateKeyAndCSR()
	if err != nil {
		return false, err
	}
	resp, err := client.Enroll(ctx, &agentlinkv1.EnrollRequest{
		CsrPem:       csrPEM,
		AgentVersion: c.cfg.AgentVersion, // NodeToken empty: renewal authorizes by the live cert
	})
	if err != nil {
		return false, err
	}
	if err := c.writeMaterial(keyPEM, resp.GetCertPem(), resp.GetCaBundlePem()); err != nil {
		return false, err
	}
	if _, leaf, err := c.loadClientLeaf(); err == nil {
		c.setCertNotAfter(leaf.NotAfter)
	}
	c.logf("[link] client certificate renewed (not_after=%s) — reconnecting",
		time.Unix(resp.GetNotAfterUnix(), 0).UTC().Format(time.RFC3339))
	return true, nil
}

// writeMaterial persists the enrolled credential atomically. The directory is
// the agent's own (0700); client.key is 0600, the public certs 0644.
func (c *Client) writeMaterial(keyPEM, certPEM, caPEM []byte) error {
	dir := c.cfg.TLSCertDir
	if dir == "" {
		return errors.New("tls_cert_dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	keyPath, crtPath, caPath := c.certPaths()
	if err := atomicWrite(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	if err := atomicWrite(crtPath, certPEM, 0o644); err != nil {
		return err
	}
	if err := atomicWrite(caPath, caPEM, 0o644); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil { // best-effort durability of the renames
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// generateKeyAndCSR generates a fresh ECDSA P-256 identity key (never leaves the
// node) and a CSR carrying its public key. The CSR subject is cosmetic — the
// master ignores it and sets CN = node_id from the authenticated identity.
func generateKeyAndCSR() (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "birdman-node"},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	return keyPEM, csrPEM, nil
}

// setCertNotAfter records the loaded cert's NotAfter for the expiry metric
// (0 clears it: token/insecure sessions have no client cert).
func (c *Client) setCertNotAfter(t time.Time) {
	if t.IsZero() {
		c.certNotAfter.Store(0)
		return
	}
	c.certNotAfter.Store(t.Unix())
}

// CertExpiryUnix is the NotAfter (unix seconds) of the loaded client cert, or 0
// when none is loaded — feeds birdman_agent_cert_expiry_timestamp_seconds.
func (c *Client) CertExpiryUnix() int64 { return c.certNotAfter.Load() }
