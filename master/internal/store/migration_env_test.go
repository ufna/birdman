package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Миграция 000013 (environments v1, ERRATUM №3) — e2e-смоук на данных «до».
// Стенд регистрировал ВСЕ версии через channel='prod' при dev-флоте: маппинг
// staging→dev/prod→prod положил бы fleet-active версию в env='prod' под флотом
// 'dev' и порвал бы составной fleet_active_version_env_fk (migrate → dirty).
// Фикс — бэкфилл env='dev' для ВСЕХ версий + guard на дубли (project, semver)
// между channel'ами. Тесты шагают чистую БД ВНИЗ до v12 (channel-эра), сидят
// данные как на стенде, затем re-apply 000013 up.

// stepDownToV12 provisions a fresh DB (migrated to head) and steps the schema
// DOWN to v12 — the pre-environments, channel-era shape the стенд ran on. The
// store seeds/asserts through its pgx pool; the migrate handle drives 000013.
func stepDownToV12(t *testing.T) (*store.Store, *migrate.Migrate) {
	t.Helper()
	st, dsn := testdb.NewWithCodec(t, codecFilled(t, 0x13)) // codec irrelevant here — no secrets touched
	m := newMigrateHandle(t, stripPoolParams(t, dsn))
	if err := m.Migrate(12); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("step down to v12: %v", err)
	}
	return st, m
}

// TestEnvMigration000013BackfillsDevKeepsFleetFK is the смоук of ERRATUM №3 on
// the стенд shape: every version is channel='prod' while the fleet backfills to
// env='dev'. The pre-erratum mapping would snap the composite fleet FK; the fix
// backfills ALL versions to env='dev', so the FK holds, dev/prod are seeded, and
// no version lands in prod.
func TestEnvMigration000013BackfillsDevKeepsFleetFK(t *testing.T) {
	st, m := stepDownToV12(t)
	ctx := context.Background()
	pool := st.Pool

	// Seed the стенд as-is (v12/channel schema): project, active node, two
	// channel='prod' versions (active + disabled), fleet pointing at the active
	// one. Both versions are channel='prod', so they must differ in semver
	// (v12 unique is project+semver+channel).
	var projectID string
	if err := pool.QueryRow(ctx,
		`insert into projects (slug) values ('game') returning id::text`).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into nodes (project_id, region, hostname, public_ip, capacity_slots, last_heartbeat_at)
		values ($1::uuid, 'eu', 'node-1', '203.0.113.10', 10, now())`, projectID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	var activeVersionID string
	if err := pool.QueryRow(ctx, `
		insert into versions (project_id, semver, image_ref, channel, state)
		values ($1::uuid, '1.0.0', 'ghcr.io/example/game-server:1.0.0', 'prod', 'active')
		returning id::text`, projectID).Scan(&activeVersionID); err != nil {
		t.Fatalf("seed active version: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into versions (project_id, semver, image_ref, channel, state)
		values ($1::uuid, '0.9.0', 'ghcr.io/example/game-server:0.9.0', 'prod', 'disabled')`, projectID); err != nil {
		t.Fatalf("seed disabled version: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into fleet_configs (project_id, region, active_version)
		values ($1::uuid, 'eu', $2::uuid)`, projectID, activeVersionID); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	// Re-apply 000013 up: on the стенд data the pre-erratum mapping would die on
	// the fleet FK; ERRATUM №3 must let it through cleanly.
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up 000013 (ERRATUM №3) on стенд data: %v", err)
	}

	// Every version backfilled to env='dev', none in prod.
	var devVersions, nonDev int
	if err := pool.QueryRow(ctx,
		`select count(*) from versions where project_id = $1::uuid and env = 'dev'`, projectID).Scan(&devVersions); err != nil {
		t.Fatalf("count dev versions: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`select count(*) from versions where project_id = $1::uuid and env <> 'dev'`, projectID).Scan(&nonDev); err != nil {
		t.Fatalf("count non-dev versions: %v", err)
	}
	if devVersions != 2 || nonDev != 0 {
		t.Fatalf("all versions must backfill to env='dev', got dev=%d nonDev=%d", devVersions, nonDev)
	}

	// dev+prod environments seeded for the project.
	var envs int
	if err := pool.QueryRow(ctx, `
		select count(*) from environments e join projects p on p.id = e.project_id
		where p.slug = 'game' and e.name in ('dev','prod')`).Scan(&envs); err != nil {
		t.Fatalf("count environments: %v", err)
	}
	if envs != 2 {
		t.Fatalf("migration must seed dev+prod, got %d", envs)
	}

	// The composite fleet_active_version_env_fk is satisfiable: the fleet row and
	// its active_version share (project, env='dev'). Zero here would mean the
	// pre-erratum split had left the version in the wrong env (FK would have
	// snapped mid-migration).
	var fleetFKAlive int
	if err := pool.QueryRow(ctx, `
		select count(*) from fleet_configs f
		join versions v on v.id = f.active_version and v.project_id = f.project_id and v.env = f.env
		where f.project_id = $1::uuid and f.env = 'dev'`, projectID).Scan(&fleetFKAlive); err != nil {
		t.Fatalf("check fleet FK: %v", err)
	}
	if fleetFKAlive != 1 {
		t.Fatalf("fleet active_version must resolve within (project, dev), got %d", fleetFKAlive)
	}
}

// TestEnvMigration000013GuardRejectsDuplicateSemver covers the ERRATUM №3 guard:
// a (project, semver) present in two channels would collapse into the same
// (project, env, semver) under the blanket env='dev' backfill and violate the
// new unique — so the migration must FAIL loudly (operator resolves by hand),
// not go dirty midway.
func TestEnvMigration000013GuardRejectsDuplicateSemver(t *testing.T) {
	st, m := stepDownToV12(t)
	ctx := context.Background()
	pool := st.Pool

	var projectID string
	if err := pool.QueryRow(ctx,
		`insert into projects (slug) values ('game') returning id::text`).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// Same semver in two channels — legal at v12 (unique is project+semver+channel).
	if _, err := pool.Exec(ctx, `
		insert into versions (project_id, semver, image_ref, channel, state) values
		($1::uuid, '1.0.0', 'ghcr.io/example/game-server:1.0.0', 'staging', 'active'),
		($1::uuid, '1.0.0', 'ghcr.io/example/game-server:1.0.0', 'prod', 'active')`, projectID); err != nil {
		t.Fatalf("seed duplicate-semver versions: %v", err)
	}

	err := m.Up()
	if err == nil {
		t.Fatal("guard must reject a (project, semver) duplicated across channels")
	}
	if !strings.Contains(err.Error(), "duplicate (project, semver)") {
		t.Fatalf("want the ERRATUM №3 guard error, got: %v", err)
	}
}
