package store_test

// Эпик #968, шаг 2: запись проставляет project_id, чтение сужается по проекту.
//
// Две половины проверяются вместе, потому что порознь каждая выглядит рабочей:
// можно писать атрибуцию и не пользоваться ей на чтении, а можно фильтровать по
// колонке, которую никто не заполняет.

import (
	"context"
	"testing"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestInsertEventAttributesByRef: событие со ссылкой на сущность получает её
// проект — без единой правки в двух десятках вызывающих.
func TestInsertEventAttributesByRef(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := context.Background()

	if err := st.InsertEvent(ctx, "probe_by_node", store.EventRef{NodeID: &f.NodeID}, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.InsertEvent(ctx, "probe_by_version", store.EventRef{VersionID: &f.VersionID}, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}
	want := projectID(t, st, f.Project)
	if got := eventProject(t, st, "probe_by_node"); got != want {
		t.Fatalf("событие с node_id: project_id = %q, want %q", got, want)
	}
	if got := eventProject(t, st, "probe_by_version"); got != want {
		t.Fatalf("событие с version_id: project_id = %q, want %q", got, want)
	}
}

// TestInsertEventAttributesByPayloadSlug: у событий без ссылок проект живёт в
// payload — там же его и берём (project_created, environment_deleted, deploy_*).
func TestInsertEventAttributesByPayloadSlug(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := context.Background()

	if err := st.InsertEvent(ctx, "probe_by_payload", store.EventRef{},
		map[string]any{"project": f.Project}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := eventProject(t, st, "probe_by_payload"); got != projectID(t, st, f.Project) {
		t.Fatalf("событие со слагом в payload: project_id = %q", got)
	}
}

// TestInsertEventPlatformStaysNull: платформенному событию проект не
// приписывается — иначе бекап или ротация CA «принадлежали» бы случайному
// проекту, и лента под фильтром показывала бы чужое как своё.
func TestInsertEventPlatformStaysNull(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 8)

	if err := st.InsertEvent(context.Background(), "probe_platform", store.EventRef{}, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := eventProject(t, st, "probe_platform"); got != "" {
		t.Fatalf("платформенному событию приписан проект %q", got)
	}
}

// TestListEventsScopedNonHiding: сужение убирает ЯВНО чужое и оставляет
// платформенное. Не скрывающее — та же семантика, что у алертов (#955):
// спрятать платформенное значило бы утверждать, что на платформе ничего не
// происходит, пока выбран проект.
func TestListEventsScopedNonHiding(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := context.Background()

	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatalf("create neighbour: %v", err)
	}
	if err := st.InsertEvent(ctx, "mine", store.EventRef{NodeID: &f.NodeID}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, "theirs", store.EventRef{}, map[string]any{"project": "neighbour"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, "platform", store.EventRef{}, nil); err != nil {
		t.Fatal(err)
	}

	kinds := func(project string) map[string]bool {
		t.Helper()
		evs, err := st.ListEvents(ctx, 200, project)
		if err != nil {
			t.Fatalf("list(%q): %v", project, err)
		}
		out := map[string]bool{}
		for _, e := range evs {
			out[e.Kind] = true
		}
		return out
	}

	scoped := kinds(f.Project)
	if !scoped["mine"] {
		t.Fatal("своё событие исчезло из ленты проекта")
	}
	if scoped["theirs"] {
		t.Fatal("событие ЧУЖОГО проекта видно под фильтром")
	}
	if !scoped["platform"] {
		t.Fatal("платформенное событие скрыто фильтром — сужение должно быть не скрывающим")
	}

	all := kinds("")
	if !all["mine"] || !all["theirs"] || !all["platform"] {
		t.Fatalf("пустой фильтр обязан отдавать всё: %v", all)
	}
}
