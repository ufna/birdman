package httpapi_test

// #983: /healthz отдаёт признак `panel` — вкомпилирована ли в бинарь панель.
//
// Зачем в health-эндпоинте: гейт деплоера пингует БД и про панель не знает
// ничего, поэтому сборка БЕЗ панели проходила выкат идеально — откат не
// срабатывал, метрика успеха обновлялась, и дефект замечал только человек,
// открывший панель через несколько часов (так и произошло 11.08).
//
// Признак отдаёт мастер, а не деплоер ищет маркер в HTML: иначе появилась бы
// вторая копия правды о том, как выглядит непособранная панель — ровно та мина,
// из-за которой случился #978.

import (
	"testing"

	"github.com/ufna/birdman/master/internal/panelui"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestHealthzReportsPanelState(t *testing.T) {
	st := testdb.New(t)
	ts, _, _ := deployServer(t, st)

	code, body := (&client{t: t, base: ts.URL}).do("GET", "/healthz", nil)
	if code != 200 {
		t.Fatalf("healthz: %d %v", code, body)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %v, want ok", body["status"])
	}

	got, _ := body["panel"].(string)
	if got != "embedded" && got != "placeholder" {
		t.Fatalf("panel = %q, want embedded|placeholder", got)
	}
	// Значение обязано совпадать с тем, что реально лежит в embed — иначе гейт
	// деплоера верил бы полю, которое ни на что не смотрит.
	want := "placeholder"
	if panelui.Embedded() {
		want = "embedded"
	}
	if got != want {
		t.Fatalf("panel = %q, а по содержимому embed — %q", got, want)
	}
}
