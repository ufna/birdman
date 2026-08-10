#!/usr/bin/env bash
# Харнесс birdman-devdeploy: PATH-заглушки curl/systemctl + фикстуры релиза.
# Проверяем поведение по состоянию файлов, а не по выводу. bats/shellcheck в
# окружении нет — поэтому голый bash; запускается и на Linux, и на macOS.
#
#   ./infra/roles/birdman_devdeploy/tests/run.sh
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/../files/birdman-devdeploy"
PASS=0
FAIL=0
CASE=""

# sha256 переносимо: на Linux sha256sum, на macOS shasum -a 256.
if command -v sha256sum >/dev/null 2>&1; then sha() { sha256sum "$1" | awk '{print $1}'; }
else sha() { shasum -a 256 "$1" | awk '{print $1}'; }; fi

ok() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
no() { FAIL=$((FAIL + 1)); printf '  FAIL [%s] %s\n' "$CASE" "$1"; }

# setup <имя кейса> — свежий временный мир: заглушки, фикстуры релиза,
# «установленный» бинарь мастера версии V1 и новая сборка V2 в релизе.
setup() {
  CASE="$1"
  printf '\n· %s\n' "$CASE"
  T="$(mktemp -d)"
  export T
  mkdir -p "$T/bin" "$T/state" "$T/etc" "$T/release" "$T/usr" "$T/textfile"

  printf 'MASTER-BINARY-V1' >"$T/usr/birdman-master"
  printf 'MASTER-BINARY-V2' >"$T/release/birdman-master"
  sha "$T/release/birdman-master" >"$T/release/birdman-master.sha256"
  printf 'AGENT-BINARY-V2' >"$T/release/birdman-agent"
  sha "$T/release/birdman-agent" >"$T/release/birdman-agent.sha256"
  printf 'commit=abc1234\nagent_version=dev-abc1234\nrun_id=1\nbuilt_at=2026-08-11T00:00:00Z\n' \
    >"$T/release/MANIFEST"
  printf 'test-admin-key' >"$T/etc/master-admin.key"

  # curl: URL — последний аргумент. /healthz отдаёт статус (или 22 при
  # $T/health_bad), остальное — файл из $T/release по basename. Запросы к
  # master API логируются в $T/api.log для проверок шага агентов.
  cat >"$T/bin/curl" <<'STUB'
#!/usr/bin/env bash
url="${!#}"; out=""; prev=""; body=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  [ "$prev" = "-d" ] && body="$a"
  prev="$a"
done
echo "$url ${body}" >> "$T/api.log"
case "$url" in
  */healthz)
    if [ -f "$T/health_bad" ]; then echo '{"status":"degraded"}'; exit 22; fi
    echo '{"status":"ok"}'; exit 0 ;;
  */v1/nodes*)
    [ -f "$T/nodes.json" ] || exit 22
    cat "$T/nodes.json"; exit 0 ;;
  */v1/agent-upgrade)
    echo "$body" >> "$T/upgrades.log"
    [ -f "$T/upgrade_applies" ] && cp "$T/nodes_after.json" "$T/nodes.json"
    echo '{"upgrading":["x"]}'; exit 0 ;;
esac
name="${url##*/}"
[ -f "$T/release/$name" ] || exit 22
if [ -n "$out" ]; then cp "$T/release/$name" "$out"; else cat "$T/release/$name"; fi
STUB

  cat >"$T/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$T/systemctl.log"
