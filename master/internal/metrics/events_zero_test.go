package metrics_test

// #970: гейт «не выдумывать нули при сбое БД» у нулевой базы birdman_events_total
// не был закреплён тестом — его мутация проходила весь пакет зелёным.
//
// Почему гейт важен: нулевая база нужна, чтобы increase()-правила (CrashLoop,
// AgentUpgradeFailed) видели ПЕРВОЕ событие своего вида — серия, рождающаяся со
// значением 1, для increase() неотличима от «ничего не произошло». Но выдумать
// 0 после сбоя запроса нельзя: для Prometheus это выглядит как сброс счётчика,
// и следующее реальное событие сосчитается дважды. То есть икота базы обязана
// давать НЕМОТУ, а не ложные нули.
//
// Тест — по образцу соседнего TestServerInfoNotFabricatedOnQueryFailure.

import (
	"testing"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestEventsZeroBaselineNotFabricatedOnQueryFailure(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 8)
	ctx := t.Context()

	// Здоровая база: нулевая база событий эмитится — иначе тест ничего не
	// доказывал бы (проверяем сам факт наличия серии до поломки).
	healthy := metrics.New(st, testLog())
	if countSeries(t, healthy.Registry, "birdman_events_total", map[string]string{
		"kind": "crash_loop", "project": "game",
	}) == 0 {
		t.Fatal("на здоровой базе нулевая база events отсутствует — сломана предпосылка теста")
	}

	// Ломаем чтение ПОСРЕДИ строк, а не на старте запроса — гейт защищает именно
	// это. При полном отказе запроса нули и так не эмитятся (внешний if/else), и
	// тест на такой поломке проходил бы даже со снятым гейтом: проверено
	// мутацией, первая версия этого теста не кусалась.
	//
	// Вид `kind` меняем на массив: запрос стартует нормально, а Scan в string
	// падает на первой же строке → ok=false → нулевая база обязана быть
	// пропущена, потому что картина событий прочитана ЧАСТИЧНО. (int здесь не
	// годится — pgx кладёт его в string без ошибки, проверено.)
	if _, err := st.Pool.Exec(ctx, `alter table events rename to events_real`); err != nil {
		t.Fatalf("rename events: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `create view events as select array[1,2]::int[] as kind from events_real`); err != nil {
		t.Fatalf("create view: %v", err)
	}

	m := metrics.New(st, testLog())
	if n := countSeries(t, m.Registry, "birdman_events_total", map[string]string{
		"kind": "crash_loop", "project": "game",
	}); n != 0 {
		t.Fatalf("частично прочитанная картина событий выдумала нулевую базу (%d серий): для increase() это сброс счётчика", n)
	}
	// Остальной скрейп обязан выжить: коллектор логирует-и-продолжает, как соседи.
	if !gaugeSeriesPresent(t, m.Registry, "birdman_node_heartbeat_age_seconds", map[string]string{
		"node": "node-1", "region": "eu", "project": "game",
	}) {
		t.Fatal("сбой одного запроса обнулил весь скрейп")
	}
}
