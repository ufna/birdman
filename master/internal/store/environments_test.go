package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Environments v1 (docs/superpowers/specs/2026-07-13-environments-v1-design.md
// §1–3): seeds on project create, CRUD + guardrails, node-move, sole-env resolve.

// ensureProject seeds dev+prod for any brand-new project on first reference.
func TestEnvironmentsSeededOnProjectCreate(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	if _, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "newgame", Region: "eu", Hostname: "n1", PublicIP: "203.0.113.1", CapacitySlots: 4,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	envs, err := st.ListEnvironments(ctx, "newgame")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 2 {
		t.Fatalf("want dev+prod seeded, got %d: %+v", len(envs), envs)
	}
	// Non-production first (panel convention): dev then prod.
	dev, prod := envs[0], envs[1]
	if dev.Name != "dev" || dev.Production || !dev.AutoDeploy || dev.RetentionKeep != 20 {
		t.Fatalf("dev seed wrong: %+v", dev)
	}
	if prod.Name != "prod" || !prod.Production || prod.AutoDeploy || prod.RetentionKeep != 0 {
		t.Fatalf("prod seed wrong: %+v", prod)
	}
	// The seeded node itself entered as dev (never prod implicitly).
	nodes, _ := st.ListNodes(ctx, store.NodeFilter{})
	if len(nodes) != 1 || nodes[0].Env != "dev" {
		t.Fatalf("new node must enter as dev: %+v", nodes)
	}
	if got, err := st.GetEnvironment(ctx, "newgame", "dev"); err != nil || got.Project != "newgame" {
		t.Fatalf("get dev: %+v %v", got, err)
	}
	// v3: несуществующее окружение — ErrBadEnv (→400 «no such environment»), а не
	// ErrNotFound: env здесь ссылка из запроса, а не адресуемый ресурс.
	if _, err := st.GetEnvironment(ctx, "newgame", "nope"); !errors.Is(err, store.ErrBadEnv) {
		t.Fatalf("get missing env: want ErrBadEnv, got %v", err)
	}
}

// TestEnvironmentsSeedOnlyOnProjectInsert (w2): ensureProject сеет dev+prod ТОЛЬКО
// при фактической ВСТАВКЕ проекта. Раньше сев шёл при КАЖДОМ касании (безусловный
// insert environments … on conflict do nothing), поэтому удалённое оператором
// окружение молча воскресало на первом же CreateVersion/UpsertFleet/CreateNode:
// DELETE /v1/environments отрабатывал, а env возвращался из ниоткуда.
//
// Здесь же — env у CreateNode: нода больше не входит в dev «по дефолту колонки»
// вслепую (это упало бы сырым FK-500 при удалённом dev), а валидирует окружение и
// умеет войти сразу в нужное.
func TestEnvironmentsSeedOnlyOnProjectInsert(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	// Новый проект (первая вставка) — dev+prod засеяны.
	if _, err := st.SetProjectMatchSize(ctx, "newgame", 4); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Ссылок на dev нет, поэтому его можно удалить — как это сделал бы оператор
	// (пустое окружение: confirm не требуется).
	if _, err := st.DeleteEnvironment(ctx, "newgame", "dev", ""); err != nil {
		t.Fatalf("delete dev: %v", err)
	}

	// Повторные касания проекта (ensureProject внутри) НЕ воскрешают dev.
	if _, err := st.SetProjectMatchSize(ctx, "newgame", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project: "newgame", Semver: "1.0.0", ImageRef: "ghcr.io/example/newgame:1.0.0", Env: "prod",
	}); err != nil {
		t.Fatalf("create version in prod: %v", err)
	}
	assertEnvNames := func(want ...string) {
		t.Helper()
		envs, err := st.ListEnvironments(ctx, "newgame")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range envs {
			got = append(got, e.Name)
		}
		if len(got) != len(want) {
			t.Fatalf("окружения newgame: want %v, got %v", want, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("окружения newgame: want %v, got %v", want, got)
			}
		}
	}
	assertEnvNames("prod") // dev не воскрес

	// CreateNode без env целится в дефолтный dev, которого больше нет → ErrBadEnv
	// (400 «no such environment»), а не сырой FK-500 и не тихое воскрешение.
	_, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "newgame", Region: "eu", Hostname: "n1", PublicIP: "203.0.113.1", CapacitySlots: 4,
	})
	if !errors.Is(err, store.ErrBadEnv) {
		t.Fatalf("CreateNode без env при удалённом dev: want ErrBadEnv, got %v", err)
	}
	assertEnvNames("prod")

	// Явный несуществующий env → тот же ErrBadEnv.
	if _, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "newgame", Region: "eu", Hostname: "n1", PublicIP: "203.0.113.1", CapacitySlots: 4, Env: "ghost",
	}); !errors.Is(err, store.ErrBadEnv) {
		t.Fatalf("CreateNode с несуществующим env: want ErrBadEnv, got %v", err)
	}

	// Явный env=prod → нода входит в prod.
	n, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "newgame", Region: "eu", Hostname: "n1", PublicIP: "203.0.113.1", CapacitySlots: 4, Env: "prod",
	})
	if err != nil {
		t.Fatalf("CreateNode env=prod: %v", err)
	}
	if n.Env != "prod" {
		t.Fatalf("нода обязана войти в prod, got %q", n.Env)
	}

	// Регрессия наоборот: НОВЫЙ проект по-прежнему получает dev+prod при вставке,
	// и нода без env входит в dev.
	fresh, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "fresh", Region: "eu", Hostname: "n1", PublicIP: "203.0.113.2", CapacitySlots: 4,
	})
	if err != nil {
		t.Fatalf("CreateNode нового проекта: %v", err)
	}
	if fresh.Env != "dev" {
		t.Fatalf("нода нового проекта входит как dev, got %q", fresh.Env)
	}
	if envs, err := st.ListEnvironments(ctx, "fresh"); err != nil || len(envs) != 2 {
		t.Fatalf("новый проект обязан получить dev+prod: %+v %v", envs, err)
	}
}

