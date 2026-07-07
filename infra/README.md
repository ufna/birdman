# infra — ansible-обвязка birdman

Итерации 0–1 (`docs/05-runtime-iterations.md`): дев-нода «master + агент под ним» одной командой. Ansible — целевой путь и для прода (структура — `docs/specs/ops.md` §4).

```
infra/
  ansible.cfg                 # inventory/roles_path — запускать из infra/
  inventories/dev/hosts.yml   # birdman-dev (HOSTER_A, ОБЩИЙ бокс с чужим продом)
  playbooks/dev-node.yml      # дев-нода: master (pg+бинарь+unit) → агент (демон)
  roles/birdman_master_dev/   # postgres в compose + master-бинарь под systemd
  roles/birdman_agent_dev/    # агент-демон + регистрация ноды (см. «Дев vs прод»)
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

## Дев vs прод

- **Дев-роли** рассчитаны на общий бокс: containerd/докер уже стоят и не трогаются, никакого hardening, наружу — только UDP/TCP-порты дедиков (master строго на localhost), UFW-правила только добавляются. Всё аддитивно и легко сносится.
- **Прод (позже, по `docs/specs/ops.md` §4)**: `inventories/production/hosts.yml` (группы master/nodes_*), роли `base` (sshd, nftables, sysctl), `containerd`, `node_exporter`/`vmagent`, `birdman_agent` (node_token из vault), `birdman_master` (реальные TLS-серты вместо `tls_insecure`), `postgres`, `victoria`; вход — `add-node.yml` («тачка одной командой»). Дев-роли в прод не переиспользуем.

## Секреты

- Все секреты живут только на тачке в файлах 0600: `pg.pass`, `master-admin.key`, `node.token`, `ghcr.token` (+ dsn внутри `master.yaml`). В репо, фактах и логах ansible их нет — таски с ними `no_log`.
- GHCR-токен — только env при запуске (`lookup('env', 'BIRDMAN_GHCR_TOKEN')`). Перед коммитами: `grep -rn "ghp[_]\|bmk[_]\|bnt[_]" --exclude-dir=.git .` → ничего, кроме фейковых фикстур в тестах.
- Прод-секреты — ansible-vault (ops.md §4), появятся с прод-ролями.
