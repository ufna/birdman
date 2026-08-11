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

# publish — превратить каталог $T/artifact в OCI-артефакт реестра: каждый файл
# отдельным блобом, digest = sha256 содержимого (ровно то, что делает
# `oras push file:mediaType`). Манифест несёт title-аннотации — по ним деплоер
# и находит нужный блоб.
publish() {
  mkdir -p "$T/registry/blobs"
  : >"$T/registry/layers.txt"
  local f d
  for f in birdman-master birdman-agent MANIFEST; do
    [ -f "$T/artifact/$f" ] || continue
    d="sha256:$(sha "$T/artifact/$f")"
    cp "$T/artifact/$f" "$T/registry/blobs/$d"
    printf '%s %s\n' "$f" "$d" >>"$T/registry/layers.txt"
  done
  python3 - "$T/registry/layers.txt" >"$T/registry/manifest.json" <<'PY'
import json, sys
layers = []
for line in open(sys.argv[1]):
    title, digest = line.split()
    layers.append({
        "mediaType": "application/octet-stream",
        "digest": digest,
        "annotations": {"org.opencontainers.image.title": title},
    })
print(json.dumps({"schemaVersion": 2, "layers": layers}))
PY
}

# setup <имя кейса> — свежий временный мир: заглушки, фикстуры артефакта,
# «установленный» бинарь мастера версии V1 и новая сборка V2 в реестре.
setup() {
  CASE="$1"
  printf '\n· %s\n' "$CASE"
  T="$(mktemp -d)"
  export T
  mkdir -p "$T/bin" "$T/state" "$T/etc" "$T/artifact" "$T/usr" "$T/textfile"

  printf 'MASTER-BINARY-V1' >"$T/usr/birdman-master"
  printf 'MASTER-BINARY-V2' >"$T/artifact/birdman-master"
  printf 'AGENT-BINARY-V2' >"$T/artifact/birdman-agent"
  printf 'commit=abc1234\nagent_version=dev-abc1234\nrun_id=1\nbuilt_at=2026-08-11T00:00:00Z\n' \
    >"$T/artifact/MANIFEST"
  publish
  printf 'test-admin-key' >"$T/etc/master-admin.key"

  # curl: URL — последний аргумент. Заглушка играет OCI-реестр (токен, манифест,
  # блобы с 307 на подписанный URL), /healthz и master API. Запросы логируются
  # в $T/api.log для проверок шага агентов.
  cat >"$T/bin/curl" <<'STUB'
#!/usr/bin/env bash
url="${!#}"; out=""; prev=""; body=""; follow=""; want_redirect=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  [ "$prev" = "-d" ] && body="$a"
  [ "$prev" = "-w" ] && case "$a" in *redirect_url*) want_redirect=1 ;; esac
  case "$a" in -*L*) follow=1 ;; esac
  prev="$a"
done
echo "$url ${body}" >> "$T/api.log"
case "$url" in
  */token\?*|*/token)
    echo '{"token":"anon-test-token"}'; exit 0 ;;
  */v2/*/manifests/*)
    [ -f "$T/registry/manifest.json" ] || exit 22
    cat "$T/registry/manifest.json"; exit 0 ;;
  */v2/*/blobs/*)
    dg="${url##*/blobs/}"
    [ -f "$T/registry/blobs/$dg" ] || exit 22
    # Резолв подписанной ссылки (curl -o /dev/null -w '%{redirect_url}').
    if [ -n "$want_redirect" ]; then printf 'https://signed.test/%s' "$dg"; exit 0; fi
    # Реестр отвечает 307 на pkg-containers.githubusercontent.com: без -L curl
    # вернёт ПУСТОЕ тело и код 0 — молча, без ошибки. Моделируем эту мину.
    [ -n "$follow" ] || exit 0
    if [ -n "$out" ]; then cp "$T/registry/blobs/$dg" "$out"; else cat "$T/registry/blobs/$dg"; fi
    exit 0 ;;
  https://signed.test/*)
    # То, что дёрнет агент: подписанная ссылка отдаёт блоб БЕЗ заголовков.
    dg="${url##*/}"
    [ -f "$T/registry/blobs/$dg" ] || exit 22
    if [ -n "$out" ]; then cp "$T/registry/blobs/$dg" "$out"; else cat "$T/registry/blobs/$dg"; fi
    exit 0 ;;
