package httpapi

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Инварианты таблицы маршрутов (routes.go). Таблица — источник правды для
// роутера, генератора openapi.yaml и MCP-инструментов, поэтому её собственная
// целостность проверяется раньше, чем что-либо из этого собирается.

func TestRouteTableInvariants(t *testing.T) {
	known := []string{ScopeAdmin, ScopeDeploy, ScopeMatchmaking, ScopeAllocate, ScopeReadonly}
	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

	for _, rt := range routeTable() {
		t.Run(rt.pattern(), func(t *testing.T) {
			if !slices.Contains(methods, rt.Method) {
				t.Errorf("method %q is not one of %v", rt.Method, methods)
			}
			if !strings.HasPrefix(rt.Path, "/") {
				t.Errorf("path %q must start with /", rt.Path)
			}
			if rt.H == nil {
				t.Error("handler is nil")
			}
			if rt.Summary == "" {
				t.Error("summary is empty: it becomes the OpenAPI summary and the MCP tool description")
			}
			// Скоуп заполняется тогда и только тогда, когда его действительно
			// проверяет requireScope. Скоуп у публичной ручки — ложь в спеке:
			// читатель решит, что ключ нужен, а он не нужен.
			switch rt.Auth {
			case authScope:
				if !slices.Contains(known, rt.Scope) {
					t.Errorf("scope %q is not one of %v", rt.Scope, known)
				}
			case authNone, authHandler, authCookie:
				if rt.Scope != "" {
					t.Errorf("scope %q set on a route that requireScope never gates", rt.Scope)
				}
			default:
				t.Errorf("unknown auth mode %d", rt.Auth)
			}
		})
	}
}

func TestRouteTableHasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, rt := range routeTable() {
		if seen[rt.pattern()] {
			t.Errorf("duplicate route %s", rt.pattern())
		}
		seen[rt.pattern()] = true
	}
}

// Регистрация должна проходить на голом mux без паники: http.ServeMux паникует
// и на дубле шаблона, и на синтаксически кривом шаблоне, — то есть этот тест
// ловит опечатку в Path раньше, чем её поймает запуск мастера.
func TestRegisterRoutesAcceptsTheTable(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.registerRoutes()
}
