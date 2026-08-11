package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// browser mimics the panel: session cookie + CSRF header, no Bearer.
type browser struct {
	t      *testing.T
	base   string
	cookie *http.Cookie
	csrf   bool
}

func (b *browser) do(method, path string, body any) (int, map[string]any, *http.Response) {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			b.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, b.base+path, rd)
	if err != nil {
		b.t.Fatal(err)
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	if b.csrf {
		req.Header.Set("X-Birdman-Csrf", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 && strings.Contains(resp.Header.Get("Content-Type"), "json") {
		if err := json.Unmarshal(raw, &out); err != nil {
			b.t.Fatalf("bad json (%d): %s", resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, out, resp
}

func sessionCookieOf(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == "birdman_session" {
			return c
		}
	}
	t.Fatalf("no birdman_session cookie in %v", resp.Header["Set-Cookie"])
	return nil
}

// TestSessionAuth: login → cookie → cookie-authenticated API calls with CSRF
// enforcement → logout (docs/specs/panel.md §1 п.5).
func TestSessionAuth(t *testing.T) {
	st := testdb.New(t)
	ts := apiServer(t, st)
	adminKey := scopedKey(t, st, "admin", httpapi.ScopeAdmin)
	roKey := scopedKey(t, st, "ro", httpapi.ScopeReadonly)

	// Login rejects invalid keys, GET /v1/session without auth is 401.
	b := &browser{t: t, base: ts.URL}
	if code, _, _ := b.do("POST", "/v1/session", map[string]any{"api_key": "bmk_bogus"}); code != 401 {
		t.Fatalf("bad key login: want 401, got %d", code)
	}
	if code, _, _ := b.do("GET", "/v1/session", nil); code != 401 {
		t.Fatalf("anon session probe: want 401, got %d", code)
	}

	// A key with paste whitespace (trailing newline/spaces) still logs in —
	// server trims it, so a copied key isn't a spurious 401.
	if code, _, _ := b.do("POST", "/v1/session", map[string]any{"api_key": "  " + roKey + "\n"}); code != 200 {
		t.Fatalf("whitespace-wrapped key login: want 200, got %d", code)
	}

	// Login sets a hardened HttpOnly cookie and returns the scopes.
	code, body, resp := b.do("POST", "/v1/session", map[string]any{"api_key": roKey})
	if code != 200 {
		t.Fatalf("login: %d %v", code, body)
	}
	if scopes, _ := body["scopes"].([]any); len(scopes) != 1 || scopes[0] != "readonly" {
		t.Fatalf("login scopes: %v", body)
	}
	c := sessionCookieOf(t, resp)
	// Secure follows the scheme: this harness is plain HTTP, so Secure must be
	// OFF here (else Safari drops the cookie over the dev SSH tunnel and login
	// loops). The HTTPS case is covered by TestSessionCookieSecureFollowsScheme.
	if !c.HttpOnly || c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("cookie flags: %+v", c)
	}
	b.cookie = c

	// Cookie authenticates GETs; scopes are inherited from the key.
	if code, body, _ := b.do("GET", "/v1/session", nil); code != 200 || body["name"] != "ro" {
		t.Fatalf("session probe: %d %v", code, body)
	}
	if code, _, _ := b.do("GET", "/v1/nodes", nil); code != 200 {
		t.Fatalf("cookie GET /v1/nodes: want 200, got %d", code)
	}
	// readonly session cannot mutate even with the CSRF header.
	b.csrf = true
	if code, _, _ := b.do("POST", "/v1/nodes", map[string]any{}); code != 403 {
		t.Fatalf("readonly cookie POST: want 403, got %d", code)
	}

	// Admin session: non-GET without the CSRF header → 403 csrf_required.
	ab := &browser{t: t, base: ts.URL}
	_, _, resp = ab.do("POST", "/v1/session", map[string]any{"api_key": adminKey})
	ab.cookie = sessionCookieOf(t, resp)
	code, body, _ = ab.do("POST", "/v1/nodes", map[string]any{
		"project": "game", "region": "eu", "hostname": "n1",
		"public_ip": "203.0.113.10", "capacity_slots": 4,
	})
	if code != 403 || body["error"] != "csrf_required" {
		t.Fatalf("cookie POST without CSRF: want 403 csrf_required, got %d %v", code, body)
	}
	ab.csrf = true
	if code, body, _ = ab.do("POST", "/v1/nodes", map[string]any{
		"project": "game", "region": "eu", "hostname": "n1",
		"public_ip": "203.0.113.10", "capacity_slots": 4,
	}); code != 201 {
		t.Fatalf("cookie POST with CSRF: %d %v", code, body)
	}
	// Bearer requests never need the CSRF header (unchanged behavior).
	admin := &client{t: t, base: ts.URL, key: adminKey}
	if code, _ := admin.do("POST", "/v1/nodes", map[string]any{
		"project": "game", "region": "eu", "hostname": "n2",
		"public_ip": "203.0.113.11", "capacity_slots": 4,
	}); code != 201 {
		t.Fatalf("bearer POST: want 201, got %d", code)
	}

	// Logout requires CSRF, kills the session and expires the cookie.
	b.csrf = false
	if code, _, _ := b.do("DELETE", "/v1/session", nil); code != 403 {
		t.Fatalf("logout without CSRF: want 403, got %d", code)
	}
	b.csrf = true
	code, _, resp = b.do("DELETE", "/v1/session", nil)
	if code != 204 {
		t.Fatalf("logout: want 204, got %d", code)
	}
	if c := sessionCookieOf(t, resp); c.Value != "" || c.MaxAge >= 0 {
		t.Fatalf("logout cookie not expired: %+v", c)
	}
	if code, _, _ := b.do("GET", "/v1/nodes", nil); code != 401 {
		t.Fatalf("dead session GET: want 401, got %d", code)
	}
	// Logout is idempotent without a session.
	if code, _, _ := (&browser{t: t, base: ts.URL}).do("DELETE", "/v1/session", nil); code != 204 {
		t.Fatalf("anon logout: want 204, got %d", code)
	}
}

// TestSessionReportsKeyBinding: сессия ЧЕСТНО сообщает привязку ключа
// (tracker #1000, docs/specs/master.md §6 / panel.md §1 п.5).
//
// Зачем: сессия наследует ключ целиком, поэтому привязка гейтит запросы панели
// (#974/#988/#990), а в теле /v1/session её не было — панель объясняла ЛЮБОЙ
// 403 единственным, что видела, скоупами, и говорила «нужен ключ со скоупом
// readonly или admin» привязанному readonly-ключу, у которого readonly есть.
// Тест держит обе половины контракта: привязка ВИДНА, и её появление ADDITIVE —
// у глобального ключа поля нет вовсе, тело такое же, как до #1000.
func TestSessionReportsKeyBinding(t *testing.T) {
	st := testdb.New(t)
	ts := apiServer(t, st)
	ctx := t.Context()

	if _, err := st.CreateProject(ctx, "neighbour", 2); err != nil {
		t.Fatal(err)
	}
	bProject, bEnv := "neighbour", "dev"
	_, boundSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "ro-bound", Scopes: []string{httpapi.ScopeReadonly}, Project: &bProject, Env: &bEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	globalSecret := scopedKey(t, st, "ro-global", httpapi.ScopeReadonly)
	adminSecret := scopedKey(t, st, "admin", httpapi.ScopeAdmin)

	// bindingOf возвращает пару из тела ответа и признак «поле вообще есть».
	// Отсутствие поля и пустая пара — РАЗНЫЕ вещи: первое означает «ключ
	// глобальный», второе было бы сломанной сериализацией.
	bindingOf := func(what string, body map[string]any) (string, string, bool) {
		t.Helper()
		raw, ok := body["binding"]
		if !ok {
			return "", "", false
		}
		obj, isObj := raw.(map[string]any)
		if !isObj {
			t.Fatalf("%s: binding = %#v, want объект {project, env}", what, raw)
		}
		project, _ := obj["project"].(string)
		env, _ := obj["env"].(string)
		return project, env, true
	}

	// 1) ПРИВЯЗАННЫЙ ключ: логин отдаёт пару, и это ИМЕННО его пара.
	b := &browser{t: t, base: ts.URL}
	code, body, resp := b.do("POST", "/v1/session", map[string]any{"api_key": boundSecret})
	if code != 200 {
		t.Fatalf("логин привязанным ключом: %d %v, want 200", code, body)
	}
	project, env, has := bindingOf("POST (bound)", body)
	if !has {
		t.Fatalf("POST /v1/session привязанного ключа: binding отсутствует (%v) — панель снова не знает причины 403", body)
	}
	if project != "neighbour" || env != "dev" {
		t.Fatalf("POST /v1/session: binding = %s/%s, want neighbour/dev", project, env)
	}
	// Ровно та комбинация, из-за которой диагноз панели был ложным: readonly
	// у ключа ЕСТЬ, и отказ приходит не из-за скоупа.
	if scopes, _ := body["scopes"].([]any); len(scopes) != 1 || scopes[0] != "readonly" {
		t.Fatalf("POST /v1/session: scopes = %v, want [readonly]", body["scopes"])
	}

	// 2) Та же привязка видна на GET — и по куке, и по Bearer: панель пробует
	//    GET при загрузке, и если бы поле жило только в ответе логина,
	//    перезагрузка страницы возвращала бы «непривязанную» сессию.
	bb := &browser{t: t, base: ts.URL, cookie: sessionCookieOf(t, resp)}
	code, body, _ = bb.do("GET", "/v1/session", nil)
	if code != 200 {
		t.Fatalf("GET /v1/session по куке: %d %v", code, body)
	}
	project, env, has = bindingOf("GET (cookie)", body)
	if !has || project != "neighbour" || env != "dev" {
		t.Fatalf("GET /v1/session по куке: binding = %s/%s (есть=%v), want neighbour/dev", project, env, has)
	}
	bearerReq, err := http.NewRequest("GET", ts.URL+"/v1/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	bearerReq.Header.Set("Authorization", "Bearer "+boundSecret)
	bearerResp, err := http.DefaultClient.Do(bearerReq)
	if err != nil {
		t.Fatal(err)
	}
	rawBearer, _ := io.ReadAll(bearerResp.Body)
	bearerResp.Body.Close()
	bearerBody := map[string]any{}
	if err := json.Unmarshal(rawBearer, &bearerBody); err != nil {
		t.Fatalf("GET /v1/session по Bearer: битый json %s", rawBearer)
	}
	project, env, has = bindingOf("GET (bearer)", bearerBody)
	if !has || project != "neighbour" || env != "dev" {
		t.Fatalf("GET /v1/session по Bearer: binding = %s/%s (есть=%v), want neighbour/dev", project, env, has)
	}

	// 3) ADDITIVE: у глобального и admin-ключа поля НЕТ ВООБЩЕ (не null, не
	//    пустой объект) — клиент, написанный до #1000, видит прежнее тело.
	for _, c := range []struct {
		who    string
		secret string
		scope  string
	}{{"global readonly", globalSecret, "readonly"}, {"admin", adminSecret, "admin"}} {
		gb := &browser{t: t, base: ts.URL}
		code, body, gresp := gb.do("POST", "/v1/session", map[string]any{"api_key": c.secret})
		if code != 200 {
			t.Fatalf("логин (%s): %d %v", c.who, code, body)
		}
		if _, ok := body["binding"]; ok {
			t.Fatalf("POST /v1/session (%s): binding = %#v, want отсутствие поля — контракт перестал быть additive",
				c.who, body["binding"])
		}
		if scopes, _ := body["scopes"].([]any); len(scopes) != 1 || scopes[0] != c.scope {
			t.Fatalf("POST /v1/session (%s): scopes = %v, want [%s]", c.who, body["scopes"], c.scope)
		}
		if body["name"] == nil || body["name"] == "" {
			t.Fatalf("POST /v1/session (%s): name = %v, want непустое", c.who, body["name"])
		}
		gbb := &browser{t: t, base: ts.URL, cookie: sessionCookieOf(t, gresp)}
		code, body, _ = gbb.do("GET", "/v1/session", nil)
		if code != 200 {
			t.Fatalf("GET /v1/session (%s): %d %v", c.who, code, body)
		}
		if _, ok := body["binding"]; ok {
			t.Fatalf("GET /v1/session (%s): binding = %#v, want отсутствие поля", c.who, body["binding"])
		}
	}
}

// TestSessionResponseHalfPairIsBound: ключ с ПОЛУПАРОЙ (project есть, env нет)
// описывается как ПРИВЯЗАННЫЙ, а не как глобальный.
//
// Состояние недостижимо через API (CreateAPIKey отвергает полупару, плюс
// CHECK api_keys_binding_all_or_nothing), но достижимо по схеме — и цена ошибки
// несимметрична: назвав такой ключ глобальным, панель объявила бы свободным
// ключ, который keyAllowed не пропускает НИКУДА на поверхности привязки.
// Тест бьёт прямо по sessionResponseFor, потому что через store такой ключ не
// создать; без него мутация `if key.Project != nil && key.Env != nil` (трактовать
// полупару как глобальный ключ) проходит незамеченной — проверено вторым
// независимым проходом.
func TestSessionResponseHalfPairIsBound(t *testing.T) {
	project := "game"
	full := "dev"
	for _, c := range []struct {
		name    string
		key     store.APIKey
		want    bool
		wantEnv string
	}{
		{"глобальный", store.APIKey{Name: "g", Scopes: []string{"readonly"}}, false, ""},
		{"полная пара", store.APIKey{Name: "b", Scopes: []string{"readonly"}, Project: &project, Env: &full}, true, "dev"},
		{"полупара", store.APIKey{Name: "h", Scopes: []string{"readonly"}, Project: &project}, true, ""},
	} {
		binding := httpapi.SessionBindingForTest(c.key)
		if (binding != nil) != c.want {
			t.Fatalf("%s: привязка в ответе = %v, want %v", c.name, binding != nil, c.want)
		}
		if binding == nil {
			continue
		}
		if binding.Project != project || binding.Env != c.wantEnv {
			t.Fatalf("%s: binding = %s/%s, want %s/%s", c.name, binding.Project, binding.Env, project, c.wantEnv)
		}
	}
}

// TestSessionExpiry: sessions honor their TTL (in-memory store).
func TestSessionExpiry(t *testing.T) {
	ss := httpapi.NewSessionStoreForTest(30 * time.Millisecond)
	id, err := ss.CreateForTest("k1", []string{"readonly"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ss.GetForTest(id); !ok {
		t.Fatal("fresh session must resolve")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := ss.GetForTest(id); ok {
		t.Fatal("expired session must not resolve")
	}
}
