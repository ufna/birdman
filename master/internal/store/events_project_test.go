package store_test

// Эпик #968, шаг 1: колонка events.project_id и её бэкфилл (миграция 000019).
//
// Проверяем не «колонка появилась», а СЕМАНТИКУ атрибуции, потому что ошибиться
// тут можно молча в обе стороны: недоатрибутировать (алерт останется
// платформенным) и переатрибутировать (событие уедет чужому проекту).
//
// Гоняем при этом САМ файл миграции, а не его копию в тесте: копия ловит только
// собственную мутацию, а шиппится другой файл. Идиома та же, что у соседних
// 000017/000018 (stepDownToV16/stepDownToV17): свежая БД на head → шаг схемы
// вниз → исторические строки сырым SQL → шаг вверх → ассерты.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// eventProject возвращает project_id последнего события вида kind (или "" для NULL).
func eventProject(t *testing.T, st *store.Store, kind string) string {
	t.Helper()
	var pid *string
	err := st.Pool.QueryRow(context.Background(),
		`select project_id::text from events where kind = $1 order by id desc limit 1`, kind).Scan(&pid)
	if err != nil {
		t.Fatalf("read project_id of %s: %v", kind, err)
	}
	if pid == nil {
		return ""
	}
	return *pid
}

// eventSlug возвращает снимок слага владельца последнего события вида kind
// (или "" для NULL) — то, чем осиротевшее событие отличается от платформенного.
func eventSlug(t *testing.T, st *store.Store, kind string) string {
	t.Helper()
	var slug *string
	err := st.Pool.QueryRow(context.Background(),
		`select project_slug from events where kind = $1 order by id desc limit 1`, kind).Scan(&slug)
	if err != nil {
		t.Fatalf("read project_slug of %s: %v", kind, err)
	}
	if slug == nil {
		return ""
	}
	return *slug
}

func projectID(t *testing.T, st *store.Store, slug string) string {
	t.Helper()
	p, err := st.GetProject(context.Background(), slug)
	if err != nil {
		t.Fatalf("get project %s: %v", slug, err)
	}
	return p.ID
}

// TestEventsBackfillAttributesByRefs: строки, у которых есть ссылка на сущность,
// получают проект ЭТОЙ сущности. Ссылка — факт, а не догадка.
func TestEventsBackfillAttributesByRefs(t *testing.T) {
	var f *testdb.Fixture
	var otherNode, otherVersion string
	st, m := stepDownToV18(t, func(st *store.Store) {
		f = testdb.Seed(t, st, "eu", 8)
		// Второй проект — чтобы было чем проверить ПОРЯДОК источников: у
		// строки со ссылками на разные проекты победитель обязан быть один и
		// определённый, а не «какой попадётся».
		other, _, err := st.CreateNode(context.Background(), store.CreateNodeParams{
			Project: "arena", Region: "eu", Hostname: "arena-1", PublicIP: "203.0.113.20", CapacitySlots: 4,
		})
		if err != nil {
			t.Fatalf("seed other project node: %v", err)
		}
		otherNode = other.ID
		v, err := st.CreateVersion(context.Background(), store.CreateVersionParams{
			Project: "arena", Semver: "1.0.0", ImageRef: "ghcr.io/example/arena:1.0.0", Env: "dev",
		})
		if err != nil {
			t.Fatalf("seed other project version: %v", err)
		}
		otherVersion = v.ID
		// Ревокнутая нода: строка ЖИВА (state='dead'), а значит её события
		// обязаны атрибутироваться — «нода выведена» не то же самое, что
		// «владельца не восстановить».
		if _, err := st.RevokeNode(context.Background(), f.NodeID); err != nil {
			t.Fatalf("revoke node: %v", err)
		}
	})
	ctx := context.Background()
	want := projectID(t, st, f.Project)
	otherWant := projectID(t, st, "arena")

	// Сеем события «как до миграции»: со ссылкой, но без project_id (на схеме
	// 18 этой колонки нет вовсе).
	seedLegacy := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seedLegacy(`insert into events (kind, node_id) values ('legacy_node_event', $1::uuid)`, f.NodeID)
	seedLegacy(`insert into events (kind, version_id) values ('legacy_version_event', $1::uuid)`, f.VersionID)
	// Несколько ссылок ОДНОГО проекта — согласованный случай, ответ один.
	seedLegacy(`insert into events (kind, node_id, version_id) values ('legacy_multiref_event', $1::uuid, $2::uuid)`,
		f.NodeID, f.VersionID)
	// Несколько ссылок РАЗНЫХ проектов: coalesce перечисляет источники по
	// убыванию достоверности, нода стоит первой — она и побеждает. Без этого
	// кейса перестановка веток внутри coalesce прошла бы незамеченной.
	seedLegacy(`insert into events (kind, node_id, version_id) values ('legacy_crossref_event', $1::uuid, $2::uuid)`,
		f.NodeID, otherVersion)
	seedLegacy(`insert into events (kind, node_id, version_id) values ('legacy_crossref_other_event', $1::uuid, $2::uuid)`,
		otherNode, f.VersionID)

	applyEventsMigration(t, m)

	if got := eventProject(t, st, "legacy_node_event"); got != want {
		t.Errorf("событие с node_id не атрибутировано: got %q, want %q", got, want)
	}
	if got := eventProject(t, st, "legacy_version_event"); got != want {
		t.Errorf("событие с version_id не атрибутировано: got %q, want %q", got, want)
	}
	if got := eventProject(t, st, "legacy_multiref_event"); got != want {
		t.Errorf("событие с двумя ссылками одного проекта не атрибутировано: got %q, want %q", got, want)
	}
	if got := eventProject(t, st, "legacy_crossref_event"); got != want {
		t.Errorf("при ссылках на разные проекты обязана победить нода: got %q, want %q", got, want)
	}
	if got := eventProject(t, st, "legacy_crossref_other_event"); got != otherWant {
		t.Errorf("при ссылках на разные проекты обязана победить нода: got %q, want %q", got, otherWant)
	}
	// Событие, которое написал сам store ДО шага вниз: шаг вниз снял с него
	// project_id, шаг вверх обязан вернуть — ссылка на мёртвую, но живую в
	// таблице ноду разрешается.
	if got := eventProject(t, st, store.EventNodeRevoked); got != want {
		t.Errorf("событие ревокнутой ноды (state=dead, строка жива) не атрибутировано: got %q, want %q", got, want)
	}
}

