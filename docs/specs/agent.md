# Спека: agent

> Один статический Go-бинарь `birdman-agent` на каждой тачке. Исполняет команды master, супервизирует дедики через containerd, мониторит тачку. Итерации 0–1, 3–4 из `../05-runtime-iterations.md`.

## 1. Обязанности / не-обязанности

**Делает:** pull/start/stop контейнеров (containerd), пул host-портов, супервизия и health дедиков, мост liba↔master, логи дедиков, метрики тачки и контейнеров, image GC, self-upgrade, UDP-echo для QoS.

**Не делает:** не решает *что* и *сколько* запускать (это reconcile master'а); не хранит durable-состояние (всё восстановимо из containerd-labels + master); не ходит в Postgres.

## 2. Состояние и рестарт

Агент — stateless поверх containerd. На старте: `containerd list` по namespace `birdman` → восстановить карту server_id→container (labels: `server_id`, `version`, `port`) → доложить master в Hello. Контейнеры agent-рестарт **переживают** (containerd их держит) — рестарт/апгрейд агента не трогает живые матчи.

## 3. Запуск дедика

Команда `StartServer{server_id, image_ref, env, limits{cpu_millis, mem_mb}, port?}`:

1. Ensure image (обычно уже прогрет PrePull'ом; иначе pull с прогрессом в Event).
2. Выделить порт из пула (конфиг, деф. `20000–29999`); один порт TCP+UDP на дедик (v1).
3. Создать per-server unix socket `/run/birdman/servers/{server_id}.sock` (слушает агент).
4. `containerd create+start`: host network; bind mounts: сокет → `/birdman/agent.sock` (ro-каталог, rw-сокет); env: `BIRDMAN_SOCKET=/birdman/agent.sock`, `BIRDMAN_PORT`, `BIRDMAN_SERVER_ID`, `BIRDMAN_REGION` + env из команды; cgroups-лимиты из `limits`; `oom_score_adj` дедикам > агенту.
5. Grace-период readiness: **30с** на `ready` от liba, иначе `failed` + stop.

Стейт-машина на агенте: `pulling → starting → ready → allocated → draining → stopped|failed`. Переходы `ready/allocated/draining` диктует master (allocated) и liba (ready, match_end); `failed` — exit-код ≠0, отсутствие `ready`, OOM.

`StopServer{server_id, grace_s}`: SIGTERM → grace (деф. 30с) → SIGKILL → delete container, освободить порт и сокет.

## 4. Health и heartbeat

- liba-heartbeat: `players`, `tick_ms`, match-события через UDS (см. `protocol.md`); тишина от liba >15с при state=allocated → пометить `unhealthy` в heartbeat (master решает).
- Процесс: exit-watch через containerd events; OOM-kill детектится и помечается в Event (панель покажет причину).
- Агрегированный heartbeat агента master'у — каждые **2с**: stats тачки (cpu, mem, disk, net, load) + список дедиков (id, state, players, tick_ms).

## 5. Логи дедиков

- stdout/stderr контейнера → `/var/log/birdman/servers/{server_id}.log`; ротация 100MB × 2 файла на дедик; после stop — gzip; ретенция **7 дней** (конфиг).
- `TailLogs{server_id, follow}` от master → стрим строк (для панели/CLI `GET /v1/servers/{id}/logs`).
- Опционально (ит. 4+): vector на тачке шипует в центральное хранилище; v1 — логи живут на тачке, доступ через master-proxy.

## 6. Image GC и диск

- Watermark: диск >80% → удалить неиспользуемые образы (LRU), кроме версий в состоянии `active`/`prepulling`/`deprecated`.
- Диск >90% → отказ StartServer + событие `disk_full` (алерт).

## 7. Self-upgrade

`UpgradeAgent{url, sha256, version}`: скачать во временный файл → проверить sha256 → `rename` поверх бинаря → systemd restart (контейнеры переживают, §2). Если после апгрейда агент не вышел на связь за 60с — master поднимает алерт `agent_upgrade_failed` (руками: ansible откат).

## 8. QoS-echo

UDP-echo на порту **19999**: отвечает исходным пакетом (≤64б). Клиенты меряют rtt до регионов (список отдаёт master `/v1/qos`).

## 9. Метрики

`localhost:9101/metrics` (Prometheus text): `birdman_agent_up`, `birdman_agent_servers{state}`, `birdman_server_players{server_id}`, `birdman_server_tick_ms`, per-container cpu/mem (из cgroups), диск/inode, длина пула портов. Скрейпит vmagent той же тачки (см. `ops.md`).

## 10. Конфиг `/etc/birdman/agent.yaml`

```yaml
master_addr: "master.birdman.internal:8443"
node_token: "…"            # bootstrap-токен, выдаётся при добавлении тачки (ansible)
region: "eu"
capacity_slots: 24          # обычно = физические ядра - резерв
port_range: [20000, 29999]
limits_default: { cpu_millis: 3500, mem_mb: 4096 }
log_dir: /var/log/birdman
data_dir: /var/lib/birdman
registry_auth:              # (уточнено в v0) pull приватного registry (GHCR)
  username: "ufna"
  token_file: /etc/birdman/ghcr.token   # токен только в файле — никогда в конфиге/коде
```

TLS: при первом коннекте агент обменивает `node_token` на клиентский сертификат (mTLS дальше) — см. `protocol.md` §Auth.

## 11. Acceptance

- **Ит. 0**: `birdman-agent run-once --image ghcr.io/...` поднимает дедик на голой тачке; игрок коннектится по host:port; логи пишутся; ansible-плейбук ставит containerd+агента одной командой.
- **Ит. 1**: агент под master'ом: Start/Stop по командам, heartbeat 2с, восстановление карты после своего рестарта, failed при отсутствии ready за 30с.
- **Ит. 3**: PrePull всех тачек с прогрессом; при deploy старые дедики доигрывают.
- **Ит. 4**: tail логов через master; self-upgrade drain-aware; метрики в vmagent; UDP-echo отвечает.
