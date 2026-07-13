package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Environments v1 — env-скоупинг deploy-цепочки (spec §3/§4): флип, busy-check,
// TTL-дизейбл (M4) и rollback-таргет живут строго внутри (project, env). Ноды и
// флоты обоих env делят ОДИН регион eu — именно эту коллизию проверяем.

func envVersionState(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	v, err := st.GetVersion(context.Background(), id)
	if err != nil {
		t.Fatalf("get version %s: %v", id, err)
	}
	return v.State
}

func envFleetActiveVersion(t *testing.T, st *store.Store, env, region string) string {
	t.Helper()
	var av *string
	err := st.Pool.QueryRow(context.Background(), `
		select f.active_version::text from fleet_configs f
		join projects p on p.id = f.project_id
		where p.slug = 'game' and f.env = $1 and f.region = $2`, env, region).Scan(&av)
	if err != nil {
		t.Fatalf("fleet %s/%s active_version: %v", env, region, err)
	}
	if av == nil {
		return ""
	}
	return *av
}

// TestActivateVersionEnvScoped: у проекta active в dev И active в prod; активация
// новой dev-версии демоутит ТОЛЬКО dev-active (prod-active остаётся active) и
// перепойнчивает ТОЛЬКО dev-флоты. Без env-скоупа флип демоутит prod-active и
// пытается посадить dev-версию в prod-флот → нарушение fleet_active_version_env_fk.
func TestActivateVersionEnvScoped(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev node A, dev version 1.0.0
	ctx := context.Background()

	// dev active 1.0.0 под dev-флотом eu.
	mustUpsertFleet(t, st, "game", "dev", "eu", f.VersionID)

	// prod active 2.0.0 под prod-флотом eu (та же региональная коллизия).
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatalf("move node to prod: %v", err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	mustUpsertFleet(t, st, "game", "prod", "eu", prodV)

	// Новая dev-версия → prepulling → активируем.
	devV := f.AddVersion(t, "1.1.0", "dev")
	if _, err := st.Pool.Exec(ctx,
		`update versions set state = 'prepulling' where id = $1::uuid`, devV); err != nil {
		t.Fatal(err)
	}
	res, err := st.ActivateVersion(ctx, devV, "prepulling", store.EventDeployActivated, nil)
	if err != nil {
		t.Fatalf("activate dev version: %v", err)
	}

	// dev флипнулся: 1.1.0 active, 1.0.0 deprecated.
	if got := envVersionState(t, st, devV); got != "active" {
		t.Fatalf("new dev version: want active, got %s", got)
	}
	if got := envVersionState(t, st, f.VersionID); got != "deprecated" {
		t.Fatalf("old dev version: want deprecated, got %s", got)
	}
	// prod НЕ тронут.
	if got := envVersionState(t, st, prodV); got != "active" {
		t.Fatalf("prod active must survive dev flip, got %s (cross-env demote)", got)
	}
	// fleet repoint только dev; prod-флот всё ещё на 2.0.0.
	if got := envFleetActiveVersion(t, st, "dev", "eu"); got != devV {
		t.Fatalf("dev fleet active_version: want %s, got %s", devV, got)
	}
	if got := envFleetActiveVersion(t, st, "prod", "eu"); got != prodV {
		t.Fatalf("prod fleet active_version must stay %s, got %s (cross-env repoint)", prodV, got)
	}
	// demote вернул только dev-old; repoint — только регион dev-флота.
	if res.OldActive == nil || res.OldActive.ID != f.VersionID {
		t.Fatalf("OldActive: want dev %s, got %+v", f.VersionID, res.OldActive)
	}
	if len(res.Regions) != 1 || res.Regions[0] != "eu" {
		t.Fatalf("repointed regions: want [eu] (dev only), got %v", res.Regions)
	}
}

// TestBeginDeployBusyPerEnv: in-flight dev-деплой (dev-версия prepulling) НЕ
// блокирует BeginDeploy prod-версии — busy-check скоуплен (project, env).
func TestBeginDeployBusyPerEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev node, dev version 1.0.0
	ctx := context.Background()

	f.UpsertFleet(t, 2, 50) // dev fleet eu

	// prod: нода, версия, флот (для hasFleet per env).
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatal(err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	buffer, maxServers := int32(2), int32(50)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu",
		BufferReady: &buffer, MaxServers: &maxServers,
	}); err != nil {
		t.Fatal(err)
	}

	// In-flight dev-деплой: dev-версия 1.1.0 в prepulling.
	devV := f.AddVersion(t, "1.1.0", "dev")
	if _, err := st.Pool.Exec(ctx,
		`update versions set state = 'prepulling' where id = $1::uuid`, devV); err != nil {
		t.Fatal(err)
	}

	// prod-деплой НЕ блокируется dev-prepulling.
	res, err := st.BeginDeploy(ctx, prodV)
	if err != nil {
		t.Fatalf("prod BeginDeploy must not be blocked by a dev prepull: %v", err)
	}
	if res.Version.State != "prepulling" {
		t.Fatalf("prod version: want prepulling, got %s", res.Version.State)
	}

	// Позитивный контроль: второй dev-деплой всё ещё блокируется своим env.
	devV2 := f.AddVersion(t, "1.2.0", "dev")
	if _, err := st.BeginDeploy(ctx, devV2); !errors.Is(err, store.ErrDeployInProgress) {
		t.Fatalf("second dev deploy must be blocked within env: got %v", err)
	}
}

