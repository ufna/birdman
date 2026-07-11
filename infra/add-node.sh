#!/usr/bin/env bash
# add-node.sh — добавить игровую ноду birdman «одной командой» (спека selfhost-v1).
# Пишет host-блок в git-ignored inventories/dev/hosts.local.yml и (без --dry-run)
# прогоняет playbooks/add-node.yml. Реальных IP/юзеров/ключей тут нет — примеры
# условные: 203.0.113.x (TEST-NET-3), ~/.ssh/id_ed25519.
#
# Оверлей (дефолт): overlay-IP выдаётся автоматически — первый свободный
#   10.77.0.X (X≥2) по hosts.local.yml; агент ходит на хаб 10.77.0.1.
# Self-host без оверлея: --no-overlay --master-addr <PUB_IP>:8444 → в блок
#   пишется birdman_use_overlay: false + прямой master_addr (строгий mTLS).
set -euo pipefail

die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Использование: ./add-node.sh <name> <ip> [опции]
  --port N          SSH-порт (дефолт 22)
  --user NAME       SSH-пользователь (дефолт root)
  --key PATH        приватный ключ (дефолт ~/.ssh/id_ed25519)
  --region R        регион/метка флота (дефолт nl)
  --slots N         сколько дедиков на ноду (дефолт 2, ≥1)
  --no-overlay      self-host без нашего оверлея; требует --master-addr
  --master-addr H:P публичный agentlink мастера (напр. 203.0.113.1:8444)
  --dry-run         только напечатать host-блок, ничего не менять
  -y, --yes         не спрашивать подтверждение записи
EOF
}

valid_ipv4() {
  local a="$1" o re='^([0-9]{1,3}\.){3}[0-9]{1,3}$'
  [[ "$a" =~ $re ]] || return 1
  local IFS=.
  for o in $a; do [ "$o" -le 255 ] || return 1; done
}

# ── дефолты ──────────────────────────────────────────────────────────────────
port=22; user=root; key='~/.ssh/id_ed25519'; region=nl; slots=2
overlay=1; master_addr=''; dry_run=0; assume_yes=0

# ── парсинг аргументов ───────────────────────────────────────────────────────
pos=()
while [ $# -gt 0 ]; do
  case "$1" in
    --port)         port="${2:?--port требует значение}"; shift 2;;
    --user)         user="${2:?--user требует значение}"; shift 2;;
    --key)          key="${2:?--key требует значение}"; shift 2;;
    --region)       region="${2:?--region требует значение}"; shift 2;;
    --slots)        slots="${2:?--slots требует значение}"; shift 2;;
    --master-addr)  master_addr="${2:?--master-addr требует значение}"; shift 2;;
    --no-overlay)   overlay=0; shift;;
    --dry-run)      dry_run=1; shift;;
    -y|--yes)       assume_yes=1; shift;;
    -h|--help)      usage; exit 0;;
    --) shift; while [ $# -gt 0 ]; do pos+=("$1"); shift; done;;
    -*) usage >&2; die "неизвестный флаг: $1";;
    *)  pos+=("$1"); shift;;
  esac
done
[ "${#pos[@]}" -ge 2 ] || { usage >&2; die "нужны <name> и <ip>"; }
[ "${#pos[@]}" -le 2 ] || die "лишние позиционные аргументы: ${pos[*]:2}"
name="${pos[0]}"; ip="${pos[1]}"

# ── валидации входа ──────────────────────────────────────────────────────────
name_re='^[a-zA-Z0-9][a-zA-Z0-9._-]*$'
[[ "$name" =~ $name_re ]] || die "неверное имя ноды: '$name' (латиница/цифры/.-_)"
valid_ipv4 "$ip" || die "неверный формат IP: '$ip' (пример: 203.0.113.7)"
[[ "$port" =~ ^[0-9]+$ ]] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] \
  || die "неверный SSH-порт: '$port'"
[[ "$slots" =~ ^[0-9]+$ ]] && [ "$slots" -ge 1 ] || die "слотов должно быть ≥1: '$slots'"
if [ "$overlay" -eq 0 ]; then
  [ -n "$master_addr" ] || die "--no-overlay требует --master-addr <PUB_IP>:8444"
  [[ "$master_addr" =~ ^[^:[:space:]]+:[0-9]+$ ]] || die "--master-addr в формате host:port (напр. 203.0.113.1:8444)"
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
hosts_file="$script_dir/inventories/dev/hosts.local.yml"
example_file="$script_dir/inventories/dev/hosts.example.yml"
[ -f "$hosts_file" ] || die "нет $hosts_file — скопируй шаблон: cp $example_file $hosts_file"

# ключ проверяем только для реального прогона (dry-run печатает блок как есть)
if [ "$dry_run" -eq 0 ]; then
  key_expanded="${key/#\~/$HOME}"
  [ -f "$key_expanded" ] || die "ключ не найден: $key (передай --key PATH)"
fi

# ── tmp (только для записи) ───────────────────────────────────────────────────
tmp_dir=''; tmp=''
cleanup() { [ -n "${tmp_dir:-}" ] && rm -rf "$tmp_dir"; return 0; }
trap cleanup EXIT
if [ "$dry_run" -eq 0 ]; then tmp_dir="$(mktemp -d)"; tmp="$tmp_dir/hosts.local.yml"; fi