func TestEnvironmentsCRUD(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10) // project game with dev+prod seeded

	e, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "game", Name: "staging", AutoDeploy: true, RetentionKeep: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "staging" || e.Production || !e.AutoDeploy || e.RetentionKeep != 5 || e.Project != "game" {
		t.Fatalf("created env wrong: %+v", e)
	}
	// Duplicate and re-creating a seeded env → ErrConflict.
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "staging"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup env: want ErrConflict, got %v", err)
	}
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "dev", AutoDeploy: true}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("recreate seeded dev: want ErrConflict, got %v", err)
	}

	no, keep := false, 9
	e2, err := st.PatchEnvironment(ctx, "game", "staging", store.EnvironmentPatch{AutoDeploy: &no, RetentionKeep: &keep})
	if err != nil {
		t.Fatal(err)
	}
	if e2.AutoDeploy || e2.RetentionKeep != 9 {
		t.Fatalf("patched env wrong: %+v", e2)
	}
	// order by production, name → dev, staging, prod.
	envs, _ := st.ListEnvironments(ctx, "game")
	if len(envs) != 3 || envs[0].Name != "dev" || envs[1].Name != "staging" || envs[2].Name != "prod" {
		t.Fatalf("list order wrong: %+v", envs)
	}
	if _, err := st.PatchEnvironment(ctx, "game", "nope", store.EnvironmentPatch{RetentionKeep: &keep}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("patch missing: want ErrNotFound, got %v", err)
	}
}

// production && auto_deploy is rejected on Create and on Patch, in both field
// orders, and a rejected patch must not mutate the row.
func TestEnvironmentsGuardrails(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10)

	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "game", Name: "prodlike", Production: true, AutoDeploy: true,
	}); err == nil {
		t.Fatal("create production+auto_deploy: want error")
	}

	// Order A: production env, then try to enable auto_deploy.
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "live", Production: true}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := st.PatchEnvironment(ctx, "game", "live", store.EnvironmentPatch{AutoDeploy: &yes}); err == nil {
		t.Fatal("patch auto_deploy on production env: want error")
	}

	// Order B: auto_deploy env, then try to make it production.
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "d2", AutoDeploy: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PatchEnvironment(ctx, "game", "d2", store.EnvironmentPatch{Production: &yes}); err == nil {
		t.Fatal("patch production on auto_deploy env: want error")
	}
	if got, _ := st.GetEnvironment(ctx, "game", "d2"); got.Production || !got.AutoDeploy {
		t.Fatalf("rejected patch mutated d2: %+v", got)
	}
}

