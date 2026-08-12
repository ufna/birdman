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
// Поведенческий пин на все три ручки живёт в httpapi
// (TestActiveVersionFlipsWithoutNodesAreAnnounced). Но перечисление ПУТЕЙ по
// определению не ловит ТРЕТИЙ обход — тот, которого ещё нет. Поэтому здесь
// пины СТРУКТУРНЫЕ: они читают исходники и требуют, чтобы множество мест,
// способных переставить активную версию, совпадало с реестром. Новый писатель
// → тест красный, и автор обязан либо подключить пробу нулевой ёмкости
// (deploy.Manager.NoNodesWarning + recordNoNodes), либо вписать себя сюда с
// причиной. Без этого следующий обход снова обнаружится только на живом стенде.

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
	forEachFunc(t, "../store", func(name string, fn *ast.FuncDecl) {
		ast.Inspect(fn, func(n ast.Node) bool {
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
	forEachFunc(t, ".", func(name string, fn *ast.FuncDecl) {
		ast.Inspect(fn, func(n ast.Node) bool {
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
	s := strings.ToLower(sql)
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

// forEachFunc walks the non-test .go files of a package directory, calling back
// with the (possibly method) name of every function declaration.
func forEachFunc(t *testing.T, dir string, visit func(name string, fn *ast.FuncDecl)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	seen := 0
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
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			seen++
			visit(fn.Name.Name, fn)
		}
	}
	// Контроль самого сканера: пустой обход прошёл бы как «реестр совпал».
	if seen == 0 {
		t.Fatalf("scanner read no function declarations in %s — the pin would be vacuously green", dir)
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
