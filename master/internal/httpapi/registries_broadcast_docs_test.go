package httpapi_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ufna/birdman/master/internal/httpapi"
)

// РЕЕСТР НОСИТЕЛЕЙ ПЕРЕЧНЯ РУЧЕК, РАССЫЛАЮЩИХ SetRegistries (tracker #1108).
//
// Перечень «какие ручки будят broadcast кредов» размазан по шести текстам —
// спека протокола, два doc-комментария мастера, комментарий у самой проводки
// хука, .proto и его сгенерированное зеркало. Все шесть родились в Реестрах v1
// с парой POST/DELETE и НЕ заметили, как v2 добавила третью ручку — PATCH-edit
// (ротация токена). Полгода тексты утверждали, что после правки кредов агенты
// сидят на старом наборе до реконнекта, — ровно тот класс тихо протухающего
// утверждения, который стоит пришпилить структурно, а не перечитывать глазами.
//
// Пин выводит перечень ИЗ ИСХОДНИКОВ: находит функции пакета, ВЫЗЫВАЮЩИЕ
// onRegistriesChanged, поднимает их HTTP-методы из таблицы маршрутов
// (routes.go, в порядке её объявления) и требует от каждого носителя реестра ниже
// (а) хотя бы одно вхождение канонического перечня и (б) отсутствие вхождений,
// от него отличающихся. Второе условие обязательно: без него файл с ДВУМЯ
// упоминаниями проходил бы зелёным с одним починенным и одним протухшим — это
// не гипотеза, так и лежал server.go (поле структуры знало PATCH, doc сеттера
// нет).
//
// УСТОЙЧИВОСТЬ (учтена история образца — master/internal/deploy/
// active_version_paths_test.go, чинившегося дважды):
//   - у .go-носителей читаются ТОЛЬКО комментарии, иначе строки маршрутов
//     ("GET /v1/registries") дают ложный КРАСНЫЙ на самом server.go;
//   - сравнение идёт по нормализованному тексту (маркеры комментариев, бэктики
//     и звёздочки сняты, любая пробельная последовательность — один пробел,
//     пробелы вокруг «/» внутри перечня необязательны), поэтому перенос строки
//     посреди перечня — не слепая зона и не повод покраснеть: в service.go
//     перечень и путь и сегодня живут на РАЗНЫХ строках;
//   - пробелы схлопываются, а не удаляются, и алфавит методов явный, иначе
//     «API POST/DELETE» из .proto склеилось бы в APIPOST и вхождение потерялось
//     бы (ложный ЗЕЛЁНЫЙ — проверено мутацией);
//   - функция, дёргающая хук, но не найденная в таблице маршрутов, — ФАТАЛ, а
//     не тихий пропуск: перечень методов перестал бы быть полным;
//   - маршруты берутся из САМОЙ таблицы (RouteFactsForTest), а не разбором её
//     литерала: разбор был бы третьим представлением одной правды и краснел бы
//     от переформатирования.
//
// ЧЕГО НЕ ЛОВИТ (честная граница, а не недоделка): носителя, не внесённого в
// реестр ниже (в том числе в панели и в docs вне перечисленных файлов); текст,
// выродившийся в ОДИН метод («после DELETE /v1/registries» — вхождением не
// считается, для матча нужны минимум два метода); и он ничего не говорит про
// сам факт рассылки — что broadcast доходит до всех подключённых нод, держит
// поведенческий TestBroadcastRegistriesReachesAllConnectedNodes в agentlink.
//
// Красный тест = «список ручек, будящих рассылку кредов, изменился»: пройди по
// реестру и приведи тексты к новому перечню (или впиши новый носитель).
var registriesBroadcastCarriers = map[string]string{
	"server.go":                                   "doc поля onRegistriesChanged и doc WithRegistriesHook",
	"../../cmd/birdman-master/main.go":            "комментарий у проводки хука в BroadcastRegistries",
	"../agentlink/service.go":                     "doc Service.BroadcastRegistries",
	"../agentlink/service_test.go":                "doc поведенческого пина рассылки",
	"../../../docs/specs/protocol.md":             "спека протокола, §1, message SetRegistries",
	"../../../proto/agentlink/v1/agentlink.proto": "doc message SetRegistries",
	"../../../proto/agentlink/v1/agentlink.pb.go": "сгенерированное зеркало того же doc'а",
}

// registriesHookName — имя хука, чей вызов и означает «эта ручка будит рассылку».
const registriesHookName = "onRegistriesChanged"

// registriesEnumRe матчит перечень МИНИМУМ из двух методов перед /v1/registries
// в нормализованном тексте. Алфавит методов явный: иначе `\w+` съел бы соседнее
// слово («API POST/DELETE») и перечень потерялся бы.
var registriesEnumRe = regexp.MustCompile(
	`\b(?:GET|POST|PUT|PATCH|DELETE)(?:\s*/\s*(?:GET|POST|PUT|PATCH|DELETE))+\s*/v1/registries`)

