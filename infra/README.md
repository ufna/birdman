# infra — ansible-обвязка birdman

Итерации 0–1 (`docs/05-runtime-iterations.md`): дев-нода «master + агент под ним» одной командой. Ansible — целевой путь и для прода (структура — `docs/specs/ops.md` §4).

```
infra/
  ansible.cfg                 # inventory/roles_path — запускать из infra/
  inventories/dev/hosts.yml   # birdman-dev (HOSTER_A, ОБЩИЙ бокс с чужим продом)
  playbooks/dev-node.yml      # дев-нода: master (pg+бинарь+unit) → агент (демон)
  playbooks/monitoring.yml    # наблюдаемость + ops-бэкапы (итерация 4)
  playbooks/add-node.yml      # вторая+ нода: оверлей (хаб+спок) → агент (итерация 5)
  roles/birdman_master_dev/   # postgres в compose + master-бинарь под systemd
  roles/birdman_agent_dev/    # агент-демон + регистрация ноды (см. «Дев vs прод»)
  roles/birdman_monitoring_dev/  # VM+vmagent+vmalert+Grafana+alert-sink (compose) + pg-бэкапы
  roles/birdman_overlay/      # изолированный WireGuard-оверлей birdman (wireguard-go в контейнере)
```

## Запуск

```bash
# 1) собрать бинари (docker, Go на хосте не нужен)
(cd ../agent && ./build.sh)           # → agent/dist/birdman-agent
(cd ../master && ./build.sh)          # → master/dist/birdman-master

# 2) экспортировать GHCR-токен (classic PAT, скоуп read:packages).
#    Токен живёт ТОЛЬКО в env и в /etc/birdman/ghcr.token (0600 root) на тачке.
#    Никогда: в репо, в логах, в vars/host_vars.
export BIRDMAN_GHCR_TOKEN='<токен>'   # пробел перед командой = мимо shell history (zsh/bash с HIST_IGNORE_SPACE)

# 3) применить (из каталога infra/)
ansible-playbook playbooks/dev-node.yml
```

Плейбук идемпотентен (повторный прогон — `changed=0`); обновление бинаря/конфига = пересборка + повторный запуск (рестарты сервисов — по хэндлерам).

## Что делает `dev-node.yml`

Роль `birdman_master_dev` (первая — агент регистрируется в живом master):

| Объект | Что |
|---|---|
| `/opt/birdman/compose.yml` (0600) | postgres:16, порт ТОЛЬКО `127.0.0.1:5433` (5432 занят чужим продом), volume `birdman-pgdata` |
| `/etc/birdman/pg.pass` (0600) | пароль Postgres, генерируется при первом прогоне, живёт только на тачке |
| `/usr/local/bin/birdman-master` (0755) | статический бинарь из `master/dist/` — **выбранный путь деплоя**: проще сборки образа на тачке (не нужен исходник репо), логи/рестарт — штатный systemd; миграции master применяет сам при старте |
| `/etc/birdman/master.yaml` (0600) | dsn с паролем; listen ТОЛЬКО `127.0.0.1:8100` (REST) и `127.0.0.1:8444` (gRPC) — наружу не торчит, self-signed TLS в `/var/lib/birdman-master/certs` |
| `/etc/systemd/system/birdman-master.service` | enabled + started, `Restart=always` |
| `/etc/birdman/master-admin.key` (0600) | bootstrap admin-ключ, выхваченный из журнала первого старта (печатается один раз); нужен последующим задачам |

Роль `birdman_agent_dev`:

| Объект | Что |
|---|---|
| `/etc/birdman` (0755), `/var/lib/birdman`, `/var/log/birdman/servers` | каталоги агента |
| `/etc/tmpfiles.d/birdman.conf` | `/run/birdman/servers` через tmpfiles.d — переживает ребут |
| `/usr/local/bin/birdman-agent` (0755) | бинарь из `agent/dist/` |
| `/etc/birdman/agent.yaml` | region `dev`, слоты 8, порты `[20000, 20050]`, лимиты 2000m/1024MB, `master_addr 127.0.0.1:8444`, `node_token_file`, `tls_insecure: true` (dev) |
| `/etc/birdman/ghcr.token` (0600 root) | из env `BIRDMAN_GHCR_TOKEN` |
| `/etc/birdman/node.token` (0600) | одноразовая регистрация: `POST /v1/nodes` admin-ключом при первом прогоне → node_token только на тачке |
| UFW `20000:20050` tcp+udp, comment `birdman-dev` | аддитивно; чужие правила не трогаются |
| `/etc/systemd/system/birdman-agent.service` | `birdman-agent run`, enabled + started |

Проверка после прогона: `systemctl is-active birdman-master birdman-agent`, `curl -s localhost:8100/healthz` (на тачке), нода в `GET /v1/nodes` — `active` со свежим heartbeat.

Дальше жизнью дедиков управляет только master/agent-цикл (версии/флоты — через REST API master, см. `master/README.md`): ansible дедики не деплоит.

## Наблюдаемость + ops (`monitoring.yml`)

Итерация 4 (`docs/specs/ops.md` §1, §5). Запускать **после** `dev-node.yml`
(нужны живой `birdman-postgres` для бэкапов и цели скрейпа agent/master):

```bash
ansible-playbook playbooks/monitoring.yml
```

