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
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
role="$(dirname "$here")"
image="${BIRDMAN_VECTOR_IMAGE:-$(sed -n 's/^birdman_vector_image: *//p' "$role/defaults/main.yml")}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

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

# ── 5. VRL-трансформ шиппера — настоящим vector'ом ──────────────────────────
echo "vector: $image"
cp "$here/vector_test.yaml" "$work/one/"
docker run --rm -v "$work/one:/w" "$image" test /w/vector.yaml /w/vector_test.yaml

cp "$here/vector_test.yaml" "$here/vector_multinode_test.yaml" "$work/two/"
# Оба набора против ОДНОГО конфига двухнодового бокса: случаи #1014 обязаны
# остаться зелёными после того, как корень стал перечислением.
docker run --rm -v "$work/two:/w" "$image" \
  test /w/vector.yaml /w/vector_test.yaml /w/vector_multinode_test.yaml

echo "ALL OK"
