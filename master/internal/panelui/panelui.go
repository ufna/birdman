// Package panelui serves the admin panel SPA embedded into the master
// binary (docs/specs/panel.md §1 принцип 2: self-host получает панель
// бесплатно, «два бинаря + Postgres»).
//
// How the embed works (documented in master/README.md):
//   - `static/` is populated by `panel/build.sh` (vite build) and is
//     git-ignored except the committed `.gitkeep`, so `go build` and
//     master/test.sh work on machines without node;
//   - with no built panel in the tree the handler serves a small built-in
//     placeholder page instead;
//   - routing: `/` and `/assets/*` → static files, unknown extensionless
//     paths → SPA fallback to index.html (client-side router),
//     `/v1/*` → JSON 404 (the API namespace never falls through to HTML).
package panelui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:static
var embedded embed.FS

// Handler serves the panel embedded at build time.
func Handler() http.Handler {
	sub, err := fs.Sub(embedded, "static")
	if err != nil { // unreachable: static/ is committed
		panic(err)
	}
	return NewHandler(sub)
}

// Embedded reports whether a real panel build is compiled into this binary.
//
// Тот же fs.Stat("index.html"), которым хендлер решает, отдавать SPA или
// placeholder, — намеренно ОДИН источник правды. Проверять сборку по содержимому
// HTML («есть ли в ответе маркер placeholder'а») значило бы завести вторую копию
// правды о том, как выглядит непособранная панель: ровно та мина, из-за которой
// случился #978, когда CI собирал панель не тем способом.
//
// Нужен деплоеру: health-gate по /healthz пингует БД и про панель не знает
// НИЧЕГО, поэтому бинарь без панели проходил его идеально — откат не срабатывал,
// и дефект замечал только человек, открывший панель (#983).
func Embedded() bool {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		return false
	}
	_, err = fs.Stat(sub, "index.html")
	return err == nil
}

// NewHandler serves a panel build from fsys (split from Handler for tests).
func NewHandler(fsys fs.FS) http.Handler {
	return &handler{fsys: fsys}
}

type handler struct{ fsys fs.FS }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The mux "/" pattern catches everything unrouted, including unknown
	// API paths — those must answer JSON, not HTML.
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		writeJSONError(w, http.StatusNotFound, "not_found", "unknown API endpoint")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "static content is read-only")
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name != "" && name != "." && fs.ValidPath(name) {
		if st, err := fs.Stat(h.fsys, name); err == nil && !st.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				// Vite content-hashes asset filenames — cache forever.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, h.fsys, name)
			return
		}
	}

	// SPA fallback: every non-file path renders index.html and the client
	// router takes over. Always revalidated so deploys are picked up.
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := fs.Stat(h.fsys, "index.html"); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(placeholder))
		return
	}
	r.URL.Path = "/" // ServeFileFS redirects paths ending in /index.html
	http.ServeFileFS(w, r, h.fsys, "index.html")
}

func writeJSONError(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + code + `","detail":"` + detail + `"}`))
}

// placeholder is what a master built without the panel serves at `/`
// (brand palette from docs/birdman-report.html).
const placeholder = `<!doctype html>
<html lang="ru">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>birdman</title>
<style>
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         background: #151B23; color: #E8ECF0;
         font: 16px/1.6 ui-sans-serif, system-ui, sans-serif; }
  main { max-width: 34rem; padding: 2rem; background: #1C242E;
         border: 1px solid #2C3743; border-radius: 12px; }
  h1 { font-size: 1.2rem; margin: 0 0 .5rem; }
  h1 b { color: #F0713A; }
  p { margin: .5rem 0; color: #97A5B4; }
  code { color: #E8ECF0; background: #151B23; padding: .1rem .4rem;
         border-radius: 4px; font-family: ui-monospace, monospace; }
</style>
<main>
  <h1><b>birdman</b> master</h1>
  <p>API работает, но панель не была собрана в этот бинарь.</p>
  <p>Соберите её и пересоберите master: <code>./master/build.sh</code>
     (панель собирается автоматически) — или <code>./panel/build.sh</code>
     отдельно.</p>
</main>
`
