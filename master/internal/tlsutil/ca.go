package tlsutil

// Internal CA for agentlink mTLS (mTLS agentlink v1,
// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §1). One root CA
// lives in Postgres (store.EnsureInternalCA); it signs the master's server
// leaf and every node's client leaf. ECDSA P-256 throughout; NotBefore = now-1h
// on every cert (clock-skew tolerance, same pattern as EnsureServerCert). The
// node's private key never leaves the node — it arrives only as the public key
// inside a CSR, whose signature is verified (proof of possession). CA key
// material is never placed in an error string, log line, or %v.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

const (
	caCommonName     = "birdman internal CA"
	serverCommonName = "birdman-master"
	clockSkew        = time.Hour
	caYears          = 10
	leafDays         = 90
)

func randSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func marshalKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// GenerateCA creates a self-signed internal root CA (ECDSA P-256, IsCA, CN
// "birdman internal CA", 10-year TTL). Returns the public cert PEM and the
// private key PEM; the key PEM is a reversible secret and must never be logged.
func GenerateCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.AddDate(caYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true, // signs end-entity leaves only, no sub-CAs
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = marshalKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certPEM, keyPEM, nil
}

// parseCAKeypair decodes a CA cert+key PEM. Errors never echo key bytes.
func parseCAKeypair(caCertPEM, caKeyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode(caCertPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, nil, errors.New("ca cert: not a CERTIFICATE PEM")
	}
	caCert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca cert: %w", err)
	}
	kb, _ := pem.Decode(caKeyPEM)
	if kb == nil {
		return nil, nil, errors.New("ca key: not a PEM block")
	}
	caKey, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		// Deliberately generic — do not wrap/echo key material.
		return nil, nil, errors.New("ca key: invalid EC private key")
	}
	return caCert, caKey, nil
}

// IssueServerLeaf signs a 90-day server leaf for the master's gRPC listener.
// SANs: DNS birdman-master, localhost, hostname; IP 127.0.0.1, ::1; plus each
// extra SAN classified as IP or DNS. Agents verify the chain to the CA and
// ServerName "birdman-master", so IP/hostname changes are transparent.
func IssueServerLeaf(caCertPEM, caKeyPEM []byte, hostname string, extraSANs []string) (certPEM, keyPEM []byte, err error) {
	caCert, caKey, err := parseCAKeypair(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}

	dns := []string{serverCommonName, "localhost"}
	if hostname != "" && hostname != serverCommonName && hostname != "localhost" {
		dns = append(dns, hostname)
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	for _, s := range extraSANs {
		if s == "" {
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, s)
		}
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: serverCommonName},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.AddDate(0, 0, leafDays),
		// ECDSA leaves negotiate ECDHE only; KeyEncipherment is RSA
		// key-transport and would be meaningless here — DigitalSignature alone.
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = marshalKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certPEM, keyPEM, nil
}

// IssueClientLeafFromCSR signs a 90-day client leaf for a node from its CSR.
// The CSR carries only the node's public key; its signature is verified (proof
// of possession) and its subject is IGNORED — the leaf's CN is always nodeID,
// the authenticated identity. The enrolling key MUST be ECDSA P-256 (ECDSA
// P-256 throughout); a malformed CSR, a bad CSR signature, or any other key
// type/curve is an error. Returns the cert PEM and the parsed leaf
// (serial/NotAfter for the caller to persist).
func IssueClientLeafFromCSR(caCertPEM, caKeyPEM []byte, nodeID string, csrPEM []byte) (certPEM []byte, leaf *x509.Certificate, err error) {
	caCert, caKey, err := parseCAKeypair(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, nil, errors.New("csr: not a CERTIFICATE REQUEST PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, nil, fmt.Errorf("csr signature: %w", err)
	}
	// Enforce ECDSA P-256 on the enrolling key: an RSA, Ed25519, or wrong-curve
	// node identity key must never be minted into a birdman client leaf.
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, nil, errors.New("client CSR must use ECDSA P-256")
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: nodeID}, // CSR subject deliberately ignored
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.AddDate(0, 0, leafDays),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certPEM, leaf, nil
}

// CAPool builds an x509 pool from one or more CA cert PEMs (the active-CA
// bundle) — the master's ClientCAs and the agent's trust store. Errors if a
// PEM cannot be appended or if the pool ends up empty.
func CAPool(certPEMs [][]byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	added := 0
	for _, p := range certPEMs {
		if len(p) == 0 {
			continue
		}
		if !pool.AppendCertsFromPEM(p) {
			return nil, errors.New("ca pool: failed to append a CA cert PEM")
		}
		added++
	}
	if added == 0 {
		return nil, errors.New("ca pool: no CA certs provided")
	}
	return pool, nil
}

// CertNotAfter parses a certificate PEM and returns its NotAfter — populates
// internal_ca.not_after and cert-expiry metrics without re-deriving from the key.
func CertNotAfter(certPEM []byte) (time.Time, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return time.Time{}, errors.New("not a CERTIFICATE PEM")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}
