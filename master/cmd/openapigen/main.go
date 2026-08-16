// Command openapigen пересобирает master/api/openapi.yaml из таблицы
// маршрутов (master/internal/httpapi/routes.go).
//
// Запускается через `go generate ./...` в каталоге master; директива живёт в
// master/api/generate.go. Расхождение закоммиченного файла с генератором ловит
// TestOpenAPISpecIsUpToDate, то есть обычный `go test ./...` в CI.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ufna/birdman/master/internal/httpapi"
)

func main() {
	out := "api/openapi.yaml"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	spec, err := httpapi.OpenAPISpec()
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapigen:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "openapigen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, spec, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "openapigen:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "openapigen: записано %s (%d байт)\n", out, len(spec))
}
