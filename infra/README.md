# infra — ansible-обвязка birdman

Итерация 0 (`docs/05-runtime-iterations.md`): дев-нода с агентом одной командой. Ansible — целевой путь и для прода (структура — `docs/specs/ops.md` §4).

```
infra/
  ansible.cfg                 # inventory/roles_path — запускать из infra/
  inventories/dev/hosts.yml   # birdman-dev (HOSTER_A, ОБЩИЙ бокс с чужим продом)
  playbooks/dev-node.yml      # дев-нода: агент + конфиг + токен + UFW
  roles/birdman_agent_dev/    # дев-роль (см. «Дев vs прод» ниже)
```

## Запуск

```bash
# 1) собрать бинарь агента (docker, Go на хосте не нужен)
(cd ../agent && ./build.sh)          # → agent/dist/birdman-agent

# 2) экспортировать GHCR-токен (classic PAT, скоуп read:packages).
#    Токен живёт ТОЛЬКО в env и в /etc/birdman/ghcr.token (0600 root) на тачке.
#    Никогда: в репо, в логах, в vars/host_vars.
export BIRDMAN_GHCR_TOKEN='<токен>'   # пробел перед командой = мимо shell history (zsh/bash с HIST_IGNORE_SPACE)

# 3) применить (из каталога infra/)
ansible-playbook playbooks/dev-node.yml
```

Плейбук идемпотентен — можно гонять повторно (обновление бинаря/конфига = пересборка + повторный запуск).

## Что делает `dev-node.yml`

| Объект | Что |
|---|---|
| `/etc/birdman` (0755), `/var/lib/birdman`, `/var/log/birdman/servers` | каталоги агента |
| `/etc/tmpfiles.d/birdman.conf` | `/run/birdman/servers` через tmpfiles.d — переживает ребут |
| `/usr/local/bin/birdman-agent` (0755) | бинарь из `agent/dist/` |
| `/etc/birdman/agent.yaml` | конфиг v0: region `dev`, слоты 8, порты `[20000, 20050]`, лимиты 2000m/1024MB |
| `/etc/birdman/ghcr.token` (0600 root) | из env `BIRDMAN_GHCR_TOKEN` |
| UFW `20000:20050` tcp+udp, comment `birdman-dev` | аддитивно; чужие правила не трогаются |
| `/etc/systemd/system/birdman-agent.service` | **заготовка, disabled/not started** — daemon-режима у v0 нет (итерация 1) |

Проверка после прогона: `sudo birdman-agent version`, запуск дедика — `agent/README.md` («Запуск»).

## Дев vs прод

- **Дев-роль (`birdman_agent_dev`)** рассчитана на общий бокс: containerd/докер уже стоят и не трогаются, никакого hardening, UFW-правила только добавляются, unit выключен. Всё аддитивно и легко сносится.
- **Прод (позже, по `docs/specs/ops.md` §4)**: `inventories/production/hosts.yml` (группы master/nodes_*), роли `base` (sshd, nftables, sysctl), `containerd`, `node_exporter`/`vmagent`, `birdman_agent` (enabled + node_token из vault), `birdman_master`, `postgres`, `victoria`; вход — `add-node.yml` («тачка одной командой»). Дев-роль в прод не переиспользуем — прод-роль ставит и containerd, и hardening, и включённый unit.

## Секреты

- GHCR-токен — только env при запуске (`lookup('env', 'BIRDMAN_GHCR_TOKEN')`), таски с ним — `no_log`. Перед коммитами: `grep -rn "ghp[_]" --exclude-dir=.git .` → ничего, кроме фейкового фикстура в тестах агента.
- Прод-секреты (node_token и пр.) — ansible-vault (ops.md §4), появятся с прод-ролями.
