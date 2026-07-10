#!/bin/bash
# Entrypoint оверлей-контейнера birdman (спека iter5 §1). Порядок жёсткий:
# wireguard-go (userspace tun в хостовом netns) → setconf/ключ → адрес/up →
# (хаб) socat-форвардеры → супервизор: смерть ЛЮБОГО процесса валит
# контейнер (exit ≠ 0: статус потомка через set -e, либо финальный exit 1),
# restart-policy compose поднимает всё заново.
#
# wg-quick НЕ используется: он сначала пробует kernel-модуль (ip link add
# type wireguard) и на хосте с загруженным модулем молча ушёл бы в
# kernel-путь — нарушение решения владельца №1 (только userspace) и потеря
# свойства «умер процесс — умер интерфейс». WG_I_PREFER_BUGGY_USERSPACE_
# TO_POLISHED_KMOD=1 удерживает wireguard-go в userspace и при наличии
# kernel-поддержки (проверено эмпирически, баннер косметический).
#
# Форвардеры слушают ТОЛЬКО overlay-IP и гоняют байты в 127.0.0.1 — TLS
# НЕ терминируют (mTLS agentlink остаётся end-to-end агент↔master).
set -eu

IFACE="${BIRDMAN_WG_IFACE:?BIRDMAN_WG_IFACE is required}"
ADDR="${BIRDMAN_WG_ADDR:?BIRDMAN_WG_ADDR is required}"   # напр. 10.77.0.1/24
MODE="${BIRDMAN_WG_MODE:?BIRDMAN_WG_MODE is required}"   # hub|spoke
FORWARDS="${BIRDMAN_WG_FORWARDS:-}"                      # "8444 9428 8428" (хаб)
CONF_DIR=/etc/birdman/overlay

PIDS=()
export WG_I_PREFER_BUGGY_USERSPACE_TO_POLISHED_KMOD=1

LOG_LEVEL=error wireguard-go -f "$IFACE" &
PIDS+=($!)

# ждём control-сокет wireguard-go (≤5с)
for _ in $(seq 1 50); do
  [ -S "/var/run/wireguard/${IFACE}.sock" ] && break
  sleep 0.1
done
[ -S "/var/run/wireguard/${IFACE}.sock" ] || { echo "wireguard-go socket missing" >&2; exit 1; }

# конфиг без секретов (peers/port); приватный ключ — отдельным файлом 0600
wg setconf "$IFACE" "$CONF_DIR/wg.conf"
wg set "$IFACE" private-key "$CONF_DIR/wg.key"
ip address replace "$ADDR" dev "$IFACE"
ip link set up dev "$IFACE"

if [ "$MODE" = "hub" ]; then
  BIND_IP="${ADDR%/*}"
  for port in $FORWARDS; do
    socat "TCP-LISTEN:${port},bind=${BIND_IP},fork,reuseaddr" "TCP:127.0.0.1:${port}" &
    PIDS+=($!)
  done
fi

echo "birdman-overlay up: iface=$IFACE addr=$ADDR mode=$MODE forwards=[$FORWARDS]"

trap 'kill "${PIDS[@]}" 2>/dev/null || true; exit 0' TERM INT
wait -n "${PIDS[@]}"
exit 1
