#!/usr/bin/env bash
# Сборка панели и выкладка в master/internal/panelui/static — каталог,
# который master встраивает через go:embed (internal/panelui/panelui.go).
# node ≥20 на хосте используется, если есть; иначе — docker node:22
# (node на машине не обязателен). Тесты панели гоняет CI/`npm test`,
# сборка проверяет типы (tsc --noEmit) и собирает vite build.
set -euo pipefail
cd "$(dirname "$0")"

OUT="../master/internal/panelui/static"

host_node_ok() {
  command -v node >/dev/null 2>&1 &&
    node -e 'process.exit(Number(process.versions.node.split(".")[0]) >= 20 ? 0 : 1)'
}

if host_node_ok; then
  echo "panel: сборка локальным node $(node --version)"
  npm ci --no-fund --no-audit
  npm run check
  npm run build
else
  echo "panel: node ≥20 не найден — сборка в docker node:22"
  docker run --rm \
    -v "$PWD":/src -w /src \
    -v birdman-npm-cache:/root/.npm \
    node:22 sh -c 'npm ci --no-fund --no-audit && npm run check && npm run build'
fi

# Гейт витрины: демо-фикстуры живут во ВТОРОЙ точке входа (demo.html) и в
# прод-бандл входить не должны. Если маркер нашёлся в dist/, значит демо-модуль
# просочился в граф index.html и уехал бы в бинарь master через go:embed.
if grep -rq 'birdman-demo-fixture' dist/; then
  echo "panel: ОШИБКА — демо-фикстуры попали в прод-бандл (маркер birdman-demo-fixture в dist/)" >&2
  echo "panel: смотри panel/src/demo/ и граф импортов index.html" >&2
  exit 1
fi

rm -rf "$OUT"
mkdir -p "$OUT"
cp -R dist/. "$OUT/"
touch "$OUT/.gitkeep"   # якорь go:embed, единственный закоммиченный файл каталога
echo "panel: статика выложена в master/internal/panelui/static"
