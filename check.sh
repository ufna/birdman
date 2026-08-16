#!/usr/bin/env bash
# Проверить репозиторий одной командой.
#
# ЗАЧЕМ. Проверки жили только внутри восьми воркфлоу GitHub Actions, и человек
# (или агент), склонировавший репозиторий, не имел ответа на вопрос «чем
# убедиться, что моя правка не сломала соседей» — кроме как открыть YAML и
# переписать команды руками. Этот скрипт и есть такой ответ.
#
# Он НЕ дублирует CI и не заменяет его: CI гоняет ещё и то, чему нужен Docker,
# Postgres, buf, ansible и tsan-сборка. Здесь — быстрый честный слой, который
# работает на ноутбуке.
#
#   ./check.sh            # всё, что доступно на этой машине
#   ./check.sh master     # только один компонент: master | agent | panel | sdk
#
# Компонент, которому не хватает инструментов (нет Go, нет npm, нет Postgres),
# ПРОПУСКАЕТСЯ ГРОМКО — с явной строкой SKIP и причиной. Молчаливый пропуск
# читался бы как «проверено», а это худший из возможных исходов.

set -uo pipefail
cd "$(dirname "$0")"

only="${1:-all}"
failed=()
skipped=()

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
skip() { printf '   \033[33mSKIP\033[0m %s\n' "$*"; skipped+=("$*"); }
run()  {
  local label="$1"; shift
  printf '   -- %s\n' "$label"
  if ! "$@"; then failed+=("$label"); fi
}

want() { [ "$only" = all ] || [ "$only" = "$1" ]; }

# --- master (Go) -------------------------------------------------------------
# Здесь НЕ весь `go test ./...`: интеграционным пакетам master'а нужна живая
# база, и без BIRDMAN_TEST_DSN они поднимают Postgres в докере — минуты, а не
# секунды. Гоняем сборку, vet и быстрые пины контракта; полный набор остаётся за
# CI и за явным `go -C master test ./...`, когда база под рукой.
if want master; then
  say "master"
  if ! command -v go >/dev/null; then
    skip "master: go не найден"
  else
    run "master: build" go -C master build ./...
    run "master: vet" go -C master vet ./...
    run "master: openapi.yaml не разошёлся с таблицей маршрутов" \
      go -C master test ./internal/httpapi/ -run 'TestOpenAPISpecIsUpToDate|TestRouteTable|TestMCP|TestRoutesHaveResponseSamples' -count=1
  fi
fi

# --- agent (Go) --------------------------------------------------------------
if want agent; then
  say "agent"
  if ! command -v go >/dev/null; then
    skip "agent: go не найден"
  else
    run "agent: build" go -C agent build ./...
    run "agent: vet" go -C agent vet ./...
    run "agent: test" go -C agent test ./... -count=1
  fi
fi

# --- panel (TypeScript/React) ------------------------------------------------
if want panel; then
  say "panel"
  if ! command -v npm >/dev/null; then
    skip "panel: npm не найден"
  elif [ ! -d panel/node_modules ]; then
    skip "panel: нет node_modules — сначала (cd panel && npm ci)"
  else
    run "panel: typecheck" npm --prefix panel run check
    run "panel: lint" npm --prefix panel run lint
    run "panel: test" npm --prefix panel test
  fi
fi

# --- sdk (C++) ---------------------------------------------------------------
if want sdk; then
  say "sdk"
  if ! command -v cmake >/dev/null; then
    skip "sdk: cmake не найден"
  else
    run "sdk: configure" cmake -S sdk -B sdk/build -DCMAKE_BUILD_TYPE=Release
    run "sdk: build" cmake --build sdk/build -j
    run "sdk: ctest" ctest --test-dir sdk/build --output-on-failure
  fi
fi

# --- итог --------------------------------------------------------------------
echo
if [ ${#skipped[@]} -gt 0 ]; then
  printf '\033[33mПропущено (%d):\033[0m\n' "${#skipped[@]}"
  printf '  - %s\n' "${skipped[@]}"
fi
if [ ${#failed[@]} -gt 0 ]; then
  printf '\033[31mУПАЛО (%d):\033[0m\n' "${#failed[@]}"
  printf '  - %s\n' "${failed[@]}"
  exit 1
fi
printf '\033[32mВсё, что удалось проверить на этой машине, — зелёное.\033[0m\n'
[ ${#skipped[@]} -gt 0 ] && printf 'Но не всё было проверено: см. пропуски выше.\n'
exit 0
