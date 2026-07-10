// Package store is the Postgres persistence layer of birdman-master
// (pgx/v5). All state lives in Postgres (ADR-3); this package owns every SQL
// statement in the process.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for golang-migrate

	"github.com/ufna/birdman/master/internal/secrets"
	"github.com/ufna/birdman/master/migrations"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// CommandSender dispatches a command to a node's agent. Implemented by
// agentlink.Hub (cmd_id/Ack tracking, replay on reconnect); fakes in tests.
// Send must not block: commands to offline nodes stay queued in the hub.
//
// The store owns the AllocateServer dispatch (итерация 2): a successful
// allocation must reach the dedik's liba whichever door it came through —
// REST POST /v1/allocate or the built-in matchmaker — and store.Allocate is
// the single shared point of both paths.
type CommandSender interface {
	Send(nodeID string, msg *agentlinkv1.MasterMsg) (cmdID string)
}

type Store struct {
	Pool *pgxpool.Pool

	// codec encrypts/decrypts the reversible at-rest secrets (registries.token,
	// internal_ca.key_pem). Required — every Open supplies one (main via the
	// loaded box key, testdb via a random per-database key). The read/write
	// paths that touch those two columns go through it; nothing else does.
	codec  *secrets.Codec
	sender CommandSender // nil until SetCommandSender (some tests)
}

// SetCommandSender wires the agent command dispatcher. Call once at startup,
// before the API/matchmaker start allocating.
func (s *Store) SetCommandSender(sender CommandSender) { s.sender = sender }

// Open connects a pgx pool, verifies connectivity, and binds the at-rest
// secrets codec. codec is mandatory — the master loads it from the box key at
// startup (fail-loud if absent, see cmd/birdman-master) and testdb generates a
// random one per database, so every Store can encrypt/decrypt its reversible
// secrets. Callers must run EncryptExistingSecrets before the first secret read
// (main does, immediately after Open) so legacy plaintext rows are upgraded
// under the strict-read invariant.
func Open(ctx context.Context, dsn string, codec *secrets.Codec) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{Pool: pool, codec: codec}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// MigrateUp applies embedded migrations (auto-upgrade on start).
func MigrateUp(dsn string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("migrate driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Ping verifies database connectivity (used by /healthz).
func (s *Store) Ping(ctx context.Context) error {
	return s.Pool.Ping(ctx)
}

// ErrNotFound is returned when a referenced entity does not exist.
var ErrNotFound = errors.New("not_found")
