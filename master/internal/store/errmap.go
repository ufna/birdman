package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// envFKConstraints are the (project_id, env) → environments foreign keys added
// by migration 000013_environments. A 23503 violation on any of them means the
// row's environment vanished — in practice a race with DELETE /v1/environments
// between an in-tx «does this env exist?» pre-check and the insert.
var envFKConstraints = map[string]bool{
	"versions_env_fk": true, // versions(project_id, env)
	"fleet_env_fk":    true, // fleet_configs(project_id, env)
	"api_keys_env_fk": true, // api_keys(project_id, env)
	"nodes_env_fk":    true, // nodes(project_id, env) — for parity/future callers
}

// mapEnvFKViolation turns a 23503 foreign-key violation on one of the env FKs
// (envFKConstraints) into a clean «no such environment» ErrNotFound, so the
// operator gets a 400 «no such environment» instead of a raw 500 when the
// environment is dropped in the TOCTOU window between the existence pre-check
// and the insert. The pre-checks handle the common case; this closes the race.
// Returns nil when err is NOT such a violation — the caller keeps its own
// handling (a different constraint, a unique 23505, or a plain error).
func mapEnvFKViolation(err error, project, env string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" && envFKConstraints[pgErr.ConstraintName] {
		return fmt.Errorf("no such environment %s/%s: %w", project, env, ErrNotFound)
	}
	return nil
}
