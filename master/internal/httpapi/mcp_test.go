package httpapi

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Пины MCP. Ловят ровно то, что ломается молча: инструмент, зовущий несуществующую
// ручку, и гейт записи, который перестал быть гейтом.

// TestMCPToolsMatchRouteTable — тот самый пин от дрейфа, ради которого MCP и
// исполняет инструменты ЧЕРЕЗ роутер: каждая пара (метод, путь) инструмента
// обязана быть в таблице маршрутов, и скоуп обязан совпадать с маршрутным.
//
// Без него переехавшая ручка проявлялась бы только у оператора в проде — 404
// внутри вызова инструмента, который агент почти наверняка объяснил бы себе
// как «этой ноды нет», а не «этот инструмент сломан».
func TestMCPToolsMatchRouteTable(t *testing.T) {
	byPattern := map[string]route{}
	for _, rt := range routeTable() {
		byPattern[rt.pattern()] = rt
	}

	for _, tool := range mcpToolTable() {
		t.Run(tool.Name, func(t *testing.T) {
			pattern := tool.Method + " " + tool.Path
			rt, ok := byPattern[pattern]
			if !ok {
				t.Fatalf("инструмент зовёт %s, но такой ручки в таблице маршрутов нет "+
					"(переехала? удалена?)", pattern)
			}
			if rt.Scope != tool.Scope {
				t.Errorf("инструмент объявляет скоуп %q, а ручка требует %q — "+
					"tools/list покажет инструмент ключу, которым его не вызвать",
					tool.Scope, rt.Scope)
			}
			// Пишущий инструмент обязан быть помечен Write: иначе он проскочит
			// мимо рубильника mcp_write_enabled и окажется у агента, которому
			// запись не открывали.
			if rt.Method != http.MethodGet && !tool.Write {
				t.Errorf("инструмент вызывает %s и обязан быть помечен Write", rt.Method)
			}
			if rt.Method == http.MethodGet && tool.Write {
				t.Errorf("инструмент только читает, но помечен Write — он зря спрятан за рубильником")
			}
		})
	}
}

func TestMCPToolNamesAreUniqueAndDescribed(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range mcpToolTable() {
		if seen[tool.Name] {
			t.Errorf("дубль имени инструмента %q", tool.Name)
		}
		seen[tool.Name] = true
		if len(tool.Description) < 40 {
			t.Errorf("%s: описание слишком короткое — по нему модель решает, звать ли инструмент", tool.Name)
		}
		for _, a := range tool.Args {
			if a.Description == "" {
				t.Errorf("%s: у аргумента %q нет описания", tool.Name, a.Name)
			}
			if !slices.Contains([]string{"string", "integer", "boolean"}, a.Type) {
				t.Errorf("%s: у аргумента %q тип %q, схема будет невалидной", tool.Name, a.Name, a.Type)
			}
		}
	}
}

// Аргумент, уезжающий в путь, обязан иметь в шаблоне своё место, и наоборот:
// каждый {placeholder} обязан заполняться аргументом. Иначе инструмент соберёт
// запрос с буквальным "{id}" в пути.
func TestMCPPathArgsMatchTemplates(t *testing.T) {
	for _, tool := range mcpToolTable() {
		placeholders := map[string]bool{}
		rest := tool.Path
		for {
			i := strings.Index(rest, "{")
			if i < 0 {
				break
			}
			j := strings.Index(rest[i:], "}")
			if j < 0 {
				t.Errorf("%s: незакрытая скобка в пути %q", tool.Name, tool.Path)
				break
			}
			placeholders[rest[i+1:i+j]] = true
			rest = rest[i+j:]
		}
		for _, a := range tool.Args {
			if a.In == argPath {
				if !placeholders[a.Name] {
					t.Errorf("%s: аргумент %q объявлен как путевой, но в шаблоне %q его нет",
						tool.Name, a.Name, tool.Path)
				}
				if !a.Required {
					t.Errorf("%s: путевой аргумент %q обязан быть Required", tool.Name, a.Name)
				}
				delete(placeholders, a.Name)
			}
		}
		for name := range placeholders {
			t.Errorf("%s: {%s} в пути некому заполнить", tool.Name, name)
		}
	}
}

// --- гейты записи -------------------------------------------------------------

func TestMCPWriteToolsHiddenWhenSwitchOff(t *testing.T) {
	s := &Server{} // рубильник выключен — состояние по умолчанию
	admin := []string{ScopeAdmin}
	for _, tool := range mcpToolTable() {
		if !tool.Write {
			continue
		}
		if s.mcpToolAllowed(tool, admin) {
			t.Errorf("%s виден админскому ключу при выключенном mcp_write_enabled — "+
				"рубильник перестал быть рубильником", tool.Name)
		}
	}
}

