package store_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"net/url"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for golang-migrate

	"github.com/ufna/birdman/master/internal/secrets"
	"github.com/ufna/birdman/master/internal/testdb"
	"github.com/ufna/birdman/master/migrations"
)

// TestRegistryTypeMigration exercises migration 000010 end to end (Реестры v2
// §1): the down drops the type column, the up re-adds it with default
// 'generic' AND backfills a pre-existing ghcr.io row to 'ghcr'. The backfill
// can only be observed against a row that predates the column, so the test
// steps the schema down to v9, seeds rows, then steps back up to v10.
func TestRegistryTypeMigration(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("random key: %v", err)
	}
	codec, err := secrets.New(key)
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	// NewWithCodec creates a fresh DB migrated to head and returns its DSN.
	_, dsn := testdb.NewWithCodec(t, codec)
	dsn = stripPoolParams(t, dsn) // sql.Open("pgx", ...) rejects pgxpool-only params

	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	m := newMigrateHandle(t, dsn)

	// Down 000010 → the type column must be gone.
	if err := m.Migrate(9); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate down to v9: %v", err)
	}
	if registriesHasTypeColumn(t, ctx, db) {
		t.Fatal("after down to v9, registries.type must not exist")
	}

	// Seed rows that predate the column: a ghcr.io row (the backfill target)
	// and a non-ghcr row (must get the 'generic' default).
	if _, err := db.ExecContext(ctx, `insert into registries (host, username, token, note) values
		('ghcr.io','alice','x',''),
		('other.example','bob','y','')`); err != nil {
		t.Fatalf("seed v9 rows: %v", err)
	}

	// Up 000010 → column re-added (default 'generic'), ghcr.io backfilled to 'ghcr'.
	if err := m.Migrate(10); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to v10: %v", err)
	}
	if !registriesHasTypeColumn(t, ctx, db) {
		t.Fatal("after up to v10, registries.type must exist")
	}
	if got := registryType(t, ctx, db, "ghcr.io"); got != "ghcr" {
		t.Fatalf("backfill: ghcr.io type = %q, want ghcr", got)
	}
	if got := registryType(t, ctx, db, "other.example"); got != "generic" {
		t.Fatalf("default: other.example type = %q, want generic", got)
	}

	// The check constraint rejects an off-menu type.
	if _, err := db.ExecContext(ctx, `update registries set type='nope' where host='ghcr.io'`); err == nil {
		t.Fatal("the type check constraint must reject an unknown value")
	}
}

func newMigrateHandle(t *testing.T, dsn string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("migrations fs: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("migrate open db: %v", err)
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The handle owns its db; the test DB is dropped on testdb cleanup, so no
	// m.Close() (which would also race the shared drop).
	return m
}

func registriesHasTypeColumn(t *testing.T, ctx context.Context, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`select count(*) from information_schema.columns where table_name='registries' and column_name='type'`).Scan(&n); err != nil {
		t.Fatalf("column check: %v", err)
	}
	return n > 0
}

func registryType(t *testing.T, ctx context.Context, db *sql.DB, host string) string {
	t.Helper()
	var typ string
	if err := db.QueryRowContext(ctx, `select type from registries where host=$1`, host).Scan(&typ); err != nil {
		t.Fatalf("select type for %q: %v", host, err)
	}
	return typ
}

// stripPoolParams removes pgxpool-only query params (pool_max_conns) from a
// DSN so the database/sql pgx driver — which parses it via pgconn, not
// pgxpool — does not send them as unknown server startup params.
func stripPoolParams(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	q.Del("pool_max_conns")
	u.RawQuery = q.Encode()
	return u.String()
}
