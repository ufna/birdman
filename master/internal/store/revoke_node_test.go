package store_test

// Вывод ноды из флота (слайс C спеки panel-scope-projects-nodes): ревокация —
// единственный путь в `dead`, и до неё это состояние ставилось только SQL-ом на
// боксе. Проверяем три вещи, каждая из которых защищает своё:
// предусловие «нет живых серверов» (иначе ревокация оборвала бы идущий матч),
// идемпотентность (панель может отправить дважды) и то, ради чего всё затеяно —
// мёртвая нода перестаёт блокировать sweep снятия образов своего окружения.

import (
	"context"
	"errors"
	"testing"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestRevokeNodeRequiresNoLiveServers: нода с живым сервером не ревокается —
// для неё есть drain. Реапнутый сервер ревокации не мешает.
func TestRevokeNodeRequiresNoLiveServers(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	srv := f.InsertServerOn(t, f.NodeID, f.VersionID, "allocated")

	if _, err := st.RevokeNode(ctx, f.NodeID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("revoke with a live server: want ErrConflict, got %v", err)
	}
	if got := nodeState(t, st, f.NodeID); got == "dead" {
		t.Fatal("нода с живым сервером ревокнулась — матч оборвался бы")
	}

	// Сервер отыграл (reaped) — теперь ревокация обязана пройти.
	if _, err := st.Pool.Exec(ctx,
		`update servers set state = 'reaped' where id = $1::uuid`, srv); err != nil {
		t.Fatalf("reap server: %v", err)
	}
	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke after reap: %v", err)
	}
	if got := nodeState(t, st, f.NodeID); got != "dead" {
		t.Fatalf("state = %s, want dead", got)
	}
}

// TestRevokeNodeIsIdempotent: повтор не плодит события — иначе ретрай панели
// засорял бы историю, по которой оператор читает, что случилось с нодой.
func TestRevokeNodeIsIdempotent(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()

	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	ev := lastEvent(t, st, store.EventNodeRevoked)
	if ev.NodeID == nil || *ev.NodeID != f.NodeID {
		t.Fatalf("node_revoked node_id = %v, want %s", ev.NodeID, f.NodeID)
	}
	if from, _ := ev.Payload["from"].(string); from != "active" {
		t.Fatalf("payload from = %q, want active", from)
	}
	if host, _ := ev.Payload["hostname"].(string); host == "" {
		t.Fatal("payload без hostname — в Events нода будет неузнаваема")
	}

	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if c, _ := st.CountEvents(ctx, store.EventNodeRevoked); c != 1 {
		t.Fatalf("want exactly 1 node_revoked event, got %d", c)
	}
}

// TestRevokeNodeNotFound: несуществующая нода — 404-класс, а не тихий успех.
func TestRevokeNodeNotFound(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10)

	_, err := st.RevokeNode(context.Background(), "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
