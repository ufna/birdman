package deploy_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// РЕЕСТР ПУТЕЙ СМЕНЫ АКТИВНОЙ ВЕРСИИ (tracker #1088, продолжение #1071).
//
// Сигнал «катить некуда» родился в startJob, и через неё идут ПЯТЬ путей
// (ручной POST /v1/deploy, промоут, авто-деплой из POST /v1/versions, Resume,
// SweepOrphans). Отсюда легко прочитать, что startJob — единственная воронка.
// Это НЕВЕРНО: единственной она была не структурно, а по факту на тот день, и
// обходов уже нашлось два — Manager.Rollback (мимо, потому что откат не греет
// образы) и PUT /v1/fleets/{region} с active_version (прямой UPSERT мимо
// deploy-менеджера, задокументированный bootstrap/ops-override). Оба
// переключали fleet_configs.active_version при нуле живых нод так же вхолостую
// и так же молча.
//
// Поведенческие пины: два новых обхода — в httpapi
// (TestActiveVersionFlipsWithoutNodesAreAnnounced), исходный путь startJob —
// там же, в TestDeployWithoutNodesIsAnnounced (#1071). Но перечисление ПУТЕЙ по
// определению не ловит ТРЕТИЙ обход — тот, которого ещё нет. Поэтому здесь
// пины СТРУКТУРНЫЕ: они читают исходники и требуют совпадения с реестром.
//
// ЧТО ИМЕННО ОНИ ЛОВЯТ — и это уже, чем «любое место, способное переставить
// активную версию»; читать их шире значит на них не проверять:
//   - новый SQL, пишущий НЕ-null fleet_configs.active_version, в НЕтестовом
//     .go-файле каталога ../store — как внутри функции, так и в объявлении
//     уровня пакета (в этом пакете есть и такие: claimSQL, matchSelect);
//   - новый вызывающий store.ActivateVersion в САМОМ пакете deploy (сравнение
//     по имени селектора).
// Пробелы и переносы строк на ответ SQL-предиката не влияют: он матчит
// нормализованный текст (см. writesActiveVersion) — перенос строки внутри
// `set …` не слепая зона, а `=` без пробелов не даёт ложного КРАСНОГО на
// обнуляющих (tracker #1094). А вот ПОРЯДОК колонок в `set` влияет, см. ниже.
// ЧЕГО НЕ ЛОВЯТ (и это честная граница, а не недоделка): нового вызывающего
// УЖЕ зарегистрированных store.UpsertFleet / store.ActivateVersion из другого
// пакета — например ещё одну HTTP-ручку; SQL, собранный конкатенацией или
// fmt.Sprintf; UPDATE, где active_version переставляется НЕ первой колонкой
// списка `set` и не через coalesce (`update fleet_configs set buffer_ready = $1,
// active_version = $2` — проверено мутацией: проходит зелёным); и они НЕ
// проверяют, что кто-то реально зовёт NoNodesWarning — строки реестра говорят,
// КТО обязан, но исполняет это поведенческий пин.
// Последнее оставлено сознательно: предикат «`update fleet_configs set` И
// `active_version = `» ту форму закрыл бы, но — тоже замерено — ложно краснеет
// на честном запросе, который active_version лишь ЧИТАЕТ в where
// (`update fleet_configs set buffer_ready = 1 where active_version = $1`).
// Красный тест здесь = «появилось новое место смены активной версии»: подключи
// пробу нулевой ёмкости (deploy.Manager.NoNodesWarning + recordNoNodes) и
// впиши себя в реестр с причиной. Иначе следующий обход снова обнаружится
// только на живом стенде.

// storeActiveVersionWriters — функции пакета store, чей SQL ставит НЕ-null
// fleet_configs.active_version, то есть реально включает версию.
var storeActiveVersionWriters = map[string]string{
	"ActivateVersion": "атомарный флип деплоя и отката; пробу зовут его вызывающие — " +
		"Manager.activate (через startJob) и Manager.Rollback",
	"UpsertFleet": "bootstrap/ops-override PUT /v1/fleets/{region}; пробу зовёт " +
		"handleUpsertFleet при непустом active_version",
}

// deployActivateCallers — функции пакета deploy, зовущие store.ActivateVersion.
var deployActivateCallers = map[string]string{
	"activate": "хвост startJob: сигнал уже выдан там, где обнаружился нулевой флот",
	"Rollback": "свой вызов NoNodesWarning + recordNoNodes (tracker #1088)",
}