func TestEnvironmentsNameValidation(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10)
	for _, name := range []string{"all", "global", "DEV", "-x", "", "has space", "way-too-long-environment-name-over-limit"} {
		if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: name}); err == nil {
			t.Fatalf("name %q: want validation error", name)
		}
	}
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "qa-1"}); err != nil {
		t.Fatalf("valid name qa-1: %v", err)
	}
}

// An unused env deletes without confirm; an env WITH NODES is a hard 409 (the
// precondition — move them first); a missing one is a 404.
func TestEnvironmentsDelete(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10) // dev holds the seeded node + version 1.0.0

	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "temp"}); err != nil {
		t.Fatal(err)
	}
	res, err := st.DeleteEnvironment(ctx, "game", "temp", "")
	if err != nil {
		t.Fatalf("delete unused env: %v", err)
	}
	if !res.WasEmpty {
		t.Fatalf("unused env must report WasEmpty: %+v", res)
	}
	if _, err := st.GetEnvironment(ctx, "game", "temp"); !errors.Is(err, store.ErrBadEnv) {
		t.Fatalf("temp still present after delete: %v", err)
	}

	// dev держит ноду → предусловие: 409 даже с верным confirm.
	_, err = st.DeleteEnvironment(ctx, "game", "dev", "dev")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete env with nodes: want ErrConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "node(s)") {
		t.Fatalf("conflict must name the nodes precondition: %q", err.Error())
	}

	if _, err := st.DeleteEnvironment(ctx, "game", "nope", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete missing env: want ErrNotFound, got %v", err)
	}
}