// TestEventsBackfillAttributesByPayloadSlug: у части событий ссылок нет вовсе
// (project_created, environment_deleted, deploy_*) — их проект живёт в payload.
func TestEventsBackfillAttributesByPayloadSlug(t *testing.T) {
	var f *testdb.Fixture
	st, m := stepDownToV18(t, func(st *store.Store) { f = testdb.Seed(t, st, "eu", 8) })
	ctx := context.Background()

	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind, payload) values ('legacy_payload_event', jsonb_build_object('project', $1::text))`,
		f.Project); err != nil {
		t.Fatalf("seed: %v", err)
	}
	applyEventsMigration(t, m)

	if got := eventProject(t, st, "legacy_payload_event"); got != projectID(t, st, f.Project) {
		t.Fatalf("событие со слагом в payload не атрибутировано: got %q", got)
	}
}

// TestEventsBackfillLeavesPlatformEventsNull: платформенное событие (бекап, CA,
// сессия) проекта не имеет — приписать его первому попавшемуся значило бы
// соврать в аудите. Тот же смысл, что у project='' в match_stats_daily.
func TestEventsBackfillLeavesPlatformEventsNull(t *testing.T) {
	st, m := stepDownToV18(t, func(st *store.Store) { testdb.Seed(t, st, "eu", 8) })
	ctx := context.Background()

	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind) values ('legacy_platform_event')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Ссылка на УДАЛЁННУЮ сущность тоже не должна ничего выдумывать.
	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind, node_id) values ('legacy_dangling_event', gen_random_uuid())`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Слаг в payload, которого нет среди проектов: подстановка «похожего»
	// увела бы аудит чужому арендатору.
	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind, payload) values ('legacy_unknown_slug_event', jsonb_build_object('project', 'nope'))`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	applyEventsMigration(t, m)

	if got := eventProject(t, st, "legacy_platform_event"); got != "" {
		t.Fatalf("платформенному событию приписан проект %q", got)
	}
	if got := eventProject(t, st, "legacy_dangling_event"); got != "" {
		t.Fatalf("событию с висячей ссылкой приписан проект %q", got)
	}
	if got := eventProject(t, st, "legacy_unknown_slug_event"); got != "" {
		t.Fatalf("событию с несуществующим слагом приписан проект %q", got)
	}
}

// TestDeleteProjectKeepsEventsAsOrphaned: удаление проекта не стирает аудит —
// события остаются, теряя ССЫЛКУ на проект (on delete set null). Стереть их
// значило бы потерять след того, что с проектом происходило.
//
// Но «потеряли ссылку» больше не значит «стали платформенными» (tracker #1083):
// снимок слага остаётся, и именно по нему читатели отличают историю удалённого
// проекта от бекапа с ротацией CA. Раньше этот тест назывался
// …KeepsEventsAsPlatform и фиксировал ровно то, что оказалось утечкой: сегодня
// он пинует ОБА свойства — строка жива И помечена как осиротевшая.
func TestDeleteProjectKeepsEventsAsOrphaned(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := context.Background()

	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind, project_id, project_slug) values ('doomed_project_event', $1::uuid, $2)`,
		projectID(t, st, f.Project), f.Project); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := st.DeleteProject(ctx, f.Project, f.Project); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	var n int
	if err := st.Pool.QueryRow(ctx,
		`select count(*) from events where kind = 'doomed_project_event'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("удаление проекта стёрло его события из аудита: осталось %d", n)
	}
	if got := eventProject(t, st, "doomed_project_event"); got != "" {
		t.Fatalf("после удаления проекта событие всё ещё ссылается на него: %q", got)
	}
	if got := eventSlug(t, st, "doomed_project_event"); got != f.Project {
		t.Fatalf("осиротевшее событие потеряло имя владельца: got %q, want %q", got, f.Project)
	}
}

// TestEventProjectNamedConstraint: строка, назвавшая проект по id, обязана
// назвать его и по имени. Инвариант держится СХЕМОЙ, а не соглашением: без него
// любой писатель, забывший снимок, молча вернул бы утечку #1083, и заметили бы
// это только на удалении проекта — то есть уже на живом чужом аудите.
func TestEventProjectNamedConstraint(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)

	_, err := st.Pool.Exec(context.Background(),
		`insert into events (kind, project_id) values ('unnamed_project_event', $1::uuid)`,
		projectID(t, st, f.Project))
	if err == nil {
		t.Fatal("событие с project_id и без project_slug записалось — инвариант ничем не держится")
	}
	if !strings.Contains(err.Error(), "events_project_named") {
		t.Fatalf("отказ пришёл не от того ограничения: %v", err)
	}
}

// stepDownToV18 поднимает свежую БД (мигрированную на head), даёт вызывающему
// засеять граф сущностей ЧЕРЕЗ STORE, пока схема ещё head, и только потом
// шагает схему ВНИЗ до v18 — форма до проектного измерения событий.
//
// Колбэк, а не «сначала шаг вниз, потом сидинг» — это вынужденное отличие от
// соседних stepDownToV16/stepDownToV17: на схеме 18 колонки events.project_id
// нет, а insertEvent её пишет, поэтому ЛЮБОЙ store-вызов там падает.
// Исторические строки после шага вниз сеются сырым SQL.
//
// Шаг вниз попутно снимает project_id и с событий, которые store написал на
// head, — значит настоящий бэкфилл прогоняется и по ним тоже.
func stepDownToV18(t *testing.T, seed func(*store.Store)) (*store.Store, *migrate.Migrate) {
	t.Helper()
	st, dsn := testdb.NewWithCodec(t, codecFilled(t, 0x19))
	if seed != nil {
		seed(st)
	}
	m := newMigrateHandle(t, stripPoolParams(t, dsn))
	if err := m.Migrate(18); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("step down to v18: %v", err)
	}
	var hasColumn int
	if err := st.Pool.QueryRow(context.Background(),
		`select count(*) from information_schema.columns
		  where table_name = 'events' and column_name = 'project_id'`).Scan(&hasColumn); err != nil {
		t.Fatalf("column check: %v", err)
	}
	if hasColumn != 0 {
		t.Fatal("после шага вниз колонки events.project_id быть не должно — иначе тест сеял бы уже мигрированные строки")
	}
	return st, m
}

// applyEventsMigration прогоняет НАСТОЯЩИЙ 000019_events_project.up.sql
// (включая его бэкфилл) — то, что и шиппится, а не копию SQL в тесте.
func applyEventsMigration(t *testing.T, m *migrate.Migrate) {
	t.Helper()
	if err := m.Migrate(19); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("apply 000019: %v", err)
	}
}
