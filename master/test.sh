#!/usr/bin/env bash
# Прогон тестов master без Go на хосте: postgres:16 и golang:1.24 в одной
# docker-сети. Дополнительные аргументы пробрасываются в `go test`
# (например: ./test.sh -run TestAllocate -v).
set -euo pipefail
cd "$(dirname "$0")/.."   # корень репо: нужен ../proto для replace-директивы

NET="birdman-test-net-$$"
PG="birdman-test-pg-$$"
cleanup() {
  docker rm -f "$PG" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker network create "$NET" >/dev/null
docker run -d --rm --name "$PG" --network "$NET" \
  -e POSTGRES_PASSWORD=birdman postgres:16 >/dev/null

docker run --rm --network "$NET" \
  -v "$PWD":/src -w /src/master \
  -v birdman-go-mod:/go/pkg/mod -v birdman-go-cache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false \
  -e BIRDMAN_TEST_DSN="postgres://postgres:birdman@${PG}:5432/postgres?sslmode=disable" \
  golang:1.24 go test -race ./... "$@"
