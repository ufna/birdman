package httpapi_test

import (
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Стрим не теряет событие, чей id МЕНЬШЕ уже отданного, а коммит — позже
// (tracker #1013).
//
// `events.id` — identity, номер выдаётся в момент INSERT'а, а события пишутся
// внутри транзакций. Конструкция теста ровно та, что в карточке:
//
//	tx A: insert (получает id N)          — коммит задержан
//	вне tx: insert (получает id N+1)      — виден сразу
//	опрос стрима: видит N+1, не видит N
//	tx A: commit                          — N становится виден
//
// Раньше курсор в этот момент уже стоял на N+1, и событие N не приходило
// НИКОГДА (следующий опрос спрашивает `id > N+1`). Теперь курсор перешагивает
// границу окна не раньше, чем через sseSettle после того, как её УВИДЕЛ, —
// поэтому запоздавший коммит попадает в ленту.
func TestSSESurvivesCommitRace(t *testing.T) {
	st := testdb.New(t)
	ts := apiServer(t, st)
	ctx := t.Context()

	key := scopedKey(t, st, "reader", httpapi.ScopeReadonly)
	c := openSSE(t, ts.URL+"/v1/events/stream?after_id=0", key)

	// Первое событие — маркер живой ленты: без него тест прошёл бы и на
	// стриме, который вообще ничего не отдаёт.
	if err := st.InsertEvent(ctx, "project_created", store.EventRef{},
		map[string]any{"project": "warmup"}); err != nil {
		t.Fatal(err)
	}
	if ev := c.next(t, 15*time.Second); ev.kind != "project_created" {
		t.Fatalf("прогрев: кадр = %q, want project_created", ev.kind)
	}

	// ОТСТАЮЩИЙ КОММИТ: id берётся сейчас, видимым станет позже.
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var lateID int64
	if err := tx.QueryRow(ctx,
		`insert into events (kind, payload) values ('node_created', '{"marker":"late-commit"}'::jsonb) returning id`).
		Scan(&lateID); err != nil {
		t.Fatal(err)
	}

	// Обгоняющее событие: id больше, видно сразу.
	if err := st.InsertEvent(ctx, "version_registered", store.EventRef{},
		map[string]any{"marker": "overtaker"}); err != nil {
		t.Fatal(err)
	}
	if ev := c.next(t, 15*time.Second); ev.kind != "version_registered" {
		t.Fatalf("обгоняющее событие: кадр = %q, want version_registered", ev.kind)
	}
	// Строго: обгоняющее получило БОЛЬШИЙ id, иначе гонка не воспроизведена.
	var overtakerID int64
	if err := st.Pool.QueryRow(ctx,
		`select max(id) from events where kind = 'version_registered'`).Scan(&overtakerID); err != nil {
		t.Fatal(err)
	}
	if overtakerID <= lateID {
		t.Fatalf("сцена не та: id обгоняющего %d не больше отстающего %d", overtakerID, lateID)
	}

	// Запоздавший коммит — в пределах окна ожидания курсора.
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ev, ok := c.nextOrNone(time.Until(deadline))
		if !ok {
			break
		}
		if ev.id == lateID {
			if ev.kind != "node_created" {
				t.Fatalf("запоздавшее событие приехало не тем: %q", ev.kind)
			}
			return
		}
	}
	t.Fatalf("событие id=%d (коммит позже обгоняющего id=%d) не пришло в ленту — курсор перешагнул его",
		lateID, overtakerID)
}

// Повторное чтение окна, пока курсор отстаёт, не должно ДВОИТЬ кадры: id,
// отданные выше курсора, помнятся по соединению. Без этого починка гонки
// оплачивалась бы дубликатами на каждом опросе в течение sseSettle.
func TestSSEDoesNotDuplicateWhileCursorLags(t *testing.T) {
	st := testdb.New(t)
	ts := apiServer(t, st)
	ctx := t.Context()

	key := scopedKey(t, st, "reader", httpapi.ScopeReadonly)
	c := openSSE(t, ts.URL+"/v1/events/stream?after_id=0", key)

	const n = 5
	for i := 0; i < n; i++ {
		if err := st.InsertEvent(ctx, "project_created", store.EventRef{},
			map[string]any{"project": "dup-probe"}); err != nil {
			t.Fatal(err)
		}
	}

	// Читаем ДОЛЬШЕ отсрочки курсора, чтобы окно успело перечитаться много раз
	// и курсор успел сдвинуться хотя бы однажды, и считаем каждый id. Срок
	// выведен из самой отсрочки (tracker #1016): вписанный числом, он молча
	// разъехался бы с ней при первой смене — и тест перестал бы доживать до
	// повторного чтения окна, то есть до того единственного, ради чего написан.
	// Перечитываний тут ssePollInterval'ов на sseSettle, то есть их ДЕСЯТКИ.
	seen := map[int64]int{}
	deadline := time.Now().Add(httpapi.SSESettleForTest() + 5*httpapi.SSEPollIntervalForTest())
	for time.Now().Before(deadline) {
		ev, ok := c.nextOrNone(time.Until(deadline))
		if !ok {
			break
		}
		seen[ev.id]++
	}
	if len(seen) < n {
		t.Fatalf("пришло %d разных событий, want >= %d", len(seen), n)
	}
	for id, times := range seen {
		if times > 1 {
			t.Fatalf("кадр id=%d отдан %d раз — окно перечитывается без дедупликации", id, times)
		}
	}
}
