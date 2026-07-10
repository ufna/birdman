package tlsutil_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/tlsutil"
)

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("not a CERTIFICATE PEM")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return c
}

// makeCSR builds a PKCS#10 CSR whose subject CN is deliberately attacker-chosen
// — the issuer must ignore it and stamp its own CN.
func makeCSR(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func aboutOneHour(d time.Duration) bool { return d > 30*time.Minute && d < 90*time.Minute }

func TestGenerateCA(t *testing.T) {
	certPEM, keyPEM, err := tlsutil.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	ca := parseLeaf(t, certPEM)
	if !ca.IsCA || !ca.BasicConstraintsValid {
		t.Errorf("IsCA=%v BasicConstraintsValid=%v, want both true", ca.IsCA, ca.BasicConstraintsValid)
	}
	if ca.Subject.CommonName != "birdman internal CA" {
		t.Errorf("CN = %q, want %q", ca.Subject.CommonName, "birdman internal CA")
	}
	if ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("KeyUsage missing CertSign")
	}
	if !aboutOneHour(time.Since(ca.NotBefore)) {
		t.Errorf("NotBefore skew = %v, want ~1h in the past", time.Since(ca.NotBefore))
	}
	if years := ca.NotAfter.Sub(ca.NotBefore).Hours() / 24 / 365; years < 9.5 || years > 10.6 {
		t.Errorf("TTL = %.2f years, want ~10", years)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		t.Fatalf("key PEM decode failed")
	}
	if _, err := x509.ParseECPrivateKey(block.Bytes); err != nil {
		t.Fatalf("parse EC private key: %v", err)
	}
}

func TestIssueServerLeaf(t *testing.T) {
	caCertPEM, caKeyPEM, err := tlsutil.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// extras: one DNS name, one IP — the issuer must classify each correctly.
	certPEM, keyPEM, err := tlsutil.IssueServerLeaf(caCertPEM, caKeyPEM, "host-42", []string{"probe.example.com", "203.0.113.7"})
	if err != nil {
		t.Fatalf("IssueServerLeaf: %v", err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("cert/key mismatch: %v", err)
	}
	leaf := parseLeaf(t, certPEM)
	if days := leaf.NotAfter.Sub(leaf.NotBefore).Hours() / 24; days < 89 || days > 91 {
		t.Errorf("TTL = %.1f days, want ~90", days)
	}
	if !aboutOneHour(time.Since(leaf.NotBefore)) {
		t.Errorf("NotBefore skew = %v, want ~1h", time.Since(leaf.NotBefore))
	}
	wantDNS := map[string]bool{"birdman-master": false, "localhost": false, "host-42": false, "probe.example.com": false}
	for _, d := range leaf.DNSNames {
		if _, ok := wantDNS[d]; ok {
			wantDNS[d] = true
		}
	}
	for d, seen := range wantDNS {
		if !seen {
			t.Errorf("missing DNS SAN %q (got %v)", d, leaf.DNSNames)
		}
	}
	for _, ip := range []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback, net.ParseIP("203.0.113.7")} {
		found := false
		for _, got := range leaf.IPAddresses {
			if got.Equal(ip) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing IP SAN %v (got %v)", ip, leaf.IPAddresses)
		}
	}
	pool, err := tlsutil.CAPool([][]byte{caCertPEM})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "birdman-master",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("server leaf must chain to CA with ServerName birdman-master: %v", err)
	}
}

func TestIssueClientLeafFromCSR(t *testing.T) {
	caCertPEM, caKeyPEM, err := tlsutil.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	nodeID := "11111111-1111-1111-1111-111111111111"
	csr := makeCSR(t, "attacker-controlled-cn")

	certPEM, leaf, err := tlsutil.IssueClientLeafFromCSR(caCertPEM, caKeyPEM, nodeID, csr)
	if err != nil {
		t.Fatalf("IssueClientLeafFromCSR: %v", err)
	}
	if leaf.Subject.CommonName != nodeID {
		t.Errorf("CN = %q, want nodeID %q (foreign CSR subject must be ignored)", leaf.Subject.CommonName, nodeID)
	}
	if parsed := parseLeaf(t, certPEM); parsed.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Errorf("returned leaf serial != PEM serial")
	}
	if days := leaf.NotAfter.Sub(leaf.NotBefore).Hours() / 24; days < 89 || days > 91 {
		t.Errorf("TTL = %.1f days, want ~90", days)
	}
	if !aboutOneHour(time.Since(leaf.NotBefore)) {
		t.Errorf("NotBefore skew = %v, want ~1h", time.Since(leaf.NotBefore))
	}
	pool, _ := tlsutil.CAPool([][]byte{caCertPEM})
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("client leaf must chain to CA with ClientAuth: %v", err)
	}
}

func TestIssueClientLeafFromCSR_Broken(t *testing.T) {
	caCertPEM, caKeyPEM, err := tlsutil.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	nodeID := "22222222-2222-2222-2222-222222222222"

	if _, _, err := tlsutil.IssueClientLeafFromCSR(caCertPEM, caKeyPEM, nodeID, []byte("not a pem")); err == nil {
		t.Errorf("garbage CSR: want error, got nil")
	}
	brokenDER := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: []byte("xxxx")})
	if _, _, err := tlsutil.IssueClientLeafFromCSR(caCertPEM, caKeyPEM, nodeID, brokenDER); err == nil {
		t.Errorf("broken-DER CSR: want error, got nil")
	}
	// Valid structure, corrupted signature (last DER byte) → CheckSignature must reject.
	csr := makeCSR(t, "x")
	block, _ := pem.Decode(csr)
	der := append([]byte(nil), block.Bytes...)
	der[len(der)-1] ^= 0xff
	tampered := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	if _, _, err := tlsutil.IssueClientLeafFromCSR(caCertPEM, caKeyPEM, nodeID, tampered); err == nil {
		t.Errorf("tampered-signature CSR: want error, got nil")
	}
}

func TestCAPoolTwoCAs(t *testing.T) {
	ca1Cert, ca1Key, err := tlsutil.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	ca2Cert, ca2Key, err := tlsutil.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := tlsutil.CAPool([][]byte{ca1Cert, ca2Cert})
	if err != nil {
		t.Fatalf("CAPool: %v", err)
	}
	for _, tc := range []struct {
		name      string
		cert, key []byte
	}{{"ca1", ca1Cert, ca1Key}, {"ca2", ca2Cert, ca2Key}} {
		_, leaf, err := tlsutil.IssueClientLeafFromCSR(tc.cert, tc.key, "node-"+tc.name, makeCSR(t, "x"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:     pool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			t.Errorf("%s leaf must verify against the 2-CA bundle pool: %v", tc.name, err)
		}
	}
	if _, err := tlsutil.CAPool(nil); err == nil {
		t.Error("empty CAPool: want error, got nil")
	}
}
