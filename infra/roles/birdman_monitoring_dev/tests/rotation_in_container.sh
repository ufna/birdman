#!/bin/sh
# Ротация alerts.log — НАСТОЯЩАЯ, внутри одноразового контейнера (tracker #1073).
# Запускается не с хоста, а ВНУТРИ образа sink'а (см. tests/run.sh):
#   /conf/logrotate-alerts   — отрендеренная политика роли
#   /conf/alert-logger.py    — сам sink роли
#
# Что здесь проверяется и почему именно это:
#  1. конфиг ВООБЩЕ исполняется настоящим logrotate — опечатка в политике не
#     ротирует молча, а «прочитал глазами» уже стоило 498 МБ;
#  2. файл МЕНЬШЕ порога не ротируется — иначе порог был бы декорацией, а
#     история панели (она же текущий файл) стриглась бы на ровном месте;
#  3. файл БОЛЬШЕ порога ротируется, старое содержимое уезжает в .1.gz;
#  4. ГЛАВНОЕ: следующий алерт ложится в НОВЫЙ файл. На этом свойстве держится
#     весь выбор «rename+create без copytruncate»: заведись у sink'а
#     долгоживущий fd, ротация начала бы писать в удалённый inode, то есть
#     терять КАЖДЫЙ алерт — молча и с зелёной ротацией.
set -eu

LOG=/var/log/birdman/alerts.log
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

apk add --no-cache logrotate >/dev/null 2>&1 || fail "не поставился logrotate"

# logrotate игнорирует конфиг с чужим владельцем, а bind-маунт приезжает с
# uid'ом хостового пользователя — кладём копию как root.
mkdir -p /etc/logrotate.d /var/log/birdman
cp /conf/logrotate-alerts /etc/logrotate.d/birdman-alerts
chown root:root /etc/logrotate.d/birdman-alerts
chmod 0644 /etc/logrotate.d/birdman-alerts

BIRDMAN_ALERTS_LOG="$LOG" python3 /conf/alert-logger.py &
sink=$!
trap 'kill "$sink" 2>/dev/null || true' EXIT

post() { # post <alertname>
	python3 - "$1" <<-'PY'
		import json, sys, time, urllib.error, urllib.request
		body = json.dumps({"alerts": [{"labels": {"alertname": sys.argv[1]}}]}).encode()
		for _ in range(50):  # sink поднимается за миллисекунды, но не мгновенно
		    try:
		        urllib.request.urlopen("http://127.0.0.1:9094/alert", data=body, timeout=2)
		        sys.exit(0)
		    except urllib.error.URLError:
		        time.sleep(0.1)
		sys.exit("sink не ответил")
	PY
}

rotate() { logrotate -s /tmp/logrotate.state /etc/logrotate.d/birdman-alerts; }

# ── 1. sink пишет, файл появился ────────────────────────────────────────────
post BeforeRotation
grep -q BeforeRotation "$LOG" || fail "sink не записал алерт в $LOG"

# ── 2. МЕНЬШЕ порога — не ротируем ──────────────────────────────────────────
rotate
[ ! -e "$LOG.1" ] && [ ! -e "$LOG.1.gz" ] \
	|| fail "ротировал файл, не доросший до порога — порог декоративный"

# ── 3. БОЛЬШЕ порога — ротируем. Порог читается из САМОЙ политики: тест не
#      обязан знать дефолт роли и не поедет, когда его поменяют. ─────────────
python3 - "$LOG" <<-'PY'
	import os, re, sys
	cfg = open("/etc/logrotate.d/birdman-alerts").read()
	m = re.search(r"^\s*size\s+(\d+)\s*([kKmMgG]?)", cfg, re.M)
	if not m:
	    sys.exit("в политике нет директивы size — ротация по размеру не задана")
	mult = {"": 1, "k": 1024, "m": 1024 ** 2, "g": 1024 ** 3}[m.group(2).lower()]
	# +1 МиБ: logrotate крутит то, что БОЛЬШЕ порога. Дырявый файл — размер
	# честный (logrotate смотрит st_size), а диск теста не трогается.
	os.truncate(sys.argv[1], int(m.group(1)) * mult + 1024 * 1024)
PY
rotate
[ -e "$LOG.1.gz" ] || fail "файл выше порога не ротирован (или не сжат)"
zcat "$LOG.1.gz" | grep -q BeforeRotation \
	|| fail "старая история не уехала в архив"

# ── 4. sink продолжает писать в НОВЫЙ файл ──────────────────────────────────
post AfterRotation
grep -q AfterRotation "$LOG" \
	|| fail "после ротации алерт не попал в $LOG — sink держит fd, ротация теряет алерты"
# Не `grep && fail`: под set -e несработавший grep уронил бы скрипт молча.
if grep -q BeforeRotation "$LOG"; then
	fail "новый $LOG содержит доротационную запись — create не сработал"
fi

mode="$(stat -c %a "$LOG")"
[ "$mode" = "644" ] || fail "новый $LOG создан с режимом $mode, ожидался 644"

echo "ротация: ok (порог соблюдён, архив создан, sink пишет в новый файл)"
