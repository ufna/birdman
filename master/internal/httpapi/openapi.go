package httpapi

import (
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Генератор машиночитаемого контракта API — OpenAPI 3.1 ИЗ ТАБЛИЦЫ МАРШРУТОВ
// (routes.go), а не из файла, написанного рядом руками.
//
// ЗАЧЕМ так, а не иначе (решение #972, master.md §6): рукописная спека — вторая
// копия правды, и расходится она молча, потому что ничто не заставляет её
// перечитывать. Здесь копия одна: пути, методы и требуемые скоупы берутся из
// той же таблицы, из которой регистрируется роутер, а формы тел — рефлексией
// по типам, которые хендлеры РЕАЛЬНО пишут в ответ (dto.go). Расхождение
// возможно ровно в одном месте — если запись таблицы врёт про свой Resp, — и
// его ловит TestOpenAPISpecIsUpToDate вместе с обычными тестами ручек.
//
// Чего спека пока НЕ описывает (честная граница, а не недоделка): query-
// параметры фильтрации (`?project=`, `?limit=`, `?env=`…) и тела запросов —
// поля Req в таблице ещё не заполнены. Пути, методы, авторизация, коды и формы
// ОТВЕТОВ описаны полностью. Незакрытые ручки перечислены явным списком в
// openapi_test.go, и список закрыт на дозапись.

const openapiVersion = "3.1.0"

// specOnce кэширует спеку: она неизменна в пределах бинаря (таблица —
// константа), а собирается рефлексией, которую незачем гонять на каждый GET.
var (
	specOnce  sync.Once
	specBytes []byte
	specErr   error
)

// OpenAPISpec возвращает контракт API в YAML. Один и тот же байт-в-байт
// результат отдают `GET /v1/openapi.yaml`, ресурс `birdman://openapi` у MCP и
// генератор файла master/api/openapi.yaml.
func OpenAPISpec() ([]byte, error) {
	specOnce.Do(func() { specBytes, specErr = buildSpec() })
	return specBytes, specErr
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	spec, err := OpenAPISpec()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "openapi spec: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spec)
}

// --- модель документа --------------------------------------------------------
//
// Документ смоделирован СТРУКТУРАМИ, а не map[string]any: yaml.v3 пишет поля
// структуры в порядке объявления, а ключи карты — отсортированными. Порядок
// важен не эстетически: спека лежит в репозитории и сравнивается пином, а
// диффы читают люди.

type document struct {
	OpenAPI    string               `yaml:"openapi"`
	Info       docInfo              `yaml:"info"`
	Servers    []serverEntry        `yaml:"servers"`
	Paths      map[string]*pathItem `yaml:"paths"`
	Components docComponents        `yaml:"components"`
}

// serverEntry — база адресов. Конкретного адреса у birdman нет и быть не может
// (каждый ставит свой мастер), поэтому база подставляемая: переменная со
// значением по умолчанию из deploy/-стека.
type serverEntry struct {
	URL         string                    `yaml:"url"`
	Description string                    `yaml:"description"`
	Variables   map[string]serverVariable `yaml:"variables,omitempty"`
}

type serverVariable struct {
	Default     string `yaml:"default"`
	Description string `yaml:"description,omitempty"`
}

type docInfo struct {
	Title       string `yaml:"title"`
	Version     string `yaml:"version"`
	Description string `yaml:"description"`
	License     struct {
		Name string `yaml:"name"`
		URL  string `yaml:"url"`
	} `yaml:"license"`
}

type docComponents struct {
	Schemas         map[string]*schema        `yaml:"schemas"`
	SecuritySchemes map[string]securityScheme `yaml:"securitySchemes"`
}

type securityScheme struct {
	Type        string `yaml:"type"`
	Scheme      string `yaml:"scheme,omitempty"`
	In          string `yaml:"in,omitempty"`
	Name        string `yaml:"name,omitempty"`
	Description string `yaml:"description,omitempty"`
}

type pathItem struct {
	Parameters []parameter `yaml:"parameters,omitempty"`
	Get        *operation  `yaml:"get,omitempty"`
	Put        *operation  `yaml:"put,omitempty"`
	Post       *operation  `yaml:"post,omitempty"`
	Patch      *operation  `yaml:"patch,omitempty"`
	Delete     *operation  `yaml:"delete,omitempty"`
}

type operation struct {
	OperationID string              `yaml:"operationId"`
	Summary     string              `yaml:"summary"`
	Description string              `yaml:"description,omitempty"`
	Security    *[]securityReq      `yaml:"security,omitempty"`
	Responses   map[string]response `yaml:"responses"`
}

type securityReq map[string][]string

type parameter struct {
	Name     string  `yaml:"name"`
	In       string  `yaml:"in"`
	Required bool    `yaml:"required"`
	Schema   *schema `yaml:"schema"`
}

type response struct {
	Description string               `yaml:"description"`
	Content     map[string]mediaType `yaml:"content,omitempty"`
}

type mediaType struct {
	Schema *schema `yaml:"schema"`
}

type schema struct {
	Ref                  string             `yaml:"$ref,omitempty"`
	Type                 any                `yaml:"type,omitempty"` // строка либо ["string","null"] — форма 3.1
	Format               string             `yaml:"format,omitempty"`
	Description          string             `yaml:"description,omitempty"`
	Items                *schema            `yaml:"items,omitempty"`
	Properties           map[string]*schema `yaml:"properties,omitempty"`
	Required             []string           `yaml:"required,omitempty"`
	AdditionalProperties *schema            `yaml:"additionalProperties,omitempty"`
}

// --- сборка ------------------------------------------------------------------

func buildSpec() ([]byte, error) {
	defs := map[string]*schema{}
	paths := map[string]*pathItem{}
	table := routeTable()
	ids := operationIDs(table)

	for _, rt := range table {
		item := paths[rt.Path]
		if item == nil {
			item = &pathItem{Parameters: pathParams(rt.Path)}
			paths[rt.Path] = item
		}
		op, err := buildOperation(rt, ids[rt.pattern()], defs)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", rt.Method, rt.Path, err)
		}
		switch rt.Method {
		case http.MethodGet:
			item.Get = op
		case http.MethodPut:
			item.Put = op
		case http.MethodPost:
			item.Post = op
		case http.MethodPatch:
			item.Patch = op
		case http.MethodDelete:
			item.Delete = op
		default:
			return nil, fmt.Errorf("%s %s: метод не поддержан генератором", rt.Method, rt.Path)
		}
	}

	doc := document{OpenAPI: openapiVersion, Paths: paths}
	doc.Info.Title = "birdman master API"
	doc.Info.Version = "v1"
	doc.Info.Description = strings.TrimSpace(`
Public REST API of birdman-master: matchmaking, fleet control, deploys and observability.

This document is GENERATED from the master's route table (master/internal/httpapi/routes.go)
and cannot drift from the router: the router is registered from that same table.

Coverage: paths, methods, authentication and success status codes are complete.
Response bodies are described for every endpoint except a handful that assemble
them dynamically — each of those says so in its own response description.
Query parameters and request bodies are not described yet.
`)
	doc.Info.License.Name = "MIT"
	doc.Info.License.URL = "https://github.com/ufna/birdman/blob/main/LICENSE"
	doc.Servers = []serverEntry{{
		URL:         "{master}",
		Description: "Your own birdman master. Every install has its own address; there is no hosted service.",
		Variables: map[string]serverVariable{
			"master": {
				Default:     "http://127.0.0.1:8100",
				Description: "Base URL of the master — the default of the deploy/ compose stack.",
			},
		},
	}}
	doc.Components.Schemas = defs
	doc.Components.SecuritySchemes = map[string]securityScheme{
		"apiKey": {
			Type:   "http",
			Scheme: "bearer",
			Description: "birdman API key (bmk_…) in `Authorization: Bearer`. " +
				"A key carries scopes (admin, deploy, matchmaking, allocate, readonly) and may be " +
				"bound to one (project, environment) pair, outside of which it can reach nothing.",
		},
		"sessionCookie": {
			Type: "apiKey",
			In:   "cookie",
			Name: sessionCookie,
			Description: "Browser session cookie issued by POST /v1/session; the panel uses it " +
				"instead of carrying an API key in every request.",
		},
	}

	var b strings.Builder
	b.WriteString("# СГЕНЕРИРОВАНО, РУКАМИ НЕ ПРАВИТЬ.\n")
	b.WriteString("# Источник — таблица маршрутов master/internal/httpapi/routes.go.\n")
	b.WriteString("# Пересобрать: go generate ./... в каталоге master.\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func buildOperation(rt route, opID string, defs map[string]*schema) (*operation, error) {
	op := &operation{
		OperationID: opID,
		Summary:     rt.Summary,
		Responses:   map[string]response{},
	}

	switch rt.Auth {
	case authScope:
		op.Description = fmt.Sprintf("Requires an API key with the %q scope (or %q).", rt.Scope, ScopeAdmin)
		op.Security = &[]securityReq{{"apiKey": {}}}
	case authNone:
		op.Description = "Public by design: no API key required."
		empty := []securityReq{}
		op.Security = &empty
	case authHandler:
		// Учётные данные едут в теле запроса, а не в заголовке — ни одна схема
		// авторизации к операции не применяется, и пустой security говорит
		// об этом прямо (иначе читатель ждёт заголовка, которого нет).
		op.Description = "Credentials travel in the request body, not in an Authorization header."
		empty := []securityReq{}
		op.Security = &empty
	case authCookie:
		op.Description = "Accepts the panel session cookie or an API key."
		op.Security = &[]securityReq{{"sessionCookie": {}}, {"apiKey": {}}}
	}

	successCode := fmt.Sprintf("%d", rt.successStatus())
	switch {
	case rt.Resp != nil:
		s, err := schemaFor(reflect.TypeOf(rt.Resp), defs, map[string]bool{})
		if err != nil {
			return nil, err
		}
		op.Responses[successCode] = response{
			Description: "Success",
			Content:     map[string]mediaType{"application/json": {Schema: s}},
		}
	case rt.RespOpaque:
		op.Responses[successCode] = response{
			Description: "Success. Body is not described by this contract " +
				"(an upstream proxy response, an event stream, a non-JSON payload or no body at all).",
		}
	default:
		// Ручка, чей ответ ещё не типизирован (собирается динамически из
		// map[string]any). Молча выкинуть её из спеки нельзя — читатель решит,
		// что ручки нет; поэтому она описана, а пробел назван вслух. Перечень
		// таких ручек закрыт на дозапись: routesWithoutResponseSchema.
		op.Responses[successCode] = response{
			Description: "Success. This endpoint's response body is not described yet " +
				"(it is assembled dynamically); see docs/specs/master.md §6.",
		}
	}

	errSchema, err := schemaFor(reflect.TypeOf(apiError{}), defs, map[string]bool{})
	if err != nil {
		return nil, err
	}
	errBody := map[string]mediaType{"application/json": {Schema: errSchema}}
	op.Responses["4XX"] = response{Description: "Client error", Content: errBody}
	op.Responses["5XX"] = response{Description: "Server error", Content: errBody}
	return op, nil
}

// successStatus — код успешного ответа ручки. Хранится в таблице (поле
// Status), потому что вывести его из типа ответа нельзя: 201 у создания и 200
// у чтения отличаются только вызовом writeJSON внутри хендлера.
func (r route) successStatus() int {
	if r.Status != 0 {
		return r.Status
	}
	return http.StatusOK
}

// operationIDs раздаёт операциям имена для генераторов клиентов. Имя берётся
// из хендлера (handleListNodes → listNodes): оно уникально в пакете и
// переживает переезд пути.
//
// Но один хендлер может обслуживать НЕСКОЛЬКО маршрутов — так устроен MCP, где
// POST, GET и DELETE /v1/mcp ведут в общий handleMCP, — и тогда имена
// сталкиваются. Спека с дублем operationId невалидна, а генератор клиента на
// ней молча теряет методы, поэтому столкнувшиеся имена разводятся методом.
func operationIDs(rts []route) map[string]string {
	byName := map[string][]route{}
	for _, rt := range rts {
		name := lowerFirstWord(strings.TrimPrefix(handlerName(rt.H), "handle"))
		byName[name] = append(byName[name], rt)
	}
	out := make(map[string]string, len(rts))
	for name, group := range byName {
		for _, rt := range group {
			if len(group) == 1 {
				out[rt.pattern()] = name
				continue
			}
			out[rt.pattern()] = name + strings.ToUpper(rt.Method[:1]) + strings.ToLower(rt.Method[1:])
		}
	}
	return out
}

// lowerFirstWord опускает регистр ПЕРВОГО слова, а не первой буквы: «MCP» это
// одно слово и должно стать «mcp», а не «mCP».
func lowerFirstWord(s string) string {
	runes := []rune(s)
	i := 0
	for i < len(runes) && unicode.IsUpper(runes[i]) {
		i++
	}
	switch {
	case i == 0:
		return s
	case i == len(runes): // имя целиком в верхнем регистре
		return strings.ToLower(s)
	case i > 1:
		i-- // последняя заглавная начинает следующее слово: CAHandler → caHandler
	}
	return strings.ToLower(string(runes[:i])) + string(runes[i:])
}

// handlerName поднимает короткое имя метода из значения-функции. Для выражения
// метода (`(*Server).handleFoo`) рантайм отдаёт полное имя с пакетом; у
// значения метода он добавил бы ещё суффикс "-fm" — снимаем оба оформления.
func handlerName(h func(*Server, http.ResponseWriter, *http.Request)) string {
	if h == nil {
		return ""
	}
	full := runtime.FuncForPC(reflect.ValueOf(h).Pointer()).Name()
	full = strings.TrimSuffix(full, "-fm")
	if i := strings.LastIndex(full, "."); i >= 0 {
		full = full[i+1:]
	}
	return full
}

// pathParams вытаскивает параметры пути из шаблона ServeMux: "/v1/nodes/{id}"
// → один обязательный строковый параметр id.
func pathParams(path string) []parameter {
	var out []parameter
	for _, seg := range strings.Split(path, "/") {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}")
		name = strings.TrimSuffix(name, "...") // хвостовой шаблон ServeMux
		out = append(out, parameter{Name: name, In: "path", Required: true, Schema: &schema{Type: "string"}})
	}
	return out
}

