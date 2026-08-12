package runtime

import "testing"

// Пара (project, env) владельца дедика переживает рестарт агента ровно тем же
// механизмом, что порт, образ и состояние: она лежит в label'ах КОНТЕЙНЕРА
// (tracker #1008). Это единственное место, где пара хранится между запусками
// агента, поэтому круг «записали при create → прочитали при Restore»
// закрепляется здесь: сам StartServer/Restore требуют живого containerd и
// гоняются только под -tags integration, которые CI не запускает.
func TestContainerLabelsCarryScopeAcrossRestart(t *testing.T) {
	base := ServerSpec{ID: "srv-1", Port: 30000, ImageRef: "reg/img:v1"}

	t.Run("пара записана и прочитана обратно", func(t *testing.T) {
		sp := base
		sp.ScopeProject, sp.ScopeEnv = "game", "prod"
		labels := containerLabels(sp)

		if got := labels[LabelProject]; got != "game" {
			t.Fatalf("%s = %q, want %q", LabelProject, got, "game")
		}
		if got := labels[LabelEnv]; got != "prod" {
			t.Fatalf("%s = %q, want %q", LabelEnv, got, "prod")
		}
		project, env := scopeFromLabels(labels)
		if project != "game" || env != "prod" {
			t.Fatalf("scopeFromLabels = (%q, %q), want (game, prod) — пара не переживает рестарт агента", project, env)
		}
	})

	// Дедик, запущенный ДО этой сборки, пары не получает никогда: label
	// чеканится при create и не дописывается задним числом. Контейнер обязан
	// выглядеть ровно как раньше, иначе Restore начал бы выдумывать пару.
	t.Run("пары нет — набор label'ов прежний", func(t *testing.T) {
		labels := containerLabels(base)
		for _, l := range []string{LabelProject, LabelEnv} {
			if _, ok := labels[l]; ok {
				t.Fatalf("label %s поставлен без пары: %v", l, labels)
			}
		}
		if project, env := scopeFromLabels(labels); project != "" || env != "" {
			t.Fatalf("scopeFromLabels = (%q, %q), want пусто", project, env)
		}
		// Идентичность контейнера в остальном не поехала.
		for l, want := range map[string]string{
			LabelServerID: "srv-1", LabelPort: "30000", LabelImage: "reg/img:v1", LabelState: "starting",
		} {
			if got := labels[l]; got != want {
				t.Fatalf("label %s = %q, want %q", l, got, want)
			}
		}
	})

	// ВСЁ-ИЛИ-НИЧЕГО, и это не аккуратность, а несущее условие. Серия с
	// `project`, но без `env` схлопывается под join'ом TickDegraded
	// (`on (server_id) group_left (project)`) в тот же набор лейблов, что и
	// беспарная серия того же server_id, и правило умирает целиком с
	// `duplicate output timeseries` — замерено на живом VictoriaMetrics
	// v1.102.1. Половина пары обязана значить «пары нет» на ОБЕИХ сторонах.
	for _, c := range []struct{ name, project, env string }{
		{"только project", "game", ""},
		{"только env", "", "prod"},
	} {
		t.Run("половина не проходит: "+c.name, func(t *testing.T) {
			sp := base
			sp.ScopeProject, sp.ScopeEnv = c.project, c.env
			labels := containerLabels(sp)
			if _, ok := labels[LabelProject]; ok {
				t.Fatalf("половина пары стала label'ом: %v", labels)
			}
			if _, ok := labels[LabelEnv]; ok {
				t.Fatalf("половина пары стала label'ом: %v", labels)
			}
			// И на чтении тоже: контейнер мог быть размечен чем угодно.
			half := map[string]string{LabelProject: c.project, LabelEnv: c.env}
			if project, env := scopeFromLabels(half); project != "" || env != "" {
				t.Fatalf("scopeFromLabels(половина) = (%q, %q), want пусто", project, env)
			}
		})
	}
}