STUB

  chmod +x "$T/bin/curl" "$T/bin/systemctl"
  export PATH="$T/bin:$PATH"
  export BIRDMAN_DEVDEPLOY_CONF="$T/etc/devdeploy.conf"
  export BIRDMAN_DEVDEPLOY_BASE_URL="file://$T/release"
  export BIRDMAN_DEVDEPLOY_BIN="$T/usr/birdman-master"
  export BIRDMAN_DEVDEPLOY_STATE_DIR="$T/state"
  export BIRDMAN_DEVDEPLOY_LOCK="$T/lock"
  export BIRDMAN_DEVDEPLOY_DISABLED="$T/etc/devdeploy.disabled"
  export BIRDMAN_DEVDEPLOY_ADMIN_KEY_FILE="$T/etc/master-admin.key"
  export BIRDMAN_DEVDEPLOY_TEXTFILE_DIR="$T/textfile"
  export BIRDMAN_DEVDEPLOY_HEALTH_TIMEOUT=2
  export BIRDMAN_DEVDEPLOY_AGENT_TIMEOUT=2
  export BIRDMAN_DEVDEPLOY_POLL_INTERVAL=1
  export BIRDMAN_DEVDEPLOY_SKIP_AGENTS=1
}

run() { "$SCRIPT" >>"$T/out.log" 2>&1; }

# --- шаг мастера -------------------------------------------------------------

setup "новая сборка ставится"
run
[ "$(cat "$T/usr/birdman-master")" = MASTER-BINARY-V2 ] && ok "бинарь обновлён" || no "бинарь не обновлён"
[ -f "$T/usr/birdman-master.prev" ] && [ "$(cat "$T/usr/birdman-master.prev")" = MASTER-BINARY-V1 ] &&
  ok ".prev сохранён" || no ".prev не сохранён"
grep -q 'restart birdman-master' "$T/systemctl.log" 2>/dev/null && ok "мастер перезапущен" || no "рестарта не было"
grep -q abc1234 "$T/state/deployed.json" 2>/dev/null && ok "deployed.json записан" || no "deployed.json пуст"

rm -f "$T/systemctl.log"
run
[ ! -f "$T/systemctl.log" ] && ok "повторный тик — no-op" || no "повторный тик дёрнул systemctl"

setup "битая sha отвергается"
echo deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef >"$T/release/birdman-master.sha256"
run
[ "$(cat "$T/usr/birdman-master")" = MASTER-BINARY-V1 ] && ok "бинарь не тронут" || no "поставился битый бинарь"
[ ! -f "$T/systemctl.log" ] && ok "рестарта не было" || no "был лишний рестарт"

setup "провал health-gate откатывает"
touch "$T/health_bad"
run
[ "$(cat "$T/usr/birdman-master")" = MASTER-BINARY-V1 ] && ok "откат на .prev" || no "отката не было"
grep -q '^master:' "$T/state/rejected" 2>/dev/null && ok "sha записан в rejected" || no "rejected пуст"
grep -cq 'restart birdman-master' "$T/systemctl.log" && ok "рестарт при откате" || no "нет рестарта отката"

rm -f "$T/systemctl.log" "$T/health_bad"
run
[ ! -f "$T/systemctl.log" ] && ok "отвергнутая сборка не ретраится" || no "rejected пошёл в повтор"

setup "kill-switch"
touch "$T/etc/devdeploy.disabled"
run
[ "$(cat "$T/usr/birdman-master")" = MASTER-BINARY-V1 ] && ok "выключен — ничего не делает" || no "сработал при kill-switch"

setup "релиз недоступен"
rm -f "$T/release/birdman-master.sha256"
run
[ "$(cat "$T/usr/birdman-master")" = MASTER-BINARY-V1 ] && ok "нет релиза — тихий no-op" || no "тронул бинарь без релиза"

setup "метрики пишутся"
run
grep -q 'birdman_devdeploy_last_success_timestamp [1-9]' "$T/textfile/birdman-devdeploy.prom" 2>/dev/null &&
  ok "успех отражён в textfile" || no "нет метрики успеха"
setup "метрика отката"
touch "$T/health_bad"
run
grep -q 'birdman_devdeploy_rollbacks_total 1' "$T/textfile/birdman-devdeploy.prom" 2>/dev/null &&
  ok "откат посчитан" || no "нет счётчика откатов"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
