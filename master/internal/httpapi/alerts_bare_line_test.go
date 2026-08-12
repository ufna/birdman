package httpapi_test

import "testing"

// ГОЛЫЕ строки лога алертов — второй, не запертый вход в тот же фильтр
// (tracker #1019).
//
// `parseAlertsLog` разбирает ДВЕ формы строки: batch `{received_at, alerts:[…]}`
// (её пишет сегодняшний sink) и fallback «одиночный alert-объект в строке»,
// оставленный СОЗНАТЕЛЬНО ради обратной совместимости со старым содержимым
// файла (`master.md` §6, перестройка канала — #244). Правило видимости
// применено в обеих. Но во всём репо была ровно ОДНА голая строка в фикстурах
// (`alerts_internal_test.go`, DiskHigh) и лейбла `project` у неё нет — значит
// вторая ветка фильтра не проходилась ни одним тестом с проектным лейблом, и
// снятие `keep(a)` ИМЕННО ТАМ оставляло весь пакет `httpapi` зелёным, пока
// чужой алерт тёк в `/v1/alerts/history`.
//
// Дефекта в коде на момент заведения карточки НЕТ — дыра в ЗАПИРАНИИ, и
// закрывается она здесь. Достижимость ветки узкая, но реальная:
// `/var/log/birdman/alerts.log` дописывается, а не пересоздаётся, поэтому на
// обновлённом мастере в одном файле живут строки ОБЕИХ форм — ровно так
// фикстура их и смешивает.
//
// Проверяются все три исхода, а не один: чужой проект скрыт, СВОЙ виден
// (иначе «фильтр всё режет» прошло бы как успех), платформенный виден
// (вторая половина правила #995 — без неё арендатор не узнает, что лежит
// мастер).
const (
	bareForeignLine  = `{"status":"firing","labels":{"alertname":"BareForeignGame","severity":"critical","region":"eu","project":"game"},"annotations":{"description":"bare-line alert of the neighbouring tenant"},"startsAt":"2026-08-11T09:30:00Z","endsAt":""}`
	bareOwnLine      = `{"status":"firing","labels":{"alertname":"BareOwnNeighbour","severity":"warning","region":"eu","project":"neighbour"},"annotations":{"description":"bare-line alert of my own tenant"},"startsAt":"2026-08-11T09:31:00Z","endsAt":""}`
	barePlatformLine = `{"status":"firing","labels":{"alertname":"BarePlatformNodeDown","severity":"critical","region":"eu","node":"n9"},"annotations":{"description":"bare-line platform alert"},"startsAt":"2026-08-11T09:32:00Z","endsAt":""}`
)

func TestAlertsHistoryBareLinesRespectBinding(t *testing.T) {
	f := newAlertsTenantFixture(t, bareForeignLine, bareOwnLine, barePlatformLine)
	nb := &client{t: t, base: f.base, key: f.nbKey}
	game := &client{t: t, base: f.base, key: f.gameKey}
	global := &client{t: t, base: f.base, key: f.globalKey}

	// Контроль: голые строки вообще РАЗОБРАНЫ. Без него «привязанный не видит
	// чужую строку» проходило бы и на парсере, который голую форму молча
	// выбрасывает, — то есть тест доказывал бы не то.
	all := alertsByName(t, global, "/v1/alerts/history")
	for _, name := range []string{"BareForeignGame", "BareOwnNeighbour", "BarePlatformNodeDown"} {
		if _, ok := all[name]; !ok {
			t.Fatalf("непривязанный ключ не видит голую строку %q — форма не разобрана, тест ниже ничего не доказывает: %v", name, all)
		}
	}
	// И разобрана СОДЕРЖАТЕЛЬНО, а не как пустышка: проект и область выведены
	// из top-level labels той же строки.
	if got := all["BareForeignGame"]; got["project"] != "game" || got["scope"] != "project" {
		t.Fatalf("голая строка чужого проекта разобрана неверно: %v", got)
	}
	if got := all["BarePlatformNodeDown"]; got["project"] != "" || got["scope"] != "platform" {
		t.Fatalf("голая платформенная строка разобрана неверно: %v", got)
	}

	// Привязанный к neighbour: чужая голая строка скрыта, своя и платформенная
	// видны. Это и есть мутационная точка — снятие keep(a) с fallback-ветки
	// краснеет здесь.
	nbHistory := alertsByName(t, nb, "/v1/alerts/history")
	if _, leaked := nbHistory["BareForeignGame"]; leaked {
		t.Errorf("ГОЛАЯ строка чужого проекта утекла привязанному ключу: %v", nbHistory["BareForeignGame"])
	}
	if _, ok := nbHistory["BareOwnNeighbour"]; !ok {
		t.Errorf("привязанный ключ не видит СВОЮ голую строку — фильтр режет лишнее: %v", nbHistory)
	}
	if _, ok := nbHistory["BarePlatformNodeDown"]; !ok {
		t.Errorf("привязанный ключ не видит ПЛАТФОРМЕННУЮ голую строку — сломана вторая половина правила #995: %v", nbHistory)
	}

	// Симметрия вторым привязанным ключом: без неё всё выше прошло бы и на
	// фильтре, захардкодившем один проект.
	gameHistory := alertsByName(t, game, "/v1/alerts/history")
	if _, leaked := gameHistory["BareOwnNeighbour"]; leaked {
		t.Errorf("ключ game/dev видит голую строку соседа: %v", gameHistory["BareOwnNeighbour"])
	}
	if _, ok := gameHistory["BareForeignGame"]; !ok {
		t.Errorf("ключ game/dev не видит свою голую строку: %v", gameHistory)
	}
	if _, ok := gameHistory["BarePlatformNodeDown"]; !ok {
		t.Errorf("ключ game/dev не видит платформенную голую строку: %v", gameHistory)
	}

	// Не-скрывающий `?project=` у НЕпривязанного ключа обязан вести себя на
	// голых строках так же, как на batch: чужая проектная уходит,
	// платформенная остаётся (#955).
	filtered := alertsByName(t, global, "/v1/alerts/history?project=neighbour")
	if _, leaked := filtered["BareForeignGame"]; leaked {
		t.Errorf("?project=neighbour не отфильтровал голую строку game: %v", filtered)
	}
	if _, ok := filtered["BarePlatformNodeDown"]; !ok {
		t.Errorf("?project=neighbour скрыл ПЛАТФОРМЕННУЮ голую строку — фильтр перестал быть не-скрывающим: %v", filtered)
	}
	if _, ok := filtered["BareOwnNeighbour"]; !ok {
		t.Errorf("?project=neighbour потерял голую строку самого neighbour: %v", filtered)
	}
}
