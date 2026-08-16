// Package api держит машиночитаемый контракт публичного REST API мастера.
//
// openapi.yaml СГЕНЕРИРОВАН из таблицы маршрутов
// (master/internal/httpapi/routes.go) и правится только через неё: рукописная
// правка файла — вторая копия правды, которая разъедется с роутером молча
// (решение #972, docs/specs/master.md §6).
//
// Пересобрать:  go generate ./...   (из каталога master)
// Проверить:    go test ./internal/httpapi/ -run TestOpenAPISpecIsUpToDate
package api

//go:generate go run ../cmd/openapigen ./openapi.yaml