// Каскадное удаление НЕПУСТОГО окружения (запрос владельца, снимает тупик I10).
// Двух-env фикстура: в dev — версии/флот/сервер/матч/ключ, в prod — то же самое
// плюс версия, промоученная ИЗ dev. Проверяем: снесено ровно содержимое dev, у
// соседнего prod не тронуто НИЧЕГО (кроме обнулённого provenance promoted_from —
// его держит self-FK), ключ dev отозван, событие environment_deleted с верным
// payload, а confirm обязателен и точен.
func TestEnvironmentsDeleteCascade(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // dev: node-1 + версия 1.0.0

	// --- prod: ноды нет, зато есть версия (промоут из dev), флот, сервер и матч.
	prodV, err := st.PromoteVersion(ctx, f.VersionID, "prod")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if prodV.PromotedFrom == nil || *prodV.PromotedFrom != f.VersionID {
		t.Fatalf("promote must record provenance: %+v", prodV)
	}
	// Нода-2 уезжает в prod (пустая) — на ней и будут prod-серверы.
	n2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	if _, err := st.SetNodeEnv(ctx, n2, "prod"); err != nil {
		t.Fatalf("move node-2 to prod: %v", err)
	}
	buf, maxs := int32(1), int32(5)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu", ActiveVersion: &prodV.ID, BufferReady: &buf, MaxServers: &maxs,
	}); err != nil {
		t.Fatalf("prod fleet: %v", err)
	}
	prodSrv := f.InsertServerOn(t, n2, prodV.ID, "reaped") // env берётся из ноды (prod)
	prodMatch := insertMatch(t, st, prodSrv)
	prodKey, _, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "prod-deploy", Scopes: []string{"deploy"}, Project: ptr("game"), Env: ptr("prod"),
	})
	if err != nil {
		t.Fatalf("prod key: %v", err)
	}

	// --- dev: флот, ещё версия, сервер (на ноде, которая уехала в prod — I6:
	// история не переписывается, у сервера остаётся собственный env='dev'), матч,
	// два ключа (живой и уже отозванный) и строка роллапа.
	devSrv := f.InsertServer(t, f.NodeID, f.VersionID, "reaped", 7000, 0)
	devMatch := insertMatch(t, st, devSrv)
	f.UpsertFleet(t, 2, 10) // dev/eu, active = версия 1.0.0
	f.AddVersion(t, "1.1.0", "dev")
	devKey, _, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "dev-deploy", Scopes: []string{"deploy"}, Project: ptr("game"), Env: ptr("dev"),
	})
	if err != nil {
		t.Fatalf("dev key: %v", err)
	}
	deadKey, _, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "dev-old", Scopes: []string{"deploy"}, Project: ptr("game"), Env: ptr("dev"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RevokeAPIKey(ctx, deadKey.ID); err != nil {
		t.Fatalf("pre-revoke: %v", err)
	}
	// Project в дименшенах ОБЯЗАТЕЛЕН — так их и строит живой пайплайн
	// (statsrollup из stats.AggregateDaily). Без него роллап ложится строкой
	// project='' — а это не «строка проекта game», а документированный маркер
	// «не атрибутировано» (миграция 000017): такая строка принадлежит нескольким
	// проектам сразу, и каскад одного из них её не трогает by design.
	if err := st.UpsertRollupDay(ctx, time.Now().UTC(), []store.RollupDim{
		{Project: "game", Region: "eu", Semver: "1.0.0", Env: "dev", Matches: 1},
		{Project: "game", Region: "eu", Semver: "1.0.0", Env: "prod", Matches: 1},
	}, 3, nil); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	// --- Пока в dev стоит нода — только 409, и транзакция НИЧЕГО не трогает.
	if _, err := st.DeleteEnvironment(ctx, "game", "dev", "dev"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dev with a node: want ErrConflict, got %v", err)
	}
	if n := countRows(t, st, `select count(*) from versions where env = 'dev'`); n != 2 {
		t.Fatalf("409 must not touch anything: dev versions = %d, want 2", n)
	}
	if n := countRows(t, st, `select count(*) from api_keys where env = 'dev' and revoked_at is null`); n != 1 {
		t.Fatalf("409 must not revoke keys: live dev keys = %d, want 1", n)
	}

	// Состав окружения (то, что панель показывает в диалоге).
	usage, err := st.EnvironmentUsage(ctx, "game", "dev")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	want := store.EnvironmentUsage{Versions: 2, Fleets: 1, Nodes: 1, Servers: 1, Matches: 1, APIKeys: 1}
	if usage != want {
		t.Fatalf("usage: got %+v, want %+v", usage, want)
	}
	if usage.Empty() {
		t.Fatal("usage of a used env must not be Empty()")
	}

	// --- Переводим ноду из dev (предусловие) — теперь удаление возможно.
	if _, err := st.SetNodeEnv(ctx, f.NodeID, "prod"); err != nil {
		t.Fatalf("move node-1 out of dev: %v", err)
	}
	// Непустое окружение без confirm / с неверным confirm — ErrConfirmRequired.
	if _, err := st.DeleteEnvironment(ctx, "game", "dev", ""); !errors.Is(err, store.ErrConfirmRequired) {
		t.Fatalf("no confirm: want ErrConfirmRequired, got %v", err)
	}
	if _, err := st.DeleteEnvironment(ctx, "game", "dev", "dev "); !errors.Is(err, store.ErrConfirmRequired) {
		t.Fatalf("confirm with a trailing space must not match: got %v", err)
	}
	if n := countRows(t, st, `select count(*) from versions where env = 'dev'`); n != 2 {
		t.Fatalf("failed confirm must not delete anything: dev versions = %d", n)
	}

	res, err := st.DeleteEnvironment(ctx, "game", "dev", "dev")
	if err != nil {
		t.Fatalf("cascade delete: %v", err)
	}
	if res.WasEmpty {
		t.Fatal("used env must not report WasEmpty")
	}
	if res.Name != "dev" || res.Production ||
		res.Versions != 2 || res.Fleets != 1 || res.Matches != 1 || res.Servers != 1 || res.APIKeysRevoked != 1 {
		t.Fatalf("delete result: got %+v, want {dev false 2 1 1 1 1}", res)
	}
	if len(res.RevokedKeyIDs) != 1 || res.RevokedKeyIDs[0] != devKey.ID {
		t.Fatalf("revoked key ids: %v, want [%s]", res.RevokedKeyIDs, devKey.ID)
	}

	// --- Окружения нет, содержимого dev нет.
	if _, err := st.GetEnvironment(ctx, "game", "dev"); !errors.Is(err, store.ErrBadEnv) {
		t.Fatalf("dev still present: %v", err)
	}
	for _, q := range []string{
		`select count(*) from versions where env = 'dev'`,
		`select count(*) from fleet_configs where env = 'dev'`,
		`select count(*) from servers where env = 'dev'`,
		`select count(*) from matches where env = 'dev'`,
		`select count(*) from match_stats_daily where env = 'dev'`,
	} {
		if n := countRows(t, st, q); n != 0 {
			t.Fatalf("dev content survived: %q → %d", q, n)
		}
	}

	// --- Соседний prod ЦЕЛ: версия, флот, сервер, матч, ключ, роллап.
	for q, want := range map[string]int{
		`select count(*) from versions where env = 'prod'`:          1,
		`select count(*) from fleet_configs where env = 'prod'`:     1,
		`select count(*) from servers where env = 'prod'`:           1,
		`select count(*) from matches where env = 'prod'`:           1,
		`select count(*) from match_stats_daily where env = 'prod'`: 1,
		`select count(*) from environments where name = 'prod'`:     1,
		`select count(*) from nodes`:                                2, // обе ноды уехали в prod, ни одна не удалена
	} {
		if n := countRows(t, st, q); n != want {
			t.Fatalf("neighbour env damaged: %q → %d, want %d", q, n, want)
		}
	}
	if countRows(t, st, `select count(*) from servers where id = '`+prodSrv+`'`) != 1 ||
		countRows(t, st, `select count(*) from matches where id = '`+prodMatch+`'`) != 1 ||
		countRows(t, st, `select count(*) from matches where id = '`+devMatch+`'`) != 0 {
		t.Fatal("wrong rows deleted")
	}
	// Provenance промоута обнулён (self-FK), сама prod-версия жива.
	vs, err := st.ListVersions(ctx, store.VersionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].ID != prodV.ID || vs[0].Env != "prod" {
		t.Fatalf("prod version must survive: %+v", vs)
	}
	if vs[0].PromotedFrom != nil {
		t.Fatalf("promoted_from must be nulled (source version deleted): %v", *vs[0].PromotedFrom)
	}

	// --- Ключи: dev-ключ отозван, привязка снята (иначе api_keys_env_fk не дал
	// бы удалить строку env), prod-ключ не тронут.
	keys, err := st.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.APIKey{}
	for _, k := range keys {
		byID[k.ID] = k
	}
	dk := byID[devKey.ID]
	if dk.RevokedAt == nil {
		t.Fatalf("dev key must be revoked: %+v", dk)
	}
	if dk.Env != nil || dk.ProjectID != nil {
		t.Fatalf("dev key binding must be cleared (FK): %+v", dk)
	}
	if byID[deadKey.ID].Env != nil {
		t.Fatalf("already-revoked bound key must be unbound too: %+v", byID[deadKey.ID])
	}
	pk := byID[prodKey.ID]
	if pk.RevokedAt != nil || pk.Env == nil || *pk.Env != "prod" {
		t.Fatalf("prod key must be untouched: %+v", pk)
	}

	// --- Событие environment_deleted с составом (payload) + аудит отзыва ключа.
	evs, err := st.ListEvents(ctx, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	var deleted, revoked *store.Event
	for i := range evs {
		switch evs[i].Kind {
		case store.EventEnvironmentDeleted:
			deleted = &evs[i]
		case store.EventAPIKeyRevoked:
			revoked = &evs[i]
		}
	}
	if deleted == nil {
		t.Fatal("no environment_deleted event")
	}
	for k, want := range map[string]any{
		"project": "game", "name": "dev", "production": false,
		"versions": float64(2), "fleets": float64(1), "matches": float64(1),
		"servers": float64(1), "api_keys_revoked": float64(1),
	} {
		if got := deleted.Payload[k]; got != want {
			t.Fatalf("environment_deleted payload[%s] = %v (%T), want %v", k, got, got, want)
		}
	}
	if revoked == nil || revoked.Payload["env"] != "dev" || revoked.Payload["reason"] != store.EventEnvironmentDeleted {
		t.Fatalf("apikey_revoked must keep the lost binding in the audit trail: %+v", revoked)
	}
}

// Пустое окружение (нет вообще ничего) удаляется без confirm и НЕ требует его —
// обратная совместимость v1; usage у него — все нули.
func TestEnvironmentsUsageEmpty(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10)

	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "staging"}); err != nil {
		t.Fatal(err)
	}
	usage, err := st.EnvironmentUsage(ctx, "game", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Empty() || usage != (store.EnvironmentUsage{}) {
		t.Fatalf("fresh env usage must be all zeroes: %+v", usage)
	}
	if _, err := st.EnvironmentUsage(ctx, "game", "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("usage of an unknown env: want ErrNotFound, got %v", err)
	}
	if _, err := st.DeleteEnvironment(ctx, "game", "staging", ""); err != nil {
		t.Fatalf("delete empty env without confirm: %v", err)
	}
}