func TestMCPWriteToolsVisibleWhenSwitchOn(t *testing.T) {
	s := (&Server{}).WithMCP(true)
	admin := []string{ScopeAdmin}
	var shown int
	for _, tool := range mcpToolTable() {
		if tool.Write && s.mcpToolAllowed(tool, admin) {
			shown++
		}
	}
	if shown == 0 {
		t.Error("при включённом рубильнике админский ключ не видит ни одного пишущего инструмента")
	}
}

// Ключ readonly не видит пишущих инструментов ДАЖЕ при включённом рубильнике:
// это второй, независимый гейт. Если он отвалится, агент с read-ключом начнёт
// звать деплой и получать 403 — то есть тратить контекст на заведомо
// невозможное и объяснять пользователю несуществующие поломки.
func TestMCPReadonlyKeySeesNoWriteTools(t *testing.T) {
	s := (&Server{}).WithMCP(true)
	readonly := []string{ScopeReadonly}
	for _, tool := range mcpToolTable() {
		if tool.Write && s.mcpToolAllowed(tool, readonly) {
			t.Errorf("%s виден readonly-ключу", tool.Name)
		}
	}
	// И при этом читающие инструменты ему видны — иначе тест выше проходил бы
	// на пустом наборе.
	var reads int
	for _, tool := range mcpToolTable() {
		if !tool.Write && s.mcpToolAllowed(tool, readonly) {
			reads++
		}
	}
	if reads == 0 {
		t.Error("readonly-ключ не видит ни одного читающего инструмента")
	}
}

// deploy-ключ видит деплой, но не дренаж ноды: скоупы разграничивают
// инструменты ровно так же, как ручки.
func TestMCPScopeSeparation(t *testing.T) {
	s := (&Server{}).WithMCP(true)
	deployKey := []string{ScopeDeploy}
	byName := map[string]mcpTool{}
	for _, tool := range mcpToolTable() {
		byName[tool.Name] = tool
	}
	if !s.mcpToolAllowed(byName["deploy_version"], deployKey) {
		t.Error("deploy-ключ не видит deploy_version")
	}
	if s.mcpToolAllowed(byName["drain_node"], deployKey) {
		t.Error("deploy-ключ видит drain_node, хотя дренаж — admin")
	}
}

// --- сборка запроса ------------------------------------------------------------

func TestMCPBuildRequest(t *testing.T) {
	byName := map[string]mcpTool{}
	for _, tool := range mcpToolTable() {
		byName[tool.Name] = tool
	}

	t.Run("путь и query", func(t *testing.T) {
		req, err := byName["get_server_logs"].buildRequest(t.Context(),
			map[string]any{"id": "srv-1", "tail": 100}, "Bearer bmk_test")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := req.URL.Path, "/v1/servers/srv-1/logs"; got != want {
			t.Errorf("путь %q, ожидался %q", got, want)
		}
		if got := req.URL.Query().Get("tail"); got != "100" {
			t.Errorf("tail=%q", got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer bmk_test" {
			t.Errorf("ключ вызывающего не перенесён в запрос: %q", got)
		}
	})

	t.Run("тело собирается только из переданных полей", func(t *testing.T) {
		// Ручка разбирает тело с DisallowUnknownFields, а половина полей мьюта
		// необязательна: лишний ключ с пустым значением дал бы 400.
		req, err := byName["mute_alert"].buildRequest(t.Context(),
			map[string]any{"alertname": "NodeDown"}, "")
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, req.ContentLength)
		if _, err := req.Body.Read(buf); err != nil && err.Error() != "EOF" {
			t.Fatal(err)
		}
		body := string(buf)
		if !strings.Contains(body, `"alertname":"NodeDown"`) {
			t.Errorf("тело %q не содержит alertname", body)
		}
		if strings.Contains(body, "region") || strings.Contains(body, "expires_at") {
			t.Errorf("тело %q несёт непереданные поля — ручка ответит 400", body)
		}
	})

	t.Run("обязательный аргумент отсутствует", func(t *testing.T) {
		_, err := byName["get_match"].buildRequest(t.Context(), map[string]any{}, "")
		if err == nil {
			t.Fatal("ожидалась ошибка про обязательный аргумент")
		}
		if !strings.Contains(err.Error(), "required") {
			t.Errorf("ошибка %q не называет причину", err)
		}
	})

	t.Run("путевой аргумент экранируется", func(t *testing.T) {
		// Идентификатор со слэшем не должен уводить вызов на соседнюю ручку.
		req, err := byName["get_match"].buildRequest(t.Context(),
			map[string]any{"id": "a/../../v1/apikeys"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(req.URL.EscapedPath(), "/v1/apikeys") {
			t.Errorf("путь %q вырвался из шаблона", req.URL.EscapedPath())
		}
	})
}