Роль `birdman_monitoring_dev` — отдельный compose-проект `birdman-monitoring`,
всё строго на `127.0.0.1` (наружу — только SSH-туннель):

| Сервис | Порт (127.0.0.1) | Что |
|---|---|---|
| VictoriaMetrics | 8428 | TSDB single-node, retention 30d, volume `birdman-vmdata` |
| vmagent | 8429 | скрейп 15s (host-network): agent :9101, master :8100, чужой node_exporter :4027 (read-only) → remote-write в VM |
| vmalert | 8880 | правила `ops.md §1`; query→VM, notify→sink/alertmanager |
| Grafana | 3000 | datasource VM + 2 дашборда provisioning'ом («Тачка», «birdman»); admin-пароль в `/etc/birdman/grafana_admin_password` (0600), volume `birdman-grafana` |
| alert-sink **или** alertmanager | 9094 / 9093 | по умолчанию logger-sink пишет `/var/log/birdman/alerts.log`; при `-e birdman_alert_discord_webhook=…` — реальный alertmanager с Discord |

Конфиги шаблонятся в `/etc/birdman/monitoring/…` и монтируются ro в контейнеры.

Бэкапы Postgres: `birdman-pg-backup.timer` (каждые 6ч) → `pg_dump -Fc` в
`/var/lib/birdman/backups` (держим 14 свежих). Учебный restore:
`/usr/local/bin/birdman-pg-restore-test` (поднимает throwaway postgres:16,
восстанавливает последний дамп, гоняет sanity-запрос, PASS/FAIL, сносит).

UFW `19999/udp` (QoS echo) открывает роль `birdman_agent_dev` (единственный
внешне-открытый порт ноды), при прогоне `dev-node.yml`.

## Вторая+ нода (`add-node.yml`, итерация 5)

Спека: `docs/superpowers/specs/2026-07-10-iter5-second-node-design.md`. Нода
подключается к master по **собственному изолированному оверлею birdman**
(WireGuard hub-and-spoke `10.77.0.0/24`, UDP 51827): userspace `wireguard-go`
в контейнере `birdman-overlay` (host-network, NET_ADMIN, /dev/net/tun; образ
собирается на боксе, kernel-модуль не используется). Хаб (master-бокс) несёт
socat-форвардеры `10.77.0.1:{8444,9428,8428} → 127.0.0.1` — master/VL/VM не
меняют ни бинда, ни конфига. По оверлею — только control-plane (agentlink,
логи, метрики); игровой UDP и QoS — напрямую на публичный IP ноды.

```sh
(cd ../agent && ./build.sh)              # бинарь агента для новой ноды
ansible-playbook playbooks/add-node.yml  # идемпотентно; хаб + все ноды группы
```

Добавить следующую ноду = host-блок в `inventories/dev/hosts.yml` (регион,
`birdman_overlay_ip` из 10.77.0.0/24, `birdman_master_api_host: birdman-dev`,
`birdman_registry_legacy: false`) + прогон. Регистрация ноды (`POST
/v1/nodes`, `GET /v1/ca`) выполняется `delegate_to` master-бокса — admin-ключ
его не покидает; node_token/CA приезжают на ноду copy-тасками. Агент ноды
набирает `10.77.0.1:8444` (не-loopback ⇒ конфиг-гейт агента требует mTLS) и
заходит Enroll-by-token с первого коннекта.

⚠️ Роль агента — та же дев-роль `birdman_agent_dev`, генерализованная на
удалённые ноды (не прод-суита ops.md §4: без hardening/vault/node_exporter).
⚠️ Инвариант: пока на хабе живёт форвардер — `agentlink_auth: mtls` only.
Снос оверлея: `docker compose -f /opt/birdman/overlay/compose.yml down` +
удалить `/etc/birdman/overlay`, `/opt/birdman/overlay` + 4 UFW-правила
`birdman-dev` (51827/udp и `in on birdman-wg0`).

## Дев vs прод

- **Дев-роли** рассчитаны на общий бокс: containerd/докер уже стоят и не трогаются, никакого hardening, наружу — только UDP/TCP-порты дедиков (master строго на localhost), UFW-правила только добавляются. Всё аддитивно и легко сносится. Роль агента генерализована на удалённые ноды (итерация 5, add-node.yml) — но остаётся дев-ролью.
- **Прод (позже, по `docs/specs/ops.md` §4)**: `inventories/production/hosts.yml` (группы master/nodes_*), роли `base` (sshd, nftables, sysctl), `containerd`, `node_exporter`/`vmagent`, `birdman_agent` (node_token из vault), `birdman_master` (реальные TLS-серты вместо `tls_insecure`), `postgres`, `victoria`; вход — `add-node.yml` («тачка одной командой»). Дев-роли в прод не переиспользуем.

## Секреты

- Все секреты живут только на тачке в файлах 0600: `pg.pass`, `master-admin.key`, `node.token`, `ghcr.token` (+ dsn внутри `master.yaml`). В репо, фактах и логах ansible их нет — таски с ними `no_log`.
- GHCR-токен — только env при запуске (`lookup('env', 'BIRDMAN_GHCR_TOKEN')`). Перед коммитами: `grep -rn "ghp[_]\|bmk[_]\|bnt[_]" --exclude-dir=.git .` → ничего, кроме фейковых фикстур в тестах.
- Прод-секреты — ansible-vault (ops.md §4), появятся с прод-ролями.
