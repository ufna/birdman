#!/usr/bin/env bash
# Прогон тестов master без Go на хосте: postgres:16 и golang:1.24 в одной
# docker-сети. Дополнительные аргументы пробрасываются в `go test`
# (например: ./test.sh -run TestAllocate -v).
set -euo pipefail
cd "$(dirname "$0")/.."   # корень репо: нужен ../proto для replace-директивы

NET="birdman-test-net-$$"
PG="birdman-test-pg-$$"
MINIO="birdman-test-minio-$$"
cleanup() {
  docker rm -f "$PG" >/dev/null 2>&1 || true
  docker rm -f "$MINIO" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker network create "$NET" >/dev/null
docker run -d --rm --name "$PG" --network "$NET" \
  -e POSTGRES_PASSWORD=birdman postgres:16 >/dev/null

# MinIO — S3-совместимая обвязка для интеграционных тестов backup/s3.
docker run -d --rm --name "$MINIO" --network "$NET" \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  minio/minio server /data >/dev/null

# Явный health-probe вместо ставки на прогрев компиляцией: bounded-луп
# (15×1с) на health/live изнутри docker-сети; не поднялся — явная ошибка.
minio_ok=""
for _ in {1..15}; do
  if docker run --rm --network "$NET" curlimages/curl \
       -sf "http://${MINIO}:9000/minio/health/live" >/dev/null 2>&1; then
    minio_ok=1
    break
  fi
  sleep 1
done
if [ -z "$minio_ok" ]; then
  echo "ERROR: MinIO did not become healthy at http://${MINIO}:9000" >&2
  exit 1
fi

docker run --rm --network "$NET" \
  -v "$PWD":/src -w /src/master \
  -v birdman-go-mod:/go/pkg/mod -v birdman-go-cache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false \
  -e BIRDMAN_TEST_DSN="postgres://postgres:birdman@${PG}:5432/postgres?sslmode=disable" \
  -e BIRDMAN_TEST_S3_ENDPOINT="http://${MINIO}:9000" \
  -e BIRDMAN_TEST_S3_KEY=minioadmin \
  -e BIRDMAN_TEST_S3_SECRET=minioadmin \
  golang:1.24 go test -race ./... "$@"