// TestDisableExpiredDeprecatedPerEnv (M4): TTL-дизейбл берёт max(reap_ttl_min)
// ФЛОТОВ ENV версии, не всего проекта. dev-версия с коротким dev-TTL дизейблится,
// prod-версия того же возраста с длинным prod-TTL — выживает. Раньше max по
// проекту (1000) не дизейблил бы ни одну.
func TestDisableExpiredDeprecatedPerEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev version 1.0.0
	ctx := context.Background()

	// dev-флот с КОРОТКИМ reap_ttl; prod-флот с ДЛИННЫМ.
	devTTL, prodTTL := int32(5), int32(1000)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu", ReapTTLMin: &devTTL,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu", ReapTTLMin: &prodTTL,
	}); err != nil {
		t.Fatal(err)
	}

	// dev-версия deprecated 10 минут назад — старше своего dev-TTL(5), но младше
	// prod-TTL(1000).
	devV := f.VersionID
	if _, err := st.Pool.Exec(ctx,
		`update versions set state='deprecated', deprecated_at = now() - interval '10 minutes' where id = $1::uuid`, devV); err != nil {
		t.Fatal(err)
	}
	// prod-версия deprecated 10 минут назад — младше своего prod-TTL(1000).
	prodV := f.AddVersion(t, "2.0.0", "prod")
	if _, err := st.Pool.Exec(ctx,
		`update versions set state='deprecated', deprecated_at = now() - interval '10 minutes' where id = $1::uuid`, prodV); err != nil {
		t.Fatal(err)
	}

	disabled, err := st.DisableExpiredDeprecated(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 1 || disabled[0].ID != devV {
		t.Fatalf("want only dev version disabled by its own env TTL, got %+v", disabled)
	}
	if got := envVersionState(t, st, devV); got != "disabled" {
		t.Fatalf("dev version: want disabled, got %s", got)
	}
	if got := envVersionState(t, st, prodV); got != "deprecated" {
		t.Fatalf("prod version must survive (its env TTL 1000m not elapsed), got %s", got)
	}
}

