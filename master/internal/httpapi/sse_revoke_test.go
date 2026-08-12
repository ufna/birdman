package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// waitClosed ждёт, пока сервер закроет стрим (канал строк дочитан до конца).
// Возвращает false по таймауту — то есть «лента жива».
func (c *sseClient) waitClosed(timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			return false
		case _, ok := <-c.lines:
			if !ok {
				return true
			}
		}
	}
}

// Отзыв ключа закрывает УЖЕ ОТКРЫТУЮ ленту (tracker #1016).
//
// `requireScope` аутентифицирует запрос один раз, а SSE-запрос живёт часами:
// `DELETE /v1/apikeys/{id}` делал ключ негодным для НОВЫХ запросов, а открытая
// лента продолжала отдавать события, пока клиент держит сокет. У обычных ручек
// лаг ограничен `authCacheTTL` (5 минут), у стрима лага не было вовсе.
//
// Тест держит ОБЕ стороны: отозванный ключ теряет ленту, а сосед со своим
// ключом её сохраняет — иначе «починка» могла бы закрывать все стримы подряд
// (например, на любой ошибке перепроверки), и тест бы этого не заметил.
func TestSSEClosesOnKeyRevoke(t *testing.T) {
	st := testdb.New(t)
	ts := apiServer(t, st)
	ctx := t.Context()

	victim, victimSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "reader-victim", Scopes: []string{httpapi.ScopeReadonly},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, bystanderSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "reader-bystander", Scopes: []string{httpapi.ScopeReadonly},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, adminSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "admin", Scopes: []string{httpapi.ScopeAdmin},
	})
	if err != nil {
		t.Fatal(err)
	}

	url := ts.URL + "/v1/events/stream?after_id=0"
	revoked := openSSE(t, url, victimSecret)
	bystander := openSSE(t, url, bystanderSecret)

	// Обе ленты живые: событие доезжает до каждой. Без этой проверки тест ниже
	// прошёл бы и на стриме, который никогда ничего не отдавал.
	if err := st.InsertEvent(ctx, "project_created", store.EventRef{},
		map[string]any{"project": "game"}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*sseClient{"отзываемый": revoked, "сосед": bystander} {
		if ev := c.next(t, 15*time.Second); ev.kind != "project_created" {
			t.Fatalf("%s: первый кадр = %q, want project_created", name, ev.kind)
		}
	}

	admin := &client{t: t, base: ts.URL, key: adminSecret}
	if code, b := admin.do("DELETE", "/v1/apikeys/"+victim.ID, nil); code != 200 {
		t.Fatalf("revoke: %d %v", code, b)
	}

	// Отозванный ключ теряет ленту в пределах периода опроса (1с) — ждём с
	// запасом. Именно ЗАКРЫТИЕ соединения, а не «перестали приходить события»:
	// клиент EventSource переоткроет его и получит честный 401.
	if !revoked.waitClosed(15 * time.Second) {
		t.Fatal("лента отозванного ключа не закрылась — отзыв не действует на открытые соединения")
	}

	// Сосед не пострадал и продолжает получать события.
	if err := st.InsertEvent(ctx, "project_created", store.EventRef{},
		map[string]any{"project": "arena"}); err != nil {
		t.Fatal(err)
	}
	// Кадры между ними тоже прилетят (сам `apikey_revoked` — платформенное
	// событие), поэтому ищем СВОЙ маркер, а не «следующий кадр».
	deadline := time.Now().Add(15 * time.Second)
	found := false
	for !found && time.Now().Before(deadline) {
		ev, ok := bystander.nextOrNone(time.Until(deadline))
		if !ok {
			break
		}
		found = ev.kind == "project_created" && strings.Contains(ev.data, "arena")
	}
	if !found {
		t.Fatal("сосед перестал получать события после чужого отзыва — закрыли лишнее")
	}

	// И переоткрытие отозванным ключом — 401 от requireScope, а не новая лента.
	req := &client{t: t, base: ts.URL, key: victimSecret}
	if code, _ := req.do("GET", "/v1/events", nil); code != 401 {
		t.Fatalf("отозванный ключ на обычной ручке: want 401, got %d", code)
	}
}
