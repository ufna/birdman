package httpapi

// ХВОСТ alerts.log растущим окном (tracker #1087) — и главное, что здесь
// запирается, это не производительность, а ГРАНИЦА ВИДИМОСТИ.
//
// До карточки ручка делала os.ReadFile на каждый GET /v1/alerts/history; на
// боксе файл дорос до 498 МБ. Ротация (#1073) добавила порог 32 МБ, но порог не
// потолок: политика размерная, а исполняет её cron.daily/logrotate.timer, так
// что при доальтертменеджерном темпе (~250 МБ/час) файл проходит порог за
// минуты и растёт до следующего прогона logrotate. Чтение осталось линейным по
// размеру файла.
//
// Наивное лечение — «читать последние N байт» — по байтам как раз ДЕШЕВЛЕ;
// платит оно корректностью: шумный сосед, налив последние N байт своими
// алертами, вытеснил бы историю арендатора из окна, и привязанный ключ получил
// бы ПУСТУЮ историю при непустой. То же обещание записано в docstring
// parseAlertsLog про окно ЛИМИТА, здесь оно перенесено на окно БАЙТОВОЕ, и
// держит его растущее окно.
//
// Отсюда набор пинов: обрезанная первая строка выбрасывается, а лежащая ровно
// на границе — НЕ теряется; окно растёт, пока не набран limit ВИДИМЫХ событий,
// и при этом НАКАПЛИВАЕТСЯ, а не перечитывается (иначе худший случай стоил бы
// 2.74 размера файла вместо одного); сужение одинаково на ОБЕИХ формах строки
// (batch и голый объект — вторая ветка keep(a), незапертость которой была темой
// #1019); файл меньше окна читается как раньше; пустой и отсутствующий файл
// дают пустой список и os.ErrNotExist, из которого ручка делает 200+[] (это уже
// её тест — TestAlertsUnconfigured в alerts_test.go).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// tailNow — момент «сейчас» всех тестов файла; все фикстуры пишут времена
// раньше него, чтобы alertActive не зависел от часов машины.
var tailNow = ts("2026-08-12T12:00:00Z")

// batchLine — строка в форме сегодняшнего sink'а: доставка с received_at и
// пакетом алертов.
func batchLine(recv, name, project string, pad int) string {
	labels := `"alertname":"` + name + `","severity":"warning","region":"eu"`
	if project != "" {
		labels += `,"project":"` + project + `"`
	}
	return `{"received_at":"` + recv + `","alerts":[{"status":"firing","labels":{` + labels +
		`},"annotations":{"description":"` + strings.Repeat("x", pad) +
		`"},"startsAt":"` + recv + `","endsAt":""}]}`
}

// bareLine — вторая, fallback-форма: голый alert-объект без received_at.
func bareLine(startsAt, name, project string, pad int) string {
	labels := `"alertname":"` + name + `","severity":"warning","region":"eu"`
	if project != "" {
		labels += `,"project":"` + project + `"`
	}
	return `{"status":"firing","labels":{` + labels +
		`},"annotations":{"description":"` + strings.Repeat("x", pad) +
		`"},"startsAt":"` + startsAt + `","endsAt":""}`
}

// writeLog кладёт строки в файл во временном каталоге теста и возвращает путь и
// размер.
func writeLog(t *testing.T, lines []string) (string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alerts.log")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, int64(len(body))
}

func names(events []alertEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Name)
	}
	return out
}