# ── генерация блока + (для записи) текстовая вставка в конец группы birdman_nodes.
#    Чтение — через yaml (уникальность имени, свободный overlay-IP); вставка —
#    текстовая по строкам, чтобы сохранить комментарии/формат остального файла. ──
block="$(
  BN_NAME="$name" BN_IP="$ip" BN_PORT="$port" BN_USER="$user" BN_KEY="$key" \
  BN_REGION="$region" BN_SLOTS="$slots" BN_OVERLAY="$overlay" \
  BN_MASTER_ADDR="$master_addr" BN_DRY="$dry_run" BN_FILE="$hosts_file" BN_TMP="$tmp" \
  python3 - <<'PY'
import os, re, sys

def die(msg, code=1):
    sys.stderr.write("error: %s\n" % msg); sys.exit(code)

try:
    import yaml
except Exception:
    die("нужен python3 c модулем PyYAML")

E = os.environ
name, ip, port = E["BN_NAME"], E["BN_IP"], E["BN_PORT"]
user, key, region, slots = E["BN_USER"], E["BN_KEY"], E["BN_REGION"], E["BN_SLOTS"]
overlay = E["BN_OVERLAY"] == "1"
master_addr, dry = E["BN_MASTER_ADDR"], E["BN_DRY"] == "1"
path, tmp = E["BN_FILE"], E["BN_TMP"]

with open(path) as f:
    raw = f.read()

data = yaml.safe_load(raw) or {}
children = ((data.get("all") or {}).get("children")) or {}
names, used_ips = set(), set()
for gv in children.values():
    for hn, hv in (((gv or {}).get("hosts")) or {}).items():
        names.add(hn)
        oip = (hv or {}).get("birdman_overlay_ip")
        if oip:
            used_ips.add(str(oip))

if name in names:
    die("нода '%s' уже существует в hosts.local.yml" % name, 3)

overlay_ip = None
if overlay:
    overlay_ip = next(("10.77.0.%d" % x for x in range(2, 255)
                       if "10.77.0.%d" % x not in used_ips), None)
    if overlay_ip is None:
        die("нет свободного overlay-IP в 10.77.0.0/24", 4)

I = " " * 10
L = ["        %s:" % name, I + "ansible_host: %s" % ip]
if port != "22":
    L.append(I + "ansible_port: %s" % port)
L += [I + "ansible_user: %s" % user,
      I + "ansible_ssh_private_key_file: %s" % key,
      I + "birdman_region: %s" % region]
if overlay:
    L.append(I + "birdman_overlay_ip: %s" % overlay_ip)
L += [I + "birdman_capacity_slots: %s" % slots,
      I + "birdman_limits_default: { cpu_millis: 1000, mem_mb: 768 }"]
if overlay:
    L += [I + 'birdman_master_addr: "10.77.0.1:8444"',
          I + 'birdman_vl_sink_url: "http://10.77.0.1:9428/insert"']
else:
    host = master_addr.rsplit(":", 1)[0]
    L += [I + "birdman_use_overlay: false",
          I + 'birdman_master_addr: "%s"' % master_addr,
          I + 'birdman_vl_sink_url: "http://%s:9428/insert"' % host]
L += [I + "birdman_master_api_host: birdman-dev",
      I + "birdman_registry_legacy: false",
      I + "birdman_node_vmagent: true"]
block = "\n".join(L)
sys.stdout.write(block + "\n")

if dry:
    sys.exit(0)

# текстовая вставка: конец группы birdman_nodes = первая строка после её начала
# с отступом ≤4 (следующий ключ), иначе EOF; сдаём назад через хвостовые пустые.
lines = raw.splitlines(keepends=True)
if lines and not lines[-1].endswith("\n"):
    lines[-1] += "\n"
start = next((i for i, ln in enumerate(lines)
              if re.match(r"^ {4}birdman_nodes:\s*(#.*)?$", ln)), None)
if start is None:
    die("в hosts.local.yml нет группы birdman_nodes (сверься с hosts.example.yml)", 5)
end = len(lines)
for i in range(start + 1, len(lines)):
    if re.match(r"^ {0,4}\S", lines[i]):
        end = i; break
while end > start + 1 and lines[end - 1].strip() == "":
    end -= 1
merged = "".join(lines[:end]) + block + "\n" + "".join(lines[end:])
with open(tmp, "w") as f:
    f.write(merged)
PY
)" || exit

# ── dry-run: печать блока и выход ─────────────────────────────────────────────
if [ "$dry_run" -eq 1 ]; then
  printf '%s\n' "$block"
  exit 0
fi

# ── валидация результата инвентарём ДО записи ─────────────────────────────────
if ! ansible-inventory -i "$tmp" --list >/dev/null 2>"$tmp_dir/inv.err"; then
  sed 's/^/  /' "$tmp_dir/inv.err" >&2 || true
  die "ansible-inventory отверг результат вставки — файл не изменён"
fi
# членство, не только парсабельность: нода обязана оказаться хостом инвентаря
# (деградировавшая структура группы может молча посадить блок мимо hosts:)
ansible-inventory -i "$tmp" --host "$name" >/dev/null 2>&1 \
  || die "нода '$name' не видна инвентарю после вставки — файл не изменён (сверься с hosts.example.yml)"

printf '── diff (hosts.local.yml) ──\n'
diff -u "$hosts_file" "$tmp" || true

if [ "$assume_yes" -eq 0 ]; then
  printf 'Записать блок и прогнать add-node.yml? [y/N] '
  read -r ans || ans=''
  case "$ans" in [yYдД]*) ;; *) echo "отменено, файл не тронут."; exit 0;; esac
fi

cp "$tmp" "$hosts_file"
printf 'записано в %s\n' "$hosts_file"
printf 'запуск: ansible-playbook playbooks/add-node.yml\n'
( cd "$script_dir" && ansible-playbook playbooks/add-node.yml )