// --- рефлексия типов в JSON Schema ------------------------------------------

var timeType = reflect.TypeOf(time.Time{})

// schemaFor строит схему по Go-типу. Именованные структуры выносятся в
// components/schemas и подставляются ссылкой — иначе рекурсивный тип развернул
// бы генератор в бесконечность, а спека распухла бы повторами.
func schemaFor(t reflect.Type, defs map[string]*schema, inProgress map[string]bool) (*schema, error) {
	if t == nil {
		return &schema{}, nil
	}
	if t == timeType {
		return &schema{Type: "string", Format: "date-time"}, nil
	}
	if t.Kind() == reflect.Pointer {
		inner, err := schemaFor(t.Elem(), defs, inProgress)
		if err != nil {
			return nil, err
		}
		return nullable(inner), nil
	}

	switch t.Kind() {
	case reflect.Bool:
		return &schema{Type: "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &schema{Type: "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return &schema{Type: "number"}, nil
	case reflect.String:
		return &schema{Type: "string"}, nil
	case reflect.Slice, reflect.Array:
		// []byte уезжает в JSON base64-строкой, а не массивом чисел.
		if t.Elem().Kind() == reflect.Uint8 {
			return &schema{Type: "string", Format: "byte"}, nil
		}
		items, err := schemaFor(t.Elem(), defs, inProgress)
		if err != nil {
			return nil, err
		}
		// nil-срез сериализуется в null, поэтому массив допускает null.
		return nullable(&schema{Type: "array", Items: items}), nil
	case reflect.Map:
		values, err := schemaFor(t.Elem(), defs, inProgress)
		if err != nil {
			return nil, err
		}
		return nullable(&schema{Type: "object", AdditionalProperties: values}), nil
	case reflect.Interface:
		return &schema{Description: "any JSON value"}, nil
	case reflect.Struct:
		return structSchema(t, defs, inProgress)
	default:
		return nil, fmt.Errorf("тип %s генератору неизвестен", t)
	}
}

func structSchema(t reflect.Type, defs map[string]*schema, inProgress map[string]bool) (*schema, error) {
	name := schemaName(t)
	if name == "" { // анонимная структура — раскрываем на месте
		return structBody(t, defs, inProgress)
	}
	ref := &schema{Ref: "#/components/schemas/" + name}
	if _, done := defs[name]; done {
		return ref, nil
	}
	if inProgress[name] { // рекурсивный тип: ссылка уже достаточна
		return ref, nil
	}
	inProgress[name] = true
	body, err := structBody(t, defs, inProgress)
	delete(inProgress, name)
	if err != nil {
		return nil, err
	}
	defs[name] = body
	return ref, nil
}

func structBody(t reflect.Type, defs map[string]*schema, inProgress map[string]bool) (*schema, error) {
	out := &schema{Type: "object", Properties: map[string]*schema{}}
	if err := collectFields(t, out, defs, inProgress); err != nil {
		return nil, err
	}
	sort.Strings(out.Required)
	if len(out.Properties) == 0 {
		out.Properties = nil
	}
	return out, nil
}

func collectFields(t reflect.Type, out *schema, defs map[string]*schema, inProgress map[string]bool) error {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		// Встроенное поле без своего имени вливается в родителя — ровно так же,
		// как это делает encoding/json.
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if err := collectFields(ft, out, defs, inProgress); err != nil {
					return err
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		s, err := schemaFor(f.Type, defs, inProgress)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}
		out.Properties[name] = s
		if !strings.Contains(opts, "omitempty") {
			out.Required = append(out.Required, name)
		}
	}
	return nil
}

// schemaName — имя типа в components/schemas: «пакет.Тип», чтобы Node из store
// и Node из httpapi не столкнулись. Пустое имя означает анонимную структуру.
func schemaName(t reflect.Type) string {
	if t.Name() == "" {
		return ""
	}
	pkg := t.PkgPath()
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	if pkg == "" {
		return t.Name()
	}
	return pkg + "." + t.Name()
}

// nullable переводит схему в форму 3.1 «тип или null». У ссылки ($ref) типа
// нет — её оставляем как есть: обвязка oneOf ради nullable раздула бы спеку
// сильнее, чем добавила бы точности.
func nullable(s *schema) *schema {
	if s.Ref != "" || s.Type == nil {
		return s
	}
	if t, ok := s.Type.(string); ok {
		s.Type = []string{t, "null"}
	}
	return s
}
