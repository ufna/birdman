package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// ИНВАРИАНТ №1: в ПРОДЕ секреты хешируются ровно на bcrypt.DefaultCost.
//
// Тестовый бутстрап опускает рабочий фактор до bcrypt.MinCost (tracker #1060:
// ~0.7с за хеш под `-race` × сотни ключей и нод за прогон). Ровно так и
// появляются тихие ослабления криптографии — «временно» удешевили тестам,
// а поехало и в прод. Этот тест смотрит НЕ на константу рядом с кодом, а на
// стоимость, ВЫЧИТАННУЮ ИЗ РЕАЛЬНО ПОЛУЧЕННОГО ХЕША: понизят прод-дефолт —
// покраснеет.
func TestProductionHashCostStaysBcryptDefault(t *testing.T) {
	// Стор ровно в том виде, в каком его собирает прод-путь: Open без опций
	// (cmd/birdman-master) не трогает bcryptCost.
	var prod Store
	if got := prod.hashCost(); got != bcrypt.DefaultCost {
		t.Fatalf("прод-стор хеширует на cost=%d, ожидался bcrypt.DefaultCost=%d", got, bcrypt.DefaultCost)
	}

	const secret = "s3cret-under-pin"
	hash, err := prod.hashSecret(secret)
	if err != nil {
		t.Fatalf("hashSecret: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatalf("bcrypt.Cost на выданном хеше: %v", err)
	}
	if cost != bcrypt.DefaultCost {
		t.Errorf("хеш прод-стора несёт cost=%d, а должен bcrypt.DefaultCost=%d", cost, bcrypt.DefaultCost)
	}
	// Заодно: удешевление не должно ломать сам путь «хеш → сверка».
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)); err != nil {
		t.Errorf("сверка секрета против собственного хеша не прошла: %v", err)
	}

	// Мусорное значение опции не должно превращаться в «дёшево» (и тем более в
	// cost=0): вне [MinCost, MaxCost] стор остаётся на прод-дефолте.
	for _, bad := range []int{-1, 0, bcrypt.MinCost - 1, bcrypt.MaxCost + 1} {
		s := &Store{}
		WithHashCostForTests(bad)(s)
		if got := s.hashCost(); got != bcrypt.DefaultCost {
			t.Errorf("WithHashCostForTests(%d) дал cost=%d, ожидался прод-дефолт %d", bad, got, bcrypt.DefaultCost)
		}
	}

	// И наоборот: годное значение действительно доезжает (иначе тесты молча
	// хешировали бы на прод-дефолте, а карточка #1060 — ни о чём).
	s := &Store{}
	WithHashCostForTests(bcrypt.MinCost)(s)
	if got := s.hashCost(); got != bcrypt.MinCost {
		t.Errorf("WithHashCostForTests(MinCost) дал cost=%d, ожидался %d", got, bcrypt.MinCost)
	}
}

// ИНВАРИАНТ №2: ручка удешевления недостижима из прода — ни изнутри бинаря, ни
// из окружения.
//
// Изнутри: WithHashCostForTests допустима ровно в двух местах — файл её
// определения и тестовый бутстрап master/internal/testdb (он линкуется только
// в тестовые бинари). Появилась в любом другом НЕ-ТЕСТОВОМ файле модуля —
// значит, до прод-хеширования дотянулись в обход, тест красный.
//
// Снаружи: рабочий фактор не должен читаться из окружения НИКОГДА (именно этот
// вариант — `BIRDMAN_BCRYPT_COST` — в #1060 отвергнут: он даёт рычаг снаружи,
// которого быть не должно).
func TestHashCostKnobStaysTestOnly(t *testing.T) {
	// Скоуп — весь модуль master (корень: этот пакет лежит в internal/store).
	// Другие модули репозитория до internal/ не дотянутся по правилу internal.
	root := filepath.Join("..", "..")

	// Смотрим КОД, а не текст: файл разбирается парсером без комментариев,
	// поэтому обоснования в доках (где эти же имена поминаются как отвергнутые
	// варианты) не считаются нарушением, а вызов — считается.
	const knob = "WithHashCostForTests"
	// Имя env-переменной: ЗАГЛАВНЫЙ строковый литерал про стоимость хеширования.
	envLever := regexp.MustCompile(`^[A-Z][A-Z0-9_]*(BCRYPT|HASH_COST)[A-Z0-9_]*$`)

	allowed := map[string]bool{
		filepath.Join("internal", "store", "hashcost.go"): true,
	}
	allowedDir := filepath.Join("internal", "testdb") + string(filepath.Separator)

	fset := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "dist" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		knobAllowed := allowed[rel] || strings.HasPrefix(rel, allowedDir)
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				if v.Name == knob && !knobAllowed {
					t.Errorf("%s:%d: %s вне тестового бутстрапа — это ослабление хеширования "+
						"секретов в прод-пути; допустимы только internal/store/hashcost.go и internal/testdb (#1060)",
						rel, fset.Position(v.Pos()).Line, knob)
				}
			case *ast.BasicLit:
				if v.Kind != token.STRING {
					return true
				}
				s, uerr := strconv.Unquote(v.Value)
				if uerr == nil && envLever.MatchString(s) {
					t.Errorf("%s:%d: %q — рабочий фактор bcrypt не читается из окружения: env-переменная "+
						"дала бы рычаг ослабления хеширования снаружи прод-бинаря (#1060)",
						rel, fset.Position(v.Pos()).Line, s)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