esac
case "$url" in
  */healthz)
    if [ -f "$T/health_bad" ]; then echo '{"status":"degraded"}'; exit 22; fi
    # $T/panel_placeholder — мастер поднялся, но панель в бинарь не попала (#983)
    if [ -f "$T/panel_placeholder" ]; then echo '{"status":"ok","panel":"placeholder"}'; exit 0; fi
    echo '{"status":"ok","panel":"embedded"}'; exit 0 ;;
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
# Неизвестный URL — отказ, а не молчаливая выдача файла: заглушка не должна
# «случайно» отвечать на запрос, которого мы не предусмотрели.
exit 22
STUB

  cat >"$T/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$T/systemctl.log"
STUB

  chmod +x "$T/bin/curl" "$T/bin/systemctl"
  export PATH="$T/bin:$PATH"
  export BIRDMAN_DEVDEPLOY_CONF="$T/etc/devdeploy.conf"
  export BIRDMAN_DEVDEPLOY_REG_BASE="https://registry.test/v2/ufna/birdman-dev"
  export BIRDMAN_DEVDEPLOY_TOKEN_URL="https://registry.test/token?scope=repository:ufna/birdman-dev:pull&service=registry.test"
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

# Подмена содержимого блоба при неизменном манифесте: ровно то, что должен
# поймать деплоер — digest в манифесте больше не описывает то, что скачалось.
setup "содержимое не сошлось с digest — отвергается"
BAD="$(grep '^birdman-master ' "$T/registry/layers.txt" | awk '{print $2}')"
printf 'TAMPERED' >"$T/registry/blobs/$BAD"
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

setup "артефакт недоступен"
rm -f "$T/registry/manifest.json"
run
[ "$(cat "$T/usr/birdman-master")" = MASTER-BINARY-V1 ] && ok "нет артефакта — тихий no-op" || no "тронул бинарь без артефакта"

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
# Агент качает бинарь сам и Bearer'а не умеет — в команде обязана ехать
# ПОДПИСАННАЯ ссылка реестра, а не URL, требующий заголовка.
grep -q 'signed.test' "$T/upgrades.log" 2>/dev/null && ok "агенту ушла подписанная ссылка" || no "агенту ушла ссылка без подписи"
! grep -q '"url":"https://registry.test' "$T/upgrades.log" 2>/dev/null &&
  ok "URL реестра агенту не отдаётся" || no "агенту отдали URL, который он не скачает"
grep -q 'birdman_devdeploy_agent_upgrades_total{status="ok"} 2' "$T/textfile/birdman-devdeploy.prom" 2>/dev/null &&
  ok "оба апгрейда посчитаны" || no "счётчик успешных апгрейдов неверен"
grep -q 'birdman_devdeploy_agents_behind 0' "$T/textfile/birdman-devdeploy.prom" 2>/dev/null &&
  ok "дрейфа нет — гейдж 0" || no "гейдж дрейфа не обнулился после успешной цепочки"

# Типичный случай в жизни: мастер уже актуален (тик — no-op), а агенты отстали.
# Целевая версия агента обязана браться из MANIFEST независимо от того,
# понадобился ли выкат мастера, иначе агенты не сойдутся НИКОГДА.
setup "агенты: мастер актуален, агенты отстали"
fleet
printf 'MASTER-BINARY-V2' >"$T/usr/birdman-master"
run
n="$(wc -l <"$T/upgrades.log" 2>/dev/null | tr -d ' ')"
[ ! -f "$T/systemctl.log" ] && ok "мастер не перевыкачивался" || no "лишний выкат мастера"
[ "$n" = 2 ] && ok "агенты обновились на no-op тике" || no "агенты не тронуты: $n команд"

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
# Главное про отвергнутую версию: ретрая нет — значит ноды так и останутся на
# старом агенте. Молчать об этом нельзя, иначе дрейф не увидит никто:
# DevDeployFailed не горит (выкат мастера успешен), DevDeployStale тоже.
grep -q 'birdman_devdeploy_agents_behind 2' "$T/textfile/birdman-devdeploy.prom" 2>/dev/null &&
  ok "дрейф после rejected виден в гейдже" || no "дрейф после rejected не отражён в метрике"

