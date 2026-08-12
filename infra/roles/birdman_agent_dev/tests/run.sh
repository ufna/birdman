#!/usr/bin/env bash
# Тесты роли birdman_agent_dev — LOCAL only, ни один хост не трогается и
# плейбук не гоняется: шаблоны роли рендерятся настоящим ansible'ом в temp-каталог,
# а VRL-трансформ шиппера прогоняется настоящим бинарём vector'а.
#
#   ./infra/roles/birdman_agent_dev/tests/run.sh
#
# Engine: `vector test` (тот же образ, который роль катит, пин
# birdman_vector_image) + `ansible-playbook` (tests/render_role.yml). Переписывать
# ни то, ни другое на другом языке ради теста нельзя: у регекса важна СЕМАНТИКА
# ЧАСТИЧНОГО СОВПАДЕНИЯ настоящей машинки (ради неё и нужен якорь `^`), а у
# рендера — фильтры и trim_blocks именно ansible'а.
#
# Что покрыто:
#   1. #1014 (хвост #994) — пара (project, env) вешается стрим-лейблами из ПУТИ,
#      и по ней master сужает запрос привязанного ключа. Снятие якоря
#      `^/var/log/birdman/servers/` превращает плоский `.../servers/legacy.log` в
#      пару project=birdman, env=servers (оба валидные слаги), и ключ,
#      привязанный к ней, получает всю нелейблованную историю флота.
#   2. #1065 — бокс с НЕСКОЛЬКИМИ нодами birdman: корень регекса стал
#      перечислением корней нод, и перечисление обязано остаться перечислением
#      (каталог-двойник пары не получает); плюс развод путей/портов/юнитов между
#      нодами и отказ роли на столкновении.
#   3. #1068 — UDP-эхо QoS общее на хост, но ВЛАДЕЛЬЦА У НЕГО НЕ НАЗНАЧАЮТ:
#      претендуют все ноды бокса, порт достаётся живому. Назначенный конфигом
#      владелец уносил бы ping-таргет бокса с собой — в том числе у соседнего
#      проекта, чью ноду мастер продолжает отдавать в GET /v1/qos.
#   4. #1069 — бинарём агента на стенде владеет pull-деплоер, а не роль: прогон
#      плейбука по живому боксу обязан бинарь НЕ ТРОГАТЬ (иначе откат агента на
#      локальную сборку + расхождение строки версии с тем, что мастер запросил
#      в POST /v1/agent-upgrade, то есть ложные agent_upgrade_failed).
#   4. #1070 — тот же гард БЕЗ ФЛАГА, как в add-node.yml: владение выводится из
#      состояния МАСТЕР-БОКСА ФЛОТА, поэтому в харнессе два контейнера (нода и
#      мастер) — один не отличил бы «спросили мастера» от «спросили хоть
#      кого-нибудь», а деплоер живёт не на ноде.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
role="$(dirname "$here")"
image="${BIRDMAN_VECTOR_IMAGE:-$(sed -n 's/^birdman_vector_image: *//p' "$role/defaults/main.yml")}"
# Контейнеры-«боксы» для гарда бинаря (#1069, #1070): имена от id задач, за
# собой убираем. Их ДВА, и это несущее: у удалённой ноды деплоер живёт не на
# ней, а на master-боксе флота, поэтому один контейнер не отличил бы «проба
# спросила мастера» от «проба спросила хоть кого-нибудь» (#1070).
box_image="${BIRDMAN_TEST_BOX_IMAGE:-python:3-alpine}"
# Имена переопределяемы: в дереве работают несколько сессий разом, а `docker rm -f`
# на фиксированном имени убил бы контейнер чужого прогона на середине.
box="${BIRDMAN_TEST_BOX_NAME:-bm1069-box}"
master="${BIRDMAN_TEST_MASTER_NAME:-bm1070-master}"