// Файл МЕНЬШЕ окна читается как раньше: весь, целиком, и БАЙТ В БАЙТ тем же
// результатом, что даёт прямой parseAlertsLog по всему файлу — сверка идёт с
// ним, а не с руками выписанным ожиданием, иначе тест закреплял бы мою же
// ошибку. Это контроль «хвостовое чтение не поломало обычный случай»: без него
// всё ниже могло бы проходить на реализации, которая молча теряет половину.
func TestReadAlertsTailSmallFileIsWholeFile(t *testing.T) {
	lines := []string{
		batchLine("2026-08-12T09:00:00Z", "OldestAlert", "", 10),
		batchLine("2026-08-12T10:00:00Z", "MiddleAlert", "alpha", 10),
		bareLine("2026-08-12T11:00:00Z", "NewestBare", "alpha", 10),
	}
	path, size := writeLog(t, lines)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	got, read, err := readAlertsTail(path, tailNow, 100, alertVisibility{})
	if err != nil {
		t.Fatalf("readAlertsTail: %v", err)
	}
	if read != size {
		t.Fatalf("файл меньше окна обязан читаться целиком: прочитано %d из %d", read, size)
	}
	if want := parseAlertsLog(raw, tailNow, 100, alertVisibility{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("хвост разошёлся с разбором всего файла:\n got %+v\nwant %+v", got, want)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d: %v", len(got), names(got))
	}
	// Порядок — newest-first, как и при чтении файла целиком.
	if got[0].Name != "NewestBare" || got[2].Name != "OldestAlert" {
		t.Fatalf("порядок newest-first сломан: %v", names(got))
	}
}

// Пустой файл — пустой список, а не ошибка и не битое событие.
func TestReadAlertsTailEmptyFile(t *testing.T) {
	path, size := writeLog(t, nil) // writeLog добавит один "\n"
	_ = size
	got, _, err := readAlertsTail(path, tailNow, 100, alertVisibility{})
	if err != nil {
		t.Fatalf("readAlertsTail: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("пустой файл → []: %+v", got)
	}
	// И файл нулевой длины (только что созданный sink'ом) — тоже.
	zero := filepath.Join(t.TempDir(), "alerts.log")
	if err := os.WriteFile(zero, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, read, err := readAlertsTail(zero, tailNow, 100, alertVisibility{})
	if err != nil {
		t.Fatalf("readAlertsTail(0 байт): %v", err)
	}
	if read != 0 || len(got) != 0 {
		t.Fatalf("файл нулевой длины: прочитано %d, событий %d", read, len(got))
	}
}

// Отсутствующий файл отдаёт os.ErrNotExist — ручка обязана уметь отличить его
// от настоящей ошибки ввода-вывода, иначе «синка ещё нет» превратится в 500.
func TestReadAlertsTailMissingFile(t *testing.T) {
	_, _, err := readAlertsTail(filepath.Join(t.TempDir(), "nope.log"), tailNow, 100, alertVisibility{})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("отсутствующий файл: want os.ErrNotExist, got %v", err)
	}
	// Пустой путь (мастер без синка) — та же ветка.
	if _, _, err := readAlertsTail("", tailNow, 100, alertVisibility{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("пустой путь: want os.ErrNotExist, got %v", err)
	}
}

// ОБРЕЗАННАЯ первая строка. Окно почти наверняка ложится посередине строки — её
// надо выбросить, а не пытаться разобрать, и выбросить РОВНО ЕЁ.
//
// Что здесь пинуется честно, а что нет. «Съесть лишнюю строку» ловится точно:
// проверяется и состав выдачи, и ЧИСЛО ПРОЧИТАННЫХ БАЙТ (окно+1) — потерянная
// строка не набирает limit, окно удваивается, и байты выдают это даже там, где
// отсечение по limit скрыло бы разницу в составе. А вот «не выбросить обрезок
// вовсе» этот тест поймать НЕ может, и это свойство парсера, а не недосмотр:
// огрызок JSON-строки не разбирается ни одной из двух форм (обе ветки
// parseAlertsLog молча пропускают неразбираемую строку), так что битого события
// из него не выйдет при любом окне. Проверки на `CutInHalf` и на пустое имя
// ниже поэтому мёртвые по построению — они стоят как страховка на случай, если
// разбор когда-нибудь станет мягче.
func TestReadAlertsTailDropsTruncatedFirstLine(t *testing.T) {
	first := batchLine("2026-08-12T09:00:00Z", "CutInHalf", "", 200)
	second := batchLine("2026-08-12T10:00:00Z", "WholeOne", "", 10)
	third := batchLine("2026-08-12T11:00:00Z", "WholeTwo", "", 10)
	path, size := writeLog(t, []string{first, second, third})

	// Окно, гарантированно вскрывающее первую строку посередине: длина второй и
	// третьей плюс половина первой.
	window := int64(len(second)+len(third)+2) + int64(len(first))/2
	if window >= size {
		t.Fatalf("окно %d не меньше файла %d — тест ничего не режет", window, size)
	}
	// limit ровно по числу ЦЕЛЫХ строк окна: столько и должно набраться с первого
	// чтения, без удвоения.
	got, read, err := readAlertsTailWindow(path, tailNow, 2, alertVisibility{}, window)
	if err != nil {
		t.Fatalf("readAlertsTailWindow: %v", err)
	}
	if read != window+1 {
		t.Fatalf("окно выросло (прочитано %d вместо %d) — из первого чтения пропала целая строка", read, window+1)
	}
	if len(got) != 2 {
		t.Fatalf("want ровно две целые строки, got %d: %v", len(got), names(got))
	}
	for _, e := range got {
		if e.Name == "CutInHalf" {
			t.Fatalf("обрезанная строка разобрана как событие: %+v", e)
		}
		if e.Name == "" {
			t.Fatalf("битое событие из обрезка: %+v", e)
		}
	}
}

// Окно, легшее РОВНО по границе строк. Тут ломается наивное «читать с off и
// выбрасывать всё до первого \n»: строка целая, и съедать её нельзя. Читаем
// поэтому с off-1, и лишний байт отвечает, обрезана строка или нет.
//
// ЧЕМ ИМЕННО КРАСНЕЕТ мутация «читать с off»: пин идёт ПО ЧИСЛУ БАЙТ, а не по
// составу выдачи, и это не слабость, а точное описание цены ошибки. Съеденная
// строка — всегда САМАЯ СТАРАЯ в окне; limit из-за неё не набирается, окно
// удваивается, финальный проход идёт уже с off == 0 (где резать нечего) и
// возвращает правильный состав. То есть на монотонном файле ошибка стоит
// лишнего удвоения и повторного чтения — в пределе всего файла, — а не
// потерянного события. ПОТЕРЯТЬ она может там, где порядок в файле
// немонотонен: у голых строк без received_at (сортируются по startsAt),
// монотонность которых докстринг readAlertsTail честно не обещает. Отсюда и
// прогон обеих форм.
func TestReadAlertsTailExactLineBoundaryKeepsLine(t *testing.T) {
	for _, form := range []struct {
		name string
		line func(tm, name, project string, pad int) string
	}{
		{"batch", batchLine},
		{"bare", bareLine},
	} {
		t.Run(form.name, func(t *testing.T) {
			older := form.line("2026-08-12T09:00:00Z", "BeforeBoundary", "", 10)
			onEdge := form.line("2026-08-12T10:00:00Z", "FirstInWindow", "", 10)
			newest := form.line("2026-08-12T11:00:00Z", "LastInWindow", "", 10)
			path, size := writeLog(t, []string{older, onEdge, newest})

			// Окно = ровно две последние строки со своими \n: его левый край
			// попадает в байт сразу после \n предыдущей строки.
			window := int64(len(onEdge) + 1 + len(newest) + 1)
			if window >= size {
				t.Fatalf("окно %d не меньше файла %d", window, size)
			}
			got, read, err := readAlertsTailWindow(path, tailNow, 2, alertVisibility{}, window)
			if err != nil {
				t.Fatalf("readAlertsTailWindow: %v", err)
			}
			if read != window+1 {
				t.Fatalf("прочитано %d, ожидалось окно+1 байт левее = %d "+
					"(лишнее чтение значит, что целую строку на границе окна съели, "+
					"limit не набрался и окно выросло)", read, window+1)
			}
			if len(got) != 2 {
				t.Fatalf("строка на границе окна потеряна: got %d %v", len(got), names(got))
			}
			if got[0].Name != "LastInWindow" || got[1].Name != "FirstInWindow" {
				t.Fatalf("не те строки в окне: %v", names(got))
			}
		})
	}
}

// ГЛАВНЫЙ пин карточки: ШУМНЫЙ СОСЕД НЕ ВЫТЕСНЯЕТ ИСТОРИЮ АРЕНДАТОРА.
//
// Свои алерты арендатора лежат в самом НАЧАЛЕ файла, а дальше сосед наливает
// заведомо больше стартового окна. Фиксированное окно вернуло бы привязанному
// ключу пустую историю (в последних 64 КиБ нет ни одного его алерта) —
// растущее обязано дочитать до своих. Ровно здесь краснеет мутация «сделать
// окно фиксированным».
//
// Тот же тест — пин ОБЕИХ веток keep(a): гоняется и на batch-форме, и на голой.
// Снятие фильтра в любой из них делает выдачу непустой мгновенно (в окно
// попадают чужие алерты), но чужим именем — и падает проверка утечки.
func TestReadAlertsTailGrowsPastNoisyNeighbour(t *testing.T) {
	for _, form := range []struct {
		name string
		line func(tm, name, project string, pad int) string
	}{
		{"batch", batchLine},
		{"bare", bareLine},
	} {
		t.Run(form.name, func(t *testing.T) {
			var lines []string
			// Три своих (самые старые) + один платформенный: привязанный ключ
			// обязан видеть и их тоже (вторая половина правила #995).
			for i := 0; i < 3; i++ {
				lines = append(lines, form.line(
					fmt.Sprintf("2026-08-12T08:%02d:00Z", i), fmt.Sprintf("OwnOld%d", i), "neighbour", 20))
			}
			lines = append(lines, form.line("2026-08-12T08:30:00Z", "PlatformOld", "", 20))
			// Шум соседа: заведомо больше стартового окна, чтобы одного удвоения
			// точно не хватило.
			noise := 0
			for i := 0; noise < 4*alertsTailInitialWindow; i++ {
				ln := form.line(
					fmt.Sprintf("2026-08-12T09:%02d:00Z", i%60), fmt.Sprintf("NoisyGame%d", i), "game", 400)
				lines = append(lines, ln)
				noise += len(ln) + 1
			}
			path, size := writeLog(t, lines)
			if size <= alertsTailInitialWindow {
				t.Fatalf("фикстура %d байт не больше стартового окна %d — тест ничего не растит",
					size, alertsTailInitialWindow)
			}

			vis := alertVisibility{project: "neighbour", bound: true}
			got, read, err := readAlertsTail(path, tailNow, 4, vis)
			if err != nil {
				t.Fatalf("readAlertsTail: %v", err)
			}
			for _, e := range got {
				if e.Project == "game" {
					t.Fatalf("АЛЕРТ ЧУЖОГО ПРОЕКТА утёк привязанному ключу: %+v", e)
				}
			}
			if len(got) != 4 {
				t.Fatalf("шумный сосед вытеснил историю арендатора: got %d из 4, %v (прочитано %d из %d)",
					len(got), names(got), read, size)
			}
			// И это именно свои плюс платформенный, а не что попало.
			want := map[string]bool{"OwnOld0": true, "OwnOld1": true, "OwnOld2": true, "PlatformOld": true}
			for _, e := range got {
				if !want[e.Name] {
					t.Fatalf("в выдаче арендатора посторонний алерт %q: %v", e.Name, names(got))
				}
				delete(want, e.Name)
			}
			if len(want) != 0 {
				t.Fatalf("не хватает своих алертов: %v (получено %v)", want, names(got))
			}
			// ЦЕНА худшего случая: файл прочитан РОВНО ОДИН раз, как и прежним
			// os.ReadFile. Это пин на то, что окно НАКАПЛИВАЕТСЯ: перечитывай
			// реализация всё окно на каждом удвоении, здесь вышло бы
			// 65537+131073+262145+<файл> ≈ 2.74 размера файла, то есть в самом
			// случае, ради которого растущее окно и заведено, лечение оказалось
			// бы дороже болезни.
			if read != size {
				t.Fatalf("худший случай прочитал %d байт при файле %d — окно перечитывается вместо накопления",
					read, size)
			}
			// Контроль обратной стороны: НЕпривязанный ключ на том же файле
			// набирает limit сразу и НЕ дочитывает файл до начала. Без него
			// «растущее окно» прошло бы и на реализации, которая всегда читает
			// всё, — то есть карточка не была бы решена вовсе.
			_, readAll, err := readAlertsTail(path, tailNow, 4, alertVisibility{})
			if err != nil {
				t.Fatalf("readAlertsTail (непривязанный): %v", err)
			}
			if readAll > alertsTailInitialWindow+1 {
				t.Fatalf("без сужения хвоста хватило бы стартового окна, а прочитано %d из %d",
					readAll, size)
			}
		})
	}
}

// ЗАМЕР на файле большого РАЗМЕРА: 8 МБ лога, дефолтный ?limit=100 без
// сужения. Раньше ручка читала все 8 МБ (а на боксе файл был 498 МБ, замер
// #1073), теперь обязана уложиться в ОДНО стартовое окно.
//
// Что здесь СИНТЕТИКА, чтобы никто не принял замер за обещание. Строка фикстуры
// — 368 байт: одна доставка с ОДНИМ алертом и 150-байтовым описанием. Размер
// боевой доставки не измерен: alertmanager группирует (group_by по
// alertname/severity/region), в строке лежит пакет алертов со всеми лейблами,
// description_ru, generatorURL и fingerprint. Так что «100 событий влезают в
// 64 КиБ» — свойство ЭТОЙ фикстуры, а не боевого файла; на крупной доставке
// окно просто удвоится, и код это умеет (соседний тест про рост).
func TestReadAlertsTailReadsFractionOfBigFile(t *testing.T) {
	var lines []string
	total := 0
	for i := 0; total < 8*1024*1024; i++ {
		ln := batchLine(fmt.Sprintf("2026-08-12T%02d:%02d:00Z", 6+i%4, i%60),
			fmt.Sprintf("Churn%d", i), "", 150)
		lines = append(lines, ln)
		total += len(ln) + 1
	}
	path, size := writeLog(t, lines)
	// Комментарий выше называет 368 байт на строку — держим фикстуру в этих
	// рамках, иначе замер перестанет быть замером описанного.
	if avg := size / int64(len(lines)); avg < 360 || avg > 380 {
		t.Fatalf("строка фикстуры стала %d байт (описано ~368) — замер ниже уже не про то, что написано", avg)
	}

	got, read, err := readAlertsTail(path, tailNow, 100, alertVisibility{})
	if err != nil {
		t.Fatalf("readAlertsTail: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("want 100 событий, got %d", len(got))
	}
	if read > alertsTailInitialWindow+1 {
		t.Fatalf("дефолтный limit=100 не уложился в стартовое окно: прочитано %d из %d", read, size)
	}
	t.Logf("замер: файл %d байт (%d строк), прочитано %d байт (%.3f%%) ради limit=100",
		size, len(lines), read, 100*float64(read)/float64(size))
}

// Sanity: время фикстур раньше «сейчас», иначе alertActive считал бы иначе и
// тесты выше зависели бы от часов машины.
func TestTailFixtureTimesArePast(t *testing.T) {
	if !tailNow.After(ts("2026-08-12T11:00:00Z")) {
		t.Fatal("tailNow обязан быть позже всех времён фикстур")
	}
	if tailNow.After(time.Now().AddDate(1, 0, 0)) {
		t.Fatal("tailNow улетел в будущее больше чем на год — фикстура протухла")
	}
}
