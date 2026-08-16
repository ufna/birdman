package httpapi

// Мостик из внутреннего пакета во внешний тестовый (httpapi_test): пины,
// которым нужны факты о маршрутах, читают ТАБЛИЦУ (routes.go), а не исходный
// текст её объявления. Разбор AST под маршруты был бы третьим по счёту
// представлением одной правды и краснел бы от переформатирования — ровно то,
// ради чего таблица и заводилась.

// RouteFact — одна ручка глазами пина: метод, путь и имя функции-хендлера.
type RouteFact struct {
	Method  string
	Path    string
	Handler string // короткое имя метода, например "handleCreateRegistry"
}

// RouteFactsForTest возвращает таблицу маршрутов в порядке её объявления.
func RouteFactsForTest() []RouteFact {
	out := make([]RouteFact, 0, len(routeTable()))
	for _, rt := range routeTable() {
		out = append(out, RouteFact{Method: rt.Method, Path: rt.Path, Handler: handlerName(rt.H)})
	}
	return out
}