work="$(mktemp -d)"
cleanup() {
  rm -rf "$work"
  docker rm -f "$box" "$master" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Бокс из ДВУХ нод — тот самый случай, ради которого заведён #1065: одна
# коробка, один айпи, два проекта.
two_nodes='{"birdman_box_instances":[{},{"name":"khl","project":"khl-legends","capacity_slots":4,"port_range":[20100,20150],"metrics_port":9111}]}'
one_node='{"birdman_box_instances":[{}]}'

render() { # render <out-dir> <extra-vars-json>
  mkdir -p "$1"
  ansible-playbook -i birdman-test-node, "$here/render_role.yml" \
    -e "birdman_out=$1" -e "$2" >"$1/ansible.log" 2>&1 \
    || { cat "$1/ansible.log" >&2; return 1; }
}

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# ── 1. Бокс с одной нодой: сегодняшний результат обязан остаться сегодняшним ──
echo "── render: box with one node"
render "$work/one" "$one_node"
grep -qx 'd /run/birdman 0755 root root -' "$work/one/tmpfiles-birdman.conf" \
  || fail "легаси-нода потеряла свой /run/birdman в tmpfiles.d"
for key in node_name containerd_namespace metrics_addr qos_echo_addr; do
  ! grep -q "^$key:" "$work/one/agent.yaml--default" \
    || fail "конфиг легаси-ноды получил ключ $key — у бокса с одной нодой он обязан отсутствовать (иначе прогон по живой ноде даёт changed на пустом месте)"
done
grep -q 'node_token_file: /etc/birdman/node.token' "$work/one/agent.yaml--default" \
  || fail "легаси-нода сменила путь node.token — повторная регистрация вместо идемпотентного прогона"
grep -q 'ExecStart=.*--config /etc/birdman/agent.yaml' "$work/one/birdman-agent.service--default" \
  || fail "юнит легаси-ноды сменил путь конфига"

# ── 2. Бокс с двумя нодами: всё, чем ноды делят хост, обязано быть разным ─────
echo "── render: box with two nodes"
render "$work/two" "$two_nodes"

python3 - "$work/two/instances.json" <<'PY' || exit 1
import json, sys
inst = json.load(open(sys.argv[1]))
if len(inst) != 2:
    sys.exit("FAIL: ожидались две ноды, получено %d" % len(inst))
# Каждое из этих полей — общий на хост ресурс. Совпадение любого означает, что
# вторая нода отбирает его у первой, а не сосуществует с ней.
for field in ("node_name", "unit", "config_file", "token_file", "data_dir",
              "log_dir", "run_dir", "socket_dir", "containerd_namespace",
              "metrics_port"):
    vals = [i[field] for i in inst]
    if len(set(map(str, vals))) != len(vals):
        sys.exit("FAIL: ноды делят %s: %r" % (field, vals))
a, b = (tuple(int(x) for x in i["port_range"]) for i in inst)
if not (a[1] < b[0] or b[1] < a[0]):
    sys.exit("FAIL: диапазоны портов дедиков пересекаются: %r и %r" % (a, b))
# Эхо QoS — наоборот, ОБЩЕЕ на хост, и претендуют на него ВСЕ ноды бокса
# (#1068): владелец, назначенный конфигом, уносил бы ping-таргет бокса с собой.
claimers = [i["node_name"] for i in inst if i["qos_echo"]]
if len(claimers) != len(inst):
    sys.exit("FAIL: за QoS-эхо состязаются %d нод из %d: %r" % (len(claimers), len(inst), claimers))
print("ok: две ноды не делят ни одного из %d ресурсов; за эхо состязаются %s" % (10, claimers))
PY

# Ни одна нода бокса не отключает эхо по умолчанию: `off` у соседа означал бы,
# что смерть агента-владельца гасит ping-таргет и для ЖИВОЙ ноды этого соседа
# (мастер продолжает отдавать её в GET /v1/qos — один таргет на (регион, ip)).
for cfg in "$work/two/agent.yaml--default" "$work/two/agent.yaml--khl"; do
  ! grep -q '^qos_echo_addr' "$cfg" \
    || fail "$cfg пинует qos_echo_addr — обе ноды обязаны состязаться за общий порт бокса на код-дефолте :19999"
done
grep -q '"127.0.0.1:9111"' "$work/two/vmagent.yaml" \
  || fail "нодовый vmagent не скрейпит вторую ноду — её DiskHigh/TickDegraded молчали бы, выглядя как здоровье"
grep -q 'job_name: birdman-agent-khl' "$work/two/vmagent.yaml" \
  || fail "джоб второй ноды не назван отдельно — up==0 и absent_over_time не различили бы ноды"
grep -q '/var/log/birdman-khl/servers:/var/log/birdman-khl/servers:ro' "$work/two/compose.yml" \
  || fail "vector не смонтировал каталог логов второй ноды"

# ── 3. Идемпотентность ПО КАЖДОЙ НОДЕ ────────────────────────────────────────
# Второй прогон в тот же каталог обязан дать changed=0. Это не про «чисто в
# выводе»: рестарт агента ноды роль ставит в очередь ровно по `changed` её
# конфига/юнита/токена (tasks/instance.yml), поэтому нестабильный рендер —
# порядок ключей, дата, что угодно — молча перезапускал бы СОСЕДНЮЮ ноду при
# прогоне по этой. Отдельный пункт приёмки #1065.
echo "── idempotency: a second render of the same box must change nothing"
render "$work/two" "$two_nodes"
grep -qE 'changed=0 ' "$work/two/ansible.log" \
  || { grep -E 'PLAY RECAP' -A 2 "$work/two/ansible.log" >&2; \
       fail "повторный рендер того же бокса дал changed — соседняя нода перезапускалась бы на пустом месте"; }
echo "ok: повторный прогон — changed=0"

# ── 4. Отказ на столкновении: гейт обязан ловить это до записи на диск ───────
echo "── render: colliding box must be refused"
# Пересечение диапазонов портов дедиков — единственное отличие этого бокса от
# рабочего: остальное у второй ноды своё, чтобы отказ пришёл именно отсюда.
if render "$work/bad" '{"birdman_box_instances":[{},{"name":"khl","metrics_port":9111,"port_range":[20040,20090]}]}' 2>/dev/null; then
  fail "роль приняла бокс с пересекающимися диапазонами портов дедиков"
fi
grep -q 'Диапазоны портов дедиков пересекаются' "$work/bad/ansible.log" \
  || fail "отказ произошёл не по той причине (см. $work/bad/ansible.log)"
# Общий порт метрик — вторая нода не поднимет листенер, а скрейп покажет одну
# ноду дважды.
if render "$work/bad3" '{"birdman_box_instances":[{},{"name":"khl","port_range":[20100,20150]}]}' 2>/dev/null; then
  fail "роль приняла бокс, на котором две ноды делят порт /metrics"
fi
if render "$work/bad2" '{"birdman_box_instances":[{"qos_echo":false},{"name":"khl","port_range":[20100,20150],"metrics_port":9111,"qos_echo":false}]}' 2>/dev/null; then
  fail "роль приняла бокс, на котором UDP-эхо QoS не держит никто"
fi
echo "ok: столкновения отвергнуты"

# ── 4a. Нестандартный порт эха обязан доехать до agent.yaml (#1068) ──────────
# UFW роль открывает birdman_qos_echo_port, а респондер без этой строки слушал
# бы код-дефолт агента :19999 — открытый порт и слушающий разошлись бы молча.
echo "── render: custom QoS echo port must reach every agent.yaml"
render "$work/port" '{"birdman_box_instances":[{},{"name":"khl","port_range":[20100,20150],"metrics_port":9111}],"birdman_qos_echo_port":19998}'
for cfg in "$work/port/agent.yaml--default" "$work/port/agent.yaml--khl"; do
  grep -q '^qos_echo_addr: ":19998"' "$cfg" \
    || fail "$cfg не получил нестандартный порт эха — UFW открыл бы один порт, а агент слушал бы другой"
done
echo "ok: порт эха доехал до обеих нод"

# ── 5. Гард бинаря агента (#1069): владелец — pull-деплоер ───────────────────
# Играется настоящий tasks/binary.yml роли против КОНТЕЙНЕРА-«бокса»: copy в
# роли ставит owner/group root, поэтому в temp-каталоге под обычным
# пользователем прогон упал бы на chown, а --check показал бы решение движка,
# но не то, что байты на боксе остались прежними. Содержимое сверяем отдельным
# каналом (docker exec), а не выводом того же прогона.
echo "── binary guard: the pull deployer owns /usr/local/bin/birdman-agent"
ansible-galaxy collection list community.docker >/dev/null 2>&1 \
  || fail "нужна коллекция community.docker (connection-плагин docker): ansible-galaxy collection install community.docker"

# Пути, по которым роль спрашивает мастер-бокс, обязаны быть теми, которые роль
# деплоера реально СТАВИТ. Расхождение здесь не падало бы ни на одном прогоне:
# гард просто перестал бы находить деплоер и тихо выключился — то есть вернулся
# бы ровно к дефекту #1070, но уже необнаружимо. Поэтому сверка, а не доверие.
dd_script="$(sed -n 's/^birdman_devdeploy_script_path: *//p' "$role/defaults/main.yml")"
dd_disabled="$(sed -n 's/^birdman_devdeploy_disabled_path: *//p' "$role/defaults/main.yml")"
dd_tasks="$(dirname "$role")/birdman_devdeploy/tasks/main.yml"
[ -n "$dd_script" ] && [ -n "$dd_disabled" ] \
  || fail "в defaults роли агента нет путей пробы деплоера"
grep -q "dest: $dd_script\$" "$dd_tasks" \
  || fail "роль агента пробует $dd_script, а роль деплоера ставит скрипт не туда — гард молча выключится"
grep -q "path: $dd_disabled\$" "$dd_tasks" \
  || fail "роль агента пробует kill-switch $dd_disabled, а роль деплоера держит его не там"
echo "ok: пути пробы совпадают с тем, что ставит роль деплоера"

for c in "$box" "$master"; do
  docker rm -f "$c" >/dev/null 2>&1 || true
  docker run -d --name "$c" "$box_image" sleep 900 >/dev/null
done

# Инвентарь ФАЙЛОМ, а не «-i host,»: делегированная задача берёт connection из
# hostvars ЦЕЛИ делегирования, а у хоста из inline-списка их задать негде.
cat >"$work/guard-inventory.yml" <<INV
all:
  hosts:
    $box:
      ansible_connection: community.docker.docker
      ansible_python_interpreter: /usr/local/bin/python3
    $master:
      ansible_connection: community.docker.docker
      ansible_python_interpreter: /usr/local/bin/python3
INV

mkdir -p "$work/build"
printf 'LOCAL-BUILD\n' >"$work/build/birdman-agent"

# Мастер флота — ОТДЕЛЬНЫЙ хост, как у настоящей удалённой ноды в add-node.yml:
# -l оставляет в плее только ноду, мастер остаётся целью delegate_to.
guard() { # guard <лог> [доп. -e ansible'у...]
  local log="$1"; shift
  ansible-playbook -i "$work/guard-inventory.yml" -l "$box" "$here/binary_guard.yml" \
    -e "birdman_agent_binary=$work/build/birdman-agent" \
    -e "birdman_master_api_host=$master" "$@" >"$log" 2>&1 \
    || { cat "$log" >&2; fail "прогон гарда упал (см. вывод выше)"; }
}
box_bin() { docker exec "$box" cat /usr/local/bin/birdman-agent 2>/dev/null; }
put_box_bin() { docker exec "$box" sh -c 'printf "DEPLOYED-BY-DEVDEPLOY\n" >/usr/local/bin/birdman-agent && chmod 0755 /usr/local/bin/birdman-agent'; }
# Деплоер «стоит» на боксе = там лежит его скрипт (ровно то, что ставит роль).
put_deployer() { docker exec "$1" sh -c "mkdir -p \$(dirname $dd_script) && printf '#!/bin/sh\n' >$dd_script && chmod 0755 $dd_script"; }
rm_deployer() { docker exec "$1" rm -f "$dd_script"; }
kill_switch() { docker exec "$1" sh -c "mkdir -p \$(dirname $dd_disabled) && touch $dd_disabled"; }
rm_kill_switch() { docker exec "$1" rm -f "$dd_disabled"; }
note='Бинарь агента не трогаю'

# (а) Первичный bring-up: деплоер включён, но бинаря на боксе ЕЩЁ НЕТ — роль
# обязана поставить его, иначе новая нода не поднимется вовсе (деплоер догонит
# релизной сборкой следующим тиком).
docker exec "$box" rm -f /usr/local/bin/birdman-agent
guard "$work/guard-bringup.log" -e birdman_devdeploy_enabled=true
grep -q 'BINARY_GUARD changed=True skipped=False instances=1' "$work/guard-bringup.log" \
  || fail "bring-up: бинаря на боксе не было, а роль его не поставила"
[ "$(box_bin)" = "LOCAL-BUILD" ] || fail "bring-up: на боксе оказался не тот бинарь"
[ "$(docker exec "$box" stat -c '%a %U' /usr/local/bin/birdman-agent)" = "755 root" ] \
  || fail "bring-up: бинарь встал с чужими правами/владельцем"
! grep -q "$note" "$work/guard-bringup.log" || fail "bring-up: заметка «владеет деплоер» напечатана там, где роль сама поставила бинарь"

# (б) Живой бокс: деплоер включён, бинарь на месте — прогон обязан его НЕ
# ТРОГАТЬ и сказать об этом в выводе.
put_box_bin
guard "$work/guard-owned.log" -e birdman_devdeploy_enabled=true
grep -q 'BINARY_GUARD changed=False skipped=True instances=1' "$work/guard-owned.log" \
  || fail "прогон при включённом деплоере не пропустил установку бинаря"
[ "$(box_bin)" = "DEPLOYED-BY-DEVDEPLOY" ] \
  || fail "прогон затёр бинарь, которым владеет деплоер (откат агента + расхождение версии)"
grep -q "$note" "$work/guard-owned.log" \
  || fail "в выводе нет заметки о том, ПОЧЕМУ бинарь пропущен — пропуск неотличим от потери задачи"

# (в) Бокс без деплоера — ставит, как раньше. Проверяются обе формы «нет
# деплоера»: флаг не задан вовсе (тогда роль спрашивает мастер-бокс флота, а на
# нём деплоера нет — случай self-host в add-node.yml) и явный false.
guard "$work/guard-nodeploy.log"
grep -q 'BINARY_GUARD changed=True skipped=False instances=1' "$work/guard-nodeploy.log" \
  || fail "без флага деплоера роль перестала ставить бинарь"
[ "$(box_bin)" = "LOCAL-BUILD" ] || fail "без флага деплоера бинарь не обновился"
put_box_bin
guard "$work/guard-off.log" -e birdman_devdeploy_enabled=false
grep -q 'BINARY_GUARD changed=True skipped=False instances=1' "$work/guard-off.log" \
  || fail "при выключенном деплоере роль перестала ставить бинарь"
[ "$(box_bin)" = "LOCAL-BUILD" ] || fail "при выключенном деплоере бинарь не обновился"
! grep -q "$note" "$work/guard-off.log" || fail "заметка «владеет деплоер» напечатана при выключенном деплоере"

# (г) Ради чего всё и затевалось: ВТОРАЯ нода на боксе с живым агентом. Бинарь
# ОДИН на хост, поэтому прогон обязан пройти, ничего с ним не сделав. Локальной
# сборки при этом НЕТ ВОВСЕ — до гарда такой прогон падал бы ещё и на src.
put_box_bin
rm -f "$work/build/birdman-agent"
guard "$work/guard-secondnode.log" -e birdman_devdeploy_enabled=true \
  -e '{"birdman_agent_instances":[{},{"name":"khl","project":"khl-legends","capacity_slots":4,"port_range":[20100,20150],"metrics_port":9111}]}'
grep -q 'BINARY_GUARD changed=False skipped=True instances=2' "$work/guard-secondnode.log" \
  || fail "добавление второй ноды не прошло мимо бинаря (см. $work/guard-secondnode.log)"
[ "$(box_bin)" = "DEPLOYED-BY-DEVDEPLOY" ] \
  || fail "добавление второй ноды затёрло бинарь живого агента"
grep -q 'changed определён (False)' "$work/guard-secondnode.log" \
  || fail "assert про определённость changed не отработал — main.yml упал бы на наборе рестартов"
echo "ok: bring-up ставит, деплоер владеет — пропуск, вторая нода бинарь не трогает"

# ── 5b. Тот же гард БЕЗ ФЛАГА: add-node.yml по удалённой ноде (#1070) ────────
# В add-node.yml роли деплоера в плее нет, флаг не определён — и до #1070 гард
# в этом плее был инертен: прогон по удалённой ноде откатывал её агента на
# локальную сборку, хотя деплоер её катает (цели GET /v1/nodes?env=dev
# фильтруются по env, а не по боксу). Ниже флага НЕТ НИ В ОДНОМ прогоне —
# решение выводится из состояния мастер-бокса флота.
echo "── binary guard without the flag: add-node.yml against a remote node"
printf 'LOCAL-BUILD\n' >"$work/build/birdman-agent"

# (д) Деплоер стоит на МАСТЕРЕ флота, нода удалённая — бинарь ноды не трогаем.
# Это и есть дефект карточки: сегодня здесь была бы локальная сборка.
put_deployer "$master"
put_box_bin
guard "$work/guard-remote-owned.log"
grep -q 'BINARY_GUARD changed=False skipped=True instances=1' "$work/guard-remote-owned.log" \
  || fail "без флага гард не увидел деплоера на мастер-боксе — прогон по удалённой ноде откатил бы её агента"
[ "$(box_bin)" = "DEPLOYED-BY-DEVDEPLOY" ] \
  || fail "прогон по удалённой ноде затёр бинарь, которым владеет деплоер (#1070)"
grep -q "$note" "$work/guard-remote-owned.log" \
  || fail "в выводе нет заметки о владельце — пропуск неотличим от потери задачи"
grep -q "$master" "$work/guard-remote-owned.log" \
  || fail "заметка не называет бокс, у которого спросили — оператору негде смотреть"

# (е) Kill-switch на мастере: деплоер стоит, но выключен — владелец снова
# ansible. Отмычка из заметки гарда обязана РАБОТАТЬ, а не быть присказкой.
kill_switch "$master"
guard "$work/guard-killswitch.log"
grep -q 'BINARY_GUARD changed=True skipped=False instances=1' "$work/guard-killswitch.log" \
  || fail "kill-switch на мастере не вернул владение ansible — документированная отмычка не работает"
[ "$(box_bin)" = "LOCAL-BUILD" ] || fail "kill-switch: бинарь не обновился"
rm_kill_switch "$master"

# (ж) Деплоер стоит на САМОЙ НОДЕ, а мастер флота чист. Проба обязана спрашивать
# мастера, а не «хоть кого-нибудь»: иначе тест (д) проходил бы и на неверной
# реализации, которая смотрит под ноги.
rm_deployer "$master"
put_deployer "$box"
put_box_bin
guard "$work/guard-wrongbox.log"
grep -q 'BINARY_GUARD changed=True skipped=False instances=1' "$work/guard-wrongbox.log" \
  || fail "проба приняла деплоер НА НОДЕ за владельца — она обязана спрашивать мастер-бокс флота"
[ "$(box_bin)" = "LOCAL-BUILD" ] || fail "деплоер на неверном боксе остановил установку бинаря"
rm_deployer "$box"
echo "ok: без флага владение выводится из мастер-бокса флота (есть/kill-switch/не тот бокс)"

docker rm -f "$box" "$master" >/dev/null

# ── 6. VRL-трансформ шиппера — настоящим vector'ом ──────────────────────────
echo "vector: $image"
cp "$here/vector_test.yaml" "$work/one/"
docker run --rm -v "$work/one:/w" "$image" test /w/vector.yaml /w/vector_test.yaml

cp "$here/vector_test.yaml" "$here/vector_multinode_test.yaml" "$work/two/"
# Оба набора против ОДНОГО конфига двухнодового бокса: случаи #1014 обязаны
# остаться зелёными после того, как корень стал перечислением.
docker run --rm -v "$work/two:/w" "$image" \
  test /w/vector.yaml /w/vector_test.yaml /w/vector_multinode_test.yaml

echo "ALL OK"
