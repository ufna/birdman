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
#   3. #1069 — бинарём агента на стенде владеет pull-деплоер, а не роль: прогон
#      плейбука по живому боксу обязан бинарь НЕ ТРОГАТЬ (иначе откат агента на
#      локальную сборку + расхождение строки версии с тем, что мастер запросил
#      в POST /v1/agent-upgrade, то есть ложные agent_upgrade_failed).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
role="$(dirname "$here")"
image="${BIRDMAN_VECTOR_IMAGE:-$(sed -n 's/^birdman_vector_image: *//p' "$role/defaults/main.yml")}"
# Контейнер-«бокс» для гарда бинаря (#1069): имя от id задачи, за собой убираем.
box_image="${BIRDMAN_TEST_BOX_IMAGE:-python:3-alpine}"
box=bm1069-box

work="$(mktemp -d)"
cleanup() { rm -rf "$work"; docker rm -f "$box" >/dev/null 2>&1 || true; }
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
# Эхо QoS — наоборот, ОБЩЕЕ на хост, и владелец у него ровно один.
owners = [i["node_name"] for i in inst if i["qos_echo"]]
if len(owners) != 1:
    sys.exit("FAIL: владельцев QoS-эха %d, ожидался ровно один: %r" % (len(owners), owners))
print("ok: две ноды не делят ни одного из %d ресурсов; эхо держит %s" % (10, owners[0]))
PY

grep -q 'qos_echo_addr: "off"' "$work/two/agent.yaml--khl" \
  || fail "вторая нода не отключила UDP-эхо — два респондера на одном хосте гоняются за одним портом"
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

# ── 5. Гард бинаря агента (#1069): владелец — pull-деплоер ───────────────────
# Играется настоящий tasks/binary.yml роли против КОНТЕЙНЕРА-«бокса»: copy в
# роли ставит owner/group root, поэтому в temp-каталоге под обычным
# пользователем прогон упал бы на chown, а --check показал бы решение движка,
# но не то, что байты на боксе остались прежними. Содержимое сверяем отдельным
# каналом (docker exec), а не выводом того же прогона.
echo "── binary guard: the pull deployer owns /usr/local/bin/birdman-agent"
ansible-galaxy collection list community.docker >/dev/null 2>&1 \
  || fail "нужна коллекция community.docker (connection-плагин docker): ansible-galaxy collection install community.docker"

docker rm -f "$box" >/dev/null 2>&1 || true
docker run -d --name "$box" "$box_image" sleep 900 >/dev/null

mkdir -p "$work/build"
printf 'LOCAL-BUILD\n' >"$work/build/birdman-agent"

guard() { # guard <лог> [доп. -e ansible'у...]
  local log="$1"; shift
  ansible-playbook -i "$box," "$here/binary_guard.yml" \
    -e "birdman_agent_binary=$work/build/birdman-agent" "$@" >"$log" 2>&1 \
    || { cat "$log" >&2; fail "прогон гарда упал (см. вывод выше)"; }
}
box_bin() { docker exec "$box" cat /usr/local/bin/birdman-agent 2>/dev/null; }
put_box_bin() { docker exec "$box" sh -c 'printf "DEPLOYED-BY-DEVDEPLOY\n" >/usr/local/bin/birdman-agent && chmod 0755 /usr/local/bin/birdman-agent'; }
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
# деплоера»: флаг не задан вовсе (ветка default(false), как в add-node.yml) и
# явный false.
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

docker rm -f "$box" >/dev/null

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
