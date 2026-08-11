package store_test

// Эпик #968, шаг 1: колонка events.project_id и её бэкфилл (миграция 000019).
//
// Проверяем не «колонка появилась», а СЕМАНТИКУ атрибуции, потому что ошибиться
// тут можно молча в обе стороны: недоатрибутировать (алерт останется
// платформенным) и переатрибутировать (событие уедет чужому проекту).

import (
	"context"
	"testing"

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
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := context.Background()
	want := projectID(t, st, f.Project)

	// Сеем событие "как до миграции": со ссылкой, но без project_id.
	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind, node_id) values ('legacy_node_event', $1::uuid)`, f.NodeID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind, version_id) values ('legacy_version_event', $1::uuid)`, f.VersionID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Повторяем сам бэкфилл миграции (она уже отработала на пустой базе).
	backfill(t, st)

	if got := eventProject(t, st, "legacy_node_event"); got != want {
		t.Fatalf("событие с node_id не атрибутировано: got %q, want %q", got, want)
	}
	if got := eventProject(t, st, "legacy_version_event"); got != want {
		t.Fatalf("событие с version_id не атрибутировано: got %q, want %q", got, want)
	}
}

// TestEventsBackfillAttributesByPayloadSlug: у части событий ссылок нет вовсе
// (project_created, environment_deleted, deploy_*) — их проект живёт в payload.
func TestEventsBackfillAttributesByPayloadSlug(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := context.Background()

	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind, payload) values ('legacy_payload_event', jsonb_build_object('project', $1::text))`,
		f.Project); err != nil {
		t.Fatalf("seed: %v", err)
	}
	backfill(t, st)

	if got := eventProject(t, st, "legacy_payload_event"); got != projectID(t, st, f.Project) {
		t.Fatalf("событие со слагом в payload не атрибутировано: got %q", got)
	}
}

// TestEventsBackfillLeavesPlatformEventsNull: платформенное событие (бекап, CA,
// сессия) проекта не имеет — приписать его первому попавшемуся значило бы
// соврать в аудите. Тот же смысл, что у project='' в match_stats_daily.
func TestEventsBackfillLeavesPlatformEventsNull(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 8)
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
	backfill(t, st)

	if got := eventProject(t, st, "legacy_platform_event"); got != "" {
		t.Fatalf("платформенному событию приписан проект %q", got)
	}
	if got := eventProject(t, st, "legacy_dangling_event"); got != "" {
		t.Fatalf("событию с висячей ссылкой приписан проект %q", got)
	}
}

// TestDeleteProjectKeepsEventsAsPlatform: удаление проекта не стирает аудит —
// события остаются, теряя атрибуцию (on delete set null). Стереть их значило бы
// потерять след того, что с проектом происходило.
func TestDeleteProjectKeepsEventsAsPlatform(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := context.Background()

	if _, err := st.Pool.Exec(ctx,
		`insert into events (kind, project_id) values ('doomed_project_event', $1::uuid)`,
		projectID(t, st, f.Project)); err != nil {
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
}

// backfill повторяет UPDATE из миграции 000019 — она отрабатывает на пустой
// базе, поэтому исторические строки тесты сеют сами.
func backfill(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.Pool.Exec(context.Background(), `
		update events e
		   set project_id = coalesce(
		         (select n.project_id from nodes    n where n.id = e.node_id),
		         (select s.project_id from servers  s where s.id = e.server_id),
		         (select v.project_id from versions v where v.id = e.version_id),
		         (select m.project_id from matches  m where m.id = e.match_id),
		         (select p.id from projects p where p.slug = e.payload->>'project')
		       )
		 where e.project_id is null`)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
}
