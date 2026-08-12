package metrics_test

// tracker #1084: удаление проекта не поднимает ложный платформенный CrashLoop.
//
// birdman_events_total деривится ЗАПРОСОМ по живому join'у, то есть значение
// серии выводится из БД на КАЖДОМ скрейпе, а не копится в процессе. Пока
// осиротевшая строка была неотличима от платформенной, `on delete set null`
// переносил весь исторический объём проекта из {kind, project="A"} в
// {kind, project=""} за один скрейп — платформенный Counter ступенчато рос на
// историческое число, и `increase(...[5m]) > 0` читал это как всплеск.
// Рутинное удаление проекта поднимало critical с ПУСТЫМ $labels.project.
//
// Гейт свежести скрейпа (#1063) здесь бессилен по построению: он требует лишь
// непрерывно здорового `up`, а тут скрейп исправен и растёт САМО значение.
// Поэтому проверяется источник, и проверяется РОСТОМ МЕЖДУ ДВУМЯ СКРЕЙПАМИ —
// ровно тем, что видит правило, — а не наличием колонки в схеме.
//
// Не путать с #1066: там серии in-process счётчика не исчезали после удаления
// проекта (desired state вёлся только в плюс). Здесь серия DB-derived, и
// вектор обратный — значение ПЕРЕЕЗЖАЕТ между сериями.

import (
	"context"
	"testing"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestDeletedProjectDoesNotInflatePlatformEventCounters — приёмка карточки.
//
// Оба алертных вида берутся вместе не для полноты: правила CrashLoop и
// AgentUpgradeFailed читают одну и ту же серию с разным `kind`, и починка,
// сделанная в выборке, обязана накрывать оба разом — если она накрыла только
// один, значит сделана не в выборке.
func TestDeletedProjectDoesNotInflatePlatformEventCounters(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := context.Background()

	kinds := []string{store.EventCrashLoop, store.EventAgentUpgradeFailed}
	for _, kind := range kinds {
		if err := st.InsertEvent(ctx, kind, store.EventRef{NodeID: &f.NodeID}, nil); err != nil {
			t.Fatalf("insert %s: %v", kind, err)
		}
	}

	m := metrics.New(st, testLog())

	// СКРЕЙП ДО. История лежит в серии проекта, платформенная — на нулевой базе.
	before := map[string]float64{}
	for _, kind := range kinds {
		if got := findCounter(t, m.Registry, "birdman_events_total", map[string]string{
			"kind": kind, "project": f.Project,
		}); got != 1 {
			t.Fatalf("до удаления: серия проекта (%s) = %v, want 1", kind, got)
		}
		before[kind] = findCounter(t, m.Registry, "birdman_events_total", map[string]string{
			"kind": kind, "project": "",
		})
		if before[kind] != 0 {
			t.Fatalf("до удаления: платформенная серия (%s) = %v, want 0 — сцена собрана неверно",
				kind, before[kind])
		}
	}

	if _, err := st.RevokeNode(ctx, f.NodeID); err != nil {
		t.Fatalf("revoke node: %v", err)
	}
	if _, err := st.DeleteProject(ctx, f.Project, f.Project); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	// СКРЕЙП ПОСЛЕ. Правило видит именно это: значение той же серии на
	// следующем скрейпе. Требуется РАВЕНСТВО, а не «не больше единицы»: рост на
	// любую величину — это уже всплеск для increase().
	for _, kind := range kinds {
		got := findCounter(t, m.Registry, "birdman_events_total", map[string]string{
			"kind": kind, "project": "",
		})
		if got != before[kind] {
			t.Errorf("платформенная серия (%s) выросла за один скрейп: %v -> %v; "+
				"increase() прочтёт это как всплеск и поднимет critical с пустым project",
				kind, before[kind], got)
		}
	}

	// И серия удалённого проекта уходит с метрической поверхности целиком —
	// ни строкой из events, ни нулевой базой desired state. Алерта это не
	// поднимает (increase() по константным сэмплам даёт 0), а держать её значило
	// бы копить кардинальность мёртвыми проектами; то же направление, что #1066.
	for _, kind := range kinds {
		if gaugeSeriesPresent(t, m.Registry, "birdman_events_total", map[string]string{
			"kind": kind, "project": f.Project,
		}) {
			t.Errorf("серия удалённого проекта (%s) осталась на метрической поверхности", kind)
		}
	}
}
