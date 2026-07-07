#!/usr/bin/env bash
# Сборка статического birdman-agent (linux/amd64) через docker —
# Go на хосте не нужен. Результат: agent/dist/birdman-agent.
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

mkdir -p dist
docker run --rm \
  -v "$PWD":/src -w /src \
  -v birdman-agent-gomod:/go/pkg/mod \
  -v birdman-agent-gocache:/root/.cache/go-build \
  -e GOOS=linux -e GOARCH=amd64 -e CGO_ENABLED=0 \
  golang:1.24 \
  go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
  -o dist/birdman-agent ./cmd/birdman-agent

echo "built dist/birdman-agent (version ${VERSION})"
