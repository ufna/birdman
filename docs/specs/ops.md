# Спека: ops — наблюдаемость, CI/CD, версии, ansible, runbooks

> Итерации 3–5 из `../05-runtime-iterations.md`. Принцип: скучные проверенные инструменты, ноль самописного там, где это не наш продукт.

## 1. Наблюдаемость

Стек: **VictoriaMetrics** (single-node, на тачке master) + **vmagent** на каждой тачке (скрейпит localhost: agent :9101, node_exporter :9100, master :443/metrics) + **Grafana** (внутренний ops-инструмент; продуктовое лицо — панель) + **vmalert** → Discord webhook.

### Канонические метрики

| Метрика | Источник | Зачем |
|---|---|---|
| `birdman_node_heartbeat_age_seconds` | master | карантин/алерт NodeDown |
| `birdman_servers{project,region,version,state}` | master | буферы, окно мультиверсий |
| `birdman_allocation_duration_seconds` (hist) | master | SLO p95 <1с |
| `birdman_allocation_failures_total{reason}` | master | no_capacity и пр. |
| `birdman_mm_queue_depth{region}` / `birdman_mm_time_to_match_seconds` (hist) | master | здоровье матчмейкера |
| `birdman_matches_running` / `birdman_players_online` | master | продуктовые |
| `birdman_server_tick_ms{server_id}` / `birdman_server_players` | agent (из liba) | game-health |
| cpu/mem/disk/net/load | node_exporter + agent (cgroups) | железо |

### Алерты (vmalert → Discord)

| Алерт | Условие | Severity |
|---|---|---|
| NodeDown | heartbeat_age > 30s | critical |
| BufferEmpty | ready==0 в регионе > 3м ИЛИ allocation_failures{no_capacity} > 0 за 1м | critical |
| CrashLoop | событие crash_loop (см. master §2) | critical |
| DiskHigh | disk > 85% (critical: > 92%) | warning |
| MasterDown | внешний probe (healthchecks.io/UptimeRobot на `GET /healthz`) | critical |
| TickDegraded | p95 tick_ms > порога игры 5м | warning |
| AgentlinkPendingStuck | unacked-команды master→agent висят дольше `birdman_pending_stuck_for` (деф. 10м) | warning |
| CertExpiry | mTLS/TLS серты < 14 дней | warning |
| AgentUpgradeFailed | событие agent_upgrade_failed | critical |

MasterDown обязан приходить **не** через master (внешний probe) — иначе немой отказ.

