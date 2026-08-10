#!/usr/bin/env bash
# Сборка статического birdman-agent (linux/amd64) через docker —
# Go на хосте не нужен. Результат: agent/dist/birdman-agent.
# Монтируется корень репо: agent/go.mod содержит replace ../proto.
set -euo pipefail
cd "$(dirname "$0")"

# Строка версии попадает в -X main.version, а оттуда — в Hello агента. Мастер
# сверяет её с тем, что запросили в POST /v1/agent-upgrade, поэтому в CI VERSION
# задаётся ЯВНО (.github/workflows/dev-build.yml): git describe на shallow-клоне
# вернул бы другую строку, и watchdog слал бы ложные agent_upgrade_failed.
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

mkdir -p dist
docker run --rm \
  -v "$PWD/..":/src -w /src/agent \
  -v birdman-agent-gomod:/go/pkg/mod \
  -v birdman-agent-gocache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false \
  -e GOOS=linux -e GOARCH=amd64 -e CGO_ENABLED=0 \
  golang:1.24 \
  go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
  -o dist/birdman-agent ./cmd/birdman-agent

echo "built dist/birdman-agent (version ${VERSION})"
