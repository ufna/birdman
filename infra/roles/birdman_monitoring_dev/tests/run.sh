#!/usr/bin/env bash
# Unit tests for this role — LOCAL only, no host is touched and no playbook
# runs: the templates are rendered with the role defaults into a temp dir, the
# vmalert rules are evaluated against synthetic series, and the config mounts of
# the rendered compose file are checked for being readable by their containers.
#
#   ./infra/roles/birdman_monitoring_dev/tests/run.sh
#
# Engine: `promtool test rules` (Prometheus). The rules run under vmalert in
# production, and MetricsQL is a superset of PromQL — every expression in
# rules.yml.j2 is plain PromQL, and the semantics these tests pin down
# (an aggregation over an ABSENT series yields nothing, not 0; increase() over
# a series that never rose yields 0) are identical in both engines. A local
# promtool is used when present, otherwise the pinned container image; nothing
# is installed on the host either way (containerization rule).
#
# Why these tests exist: BufferEmptyReadyProd/NonProd were DEAD — the ready
# series simply disappears when ready hits 0, so `== 0` never held and the case
# only looked covered (tracker #960).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
role="$(dirname "$here")"
repo="$(cd "$role/../../.." && pwd)"
image="${BIRDMAN_PROMTOOL_IMAGE:-prom/prometheus:v3.10.0}"
# Образ sink'а берётся из ПИНА РОЛИ, а не пишется тут ещё раз: второй пин рядом
# с первым — это будущее расхождение, о котором никто не узнает.
sink_image="$(sed -n 's/^birdman_alert_sink_image:[[:space:]]*//p' "$role/defaults/main.yml")"
[ -n "$sink_image" ] || { echo "не нашёл birdman_alert_sink_image в defaults" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

render() { # render <out-dir> [extra-vars-json]
	mkdir -p "$1"
	ansible-playbook -i birdman-dev, "$here/render_rules.yml" \
		-e "birdman_out=$1" ${2:+-e "$2"} >"$1/ansible.log" 2>&1 \
		|| { cat "$1/ansible.log" >&2; return 1; }
}

check_and_test() { # check_and_test <dir> <test-file>
	if command -v promtool >/dev/null 2>&1; then
		promtool check rules "$1/rules.yml"
		promtool test rules "$1/$2"
	else
		docker run --rm -v "$1:/w" -w /w --entrypoint /bin/promtool "$image" check rules rules.yml
		docker run --rm -v "$1:/w" -w /w --entrypoint /bin/promtool "$image" test rules "$2"
	fi
}

if command -v promtool >/dev/null 2>&1; then
	echo "promtool: $(command -v promtool)"
else
	echo "promtool: none on PATH, using $image"
fi

# ── бокс с одной нодой birdman: дефолтная раскладка дев-стенда ───────────────
echo "── render: box with one birdman node"
render "$work/one"
cp "$here/rules_test.yml" "$work/one/rules_test.yml"
check_and_test "$work/one" rules_test.yml

# ── бокс с ДВУМЯ нодами (tracker #1065) ─────────────────────────────────────
# Джоб второй ноды обязан приехать и в scrape.yml, и в цепочку
# ScrapeTargetMissing — из одного и того же списка. Забытый джоб здесь означал
# бы, что вторая нода не скрейпится, а её пропажу никто не сторожит: молчание,
# неотличимое от здоровья.
echo "── render: box with two birdman nodes"
render "$work/two" '{"birdman_box_instances":[{},{"name":"khl","project":"khl-legends","metrics_port":9111}]}'
grep -q 'job_name: birdman-agent-khl' "$work/two/scrape.yml" \
	|| fail "джоб второй ноды не попал в scrape.yml"
grep -q '"127.0.0.1:9111"' "$work/two/scrape.yml" \
	|| fail "джоб второй ноды указывает не на её порт /metrics"
grep -q 'absent_over_time(up{job="birdman-agent-khl"}' "$work/two/rules.yml" \
	|| fail "ScrapeTargetMissing не сторожит джоб второй ноды — пропажа её таргета осталась бы невидимой"
grep -q 'absent_over_time(up{job="birdman-agent"}' "$work/two/rules.yml" \
	|| fail "ScrapeTargetMissing потерял джоб первой ноды"
cp "$here/rules_multinode_test.yml" "$work/two/"
check_and_test "$work/two" rules_multinode_test.yml

# ── конфиг обязан читаться контейнером, в который смонтирован (tracker #1072) ─
# Роль клала alertmanager.yml 0600 root:root в контейнер, который бежит от
# nobody: он падал на «permission denied» и стоял в краш-лупе с рождения, а
# роль отрабатывала зелёно. Проверять это глазами больше не надо.
#
# Сам сторож с #1089 общий (infra/ci/) и зовётся из раннера КАЖДОЙ роли,
# кладущей монтируемые конфиги: критерий один на репозиторий, а рендер —
# роле-локальный, настоящим ansible'ом выше по этому же файлу.
echo "── mounted config access"
"$repo/infra/ci/mounted-config-access.sh" --role "$role" "$work/one/compose.yml"

# ── ротация alerts.log настоящим logrotate (tracker #1073) ───────────────────
# У файла не было потолка вовсе: 498 МБ за два часа. Политику гоняет НАСТОЯЩИЙ
# logrotate — конфиг с опечаткой не ротирует молча. Всё внутри одноразового
# контейнера: хост не трогается, портов наружу нет (sink слушает только внутри),
# имя контейнеру не даётся вовсе — конфликтовать нечему.
echo "── alerts.log rotation"
docker run --rm \
	-v "$work/one/logrotate-alerts:/conf/logrotate-alerts:ro" \
	-v "$role/files/alert-logger.py:/conf/alert-logger.py:ro" \
	-v "$here/rotation_in_container.sh:/conf/rotation.sh:ro" \
	--entrypoint /bin/sh "$sink_image" /conf/rotation.sh

echo "ALL OK"
