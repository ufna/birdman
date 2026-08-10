package httpapi_test

import (
	"testing"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// GET /v1/projects (мультипроект W1) — источник селектора проекта в панели.
// Readonly-скоуп по замыслу: панель обязана показывать, В КАКОМ проекте она
// работает, и readonly-сессии это видеть можно (в выдаче нет секретов).

// Пустая база отдаёт пустой МАССИВ, а не null: панель делает .map по нему без
// защиты, как и на остальных листингах (конвенция emptyNotNull).
func TestListProjectsEmptyIsArrayNotNull(t *testing.T) {
	st := testdb.New(t)
	ts, _, _ := deployServer(t, st)
	ro := &client{t: t, base: ts.URL, key: readonlyKey(t, st)}

	code, body := ro.do("GET", "/v1/projects", nil)
	if code != 200 {
		t.Fatalf("list projects: %d %v", code, body)
	}
	projects, ok := body["projects"].([]any)
	if !ok {
		t.Fatalf("projects must decode as an array (got %T): %v", body["projects"], body)
	}
	if len(projects) != 0 {
		t.Fatalf("fresh db must have no projects, got %v", projects)
	}
}

// Несколько проектов приезжают старейшим первым — панель берёт первый как
// дефолтный выбор, поэтому порядок часть контракта, а не деталь. У проектов,
// созданных в одну секунду, порядок разрешает слаг (ListProjects).
func TestListProjectsOrderedAndComplete(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // проект "game"
	ctx := t.Context()
	if _, err := st.SetProjectMatchSize(ctx, "arena", 4); err != nil { // второй проект
		t.Fatalf("create second project: %v", err)
	}
	ts, _, _ := deployServer(t, st)
	ro := &client{t: t, base: ts.URL, key: readonlyKey(t, st)}

	code, body := ro.do("GET", "/v1/projects", nil)
	if code != 200 {
		t.Fatalf("list projects: %d %v", code, body)
	}
	projects, _ := body["projects"].([]any)
	if len(projects) != 2 {
		t.Fatalf("want 2 projects, got %d: %v", len(projects), body)
	}
	slugs := make([]string, 0, len(projects))
	for _, p := range projects {
		slugs = append(slugs, p.(map[string]any)["slug"].(string))
	}
	// "game" завёл Seed раньше, чем тест создал "arena".
	if slugs[0] != f.Project || slugs[1] != "arena" {
		t.Fatalf("want [%s arena] oldest-first, got %v", f.Project, slugs)
	}
	// Селектор показывает слаг, а вкладка матчмейкинга — match_size: обе
	// колонки должны доезжать, а не только id.
	first := projects[0].(map[string]any)
	if first["match_size"] == nil || first["created_at"] == nil || first["id"] == nil {
		t.Fatalf("project payload is missing fields: %v", first)
	}
}

// Скоуп-матрица: аноним не видит списка вовсе, readonly видит (иначе селектор
// не нарисуется), admin тоже.
func TestListProjectsScopes(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10)
	ts, _, _ := deployServer(t, st)
	ctx := t.Context()
	_, adminKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}

	if code, _ := (&client{t: t, base: ts.URL}).do("GET", "/v1/projects", nil); code != 401 {
		t.Fatalf("anon: want 401, got %d", code)
	}
	if code, _ := (&client{t: t, base: ts.URL, key: readonlyKey(t, st)}).do("GET", "/v1/projects", nil); code != 200 {
		t.Fatalf("readonly: want 200, got %d", code)
	}
	if code, _ := (&client{t: t, base: ts.URL, key: adminKey}).do("GET", "/v1/projects", nil); code != 200 {
		t.Fatalf("admin: want 200, got %d", code)
	}
}

func readonlyKey(t *testing.T, st *store.Store) string {
	t.Helper()
	_, key, err := st.CreateAPIKey(t.Context(), store.CreateAPIKeyParams{
		Name: "ro-" + t.Name(), Scopes: []string{httpapi.ScopeReadonly},
	})
	if err != nil {
		t.Fatal(err)
	}
	return key
}