> **(Уточнено в v0, итерация 4 — реализовано.)** vmalert-стек поднят ansible'ом (`infra/roles/birdman_monitoring_dev`, playbook `monitoring.yml`): VictoriaMetrics + vmagent + vmalert + Grafana, всё на 127.0.0.1 дев-бокса. Правила: NodeDown, BufferEmpty (ready==0 3м **и** `no_capacity` 1м — две записи), CrashLoop (`increase(birdman_events_total{kind="crash_loop"}[5m])`), DiskHigh (warn/crit по `birdman_agent_disk_*`), AllocationFailures, TickDegraded (порог `birdman_tick_degraded_ms`), AgentUpgradeFailed (`birdman_events_total{kind="agent_upgrade_failed"}`). **MasterDown — НЕ реализован** (намеренно): правило на самом боксе = немой отказ при падении master'а/тачки. Нужен **внешний** probe (healthchecks.io / UptimeRobot / blackbox_exporter вне бокса) на `GET /healthz`; пока master слушает только localhost — probe через туннель/бастион либо после публичного HTTPS-ингресса master'а (прод-фаза). **CertExpiry** — реализовано (mTLS agentlink v1): правило vmalert горит warning, когда любой линк-серт < 14 дней до истечения держится > 25ч (пропущен цикл renewal), по трём метрикам — `birdman_tls_cert_expiry_timestamp_seconds{cert="ca|server"}` и `birdman_node_cert_expiry_timestamp_seconds{node}` (master, DB-derived) + `birdman_agent_cert_expiry_timestamp_seconds` (агент, локально); роль `birdman_monitoring_dev`, `rules.yml.j2`. **(Дополнено в итерации 5.2:** метрики игровых нод пушатся нодовыми vmagent-сайдкарами через оверлей (`birdman_node_vmagent`, роль агента) с лейблами `node`/`region` — DiskHigh/TickDegraded работают для всех нод; центральный джоб дев-агента несёт те же лейблы.**)** **(Дополнено follow-ups итерации 5:** к перечню правил добавлено `AgentlinkPendingStuck` — warning, когда очередь unacked-команд master→agent (`birdman_agentlink_pending_commands{node,node_id}`, эмитится только для непустых очередей → серия исчезает при дренаже, алерт absent-безопасен) держит pending>0 дольше `birdman_pending_stuck_for` (деф. **10м**). Плюс на стороне master (`MarkDownNodes`) состояние ноды `down` и событие `node_down`, когда карантин длится дольше `node_down_after_min` (деф. **10м**) — оператор/алерты отличают моргнувшую ноду от лежащей давно (`dead` = ручная ревокация, автоматикой не выставляется).**)** Канал алертов: Discord-webhook'а пока нет → работает logger-sink (`/var/log/birdman/alerts.log`); переключение на `prom/alertmanager` с `discord_configs` — ansible-переменной `birdman_alert_discord_webhook`.
>
> **(Уточнено в v0, панель П2.)** master проксирует этот стек для панели (read-only, `master.md` §6): `GET /v1/alerts/rules`·`/active` — из vmalert HTTP API (`alerts.vmalert_url`, деф. `http://127.0.0.1:8880`), `GET /v1/alerts/history` — парсит этот logger-sink (`alerts.log_path`). **description алертов сейчас на русском** (из vmalert-правил в `infra/roles/birdman_monitoring_dev`) — master пробрасывает строку как есть, панель показывает её без перевода. **Двуязычие алертов (EN/RU) — TODO уровня конфига vmalert** (аннотации правил), вне скоупа master: панель i18n-слой переводит свои подписи, но текст `description`/`summary` приходит из vmalert и переводится только там.
>
> **(Уточнено в v0, панель П2 — mute.)** `POST/GET/DELETE /v1/alerts/mutes` (`master.md` §6, таблица `alert_mutes`) — это **аннотация/подавление на уровне master+панели, а НЕ настоящий silence**: master помечает совпадающие алерты `muted:true` в `/v1/alerts/{active,history}`, чтобы панель их приглушала/скрывала и вёлся аудит (события `alert_muted`/`alert_unmuted`). **Ограничение v0: vmalert продолжает фаерить, а Discord/logger-sink продолжают получать muted-алерты** — mute на них не влияет. **Настоящий silence — TODO**: нужен `prom/alertmanager` c его **silence API** (`POST /api/v2/silences` по matchers `alertname`/`region`, с истечением) — master проксировал бы mute туда, чтобы Discord реально замолкал. Пока alertmanager не включён (Discord-webhook'а нет, работает logger-sink), master-mute — это только UI/аудит-слой; при переходе на alertmanager (`birdman_alert_discord_webhook`) сюда добавляется зеркалирование mute → silence.

### Логи

Дедики — на тачках с tail/скачиванием через master (см. `agent.md` §5). master — journald (JSON). Централизованное хранилище логов дедиков — **реализовано** (Логи v1): VictoriaLogs + vector на нодах + read-only прокси через master.

> **(Уточнено в v0, Логи v1 — реализовано, ветка `logs-v1`.)** `birdman-victorialogs` (`victoriametrics/victoria-logs:v1.51.0`) в роли `birdman_monitoring_dev`: слушает **127.0.0.1:9428** (наружу ничего, как и весь стек), `-retentionPeriod` **14 дней** (`birdman_vl_retention`), volume `birdman-vldata`. На каждой ноде — `birdman-vector` (роль `birdman_agent_dev`, `timberio/vector`, host-network, свой API выключен): читает те же файлы `{log_dir}/servers/*.log`, что и live-tail (`.log.1` игнорирует — уже прочитан до ротации), лейблы `server_id`/`node`/`region`, loki-push в VL (`birdman_vl_sink_url`, дев-деф. `http://127.0.0.1:9428/insert`), диск-буфер 256MB `drop_newest` — доставка best-effort, нода никогда не блокируется на логах. Панель ходит только через master-прокси `GET /v1/logs/query` (`master.md` §6), напрямую в VL не лезет. **Агент не менялся вообще** — vector лишь читает уже существующие файлы; node-local ротация/ретенция (7д, `agent.md` §5) остаются независимой ручкой от ретенции VL. Live-tail (`/v1/servers/{id}/logs`) не тронут и от VL не зависит. **Прод-транспорт логов между регионами (итерация 5) — private overlay** (WireGuard/nebula): vector шлёт на приватный адрес центрального VL, без публичного порта наружу. **(Реализовано в итерации 5.1:** оверлей birdman `10.77.0.0/24` — роль `birdman_overlay`, userspace wireguard-go в контейнере; vector нод шлёт в `http://10.77.0.1:9428/insert` через socat-форвардер хаба; спека `docs/superpowers/specs/2026-07-10-iter5-second-node-design.md`.**)** Rollout на дев-бокс (VL → vector → master → приёмка) — выполнен, см. `STATUS.md`.

## 2. CI/CD (GitHub Actions)

### Репозитории и артефакты

- Monorepo `birdman`: master, agent, sdk, panel, proto, infra. Образы/бинари: `ghcr.io/<org>/birdman-master` (образ не обязателен — деплой бинарём через ansible), agent — бинарь в GH Releases (для self-upgrade), panel — статика внутри master.
- Репо игры: workflow собирает **Linux Server** → Docker-образ `ghcr.io/<org>/<game>-server:<semver>`.

### Пайплайн серверного билда игры

```
push в main:        build → push :X.Y.Z-dev.N → POST /v1/versions {channel: staging} → POST /v1/deploy (staging-флот)
tag vX.Y.Z:         build → push :X.Y.Z → POST /v1/versions {channel: prod} → manual approval (environment) → POST /v1/deploy
```

Секреты: `BIRDMAN_DEPLOY_KEY` (API-ключ со скоупом deploy) в GH environment secrets. Rollback — кнопкой в панели или `POST /v1/rollback`, CI не нужен.

> **(Уточнено в v0, итерация 3.)** Реализована механика версий без деплой-хука: `stub-server.yml` умеет `workflow_dispatch` с input `tag` (semver) → build+push `ghcr.io/<org>/birdman-stub-server:<tag>`; регистрация (`POST /v1/versions`) и `POST /v1/deploy` выполняются оператором/скриптом рядом с master. **Автовызов master API из GitHub Actions (`POST /v1/versions` → `/v1/deploy` прямо из workflow) — TODO прод-фазы: master не в интернете** (dev-бокс слушает только localhost); понадобится публичный HTTPS-ингресс master или self-hosted runner в его сети.

### Пайплайн платформы

lint (golangci-lint) + unit → integration (docker-compose: PG + master + agent + mockliba, сценарии: allocate, карантин, deploy) → build бинарей (linux/amd64) → GH Release. Агенты обновляет master командой UpgradeAgent (URL релиза + sha256), master — ansible-плейбуком.

## 3. Версии и совместимость клиент↔сервер

- Semver `MAJOR.MINOR.PATCH` для серверного билда; клиент шлёт свой `client_version` в тикете.
- **Правило по умолчанию: совместимы при равных MAJOR.MINOR** (PATCH свободен). Переопределение — таблица в конфиге master:

```yaml
compat:
  default: "major.minor"
  overrides:
    - client: "1.4.x"; servers: ["1.4.x", "1.5.x"]   # окно миграции
```

- Мягкий деплой (master §5): в окне мультиверсий старые клиенты матчатся на deprecated-версию, пока она в compat; вышла из compat → `update_required`.
- Конвенцию утвердить с командой игры **до итерации 3** (чек-пункт).

> **(Уточнено в v0; итерация 3 — реализовано целиком.)** Матчмейкер реализует правило по умолчанию (равные MAJOR.MINOR, pre-release/build-суффиксы игнорируются) **и `compat.overrides` из конфига master**: паттерны `MAJOR[.MINOR[.PATCH]]` с wildcard `x`/`*` (например `1.4.x`), overrides аддитивны к default-правилу (окно миграции расширяет, а не сужает совместимость); клиенты с разными наборами подходящих overrides не смешиваются в один матч (override-set входит в ключ очереди). Кандидаты региона, в порядке предпочтения: `fleet_configs.active_version` → versions(state=`active`, channel=`prod`) → versions(state=`deprecated`) — окно мультиверсий (`master.md` §5): старые клиенты матчатся на deprecated, пока она не `disabled`; клиент, совместимый с active, на deprecated не попадает; `update_required` — только когда клиент не совместим ни с одной живой версией. Несовместимость проверяется на submit тикета и на каждом тике; клиент, чей `client_version` не парсится как semver, получает `400`.

## 4. Ansible (`infra/`)

```
infra/
  inventories/
    production/hosts.yml      # группы: master, nodes_eu, nodes_us
  playbooks/
    site.yml                  # всё
    add-node.yml              # «тачка одной командой»
    master.yml  monitoring.yml
  roles/
    base        # юзеры, sshd hardening, nftables, sysctl, chrony, unattended-upgrades
    containerd  # containerd + конфиг namespace birdman
    node_exporter  vmagent
    birdman_agent  # бинарь, конфиг из шаблона, systemd; node_token из vault
    birdman_master # бинарь, конфиг, systemd, certbot(:443), внутренняя CA
    postgres    # PG16 на тачке master, pg_dump-cron
    victoria    # VM + vmalert + Grafana (тачка master)
```

**Добавить тачку** = 1 строка в `hosts.yml` + `ansible-playbook playbooks/add-node.yml -l node-eu-3` (создаёт node в master API → `POST /v1/nodes` кладёт token; `GET /v1/ca` кладёт публичный `master-ca.pem` 0644 в `tls_ca_file`; ставит всё). (Уточнено в v1, mTLS.) `tls_insecure` из агентского шаблона **убран** — тачка выходит на связь **сразу mTLS** с первого коннекта: агент по server-auth TLS вызывает `Enroll(node_token)` → получает клиентский серт → mTLS-сессия (`protocol.md` §Auth). **Вывести** = `POST /nodes/{id}/drain` → дождаться пустоты → убрать из inventory.

> **(Уточнено в итерации 5.1 — реализована dev-форма.)** `infra/playbooks/add-node.yml` работает поверх дев-ролей: новая роль `birdman_overlay` (изолированный WireGuard-оверлей birdman: userspace `wireguard-go` + socat-форвардеры control-plane в контейнере, спека `docs/superpowers/specs/2026-07-10-iter5-second-node-design.md`) + генерализованная `birdman_agent_dev` (REST-вызовы регистрации — `delegate_to` master-бокса; `birdman_registry_legacy: false` — без ghcr-токена на ноде). Прод-роль-суита этого раздела (`base`/`containerd`/`birdman_agent`/…) — по-прежнему TODO до первого прод-железа; add-node-флоу, delegate_to-паттерн и оверлей-роль переедут в неё без переделки.

Ключевые параметры `base`: nftables — allow 22 (allowlist админ-IP), 443+8443 только на master, 19999/udp + 20000–29999 tcp/udp на нодах; sysctl: `net.core.rmem_max/wmem_max=8388608`, `net.netfilter.nf_conntrack_max` под UDP-нагрузку, `fs.nr_open`; секреты — ansible-vault (карве-аут: `/etc/birdman/secrets.key` — исключение, эскроу в менеджер паролей владельца, НЕ vault-в-git, даже когда прод-роли примут vault для прочих секретов; решение владельца, см. §5).

## 5. Бэкапы и восстановление

- Postgres: `pg_dump` каждый час → S3-совместимое хранилище (Backblaze/Wasabi), retention 14 дней; еженедельный тест-restore в docker (CI-джоба).

> **(Уточнено в v0, итерация 4.)** Реализованы **локальные** дампы (роль `birdman_monitoring_dev`): `birdman-pg-backup.timer` каждые 6ч → `docker exec birdman-postgres pg_dump -Fc` в `/var/lib/birdman/backups`, ротация 14 свежих; тест-restore — `/usr/local/bin/birdman-pg-restore-test` (одноразовый `postgres:16`, `pg_restore` + sanity-запрос). **Оффсайт в S3 — TODO** (бакета нет; гейт шифрования секретов теперь СНЯТ — см. ниже): когда появится бакет — `rclone copy`/`aws s3 cp` в `/usr/local/bin/birdman-pg-backup` (место помечено `TODO(offsite)`), креды в vault. Еженедельный тест-restore как CI-джоба — тоже прод-фаза (сейчас скрипт запускается вручную/по расписанию на боксе).
>
> **(Уточнено в v1, Шифрование секретов at-rest — гейт СНЯТ, ветка `secrets-encryption-v1`.)** Обратимые секреты `registries.token` и `internal_ca.key_pem` (`master.md` §1) хранятся в БД **AEAD-шифротекстом** (самоописывающийся конверт `birdman:v1:<key_id>:…`, AES-256-GCM: master шифрует значение перед INSERT и расшифровывает после SELECT — дизайн `../superpowers/specs/2026-07-10-secrets-encryption-design.md`). Мастер-ключ — `/etc/birdman/secrets.key` (0600, полноэнтропийные 32 байта, провижинится ansible-ролью `birdman_master_dev` один раз). **Дамп `pg_dump` по построению содержит только шифротекст; `birdman-pg-backup.sh.j2` не меняется ни на строку** — он выгружает ровно то, что лежит в БД. Ключ `/etc/birdman/secrets.key` **в дамп не входит и НИКОГДА не покидает бокс в сторону бэкап-хранилища** — ни в S3, ни рядом с дампами; эскроу-копия ключа идёт ОТДЕЛЬНЫМ каналом (менеджер паролей владельца, НЕ vault-в-git, НЕ бакет дампов; restore-runbook ниже). **Прежний гейт «оффсайт-выгрузку дампов не включать, пока секреты не зашифрованы (age/gpg) либо at-rest» — СНЯТ: оффсайт-выгрузка дампов разрешена.** Само включение S3-синка — по-прежнему отдельный follow-up (нужен бакет+креды владельца, см. `TODO(offsite)` выше и STATUS.md «что дальше»): этот воркстрим снял гейт, но синк не включает.
- Потеря master-тачки: новая тачка → `master.yml` плейбук → restore дампа → **заменить сгенерённый плейбуком `/etc/birdman/secrets.key` эскроу-копией и перезапустить master (ДО того как master прочитает восстановленные секреты)** → агенты переподключаются сами; матчи, шедшие во время отказа, доигрываются (сервера живы), их `match_end` попадёт в PG после восстановления. Целевой RTO ≤ 60 мин, RPO ≤ 1 ч. (Уточнено в v1, mTLS.) Внутренняя CA живёт в PG (`internal_ca`), поэтому едет в том же дампе — восстановленный master поднимает **ту же** CA, и уже выданные клиентские серты нод остаются валидны: флот реконнектится mTLS **без ре-энроллмента**. **(Уточнено в v1, Шифрование секретов at-rest.)** Runbook получает ровно ОДИН новый шаг — эскроу-ключ выше: `master.yml` на новой тачке сгенерит свежий `/etc/birdman/secrets.key`, которым существующий шифротекст дампа не читается, поэтому его надо заменить эскроу-копией (менеджер паролей владельца) до старта master против восстановленной БД — иначе master упадёт при старте с `key_id`-mismatch (громко и диагностируемо; данные не повреждаются, лечится заменой файла + рестартом). Старые pre-encryption plaintext-дампы восстановимы и без этого нюанса — startup-проход master'а зашифрует их текущим ключом. RTO ≤60 мин сохраняется при доступном оператору эскроу. Репетиция same-key реконнекта покрыта integration-тестом `master/internal/agentlink`. **(a) Restore — это полная замена БД, не аддитивная:** дамп восстанавливается в чистую БД, `internal_ca` приезжает целиком из дампа — промежуточную CA, которую `master.yml`-плейбук сминтил бы при первом старте на пустой БД, здесь не с чем путать (при доступном эскроу-ключе master стартует сразу против восстановленной `internal_ca` и новую CA не генерит). Эскроу-ключ допустимо положить в `/etc/birdman/secrets.key` **до** прогона роли — таск генерации идемпотентен по `creates:` и существующий файл не перезапишет (убирает шаг «сгенерил → заменил вручную»). **(b) break-glass (эскроу-ключ утрачен):** зашифрованные `registries.token` и `internal_ca.key_pem` невосстановимы (нет ключа — нет расшифровки AEAD) — это потеря ровно этих двух секретов, не всей БД. Лечение: удалить эти строки (`delete from registries;` + деактивировать/удалить строки `internal_ca`) и переустановить — реестры добавить заново в Админке; CA регенерируется при следующем старте master'а на пустой `internal_ca`, из-за чего **все ноды обязаны ре-энроллиться по node_token** (свежая CA → старые клиентские серты нод невалидны; оператор ре-раздаёт свежий `GET /v1/ca` агентской ролью → Enroll-by-token самолечит линк, mtls-дизайн §4). Итог честно: даталосс двух секретов, флот самовосстанавливается через enrollment (RTO тут определяется ре-энроллментом флота, не 60 мин).

## 6. Runbooks (заготовки — довести в итерации 4–5)

1. **Тачка умерла**: подтверждение (ssh/IPMI) → если труп: inventory-минус, capacity-проверка, заказ замены; карантин уже отработал сам.
2. **DDoS**: прод-тачки только OVH game / Latitude (щит включён) → если региональная деградация: null-route у поставщика, `drain` тачки, буфер переезжает на соседние; клиентам матчмейкер выдаёт другой регион.
3. **Плохой релиз**: CrashLoop-алерт → `POST /v1/rollback` (кнопка в панели) → расследование по логам умерших дедиков.
4. **Диск полон**: image GC не справился → tail логов виновника, ручной прюнинг, поднять пороги/докупить диск.
5. **Восстановление master** — см. §5.

## 7. Acceptance

- **Ит. 3**: пуш в main игры → staging-флот обновлён без рук; тег → прод после approve; 0 оборванных матчей (учение).
- **Ит. 4**: все алерты таблицы стреляют на учениях в Discord ≤60с; логи любого умершего дедика достаются из панели/CLI; дашборд Grafana «одна тачка» и «регион» существуют.
- **Ит. 5**: add-node.yml добавляет тачку за ≤15 мин от заказа до ready-буфера; тест-restore PG проходит в CI.
