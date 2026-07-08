#!/usr/bin/env bash
# Сборка статических linux/amd64 бинарей master/dist/{birdman-master,mmcli}
# через docker (Go на хосте не нужен). Панель собирается и встраивается
# автоматически (panel/build.sh, node не обязателен — есть docker-фолбэк);
# SKIP_PANEL=1 пропускает её — бинарь тогда отдаёт placeholder вместо панели.
set -euo pipefail
cd "$(dirname "$0")/.."   # корень репо: нужен ../proto для replace-директивы

if [ "${SKIP_PANEL:-}" != "1" ]; then
  ./panel/build.sh
fi

docker run --rm \
  -v "$PWD":/src -w /src/master \
  -v birdman-go-mod:/go/pkg/mod -v birdman-go-cache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false \
  -e GOOS=linux -e GOARCH=amd64 -e CGO_ENABLED=0 \
  golang:1.24 sh -c '
    go build -trimpath -ldflags "-s -w" -o dist/birdman-master ./cmd/birdman-master &&
    go build -trimpath -ldflags "-s -w" -o dist/mmcli ./cmd/mmcli'

if command -v file >/dev/null; then
  file master/dist/birdman-master master/dist/mmcli
fi
ls -lh master/dist/birdman-master master/dist/mmcli