# Инфраструктурная осечка реестра: подписанной ссылки нет. Версию отвергать
# нельзя (сборка ни при чём), но дрейф обязан быть виден.
# tracker #982: сегодня GHCR отвечает на блоб 307-редиректом, но опираться на
# это как на контракт нельзя. Если реестр однажды отдаст блоб телом, шаг агентов
# не должен вставать НАВСЕГДА: уходит прямая ссылка на блоб — для публичного
# пакета она тоже качается, просто без подписи.
setup "агенты: реестр без редиректа — уходит прямая ссылка, цепочка живёт"
fleet
python3 - "$T/bin/curl" <<'PYSTUB'
import sys
p = sys.argv[1]
s = open(p).read()
old = """    if [ -n "$want_redirect" ]; then printf 'https://signed.test/%s' "$dg"; exit 0; fi"""
new = """    if [ -n "$want_redirect" ]; then printf ''; exit 0; fi"""
assert old in s
open(p, "w").write(s.replace(old, new))
PYSTUB
run
n="$(wc -l <"$T/upgrades.log" 2>/dev/null | tr -d ' ')"
[ "$n" = 2 ] && ok "цепочка агентов отработала без редиректа" || no "без редиректа цепочка встала: $n команд"
grep -q 'registry.test/v2/ufna/birdman-dev/blobs/sha256:' "$T/upgrades.log" 2>/dev/null &&
  ok "агенту ушла прямая ссылка на блоб" || no "прямой ссылки в команде нет"
grep -q '^agent:' "$T/state/rejected" 2>/dev/null && no "фоллбэк отверг версию" || ok "версия не отвергнута"
grep -q 'birdman_devdeploy_agents_behind 0' "$T/textfile/birdman-devdeploy.prom" 2>/dev/null &&
  ok "дрейфа нет — цепочка дошла" || no "дрейф остался при рабочем фоллбэке"

# Регрессия из ревью #982: пустой redirect_url бывает по ДВУМ разным причинам, и
# путать их нельзя. Реестр недоступен (curl вернул ненулевой код) → отдать
# прямую ссылку значит послать агенту заведомо нерабочий адрес: он не скачает,
# не перепредставится, словит таймаут — и ИСПРАВНАЯ сборка уедет в rejected
# навсегда. Транзиентный сбой сети не имеет права отвергать сборку.
setup "агенты: реестр НЕДОСТУПЕН — версия не отвергается"
fleet
python3 - "$T/bin/curl" <<'PYSTUB'
import sys
p = sys.argv[1]
s = open(p).read()
old = """  */v2/*/blobs/*)"""
new = """  */v2/*/blobs/*)
    # реестр лежит: ненулевой код, пустое тело — как curl -f на 5xx/сети
    [ -n "$want_redirect" ] && exit 7"""
assert old in s
open(p, "w").write(s.replace(old, new, 1))
PYSTUB
run
[ ! -f "$T/upgrades.log" ] && ok "команда с нерабочей ссылкой не ушла" || no "агенту ушла ссылка при лежащем реестре"
grep -q '^agent:' "$T/state/rejected" 2>/dev/null && no "сбой сети отверг исправную сборку" || ok "версия НЕ отвергнута при недоступном реестре"
grep -q 'birdman_devdeploy_agents_behind 2' "$T/textfile/birdman-devdeploy.prom" 2>/dev/null &&
  ok "дрейф виден — человек узнает" || no "дрейф не отражён"

# tracker #983: бинарь без панели проходил health-gate идеально — /healthz
# пингует БД и про панель не знает. Теперь мастер сам отдаёт panel=placeholder,
# и деплоер обязан считать такой выкат провальным: откатить и запомнить sha
# отвергнутой сборки, как при любом провале гейта.
setup "выкат без панели откатывается, хотя БД здорова"
touch "$T/panel_placeholder"
run
[ "$(cat "$T/usr/birdman-master")" = MASTER-BINARY-V1 ] && ok "откат на .prev" || no "безпанельная сборка осталась на боксе"
grep -q '^master:' "$T/state/rejected" 2>/dev/null && ok "sha записан в rejected" || no "rejected пуст"
grep -q 'birdman_devdeploy_rollbacks_total 1' "$T/textfile/birdman-devdeploy.prom" 2>/dev/null &&
  ok "откат посчитан" || no "счётчик откатов не сдвинулся"

# Мастер старой сборки поля panel не отдаёт — это НЕ повод откатываться, иначе
# первый же выкат на боксе со старым бинарём уходил бы в вечный откат.
setup "старый мастер без поля panel выкатывается как раньше"
python3 - "$T/bin/curl" <<'PYSTUB'
import sys
p = sys.argv[1]
s = open(p).read()
old = """    echo '{"status":"ok","panel":"embedded"}'; exit 0 ;;"""
new = """    echo '{"status":"ok"}'; exit 0 ;;"""
assert old in s
open(p, "w").write(s.replace(old, new))
PYSTUB
run
[ "$(cat "$T/usr/birdman-master")" = MASTER-BINARY-V2 ] && ok "выкат прошёл" || no "старый мастер вызвал ложный откат"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
