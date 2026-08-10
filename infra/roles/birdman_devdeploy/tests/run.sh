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
url="${!#}"; out=""; prev=""; body=""; follow=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  [ "$prev" = "-d" ] && body="$a"
  case "$a" in -*L*) follow=1 ;; esac
  prev="$a"
done
echo "$url ${body}" >> "$T/api.log"
# GitHub отдаёт ассеты релиза 302-редиректом на objects.githubusercontent.com.
# Без -L curl вернёт ПУСТОЕ тело и код 0 — молча, без ошибки. Моделируем это:
# запрос к релизу без -L отдаёт пустоту.
case "$url" in
  */releases/download/*|file://*)
    [ -n "$follow" ] || exit 0 ;;
esac
case "$url" in
  */healthz)
    if [ -f "$T/health_bad" ]; then echo '{"status":"degraded"}'; exit 22; fi
    echo '{"status":"ok"}'; exit 0 ;;
  */v1/nodes*)
    [ -f "$T/nodes.json" ] || exit 22
    cat "$T/nodes.json"; exit 0 ;;
  */v1/agent-upgrade)
    # Моделируем агента: если для этой ноды не подложен файл $T/fail_<id>,
    # она перепредставляется с новой версией (агент апгрейднулся). Иначе
    # версия не меняется — деплоер обязан это увидеть по таймауту.
    echo "$body" >> "$T/upgrades.log"
    python3 - "$body" "$T/nodes.json" "$T" <<'PY'
import json, os, sys
req = json.loads(sys.argv[1]); path, tdir = sys.argv[2], sys.argv[3]
if os.path.exists(os.path.join(tdir, "fail_" + req["node_id"])):
    sys.exit(0)
d = json.load(open(path))
for n in d["nodes"]:
    if n["id"] == req["node_id"]:
        n["agent_version"] = req["version"]
json.dump(d, open(path, "w"))
PY
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

# --- шаг агентов -------------------------------------------------------------

# Флот: удалённая нода намеренно ПЕРВАЯ в массиве — чтобы проверить, что
# деплоер действительно переставляет локальную вперёд, а не полагается на
# порядок ответа. Плюс мёртвая нода и нода чужого окружения.
fleet() {
  cat >"$T/nodes.json" <<'JSON'
{"nodes":[
 {"id":"n-remote","hostname":"dogi-tc","env":"dev","state":"active","agent_version":"old"},
 {"id":"n-local","hostname":"test-box","env":"dev","state":"active","agent_version":"old"},
 {"id":"n-dead","hostname":"gone","env":"dev","state":"dead","agent_version":"old"},
 {"id":"n-prod","hostname":"prodbox","env":"prod","state":"active","agent_version":"old"}
]}
JSON
  unset BIRDMAN_DEVDEPLOY_SKIP_AGENTS
  export BIRDMAN_DEVDEPLOY_LOCAL_HOSTNAME=test-box
}

setup "агенты: цели, адресность и порядок"
fleet
run
n="$(wc -l <"$T/upgrades.log" 2>/dev/null | tr -d ' ')"
[ "$n" = 2 ] && ok "апгрейд ушёл ровно двум нодам" || no "ушло $n команд вместо 2"
grep -q 'n-dead' "$T/upgrades.log" 2>/dev/null && no "мёртвая нода получила команду" || ok "мёртвая нода пропущена"
grep -q 'n-prod' "$T/upgrades.log" 2>/dev/null && no "нода чужого env получила команду" || ok "чужой env не тронут"
head -1 "$T/upgrades.log" 2>/dev/null | grep -q 'n-local' && ok "канарейка первой" || no "первой пошла не локальная нода"
tail -1 "$T/upgrades.log" 2>/dev/null | grep -q 'n-remote' && ok "удалённая после канарейки" || no "порядок нарушен"
! grep -qv 'node_id' "$T/upgrades.log" 2>/dev/null && ok "node_id есть в КАЖДОЙ команде" || no "нашлась команда без node_id"
grep -q 'dev-abc1234' "$T/upgrades.log" 2>/dev/null && ok "версия из MANIFEST" || no "версия не из MANIFEST"
grep -q 'birdman_devdeploy_agent_upgrades_total{status="ok"} 2' "$T/textfile/birdman-devdeploy.prom" 2>/dev/null &&
  ok "оба апгрейда посчитаны" || no "счётчик успешных апгрейдов неверен"

setup "агенты: уже на целевой версии — команды нет"
fleet
python3 - "$T/nodes.json" <<'PY'
import json,sys
p=sys.argv[1]; d=json.load(open(p))
for n in d["nodes"]: n["agent_version"]="dev-abc1234"
json.dump(d,open(p,"w"))
PY
run
[ ! -f "$T/upgrades.log" ] && ok "ни одной лишней команды" || no "дёрнул апгрейд на актуальной версии"

setup "агенты: канарейка легла — удалённую не трогаем"
fleet
touch "$T/fail_n-local"
run
grep -q 'n-local' "$T/upgrades.log" 2>/dev/null && ok "канарейке команда ушла" || no "канарейка не получила команду"
grep -q 'n-remote' "$T/upgrades.log" 2>/dev/null && no "удалённая нода тронута после провала" || ok "удалённая нода НЕ тронута"
grep -q '^agent:dev-abc1234$' "$T/state/rejected" 2>/dev/null && ok "версия агента в rejected" || no "rejected не содержит версию агента"
[ "$(cat "$T/usr/birdman-master")" = MASTER-BINARY-V2 ] && ok "мастер НЕ откачен из-за агента" || no "провал агента откатил мастера"
grep -q 'birdman_devdeploy_agent_upgrades_total{status="failed"} 1' "$T/textfile/birdman-devdeploy.prom" 2>/dev/null &&
  ok "провал посчитан" || no "счётчик провалов неверен"

rm -f "$T/upgrades.log" "$T/fail_n-local"
run
[ ! -f "$T/upgrades.log" ] && ok "отвергнутая версия агента не ретраится" || no "rejected-версия агента пошла в повтор"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
