package httpapi_test

import (
	"strings"
	"testing"
)

// TestStatsCostUtilizationNoteMatchesScope — сквозной пин на то, что подпись
// снимка ёмкости описывает ТОТ ЖЕ снимок, который в ответе и лежит
// (tracker #1009).
//
// #993 сузил снимок привязкой ключа, а подпись осталась константой, и ответ
// стал противоречить сам себе: тому же привязанному ключу отдавали его 7
// слотов из 25 платформенных и тут же подписывали их как «platform-wide …
// across ALL environments». Врал именно API: панель поле `utilization_note` не
// рендерит вовсе, читает его тот, кто ходит curl'ом или пишет свой клиент, —
// поэтому пин стоит на HTTP-уровне, где ложь и наблюдалась, а не только на
// чистой функции.
//
// Проверяются ОБЕ ветки одним прогоном, и это несущее условие: тест только на
// привязанном ключе прошёл бы и у мутации «переписать константу на текст про
// пару», которая сломала бы ответ глобального ключа — а его #993 специально
// сохранил байт-в-байт.
func TestStatsCostUtilizationNoteMatchesScope(t *testing.T) {
	f := newTenantFixture(t)

	note := func(key string) string {
		t.Helper()
		c := &client{t: t, base: f.base, key: key}
		code, body := c.do("GET", "/v1/stats/cost", nil)
		if code != 200 {
			t.Fatalf("/v1/stats/cost: %d %v", code, body)
		}
		s, ok := body["utilization_note"].(string)
		if !ok || s == "" {
			t.Fatalf("utilization_note отсутствует или пуст: %v", body["utilization_note"])
		}
		return s
	}

	// Глобальный ключ: снимок платформенный (25 слотов в eu) — подпись обязана
	// остаться ровно прежней.
	global := note(f.globalKey)
	if !strings.Contains(global, "platform-wide") || !strings.Contains(global, "ALL environments") {
		t.Errorf("непривязанный ключ: подпись платформенного снимка изменилась: %q", global)
	}
	if got := euCapacity(t, &client{t: t, base: f.base, key: f.globalKey}, ""); got != 25 {
		t.Fatalf("фикстура сломалась: глобальному ключу ёмкость eu = %v, want 25", got)
	}

	// Привязанный ключ: снимок сужен до его пары (7 слотов из 25) — подпись
	// обязана говорить про пару и НЕ обязана называть снимок платформенным.
	bound := note(f.nbKey)
	if got := euCapacity(t, &client{t: t, base: f.base, key: f.nbKey}, ""); got != 7 {
		t.Fatalf("фикстура сломалась: привязанному ключу ёмкость eu = %v, want 7", got)
	}
	for _, lie := range []string{"platform-wide", "ALL environments"} {
		if strings.Contains(bound, lie) {
			t.Errorf("привязанному ключу отдали 7 слотов из 25 и подписали их %q: %q", lie, bound)
		}
	}
	for _, want := range []string{"neighbour", "dev"} {
		if !strings.Contains(bound, want) {
			t.Errorf("подпись сужённого снимка не называет %q: %q", want, bound)
		}
	}

	// СИММЕТРИЯ вторым привязанным ключом — иначе всё выше прошло бы и у
	// подписи, захардкодившей одну пару из фикстуры.
	game := note(f.gameKey)
	if !strings.Contains(game, "game") || strings.Contains(game, "neighbour") {
		t.Errorf("ключ game/dev получил чужую или захардкоженную подпись: %q", game)
	}

	// И ?project=/?env= подпись НЕ двигают: снимок сужает только ПРИВЯЗКА
	// (#993), значит и подпись обязана зависеть от неё же — иначе она снова
	// начнёт описывать не тот снимок, что в ответе.
	c := &client{t: t, base: f.base, key: f.globalKey}
	code, body := c.do("GET", "/v1/stats/cost?project=neighbour&env=dev", nil)
	if code != 200 {
		t.Fatalf("/v1/stats/cost?project=neighbour&env=dev глобальным ключом: %d %v", code, body)
	}
	if got := body["utilization_note"].(string); got != global {
		t.Errorf("?project=/?env= сдвинули подпись у непривязанного ключа:\n got: %q\nwant: %q", got, global)
	}
}
