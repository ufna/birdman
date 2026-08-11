package metrics_test

// #966: BufferEmptyAllocFail/AllocationFailures пропускали ПЕРВЫЙ отказ
// аллокации, и дыра взводилась заново после КАЖДОГО рестарта master.
//
// Механика: birdman_allocation_failures_total — in-process CounterVec. Серия
// пары (reason, project) не существует, пока не случился первый отказ, и
// рождается сразу со значением 1. increase() по серии, которая всегда читалась
// 1, даёт 0 — алерт загорался только со второго отказа. У соседнего
// birdman_events_total дыра одноразовая (он выводится из append-only таблицы) и
// закрыта нулевой базой в #960; здесь же счётчик умирает вместе с процессом.

import (
	"testing"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestAllocFailuresPreinitializedToZero(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := t.Context()

	m := metrics.New(st, testLog())

	// Скрейп заводит серии для каждого проекта: increase() теперь есть от чего
	// расти, и ПЕРВЫЙ отказ станет видимым.
	for _, reason := range []string{"no_capacity", "bad_request", "env_required", "internal"} {
		if !gaugeSeriesPresent(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
			"reason": reason, "project": f.Project,
		}) {
			t.Fatalf("серия (%s, %s) не пре-инициализирована — первый отказ будет невидим для increase()",
				reason, f.Project)
		}
	}

	// Проект, заведённый ПОСЛЕ старта, подхватывается скрейпами — ради этого
	// источник проектов и взят из БД, а не из хука на создание.
	//
	// Отставание ровно на один скрейп и оно неизбежно: коллекторы
	// зарегистрированы по отдельности, и серии, созданные во время сбора,
	// попадают в ВЫДАЧУ со следующего раза. Для новорождённого проекта это
	// безобидно — отказывать в аллокации ему нечем, пока у него нет ни нод, ни
	// версий. Первый Gather ниже играет роль этого «предыдущего» скрейпа.
	if _, err := st.CreateProject(ctx, "arena", 2); err != nil {
		t.Fatalf("create project: %v", err)
	}
	_ = gaugeSeriesPresent(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
		"reason": "no_capacity", "project": "arena",
	})
	if !gaugeSeriesPresent(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
		"reason": "no_capacity", "project": "arena",
	}) {
		t.Fatal("новый проект не получил нулевую базу и через скрейп")
	}
}

// TestAllocFailuresNotPreinitializedOnQueryFailure: та же дисциплина, что у
// нулевой базы событий (#970) и collectReadyZeros — при сбое запроса ничего не
// выдумываем. Ноль, появившийся после икоты базы, для increase() выглядит как
// сброс счётчика.
func TestAllocFailuresNotPreinitializedOnQueryFailure(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 8)
	ctx := t.Context()

	if _, err := st.Pool.Exec(ctx, `alter table projects rename to projects_gone`); err != nil {
		t.Fatalf("rename projects: %v", err)
	}

	m := metrics.New(st, testLog())
	if gaugeSeriesPresent(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
		"reason": "no_capacity", "project": "game",
	}) {
		t.Fatal("сбой запроса проектов выдумал нулевую базу")
	}
}