func ptr(s string) *string { return &s }

// insertMatch — матч на сервере (env наследуется от сервера, как в heartbeat).
func insertMatch(t *testing.T, st *store.Store, serverID string) string {
	t.Helper()
	var id string
	err := st.Pool.QueryRow(context.Background(), `
		insert into matches (project_id, server_id, version_id, region, env, state)
		select s.project_id, s.id, s.version_id, n.region, s.env, 'finished'
		from servers s join nodes n on n.id = s.node_id
		where s.id = $1::uuid
		returning id::text`, serverID).Scan(&id)
	if err != nil {
		t.Fatalf("insert match: %v", err)
	}
	return id
}

func countRows(t *testing.T, st *store.Store, query string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// A node moves between envs only when empty and not dead; the move emits
// node_env_changed and is idempotent.
func TestEnvironmentsSetNodeEnv(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)

	n2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	node, err := st.SetNodeEnv(ctx, n2, "prod")
	if err != nil {
		t.Fatalf("move empty node: %v", err)
	}
	if node.Env != "prod" {
		t.Fatalf("node env: %s, want prod", node.Env)
	}
	if n, _ := st.CountEvents(ctx, store.EventNodeEnvChanged); n != 1 {
		t.Fatalf("want 1 node_env_changed, got %d", n)
	}
	// Idempotent no-op (prod→prod): ok, no new event.
	if _, err := st.SetNodeEnv(ctx, n2, "prod"); err != nil {
		t.Fatalf("idempotent move: %v", err)
	}
	if n, _ := st.CountEvents(ctx, store.EventNodeEnvChanged); n != 1 {
		t.Fatalf("no-op move must not emit an event, got %d", n)
	}

	// A node with a live server cannot be moved.
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	if _, err := st.SetNodeEnv(ctx, f.NodeID, "prod"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("move node with live server: want ErrConflict, got %v", err)
	}

	// A dead node cannot be moved.
	dead := f.AddNode(t, "node-dead", "203.0.113.12", 10)
	if _, err := st.Pool.Exec(ctx, `update nodes set state = 'dead' where id = $1::uuid`, dead); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeEnv(ctx, dead, "prod"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("move dead node: want ErrConflict, got %v", err)
	}

	// A quarantined, empty node can be moved.
	quar := f.AddNode(t, "node-quar", "203.0.113.13", 10)
	if _, err := st.Pool.Exec(ctx, `update nodes set state = 'quarantine' where id = $1::uuid`, quar); err != nil {
		t.Fatal(err)
	}
	if qn, err := st.SetNodeEnv(ctx, quar, "prod"); err != nil || qn.Env != "prod" {
		t.Fatalf("move quarantined empty node: %+v %v", qn, err)
	}

	// Non-existent target env → ErrBadEnv (ССЫЛКА в теле PATCH'а → 400, v3);
	// unknown node → ErrNotFound (адресуемый ресурс → 404).
	_, err = st.SetNodeEnv(ctx, quar, "ghost")
	if !errors.Is(err, store.ErrBadEnv) {
		t.Fatalf("move to missing env: want ErrBadEnv, got %v", err)
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing target env — это 400, а не 404: %v", err)
	}
	if _, err := st.SetNodeEnv(ctx, uuid.NewString(), "prod"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("move unknown node: want ErrNotFound, got %v", err)
	}
}

func TestEnvironmentsSoleEnvWithActiveNodes(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // one active dev node, fresh heartbeat

	if env, err := st.SoleEnvWithActiveNodes(ctx, "game"); err != nil || env != "dev" {
		t.Fatalf("sole env: %q %v, want dev", env, err)
	}

	// Two envs with active nodes → ErrConflict.
	n2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	if _, err := st.SetNodeEnv(ctx, n2, "prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SoleEnvWithActiveNodes(ctx, "game"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("two envs with nodes: want ErrConflict, got %v", err)
	}

	// No fresh nodes at all → ErrConflict.
	if _, err := st.Pool.Exec(ctx, `update nodes set last_heartbeat_at = now() - interval '5 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SoleEnvWithActiveNodes(ctx, "game"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("no fresh nodes: want ErrConflict, got %v", err)
	}
}
