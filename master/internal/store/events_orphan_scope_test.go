package store_test

// tracker #1083: аудит удалённого проекта не стирается — и не становится
// общедоступным.
//
// До этих тестов был закреплён ровно ОДИН из двух инвариантов. Что строка
// переживает удаление проекта, пинует TestDeleteProjectKeepsEventsAsOrphaned;
// КТО её после этого может прочитать, не проверял никто — а читали её все:
// `on delete set null` обнулял project_id, а `project_id is null` означало для
// читателей «платформенное событие, видно под любым фильтром». Привязанный
// readonly-ключ соседнего проекта получал всю историю удалённого целиком,
// с payload'ами.
//
// Проверяется СЛЕДСТВИЕ (что видит сосед), а не наличие колонки: утверждение
// про колонку пережило бы откат условия в читателях, а про выдачу — нет. И оба
// входа в таблицу (лента + выборка стрима) проверяются вместе, потому что
// правило у них общее и разойтись ему нельзя (#999): иначе сужение ленты
// обходится подпиской на стрим.

import (
	"context"
	"testing"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// seenBy собирает виды событий, которые видны под фильтром project, ОБОИМИ
// читателями сразу: ключ карты — вид, значение — пара «видно в ленте / видно в
// выборке стрима».
type seenPair struct{ list, stream bool }

func seenBy(t *testing.T, st *store.Store, project string) map[string]seenPair {
	t.Helper()
	ctx := context.Background()
	out := map[string]seenPair{}

	evs, err := st.ListEvents(ctx, 1000, project)
	if err != nil {
		t.Fatalf("ListEvents(%q): %v", project, err)
	}
	for _, e := range evs {
		p := out[e.Kind]
		p.list = true
		out[e.Kind] = p
	}

	stream, _, err := st.ListEventsAfter(ctx, 0, 1000, project)
	if err != nil {
		t.Fatalf("ListEventsAfter(%q): %v", project, err)
	}
	for _, e := range stream {
		p := out[e.Kind]
		p.stream = true
		out[e.Kind] = p
	}
	return out
}

// hiddenFrom требует, чтобы вида не было НИ У ОДНОГО из двух читателей.
func hiddenFrom(t *testing.T, seen map[string]seenPair, project, kind, why string) {
	t.Helper()
	p := seen[kind]
	if p.list {
		t.Errorf("%s: %q видно в ленте проекта %q", why, kind, project)
	}
	if p.stream {
		t.Errorf("%s: %q видно в выборке стрима проекта %q", why, kind, project)
	}
}

// visibleTo требует, чтобы вид видели ОБА читателя.
func visibleTo(t *testing.T, seen map[string]seenPair, project, kind, why string) {
	t.Helper()
	p := seen[kind]
	if !p.list {
		t.Errorf("%s: %q пропало из ленты проекта %q", why, kind, project)
	}
	if !p.stream {
		t.Errorf("%s: %q пропало из выборки стрима проекта %q", why, kind, project)
	}
}

// seedDoomedProject сеет проект game (через testdb.Seed), соседа neighbour и
// три события: своё у game (с чувствительным payload'ом), платформенное и своё
// у neighbour. Возвращает фикстуру game.
func seedDoomedProject(t *testing.T, st *store.Store) *testdb.Fixture {
	t.Helper()
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 8)
	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatalf("create neighbour: %v", err)
	}
	// Payload здесь ровно тот, ради которого заведена карточка: hostname ноды
	// чужого проекта. Атрибуция — по ссылке на ноду, то есть по факту.
	if err := st.InsertEvent(ctx, "doomed_secret", store.EventRef{NodeID: &f.NodeID},
		map[string]any{"hostname": "secret-node-1"}); err != nil {
		t.Fatalf("insert doomed event: %v", err)
	}
	if err := st.InsertEvent(ctx, "platform_born", store.EventRef{}, nil); err != nil {
		t.Fatalf("insert platform event: %v", err)
	}
	if err := st.InsertEvent(ctx, "neighbour_own", store.EventRef{},
		map[string]any{"project": "neighbour"}); err != nil {
		t.Fatalf("insert neighbour event: %v", err)
	}
	return f
}

// deleteProject выводит ноды и удаляет проект — предусловие каскада «живых нод
// не осталось».
func deleteProject(t *testing.T, st *store.Store, f *testdb.Fixture) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke node: %v", err)
	}
	if _, err := st.DeleteProject(ctx, f.Project, f.Project); err != nil {
		t.Fatalf("delete project: %v", err)
	}
}

// TestDeletedProjectEventsHiddenFromOtherTenants — приёмка карточки: событие
// удалённого проекта не видит привязанный ключ ДРУГОГО проекта. И вместе с ним
// проверяются оба соседних инварианта, потому что чинить один за счёт другого
// здесь легко: аудит не стёрт (без фильтра всё на месте) и сужение осталось не
// скрывающим (настоящее платформенное событие под фильтром видно).
func TestDeletedProjectEventsHiddenFromOtherTenants(t *testing.T) {
	st := testdb.New(t)
	f := seedDoomedProject(t, st)
	deleteProject(t, st, f)

	nb := seenBy(t, st, "neighbour")
	hiddenFrom(t, nb, "neighbour", "doomed_secret",
		"история удалённого проекта уехала соседу")
	// Событие пишет САМ каскад, и оно называет чужой проект вместе с именами
	// его ключей и размером — то есть утечка приезжает даже к тому, кто ничего
	// не писал в ленту до удаления.
	hiddenFrom(t, nb, "neighbour", store.EventProjectDeleted,
		"событие самого каскада уехало соседу")
	hiddenFrom(t, nb, "neighbour", store.EventNodeRevoked,
		"события нод удалённого проекта уехали соседу")

	visibleTo(t, nb, "neighbour", "neighbour_own", "своё событие пропало из ленты проекта")
	visibleTo(t, nb, "neighbour", "platform_born",
		"платформенное по рождению событие скрыто — сужение обязано остаться не скрывающим")

	// Аудит не стёрт: непривязанный ключ (панель, admin, глобальный) читает всё.
	all := seenBy(t, st, "")
	visibleTo(t, all, "", "doomed_secret", "аудит удалённого проекта пропал из платформенного чтения")
	visibleTo(t, all, "", store.EventProjectDeleted, "событие каскада пропало из платформенного чтения")
}

// TestRecreatedProjectDoesNotInheritDeadProjectEvents: слаг переиспользуем —
// пересоздание проекта с тем же именем штатный путь (переименования нет by
// design). Снимок слага НЕ адресует проект, поэтому наследования быть не
// должно; сузь кто-нибудь ленту по имени вместо project_id (выглядит дешевле —
// один join долой), и новый арендатор получил бы историю мёртвого.
func TestRecreatedProjectDoesNotInheritDeadProjectEvents(t *testing.T) {
	st := testdb.New(t)
	f := seedDoomedProject(t, st)
	deleteProject(t, st, f)

	if _, err := st.CreateProject(context.Background(), f.Project, 2); err != nil {
		t.Fatalf("recreate project: %v", err)
	}
	reborn := seenBy(t, st, f.Project)
	hiddenFrom(t, reborn, f.Project, "doomed_secret",
		"пересозданный под тем же слагом проект унаследовал ленту мёртвого")
	hiddenFrom(t, reborn, f.Project, store.EventProjectDeleted,
		"пересозданный под тем же слагом проект унаследовал ленту мёртвого")
	visibleTo(t, reborn, f.Project, "platform_born",
		"платформенное по рождению событие скрыто у пересозданного проекта")
}
