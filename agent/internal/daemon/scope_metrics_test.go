package daemon

import (
	"context"
	"testing"

	"github.com/ufna/birdman/agent/internal/metrics"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// Пара (project, env) владельца дедика доезжает до ПЕР-СЕРВЕРНЫХ метрик
// (tracker #1008). Без неё привязанный к паре ключ видел в панели пустые
// графики СВОИХ дедиков: master сужает его запрос по
// `extra_label=project=…&extra_label=env=…` (master.md §6), а сузить серию,
// у которой лейблов нет, нечем.
//
// Здесь держится вся проводка внутри агента — от команды master'а до сэмпла,
// который рендерит /metrics: `metrics_test.go` проверяет только рендер, а
// `runtime/scope_labels_test.go` — только круг через label'ы контейнера.
func TestServerScopeReachesMetrics(t *testing.T) {
	sampleOf := func(m *Manager, id string) (metrics.ServerSample, bool) {
		t.Helper()
		for _, sv := range m.MetricsSample().Servers {
			if sv.ID == id {
				return sv, true
			}
		}
		return metrics.ServerSample{}, false
	}

	// Старт: пара приезжает в env-мапе StartServer, уходит в label'ы
	// контейнера (StartSpec) и одновременно ложится на сэмпл метрик.
	// Флаг log_scope_dirs в testManager ВЫКЛЮЧЕН — и это часть проверки:
	// он гейтит раскладку логов (у неё внешняя зависимость — конфиг vector'а
	// кладёт ansible), а метрики от него не зависят и не должны.
	t.Run("старт: пара едет в StartSpec и в сэмпл", func(t *testing.T) {
		rt := newFakeRuntime()
		m, _, _ := testManager(t, rt)
		m.Start(context.Background(), &agentlinkv1.StartServer{
			ServerId: "s1", ImageRef: "img:1", CmdId: "c1",
			Env: map[string]string{"BIRDMAN_PROJECT": "game", "BIRDMAN_ENV": "prod"},
		})
		eventually(t, "s1 starting", stateIs(m, "s1", "starting"))

		rt.mu.Lock()
		specs := append([]StartSpec(nil), rt.started...)
		rt.mu.Unlock()
		if len(specs) != 1 {
			t.Fatalf("StartSpec'ов %d, want 1", len(specs))
		}
		if specs[0].ScopeProject != "game" || specs[0].ScopeEnv != "prod" {
			t.Fatalf("StartSpec scope = (%q, %q), want (game, prod) — пара не доедет до label'ов контейнера",
				specs[0].ScopeProject, specs[0].ScopeEnv)
		}

		sv, ok := sampleOf(m, "s1")
		if !ok {
			t.Fatal("сервера нет в сэмпле метрик")
		}
		if sv.Project != "game" || sv.Env != "prod" {
			t.Fatalf("сэмпл = (%q, %q), want (game, prod) — графики привязанного ключа останутся пустыми",
				sv.Project, sv.Env)
		}
	})

	// Половина пары и мусор значат «пары нет» — на ОБЕИХ сторонах. Полупарная
	// серия под join'ом vmalert схлопывается на беспарную серию того же
	// server_id и убивает правило TickDegraded целиком (`duplicate output
	// timeseries`, замерено на живом VictoriaMetrics v1.102.1).
	for _, c := range []struct {
		name string
		env  map[string]string
	}{
		{"пары нет вовсе (старый master)", nil},
		{"только project", map[string]string{"BIRDMAN_PROJECT": "game"}},
		{"только env", map[string]string{"BIRDMAN_ENV": "prod"}},
		{"мусор в половине", map[string]string{"BIRDMAN_PROJECT": "../etc", "BIRDMAN_ENV": "prod"}},
	} {
		t.Run("старт: "+c.name, func(t *testing.T) {
			rt := newFakeRuntime()
			m, _, _ := testManager(t, rt)
			m.Start(context.Background(), &agentlinkv1.StartServer{
				ServerId: "s2", ImageRef: "img:1", CmdId: "c1", Env: c.env,
			})
			eventually(t, "s2 starting", stateIs(m, "s2", "starting"))

			rt.mu.Lock()
			spec := rt.started[0]
			rt.mu.Unlock()
			if spec.ScopeProject != "" || spec.ScopeEnv != "" {
				t.Fatalf("StartSpec scope = (%q, %q), want пусто", spec.ScopeProject, spec.ScopeEnv)
			}
			sv, ok := sampleOf(m, "s2")
			if !ok {
				t.Fatal("сервера нет в сэмпле метрик")
			}
			if sv.Project != "" || sv.Env != "" {
				t.Fatalf("сэмпл = (%q, %q), want пусто — половина пары ломает правило целиком", sv.Project, sv.Env)
			}
		})
	}

	// Рестарт агента: пара поднимается из label'ов контейнера тем же
	// Restore, что порт и состояние, — в памяти агента она не хранится.
	// Мусорный label (правили руками на ноде) до лейбла серии Prometheus не
	// доходит: дедик просто остаётся беспарным.
	t.Run("рестарт: пара поднимается из label'ов", func(t *testing.T) {
		rt := newFakeRuntime()
		rt.restored = []RestoredServer{
			{Handle: newFakeHandle(), ID: "r1", Port: 20005, ImageRef: "img:1", State: "ready", Running: true,
				ScopeProject: "game", ScopeEnv: "prod"},
			{Handle: newFakeHandle(), ID: "r2", Port: 20006, ImageRef: "img:1", State: "ready", Running: true},
			{Handle: newFakeHandle(), ID: "r3", Port: 20007, ImageRef: "img:1", State: "ready", Running: true,
				ScopeProject: "GAME/../x", ScopeEnv: "prod"},
		}
		m, _, _ := testManager(t, rt)
		if err := m.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		eventually(t, "r1 ready", stateIs(m, "r1", "ready"))

		if sv, _ := sampleOf(m, "r1"); sv.Project != "game" || sv.Env != "prod" {
			t.Fatalf("r1 = (%q, %q), want (game, prod) — пара не пережила рестарт агента", sv.Project, sv.Env)
		}
		if sv, _ := sampleOf(m, "r2"); sv.Project != "" || sv.Env != "" {
			t.Fatalf("r2 = (%q, %q), want пусто — дедик старше апгрейда пары не имеет", sv.Project, sv.Env)
		}
		if sv, _ := sampleOf(m, "r3"); sv.Project != "" || sv.Env != "" {
			t.Fatalf("r3 = (%q, %q), want пусто — негодный label не становится лейблом серии", sv.Project, sv.Env)
		}
	})
}