func TestActiveVersionWritersAreRegistered(t *testing.T) {
	got := map[string]bool{}
	// Обходятся и функции, и объявления уровня пакета: в этом пакете SQL живёт
	// в обеих формах (claimSQL, matchSelect — константы), и пин, смотрящий
	// только в тела функций, пропустил бы ровно ту, что вынесена в константу.
	forEachDecl(t, "../store", func(name string, decl ast.Node) {
		ast.Inspect(decl, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			sql, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if !writesActiveVersion(sql) {
				return true
			}
			got[name] = true
			return true
		})
	})
	assertRegistry(t, got, storeActiveVersionWriters,
		"новый SQL, ставящий fleet_configs.active_version: подключи пробу нулевой ёмкости "+
			"(deploy.Manager.NoNodesWarning, событие через recordNoNodes) и впиши функцию в "+
			"storeActiveVersionWriters — иначе повторится #1088: активная версия переключилась, "+
			"дедиков ноль, система молчит")
}

func TestActivateVersionCallersAreRegistered(t *testing.T) {
	got := map[string]bool{}
	forEachDecl(t, ".", func(name string, decl ast.Node) {
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "ActivateVersion" {
				got[name] = true
			}
			return true
		})
	})
	assertRegistry(t, got, deployActivateCallers,
		"новый вызывающий store.ActivateVersion в пакете deploy: он меняет активную версию, "+
			"значит обязан выдать тот же сигнал при нуле живых нод (NoNodesWarning + recordNoNodes) "+
			"и попасть в deployActivateCallers")
}

// writesActiveVersion: SQL включает версию, а не гасит её. Обнуляющие
// (`set active_version = null` при удалении env/проекта) сюда НЕ попадают —
// они ничего не активируют, предупреждать им не о чем.
func writesActiveVersion(sql string) bool {
	// Матчинг идёт по НОРМАЛИЗОВАННОМУ тексту, иначе ответ пина зависит от
	// форматирования, а не от смысла запроса (tracker #1094): нижний регистр,
	// любая пробельная последовательность (включая перенос строки) — один
	// пробел, `=` — всегда с пробелами по бокам. Обе стороны получены МУТАЦИЕЙ,
	// в дереве их сегодня нет: без схлопывания пробелов писатель вида
	// `update fleet_configs set\n\tactive_version = $1` прошёл бы мимо
	// положительного предиката (ложно ЗЕЛЁНЫЙ — ровно тот класс, ради которого
	// пин заведён), а без пробелов вокруг `=` обнуляющий `set active_version=null`
	// прошёл бы мимо исключающего и пин ложно ПОКРАСНЕЛ БЫ на DeleteEnvironment.
	// Одного схлопывания пробелов тут МАЛО: в `active_version=null` схлопывать
	// нечего.
	s := strings.Join(strings.Fields(strings.ReplaceAll(strings.ToLower(sql), "=", " = ")), " ")
	if !strings.Contains(s, "fleet_configs") || !strings.Contains(s, "active_version") {
		return false
	}
	if strings.Contains(s, "active_version = null") {
		return false
	}
	return strings.Contains(s, "insert into fleet_configs") ||
		strings.Contains(s, "set active_version") ||
		strings.Contains(s, "active_version = coalesce")
}

// forEachDecl walks the non-test .go files of a package directory, calling back
// for every TOP-LEVEL declaration with a name to attribute it to: functions and
// methods by their own name, package-level const/var specs by the identifier
// they bind. Both forms are needed — SQL in this codebase lives in function
// bodies AND in package-level constants (store.claimSQL, store.matchSelect) —
// and a scanner that saw only FuncDecls would be silently blind to the second.
func forEachDecl(t *testing.T, dir string, visit func(name string, decl ast.Node)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	funcs, specs := 0, 0
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				funcs++
				visit(decl.Name.Name, decl)
			case *ast.GenDecl:
				if decl.Tok != token.CONST && decl.Tok != token.VAR {
					continue
				}
				for _, s := range decl.Specs {
					vs, ok := s.(*ast.ValueSpec)
					if !ok || len(vs.Names) == 0 {
						continue
					}
					specs++
					visit(vs.Names[0].Name, vs)
				}
			}
		}
	}
	// Контроль самого сканера: пустой обход прошёл бы как «реестр совпал».
	// Обе половины проверяются отдельно — иначе поломка одной маскируется другой.
	if funcs == 0 {
		t.Fatalf("scanner read no function declarations in %s — the pin would be vacuously green", dir)
	}
	if specs == 0 {
		t.Fatalf("scanner read no package-level const/var specs in %s — the second half of the pin is blind", dir)
	}
}

func assertRegistry(t *testing.T, got map[string]bool, want map[string]string, hint string) {
	t.Helper()
	var extra, missing []string
	for name := range got {
		if _, ok := want[name]; !ok {
			extra = append(extra, name)
		}
	}
	for name := range want {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		t.Errorf("НЕЗАРЕГИСТРИРОВАННЫЙ путь смены активной версии: %v.\n%s", extra, hint)
	}
	if len(missing) > 0 {
		t.Errorf("реестр называет путь, которого в коде больше нет: %v — почини реестр "+
			"(иначе он перестаёт быть картой того, что есть)", missing)
	}
}
