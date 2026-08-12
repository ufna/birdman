package stats

import (
	"strings"
	"testing"

	"github.com/ufna/birdman/master/internal/store"
)

// legacyUtilizationNote — подпись, которую получает НЕПРИВЯЗАННЫЙ ключ.
// Записана здесь дословно намеренно: #993 специально сохранил ответ
// глобального ключа байт-в-байт, и «поправили заодно и его» должно краснеть.
const legacyUtilizationNote = "current, platform-wide snapshot (allocated/ready/draining vs active-node " +
	"capacity across ALL environments — the ?env filter does not narrow this capacity view); " +
	"utilization over time is available via the metrics proxy (birdman_servers, query_range)"

// TestUtilizationNoteFollowsScope: подпись снимка обязана следовать за тем,
// сужен ли САМ снимок (tracker #1009). До этого константа была одна на оба
// случая, и привязанному ключу отдавали ЕГО слоты, подписанные как
// «platform-wide … across ALL environments» — то есть ответ противоречил сам
// себе. Мутация «вернуть одну константу на оба случая» краснеет здесь.
func TestUtilizationNoteFollowsScope(t *testing.T) {
	now := ts("2026-07-08T12:00:00Z")
	axis := DayAxisUTC(now, 3)
	util := []store.RegionUtil{{Region: "eu", CapacitySlots: 7, Allocated: 1}}

	t.Run("непривязанный ключ — подпись прежняя, байт-в-байт", func(t *testing.T) {
		for name, got := range map[string]string{
			"BuildCost":          BuildCost(nil, util, store.RegionUtilFilter{}, axis, 3, now).UtilizationNote,
			"BuildCostFromDaily": BuildCostFromDaily(nil, util, store.RegionUtilFilter{}, axis, 3, now).UtilizationNote,
		} {
			if got != legacyUtilizationNote {
				t.Errorf("%s: подпись глобального ключа изменилась\n got: %q\nwant: %q", name, got, legacyUtilizationNote)
			}
		}
	})

	t.Run("привязанный ключ — подпись называет пару и НЕ зовёт снимок платформенным", func(t *testing.T) {
		scope := store.RegionUtilFilter{Project: "neighbour", Env: "dev"}
		for name, got := range map[string]string{
			"BuildCost":          BuildCost(nil, util, scope, axis, 3, now).UtilizationNote,
			"BuildCostFromDaily": BuildCostFromDaily(nil, util, scope, axis, 3, now).UtilizationNote,
		} {
			if got == legacyUtilizationNote {
				t.Errorf("%s: привязанному ключу отдана платформенная подпись — ровно дефект #1009", name)
				continue
			}
			for _, lie := range []string{"platform-wide", "ALL environments"} {
				if strings.Contains(got, lie) {
					t.Errorf("%s: подпись сужённого снимка всё ещё утверждает %q: %q", name, lie, got)
				}
			}
			for _, want := range []string{"neighbour", "dev"} {
				if !strings.Contains(got, want) {
					t.Errorf("%s: подпись не называет %q, хотя снимок сужен именно им: %q", name, want, got)
				}
			}
		}
	})

	// Половинчатая привязка через публичный API недостижима (пара валидируется
	// при создании ключа), но подпись обязана оставаться честной и на ней:
	// «сужено по проекту» без упоминания несуществующего окружения.
	t.Run("половина пары — подпись не выдумывает вторую половину", func(t *testing.T) {
		got := BuildCostFromDaily(nil, util, store.RegionUtilFilter{Project: "neighbour"}, axis, 3, now).UtilizationNote
		if strings.Contains(got, "platform-wide") || got == legacyUtilizationNote {
			t.Errorf("снимок сужен по проекту, а подпись платформенная: %q", got)
		}
		if !strings.Contains(got, "neighbour") {
			t.Errorf("подпись не называет проект, которым сужен снимок: %q", got)
		}
	})
}
