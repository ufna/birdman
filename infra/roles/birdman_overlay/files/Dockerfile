# Оверлей birdman: userspace WireGuard + socat-форвардеры control-plane в
# одном контейнере (host-network, NET_ADMIN, /dev/net/tun). Kernel-модуль
# wireguard НЕ используется намеренно — изоляция от чужого меша на общем
# боксе + жизнь tun-интерфейса привязана к процессу wireguard-go (умер
# контейнер — исчез интерфейс). Образ собирается на боксе (docker build,
# тег birdman-overlay:local), внешних registry в цепочке нет.
FROM alpine:3.22
RUN apk add --no-cache wireguard-go wireguard-tools-wg socat iproute2 bash
COPY entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
