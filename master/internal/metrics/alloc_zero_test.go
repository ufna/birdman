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

	"github.com/prometheus/client_golang/prometheus"

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

// #1066: desired state проектного измерения ведётся в ОБЕ стороны. Счётчик
// живёт в памяти процесса, поэтому серия удалённого проекта раньше доживала до
// рестарта мастера (на стенде — `{project="khl"}` при слаге `khl-legends`):
// призрак в панели/Grafana и монотонно растущая кардинальность.
//
// ГЛАВНОЕ в этом тесте — не то, что чужая серия пропала, а то, что уборка НЕ
// задела соседа: у живого проекта обязаны остаться и серии, и накопленные
// значения, иначе для increase() она выглядела бы сбросом счётчика у выживших.
func TestAllocFailuresDropSeriesOfDeletedProject(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8) // проект game
	ctx := t.Context()

	if _, err := st.CreateProject(ctx, "khl", 2); err != nil {
		t.Fatalf("create project: %v", err)
	}
	m := metrics.New(st, testLog())

	// Оба проекта успели накопить отказы: сосед — три, удаляемый — один.
	m.AllocFailures.WithLabelValues("no_capacity", f.Project).Add(3)
	m.AllocFailures.WithLabelValues("no_capacity", "khl").Inc()
	// Плюс мусор от bad_request по слагу, которого в БД нет вовсе (опечатка в
	// запросе заводит серию по лейблу ИЗ ЗАПРОСА — источник роста кардинальности
	// не менее реальный, чем удалённые проекты).
	m.AllocFailures.WithLabelValues("bad_request", "no-such-project").Inc()

	if v := counterValue(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
		"reason": "no_capacity", "project": "khl",
	}); v != 1 {
		t.Fatalf("серия удаляемого проекта до удаления = %v, ожидалась 1", v)
	}

	if _, err := st.DeleteProject(ctx, "khl", "khl"); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	// Скрейпы (Gather) — единственное, что происходит между удалением и
	// проверкой: рестарта мастера здесь нет, и это весь смысл карточки.
	//
	// Отставание на один скрейп — то же самое и по той же причине, что у
	// ЗАВЕДЕНИЯ серий выше: коллекторы зарегистрированы по отдельности, и
	// вектор мог отдать свою половину выдачи до того, как dbCollector дошёл до
	// уборки. Для призрака это безобидно ровно потому, что расти он не может.
	_ = gaugeSeriesPresent(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
		"reason": "no_capacity", "project": "khl",
	})
	for _, reason := range []string{"no_capacity", "bad_request", "env_required", "internal"} {
		if gaugeSeriesPresent(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
			"reason": reason, "project": "khl",
		}) {
			t.Fatalf("серия (%s, khl) пережила удаление проекта — призрак до рестарта мастера", reason)
		}
	}
	if gaugeSeriesPresent(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
		"reason": "bad_request", "project": "no-such-project",
	}) {
		t.Fatal("серия по слагу, которого нет в БД, не убрана — кардинальность растёт с каждой опечаткой в запросе")
	}

	// Сосед не пострадал: значение ровно то, что накопил (не ноль и не пусто),
	// а нулевая база по остальным причинам на месте.
	if v := counterValue(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
		"reason": "no_capacity", "project": f.Project,
	}); v != 3 {
		t.Fatalf("счётчик живого проекта = %v, ожидалось 3 — уборка чужой серии выглядит как сброс счётчика соседу", v)
	}
	for _, reason := range []string{"bad_request", "env_required", "internal"} {
		if v := counterValue(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
			"reason": reason, "project": f.Project,
		}); v != 0 {
			t.Fatalf("нулевая база (%s, %s) = %v, ожидался 0", reason, f.Project, v)
		}
	}
}

// Сбой запроса проектов НЕ снимает ничего: пустой список живых слагов после
// икоты базы означал бы «живых проектов нет» и стёр бы вектор целиком — для
// increase() это сброс счётчика у всех сразу.
func TestAllocFailuresNotDroppedOnQueryFailure(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := t.Context()

	m := metrics.New(st, testLog())
	m.AllocFailures.WithLabelValues("no_capacity", f.Project).Add(2)

	if _, err := st.Pool.Exec(ctx, `alter table projects rename to projects_gone`); err != nil {
		t.Fatalf("rename projects: %v", err)
	}
	if v := counterValue(t, m.Registry, "birdman_allocation_failures_total", map[string]string{
		"reason": "no_capacity", "project": f.Project,
	}); v != 2 {
		t.Fatalf("после сбоя запроса счётчик живого проекта = %v, ожидалось 2 (уборка обязана не делать НИЧЕГО)", v)
	}
}

// counterValue — значение серии счётчика с точным набором лейблов (findGauge
// рядом читает gauge-половину дескриптора и на счётчике вернул бы 0).
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.GetMetric() {
			if labelsMatch(met.GetLabel(), labels) {
				return met.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("серии %s%v нет вовсе", name, labels)
	return 0
}