// TestRollbackTargetEnv: deprecated есть в ОБОИХ env; RollbackTarget(project,env)
// отдаёт версию именно этого env. prod deprecated НОВЕЕ по deprecated_at — без
// env-скоупа order-by вернул бы prod для обоих запросов.
func TestRollbackTargetEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev version 1.0.0
	ctx := context.Background()

	devV := f.VersionID
	if _, err := st.Pool.Exec(ctx,
		`update versions set state='deprecated', deprecated_at = now() - interval '10 minutes' where id = $1::uuid`, devV); err != nil {
		t.Fatal(err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	if _, err := st.Pool.Exec(ctx,
		`update versions set state='deprecated', deprecated_at = now() - interval '1 minute' where id = $1::uuid`, prodV); err != nil {
		t.Fatal(err)
	}

	dev, err := st.RollbackTarget(ctx, "game", "dev")
	if err != nil {
		t.Fatalf("dev rollback target: %v", err)
	}
	if dev.ID != devV || dev.Env != "dev" {
		t.Fatalf("dev rollback target: want dev %s, got %s/%s", devV, dev.ID, dev.Env)
	}
	prod, err := st.RollbackTarget(ctx, "game", "prod")
	if err != nil {
		t.Fatalf("prod rollback target: %v", err)
	}
	if prod.ID != prodV || prod.Env != "prod" {
		t.Fatalf("prod rollback target: want prod %s, got %s/%s", prodV, prod.ID, prod.Env)
	}
}

// TestEnvsWithDeprecated: список env проекта с deprecated-окном (для env-резолва
// rollback в httpapi). Пусто → []; один env → он; оба → отсортированы.
func TestEnvsWithDeprecated(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	if envs, err := st.EnvsWithDeprecated(ctx, "game"); err != nil || len(envs) != 0 {
		t.Fatalf("no deprecated: want [], got %v (err %v)", envs, err)
	}

	if _, err := st.Pool.Exec(ctx,
		`update versions set state='deprecated', deprecated_at = now() where id = $1::uuid`, f.VersionID); err != nil {
		t.Fatal(err)
	}
	if envs, err := st.EnvsWithDeprecated(ctx, "game"); err != nil || len(envs) != 1 || envs[0] != "dev" {
		t.Fatalf("dev deprecated: want [dev], got %v (err %v)", envs, err)
	}

	prodV := f.AddVersion(t, "2.0.0", "prod")
	if _, err := st.Pool.Exec(ctx,
		`update versions set state='deprecated', deprecated_at = now() where id = $1::uuid`, prodV); err != nil {
		t.Fatal(err)
	}
	envs, err := st.EnvsWithDeprecated(ctx, "game")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 2 || envs[0] != "dev" || envs[1] != "prod" {
		t.Fatalf("both deprecated: want [dev prod], got %v", envs)
	}
}

// TestPromoteVersion — идемпотентный промоут между окружениями (spec §4, I7):
// новая версия в to_env с тем же project/semver/image_ref и provenance
// promoted_from; повторный промоут пока registered → реюз той же строки; иной
// image_ref / не-registered state → ErrConflict; тот же env → ErrConflict;
// несуществующий to_env → понятная ошибка (не sentinel); нет источника →
// ErrNotFound. Событие version_promoted — только на insert-пути.
func TestPromoteVersion(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // dev version 1.0.0, image ghcr.io/example/game-server:1.0.0
	ctx := context.Background()
	const devImage = "ghcr.io/example/game-server:1.0.0"

	// dev 1.0.0 → prod: НОВАЯ версия, тот же image_ref/semver, promoted_from=источник.
	promoted, err := st.PromoteVersion(ctx, f.VersionID, "prod")
	if err != nil {
		t.Fatalf("promote dev→prod: %v", err)
	}
	if promoted.ID == f.VersionID {
		t.Fatalf("promote must create a NEW version row, got the source id %s", promoted.ID)
	}
	if promoted.Env != "prod" || promoted.Semver != "1.0.0" ||
		promoted.ImageRef != devImage || promoted.State != "registered" {
		t.Fatalf("promoted version fields: %+v", promoted)
	}
	if promoted.PromotedFrom == nil || *promoted.PromotedFrom != f.VersionID {
		t.Fatalf("promoted_from must point at the source %s, got %v", f.VersionID, promoted.PromotedFrom)
	}

	// Повторный промоут пока prod-версия registered → та же строка (idempotent), НЕ дубль.
	again, err := st.PromoteVersion(ctx, f.VersionID, "prod")
	if err != nil {
		t.Fatalf("idempotent re-promote: %v", err)
	}
	if again.ID != promoted.ID {
		t.Fatalf("re-promote must reuse the same row: want %s, got %s", promoted.ID, again.ID)
	}
	var cnt int
	if err := st.Pool.QueryRow(ctx, `
		select count(*) from versions v join projects p on p.id = v.project_id
		where p.slug = 'game' and v.env = 'prod' and v.semver = '1.0.0'`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("re-promote must not duplicate: prod 1.0.0 count = %d", cnt)
	}

	// Событие version_promoted записано РОВНО раз (insert-путь, не на реюзе), с
	// provenance-пейлоадом {project, semver, from_env, to_env, image_ref}.
	if n, err := st.CountEvents(ctx, store.EventVersionPromoted); err != nil || n != 1 {
		t.Fatalf("version_promoted: want exactly 1 (insert only, not on reuse), got %d (err %v)", n, err)
	}
	evs, err := st.ListEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range evs {
		if e.Kind != store.EventVersionPromoted {
			continue
		}
		found = true
		if e.Payload["project"] != "game" || e.Payload["semver"] != "1.0.0" ||
			e.Payload["from_env"] != "dev" || e.Payload["to_env"] != "prod" ||
			e.Payload["image_ref"] != devImage {
			t.Fatalf("version_promoted payload: %v", e.Payload)
		}
		if e.VersionID == nil || *e.VersionID != promoted.ID {
			t.Fatalf("version_promoted must ref the new version %s, got %v", promoted.ID, e.VersionID)
		}
	}
	if !found {
		t.Fatal("version_promoted event not written")
	}

	// Промоут в ТОТ ЖЕ env → ErrConflict «already in».
	_, err = st.PromoteVersion(ctx, f.VersionID, "dev")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("promote to same env: want ErrConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "already in") {
		t.Fatalf("same-env conflict must say «already in», got %q", err.Error())
	}

	// Несуществующий to_env → понятная ошибка «no such environment» (→400 в API),
	// НЕ ErrConflict и НЕ ErrNotFound (паритет CreateVersion).
	_, err = st.PromoteVersion(ctx, f.VersionID, "staging")
	if err == nil || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown to_env: want a plain «no such environment» error, got %v", err)
	}
	if !strings.Contains(err.Error(), "no such environment") {
		t.Fatalf("unknown to_env error must say «no such environment», got %q", err.Error())
	}

	// semver занят в prod ДРУГИМ image_ref → ErrConflict.
	devV3 := f.AddVersion(t, "3.0.0", "dev") // image ghcr.io/example/game-server:3.0.0
	if _, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "game", Semver: "3.0.0", ImageRef: "ghcr.io/example/other:3.0.0", Env: "prod",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = st.PromoteVersion(ctx, devV3, "prod")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("semver occupied by a different image_ref: want ErrConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "different image_ref") {
		t.Fatalf("conflict must mention the image_ref mismatch, got %q", err.Error())
	}

	// Существующая prod-версия в НЕ-registered state → ErrConflict «already <state>».
	if _, err := st.Pool.Exec(ctx,
		`update versions set state = 'active' where id = $1::uuid`, promoted.ID); err != nil {
		t.Fatal(err)
	}
	_, err = st.PromoteVersion(ctx, f.VersionID, "prod")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("promote onto a non-registered version: want ErrConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "already active") {
		t.Fatalf("conflict must say «already active», got %q", err.Error())
	}

	// Несуществующая исходная версия → ErrNotFound.
	_, err = st.PromoteVersion(ctx, uuid.NewString(), "prod")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown source version: want ErrNotFound, got %v", err)
	}
}