var httpMethodRe = regexp.MustCompile(`GET|POST|PUT|PATCH|DELETE`)

func TestRegistriesBroadcastTriggersAreDocumented(t *testing.T) {
	methods := registriesBroadcastMethods(t)
	want := strings.Join(methods, "/")

	paths := make([]string, 0, len(registriesBroadcastCarriers))
	for p := range registriesBroadcastCarriers {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		text := carrierText(t, path)
		found := registriesEnumRe.FindAllString(text, -1)
		if len(found) == 0 {
			t.Errorf("%s (%s): перечень ручек, будящих рассылку SetRegistries, пропал из текста — "+
				"он обязан называть %s /v1/registries (либо носитель переехал и реестр пина устарел)",
				path, registriesBroadcastCarriers[path], want)
			continue
		}
		for _, hit := range found {
			got := strings.Join(httpMethodRe.FindAllString(hit, -1), "/")
			if got != want {
				t.Errorf("%s (%s): текст перечисляет %s /v1/registries, а рассылку SetRegistries "+
					"будят %s /v1/registries — читатель сделает неверный вывод о том, после какой "+
					"ручки агенты остаются на старых кредах (tracker #1108)",
					path, registriesBroadcastCarriers[path], got, want)
			}
		}
	}
}

// registriesBroadcastMethods возвращает HTTP-методы ручек, вызывающих хук, в
// порядке объявления их маршрутов — это и есть канонический перечень.
func registriesBroadcastMethods(t *testing.T) []string {
	t.Helper()
	_, files := parsePackageDir(t, ".")

	// Шаг 1: функции, ВЫЗЫВАЮЩИЕ хук. Именно вызов, не упоминание: сеттер
	// WithRegistriesHook хуку присваивает и ручкой не является.
	firing := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == registriesHookName {
					firing[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	if len(firing) == 0 {
		t.Fatalf("сканер не нашёл ни одного вызова %s — пин прошёл бы вхолостую", registriesHookName)
	}

	// Шаг 2: их маршруты — прямо из таблицы (routes.go). Таблица перечислена в
	// порядке объявления, поэтому досортировывать по позиции в исходнике,
	// как делал прежний AST-сканер, больше не нужно.
	var methods []string
	seen := map[string]bool{}
	for _, rt := range httpapi.RouteFactsForTest() {
		if !firing[rt.Handler] {
			continue
		}
		if !strings.HasPrefix(rt.Path, "/v1/registries") {
			t.Fatalf("маршрут %s %s ведёт на %s, который будит рассылку кредов, но пин умеет "+
				"описывать только «МЕТОД /v1/registries…» — почини пин вместе с текстами",
				rt.Method, rt.Path, rt.Handler)
		}
		if seen[rt.Handler] {
			t.Fatalf("%s зарегистрирован на два маршрута — канонический перечень стал неоднозначным, "+
				"почини пин", rt.Handler)
		}
		seen[rt.Handler] = true
		methods = append(methods, rt.Method)
	}
	for name := range firing {
		if !seen[name] {
			t.Fatalf("%s вызывает %s, но не найден в таблице маршрутов (routes.go): либо это хелпер "+
				"(тогда перечень методов надо выводить иначе), либо ручку забыли зарегистрировать",
				name, registriesHookName)
		}
	}
	return methods
}

// parsePackageDir разбирает НЕтестовые .go-файлы каталога одним общим FileSet —
// общим, чтобы позиции маршрутов из разных файлов были сравнимы порядком.
func parsePackageDir(t *testing.T, dir string) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatalf("сканер не прочитал ни одного .go-файла в %s — пин прошёл бы вхолостую", dir)
	}
	return fset, files
}

// carrierText возвращает нормализованный текст носителя: у .go-файлов — ТОЛЬКО
// комментарии (строки маршрутов не текст для читателя, но регулярка их бы
// поймала), у остальных — файл целиком.
func carrierText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("носитель %s не читается: %v (переехал? почини реестр пина)", path, err)
	}
	text := string(raw)
	if strings.HasSuffix(path, ".go") {
		f, err := parser.ParseFile(token.NewFileSet(), path, raw, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		var b strings.Builder
		for _, g := range f.Comments {
			// CommentGroup.Text() уже снимает «//» и склеивает группу.
			b.WriteString(g.Text())
			b.WriteString("\n")
		}
		text = b.String()
		if strings.TrimSpace(text) == "" {
			t.Fatalf("в %s не нашлось ни одного комментария — сканер сломан", path)
		}
	}
	return normalizeCarrierText(text)
}

// normalizeCarrierText снимает оформление и схлопывает пробелы. Схлопывает, а
// НЕ удаляет: без пробела «API POST/DELETE» превратилось бы в APIPOST/DELETE и
// перестало матчиться (мутация проверена).
func normalizeCarrierText(s string) string {
	s = strings.NewReplacer("//", " ", "`", "", "*", "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}
