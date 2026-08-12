package httpapi_test

import (
	"testing"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// POST /v1/rollback: гейт привязки стоит ДО резолва env, поэтому привязанный
// deploy-ключ не различает состояние ЧУЖОГО проекта (tracker #1004).
//
// Раньше порядок был обратный — `EnvsWithDeprecated(<чужой слаг>)` звался
// РАНЬШЕ `requireBinding`, и по ответу читалось внутреннее состояние соседа:
//
//	409 "project game has no deprecated version to roll back to" — окон отката 0
//	409 "env is required: multiple environments have a rollback window" — >1
//	403 "key is bound to neighbour/dev"                            — ровно 1
//
// Тест держит ровно это: три состояния чужого проекта обязаны давать ОДИН
// байт-в-байт ответ. Не «одинаковый статус» — одинаковое ТЕЛО: разный текст на
// том же коде остаётся тем же оракулом (урок #989).
func TestRollbackBindingGateBeforeEnvResolve(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // проект game, dev-нода, версия 1.0.0 (dev)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()

	setState := func(id, state string) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx,
			`update versions set state=$2, deprecated_at = case when $2='deprecated' then now() else deprecated_at end where id = $1::uuid`,
			id, state); err != nil {
			t.Fatal(err)
		}
	}

	// Сосед, к паре которого привязан ключ: отдельный проект (CreateProject сеет
	// ему dev+prod), поэтому пара neighbour/dev существует.
	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatal(err)
	}
	np, ne := "neighbour", "dev"
	_, boundKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ci-neighbour", Scopes: []string{"deploy"}, Project: &np, Env: &ne,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, globalKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ci-global", Scopes: []string{"deploy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	bound := &client{t: t, base: ts.URL, key: boundKey}
	global := &client{t: t, base: ts.URL, key: globalKey}

	probe := func() (int, string) {
		t.Helper()
		code, body := bound.do("POST", "/v1/rollback", map[string]any{"project": "game", "region": "eu"})
		detail, _ := body["detail"].(string)
		errCode, _ := body["error"].(string)
		return code, errCode + "|" + detail
	}

	// Состояние 1: у чужого проекта НОЛЬ окружений с окном отката.
	code0, body0 := probe()
	if code0 != 403 {
		t.Fatalf("чужой проект, 0 окон отката: want 403, got %d %q", code0, body0)
	}
	// Контроль: глобальный ключ в этом же состоянии получает содержательный 409 —
	// значит гейт не «залил всё 403-ом», а закрыл ровно границу тенанта.
	if code, gb := global.do("POST", "/v1/rollback", map[string]any{"project": "game", "region": "eu"}); code != 409 {
		t.Fatalf("глобальный ключ, 0 окон отката: want 409, got %d %v", code, gb)
	}

	// Состояние 2: у чужого проекта РОВНО ОДНО окно отката (dev).
	devActive := f.AddVersion(t, "1.1.0", "dev")
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu", ActiveVersion: &devActive,
	}); err != nil {
		t.Fatal(err)
	}
	setState(devActive, "active")
	setState(f.VersionID, "deprecated")
	code1, body1 := probe()

	// Состояние 3: окон отката ДВА (dev + prod).
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 10)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatal(err)
	}
	prodDep := f.AddVersion(t, "2.0.0", "prod")
	prodActive := f.AddVersion(t, "2.1.0", "prod")
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu", ActiveVersion: &prodActive,
	}); err != nil {
		t.Fatal(err)
	}
	setState(prodActive, "active")
	setState(prodDep, "deprecated")
	code2, body2 := probe()

	// Контроль осмысленности сцены: глобальный ключ ТЕПЕРЬ видит разницу
	// (409 «env is required»), т.е. три состояния действительно различны в БД —
	// иначе тест ниже проходил бы на пустом месте.
	if code, gb := global.do("POST", "/v1/rollback", map[string]any{"project": "game", "region": "eu"}); code != 409 {
		t.Fatalf("глобальный ключ, 2 окна отката: want 409 env_required, got %d %v", code, gb)
	} else if d, _ := gb["detail"].(string); d == "" {
		t.Fatalf("глобальный ключ: 409 без detail — сцена не различима: %v", gb)
	}

	for _, c := range []struct {
		name string
		code int
		body string
	}{
		{"одно окно отката", code1, body1},
		{"два окна отката", code2, body2},
	} {
		if c.code != code0 || c.body != body0 {
			t.Fatalf("привязанный ключ различает состояние чужого проекта (%s): %d %q против %d %q",
				c.name, c.code, c.body, code0, body0)
		}
	}
	if body0 == "|" {
		t.Fatalf("отказ без тела — сравнение выше ничего не проверяет: %d %q", code0, body0)
	}

	// Несуществующий чужой проект отвечает ТЕМ ЖЕ отказом: иначе гейт различал
	// бы живого соседа и выдуманный слаг (та же дыра, только на строке).
	code, body := bound.do("POST", "/v1/rollback", map[string]any{"project": "zzz-does-not-exist", "region": "eu"})
	detail, _ := body["detail"].(string)
	errCode, _ := body["error"].(string)
	if code != code0 || errCode+"|"+detail != body0 {
		t.Fatalf("выдуманный проект отличим от живого: %d %q против %d %q", code, errCode+"|"+detail, code0, body0)
	}

	// И вторая половина гейта на месте: свой проект привязанный ключ откатывает
	// как раньше — привязка ДЕФОЛТИТ проект, а не только запрещает чужой.
	if code, b := bound.do("POST", "/v1/rollback", map[string]any{"region": "eu"}); code != 409 {
		// У соседа нет ни одной deprecated-версии, поэтому корректный ответ —
		// содержательный 409 про ПУСТОЕ окно СВОЕГО проекта, а не 403.
		t.Fatalf("свой проект: want 409 (пустое окно отката), got %d %v", code, b)
	}
}
