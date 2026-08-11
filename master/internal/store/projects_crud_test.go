package store_test

// CRUD проектов из админки (слайс B): явное создание (в отличие от молчаливого
// ensureProject), состав перед удалением и каскад. Главное, что проверяется —
// границы: дубль слага не перезаписывает чужой проект, живая нода не даёт снести
// проект под собой, а каскад уносит ровно содержимое этого проекта и отзывает
// его ключи, не задевая соседний.

import (
	"context"
	"errors"
	"testing"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestCreateProjectExplicit(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10) // проект "game" уже есть
	ctx := context.Background()

	p, err := st.CreateProject(ctx, "arena", 4)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Slug != "arena" || p.MatchSize != 4 {
		t.Fatalf("got %+v, want slug=arena match_size=4", p)
	}
	// Новый проект пригоден к работе сразу: dev+prod засеяны (ensureProject).
	envs, err := st.ListEnvironments(ctx, "arena")
	if err != nil || len(envs) != 2 {
		t.Fatalf("environments = %d (%v), want dev+prod", len(envs), err)
	}

	// Дубль слага — конфликт, а НЕ тихая перезапись чужого match_size.
	if _, err := st.CreateProject(ctx, "game", 99); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate slug: want ErrConflict, got %v", err)
	}
	if got, _ := st.GetProject(ctx, "game"); got.MatchSize == 99 {
		t.Fatal("дубль перезаписал match_size существующего проекта")
	}

	// Слаг проверяется: он едет в URL, в тела матчмейкинга и в CI.
	if _, err := st.CreateProject(ctx, "Bad Slug", 2); err == nil {
		t.Fatal("слаг с пробелом и заглавными принят")
	}
}

func TestDeleteProjectBlockedByLiveNode(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	_, err := st.DeleteProject(ctx, f.Project, f.Project)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete with a live node: want ErrConflict, got %v", err)
	}

	// Выведенная нода предусловию не мешает: бокса уже нет, и держать из-за неё
	// проект вечно неудаляемым не за что.
	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	usage, err := st.ProjectUsage(ctx, f.Project)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.Nodes != 0 {
		t.Fatalf("usage.Nodes = %d, want 0 (выведенные не считаются живыми)", usage.Nodes)
	}
	// …но «пустым» проект от этого не становится: выведенная нода — тоже
	// история, которая уедет с проектом, поэтому подтверждение спрашивается.
	// Дырку нашла Фаза D: без RetiredNodes в Empty() такой проект сносился
	// молча, без единого вопроса.
	if usage.RetiredNodes != 1 {
		t.Fatalf("usage.RetiredNodes = %d, want 1", usage.RetiredNodes)
	}
	if _, err := st.DeleteProject(ctx, f.Project, ""); !errors.Is(err, store.ErrConfirmRequired) {
		t.Fatalf("проект с выведенной нодой снёсся без подтверждения: %v", err)
	}
	if _, err := st.DeleteProject(ctx, f.Project, f.Project); err != nil {
		t.Fatalf("delete after revoke: %v", err)
	}
}

func TestDeleteProjectCascadeAndConfirm(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	// Соседний проект — контроль того, что каскад не выходит за свои границы.
	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatalf("create neighbour: %v", err)
	}
	nProject, nEnv := "neighbour", "dev"
	_, neighbourSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "neighbour-key", Scopes: []string{"matchmaking"}, Project: &nProject, Env: &nEnv,
	})
	if err != nil {
		t.Fatalf("neighbour key: %v", err)
	}

	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Непустой проект без подтверждения не сносится.
	if _, err := st.DeleteProject(ctx, f.Project, ""); !errors.Is(err, store.ErrConfirmRequired) {
		t.Fatalf("no confirm: want ErrConfirmRequired, got %v", err)
	}
	if _, err := st.DeleteProject(ctx, f.Project, "wrong"); !errors.Is(err, store.ErrConfirmRequired) {
		t.Fatalf("wrong confirm: want ErrConfirmRequired, got %v", err)
	}

	usage, err := st.ProjectUsage(ctx, f.Project)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	res, err := st.DeleteProject(ctx, f.Project, f.Project)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	// usage обещает ровно то, что сносит каскад — иначе диалог показывал бы одно,
	// а происходило другое.
	if res.Versions != usage.Versions || res.Environments != usage.Environments {
		t.Fatalf("каскад разошёлся с usage: %+v vs %+v", res, usage)
	}
	if _, err := st.GetProject(ctx, f.Project); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("проект пережил удаление: %v", err)
	}

	// Сосед цел: и сам проект, и его ключ.
	if _, err := st.GetProject(ctx, "neighbour"); err != nil {
		t.Fatalf("сосед пострадал: %v", err)
	}
	if _, err := st.AuthAPIKey(ctx, neighbourSecret); err != nil {
		t.Fatalf("ключ соседнего проекта отозван каскадом: %v", err)
	}
}
