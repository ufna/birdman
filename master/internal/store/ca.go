package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ufna/birdman/master/internal/tlsutil"
)

// caKeypair is the active internal CA's public cert PEM plus its (reversible-
// secret) private key PEM — the signer. keyPEM is never logged and never
// formatted with %v — same discipline as registries.token. It is deliberately
// unexported: EnsureInternalCA is the single key-bearing path, while ActiveCAs
// exposes certs only, so the CA key has nowhere to leak from (design §5).
type caKeypair struct {
	certPEM []byte
	keyPEM  []byte
}

// internalCALockKey names the PG advisory lock that serializes first-time CA
// generation — the same idiom as reconcile's PlanFleet lock, so two masters
// starting at once create exactly one CA row.
const internalCALockKey = "birdman:internal_ca"

// rowQuerier is satisfied by *pgxpool.Pool and pgx.Tx.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// loadActiveCA returns the newest active CA's cert+key — the signer. It is the
// only key-reading query; ActiveCAs reads cert_pem alone.
func loadActiveCA(ctx context.Context, q rowQuerier) (caKeypair, bool, error) {
	var cert, key string
	err := q.QueryRow(ctx, `
		select cert_pem, key_pem from internal_ca
		where active order by created_at desc limit 1`).Scan(&cert, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return caKeypair{}, false, nil
	}
	if err != nil {
		return caKeypair{}, false, err
	}
	return caKeypair{certPEM: []byte(cert), keyPEM: []byte(key)}, true, nil
}

// EnsureInternalCA returns the active internal CA's cert+key PEM, generating
// and persisting one on the first call. Concurrent first-calls are serialized
// by a blocking PG advisory lock so exactly one CA row is ever created; the key
// survives master-box loss with the PG dump (design §1). The key PEM is a
// reversible secret — callers must never log it.
func (s *Store) EnsureInternalCA(ctx context.Context) (certPEM, keyPEM []byte, err error) {
	if ca, ok, err := loadActiveCA(ctx, s.Pool); err != nil {
		return nil, nil, err
	} else if ok {
		return ca.certPEM, ca.keyPEM, nil
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	// Block (not try) so a concurrent starter waits on the lock and then, on
	// the double-check below, sees the row we — or it — committed.
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 42))`, internalCALockKey); err != nil {
		return nil, nil, err
	}
	if ca, ok, err := loadActiveCA(ctx, tx); err != nil {
		return nil, nil, err
	} else if ok {
		return ca.certPEM, ca.keyPEM, nil
	}

	cPEM, kPEM, err := tlsutil.GenerateCA()
	if err != nil {
		return nil, nil, err
	}
	notAfter, err := tlsutil.CertNotAfter(cPEM)
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `
		insert into internal_ca (cert_pem, key_pem, not_after)
		values ($1, $2, $3)`, string(cPEM), string(kPEM), notAfter); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return cPEM, kPEM, nil
}

// ActiveCAs returns the cert PEM of every active internal CA (normally one; two
// only during a manual CA-rotation window), oldest first — the source for the
// master's ClientCAs pool (T3), the agent-facing ca_bundle_pem (T4) and GET
// /v1/ca (T7). The signing key is deliberately NOT returned: it is reachable
// only through EnsureInternalCA (the signer path), so /v1/ca has nowhere to
// read it from by construction (design §5).
func (s *Store) ActiveCAs(ctx context.Context) ([][]byte, error) {
	rows, err := s.Pool.Query(ctx, `
		select cert_pem from internal_ca
		where active order by created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var cert string
		if err := rows.Scan(&cert); err != nil {
			return nil, err
		}
		out = append(out, []byte(cert))
	}
	return out, rows.Err()
}

// SetNodeCert records a freshly issued client cert for a node AND writes the
// matching audit event (EventNodeEnrolled for the first token→cert exchange,
// EventNodeCertRenewed for a renewal over a live cert) in ONE transaction —
// the Enroll handler (design §3) must never leave a cert recorded without its
// audit trail or vice versa (same tx+insertEvent idiom as setNodeDrain).
// serial and expiry feed admission/metrics; enrolled_at marks the FIRST
// enrollment and is preserved across renewals (coalesce). The event payload
// is exactly {serial, not_after, agent_version} — never the node_token,
// never key material. Returns ErrNotFound (nothing written) if the node no
// longer exists.
func (s *Store) SetNodeCert(ctx context.Context, nodeID, serial string, notAfter time.Time, eventKind, agentVersion string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	ct, err := tx.Exec(ctx, `
		update nodes
		set cert_serial = $2, cert_not_after = $3, enrolled_at = coalesce(enrolled_at, now())
		where id = $1::uuid`, nodeID, serial, notAfter)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := insertEvent(ctx, tx, eventKind, EventRef{NodeID: &nodeID}, map[string]any{
		"serial":        serial,
		"not_after":     notAfter.UTC().Format(time.RFC3339),
		"agent_version": agentVersion,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
