#!/usr/bin/env bash
# Прогон тестов master без Go на хосте: postgres:16 и golang:1.24 в одной
# docker-сети. Дополнительные аргументы пробрасываются в `go test`
# (например: ./test.sh -run TestAllocate -v).
set -euo pipefail
cd "$(dirname "$0")/.."   # корень репо: нужен ../proto для replace-директивы

NET="birdman-test-net-$$"
PG="birdman-test-pg-$$"
S3="birdman-test-s3-$$"
cleanup() {
  docker rm -f "$PG" >/dev/null 2>&1 || true
  docker rm -f "$S3" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker network create "$NET" >/dev/null
docker run -d --rm --name "$PG" --network "$NET" \
  -e POSTGRES_PASSWORD=birdman postgres:16 >/dev/null

# Тот же поддерживаемый S3-сервер, что в CI; MinIO community архивирован.
source infra/ci/s3-image.env
docker run -d --rm --name "$S3" --network "$NET" \
  -e AWS_ACCESS_KEY_ID=minioadmin -e AWS_SECRET_ACCESS_KEY=minioadmin \
  "$BIRDMAN_TEST_S3_IMAGE" mini -dir=/data \
  -webdav=false -admin.ui=false -s3.port.iceberg=0 -s3.port.lance=0 >/dev/null

# Явный health-probe вместо ставки на прогрев компиляцией: bounded-луп
# (30×1с) на readyz изнутри docker-сети; не поднялся — явная ошибка.
s3_ok=""
for _ in {1..30}; do
  if docker run --rm --network "$NET" curlimages/curl \
       -sf "http://${S3}:8333/readyz" >/dev/null 2>&1; then
    s3_ok=1
    break
  fi
  sleep 1
done
if [ -z "$s3_ok" ]; then
  echo "ERROR: SeaweedFS S3 did not become healthy at http://${S3}:8333" >&2
  docker logs --tail 100 "$S3" >&2
  exit 1
fi

docker run --rm --network "$NET" \
  -v "$PWD":/src -w /src/master \
  -v birdman-go-mod:/go/pkg/mod -v birdman-go-cache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false \
  -e BIRDMAN_TEST_DSN="postgres://postgres:birdman@${PG}:5432/postgres?sslmode=disable" \
  -e BIRDMAN_TEST_S3_ENDPOINT="http://${S3}:8333" \
  -e BIRDMAN_TEST_S3_KEY=minioadmin \
  -e BIRDMAN_TEST_S3_SECRET=minioadmin \
  golang:1.24 go test -race ./... "$@"
