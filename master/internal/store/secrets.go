package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ufna/birdman/master/internal/secrets"
)

// secretsEncryptLockKey names the PG advisory lock that serializes the startup
// encrypt-existing pass — the same idiom as internalCALockKey, so two masters
// starting at once run the pass one-at-a-time and never double-wrap a row.
const secretsEncryptLockKey = "birdman:secrets:encrypt"

// EncryptExistingSecrets encrypts any legacy plaintext registries.token /
// internal_ca.key_pem / backup_settings.s3_secret_key rows in place and returns
// how many rows it rewrote
// (design §3). The master calls it once at startup, immediately after Open and
// BEFORE the first secret read (EnsureInternalCA) — so a database written by a
// pre-encryption build is upgraded before the strict read paths would reject
// its plaintext, and an old plaintext pg_dump stays restorable forever (restore
// → start → this pass encrypts it on the spot).
//
// It is idempotent by construction: the scan is `not like 'birdman:v1:%'`, so
// an already-encrypted row is skipped and a second pass touches nothing. The
// whole pass is one transaction under a PG advisory lock. It logs nothing and
// never touches a secret value in a log line — the caller logs only the count.
func (s *Store) EncryptExistingSecrets(ctx context.Context) (int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Block (not try): a concurrent starter waits, then finds nothing to encrypt.
	if _, err := tx.Exec(ctx,
		`select pg_advisory_xact_lock(hashtextextended($1, 42))`, secretsEncryptLockKey); err != nil {
		return 0, err
	}

	nTok, err := encryptColumn(ctx, tx, s.codec, "registries", "token", "registries.token")
	if err != nil {
		return 0, err
	}
	nKey, err := encryptColumn(ctx, tx, s.codec, "internal_ca", "key_pem", "internal_ca.key_pem")
	if err != nil {
		return 0, err
	}
	nS3, err := encryptColumn(ctx, tx, s.codec, "backup_settings", "s3_secret_key", "backup_settings.s3_secret_key")
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return nTok + nKey + nS3, nil
}

// encryptColumn encrypts every legacy plaintext value in table.col (rows whose
// value is not already a birdman:v1: envelope AND is not empty) under aad,
// returning how many rows it rewrote. table/col/aad are hard-coded call-site
// constants (registries.token, internal_ca.key_pem,
// backup_settings.s3_secret_key) — never user input, so the fmt-built SQL has
// no injection surface. It buffers the scanned rows before issuing any UPDATE:
// pgx runs one statement at a time per tx connection, so the SELECT cursor must
// be drained before the UPDATEs on the same tx. `for update` locks the scanned
// rows (belt-and-suspenders alongside the advisory lock).
//
// The empty-value skip in the WHERE is load-bearing for backup_settings: its
// singleton row seeds s3_secret_key to an empty string (no secret configured),
// and wrapping that empty value in an envelope would make it non-empty and flip
// has_s3_secret (the not-empty check on the column) wrongly true.
// registries.token / internal_ca.key_pem never hold an empty value, so the skip
// is a no-op for them.
func encryptColumn(ctx context.Context, tx pgx.Tx, codec *secrets.Codec, table, col, aad string) (int, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(
		`select id::text, %s from %s where %s not like 'birdman:v1:%%' and %s <> '' for update`, col, table, col, col))
	if err != nil {
		return 0, err
	}
	type pending struct{ id, val string }
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.val); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, p := range todo {
		env, err := codec.Encrypt([]byte(p.val), aad)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx,
			fmt.Sprintf(`update %s set %s = $1 where id = $2::uuid`, table, col), env, p.id); err != nil {
			return 0, err
		}
	}
	return len(todo), nil
}
