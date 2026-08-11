package metrics_test

// Эпик #968, шаг 3: birdman_events_total получает лейбл project, и нулевая база
// становится (вид × проект).
//
// Главная ловушка здесь — источник набора серий. Если заводить нули по ФАКТУ
// (по строкам events), то вид, которого у проекта ещё не было, снова родится
// сразу со значением 1, и increase() даст 0 — те же грабли #960/#970, только в
// новой размерности. Поэтому источник — desired state: проекты из БД.

import (
	"context"
	"testing"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestEventsTotalCarriesProject(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := context.Background()

	if err := st.InsertEvent(ctx, store.EventCrashLoop, store.EventRef{NodeID: &f.NodeID}, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}
	m := metrics.New(st, testLog())

	if !gaugeSeriesPresent(t, m.Registry, "birdman_events_total", map[string]string{
		"kind": store.EventCrashLoop, "project": f.Project,
	}) {
		t.Fatal("crash_loop проекта не несёт лейбл project — алерт остался бы платформенным")
	}
}

// TestEventsTotalZeroBaselineIsDesiredState: нули заводятся для КАЖДОГО проекта
// из БД, даже если события такого вида у него никогда не было — иначе первый же
// краш-луп нового проекта увидели бы только со второго раза.
func TestEventsTotalZeroBaselineIsDesiredState(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := context.Background()

	if _, err := st.CreateProject(ctx, "quiet", 2); err != nil {
		t.Fatalf("create project: %v", err)
	}
	m := metrics.New(st, testLog())

	for _, project := range []string{f.Project, "quiet"} {
		for _, kind := range []string{store.EventCrashLoop, store.EventAgentUpgradeFailed} {
			if !gaugeSeriesPresent(t, m.Registry, "birdman_events_total", map[string]string{
				"kind": kind, "project": project,
			}) {
				t.Fatalf("нет нулевой базы (%s, %s): первый такой случай был бы невидим для increase()", kind, project)
			}
		}
	}
	// Платформенная серия (пустой проект) обязана существовать тоже: у
	// crash_loop бывают события, не принадлежащие никакому проекту.
	if !gaugeSeriesPresent(t, m.Registry, "birdman_events_total", map[string]string{
		"kind": store.EventCrashLoop, "project": "",
	}) {
		t.Fatal("нет платформенной нулевой базы crash_loop")
	}
}
