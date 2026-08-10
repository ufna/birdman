# Спека: ops — наблюдаемость, CI/CD, версии, ansible, runbooks

> Итерации 3–5. Принцип: скучные проверенные инструменты, ноль самописного там, где это не наш продукт.

## 1. Наблюдаемость

Стек: **VictoriaMetrics** (single-node, на тачке master) + **vmagent** на каждой тачке (скрейпит localhost: agent :9101, node_exporter :9100, master :443/metrics) + **Grafana** (внутренний ops-инструмент; продуктовое лицо — панель) + **vmalert** → Discord webhook.

### Канонические метрики

| Метрика | Источник | Зачем |
|---|---|---|
| `birdman_node_heartbeat_age_seconds` | master | карантин/алерт NodeDown |
| `birdman_servers{project,env,production,region,version,state}` | master | буферы, окно мультиверсий (env/production — из строки сервера, не из ноды; environments v1 §7). **Ready-срез несёт ЯВНЫЕ нули** для флотов с `buffer_ready > 0` — без них `== 0` недостижимо (tracker #960, ниже) |
| `birdman_versions{project,env,state}` | master | версии в стейт-машине per (project,env) — окно мультиверсий, ретеншн (environments v1 §7; W4-T1 M3; production несёт `birdman_servers`, не эту серию — (project,env)→production функционально зависимо) |
| `birdman_image_removals_total{status}` | master | результаты RemoveImage, доложенные агентами (`ImageReport`): removed, absent, busy, error. Флот, где удаления стабильно возвращаются busy/error, — утечка диска до watermark-GC (раньше была невидима: маркер ставился вслепую, `master.md` §6б) |
| `birdman_allocation_duration_seconds` (hist) | master | SLO p95 <1с |
| `birdman_allocation_failures_total{reason,project}` | master | no_capacity, bad_request, env_required (409-ambiguous allocate), internal — по проекту аллокации (`project` пуст только на самом раннем bad_request, когда тело запроса ещё не разобрано) |
| `birdman_mm_queue_depth{region,env}` / `birdman_mm_time_to_match_seconds` (hist) | master | здоровье матчмейкера |
| `birdman_matches_running` / `birdman_players_online` | master | продуктовые |
| `birdman_server_tick_ms{server_id}` / `birdman_server_players` | agent (из liba) | game-health |
| cpu/mem/disk/net/load | node_exporter + agent (cgroups) | железо |

### Алерты (vmalert → Discord)

| Алерт | Условие | Severity |
|---|---|---|
| NodeDown | heartbeat_age > 30s | critical |
| BufferEmpty | ready==0 в паре (регион, **проект**) > 3м (пара по production: prod=critical / non-prod=warning; достижимо только благодаря явным нулям ready-серии — #960) ИЛИ allocation_failures{no_capacity} > 0 за 1м (critical) | critical/warning |
| CrashLoop | событие crash_loop (см. master §2) | critical |
| DiskHigh | disk > 85% (critical: > 92%) | warning |
| MasterDown | внешний probe (healthchecks.io/UptimeRobot на `GET /healthz`) | critical |
| TickDegraded | p95 tick_ms > порога игры 5м | warning |
| AgentlinkPendingStuck | unacked-команды master→agent висят дольше `birdman_pending_stuck_for` (деф. 10м) | warning |
| CertExpiry | mTLS/TLS серты < 14 дней | warning |
| AgentUpgradeFailed | событие agent_upgrade_failed | critical |

MasterDown обязан приходить **не** через master (внешний probe) — иначе немой отказ.

> **(Уточнено в v0, итерация 4 — реализовано.)** vmalert-стек поднят ansible'ом (`infra/roles/birdman_monitoring_dev`, playbook `monitoring.yml`): VictoriaMetrics + vmagent + vmalert + Grafana, всё на 127.0.0.1 дев-бокса. Правила: NodeDown, BufferEmpty (ready==0 3м — **пара по production**: `production="true"` critical / `production="false"` warning, деривация лейбла — join environments по (project, env), environments v1 §7; **плюс** `no_capacity` 1м critical на `birdman_allocation_failures_total` — у него среза production нет, служит absent-страховкой обоих срезов), CrashLoop (`increase(birdman_events_total{kind="crash_loop"}[5m])`), DiskHigh (warn/crit по `birdman_agent_disk_*`), AllocationFailures, TickDegraded (порог `birdman_tick_degraded_ms`), AgentUpgradeFailed (`birdman_events_total{kind="agent_upgrade_failed"}`). **MasterDown — НЕ реализован** (намеренно): правило на самом боксе = немой отказ при падении master'а/тачки. Нужен **внешний** probe (healthchecks.io / UptimeRobot / blackbox_exporter вне бокса) на `GET /healthz`; пока master слушает только localhost — probe через туннель/бастион либо после публичного HTTPS-ингресса master'а (прод-фаза). **CertExpiry** — реализовано (mTLS agentlink v1): правило vmalert горит warning, когда любой линк-серт < 14 дней до истечения держится > 25ч (пропущен цикл renewal), по трём метрикам — `birdman_tls_cert_expiry_timestamp_seconds{cert="ca|server"}` и `birdman_node_cert_expiry_timestamp_seconds{node}` (master, DB-derived) + `birdman_agent_cert_expiry_timestamp_seconds` (агент, локально); роль `birdman_monitoring_dev`, `rules.yml.j2`. **(Дополнено в итерации 5.2:** метрики игровых нод пушатся нодовыми vmagent-сайдкарами через оверлей (`birdman_node_vmagent`, роль агента) с лейблами `node`/`region` — DiskHigh/TickDegraded работают для всех нод; центральный джоб дев-агента несёт те же лейблы.**)** **(Дополнено follow-ups итерации 5:** к перечню правил добавлено `AgentlinkPendingStuck` — warning, когда очередь unacked-команд master→agent (`birdman_agentlink_pending_commands{node,node_id}`, эмитится только для непустых очередей → серия исчезает при дренаже, алерт absent-безопасен) держит pending>0 дольше `birdman_pending_stuck_for` (деф. **10м**). Плюс на стороне master (`MarkDownNodes`) состояние ноды `down` и событие `node_down`, когда карантин длится дольше `node_down_after_min` (деф. **10м**) — оператор/алерты отличают моргнувшую ноду от лежащей давно (`dead` = ручная ревокация, автоматикой не выставляется).**)** Канал алертов **(перестроен в «настоящий silence» 1/2, tracker #244)**: vmalert → `prom/alertmanager` → logger-sink (`/var/log/birdman/alerts.log`) — оба безусловны; непустая ansible-переменная `birdman_alert_discord_webhook` добавляет `discord_configs` в тот же receiver (см. блок #238 ниже).
>
> **(Уточнено в v0, панель П2.)** master проксирует этот стек для панели (read-only, `master.md` §6): `GET /v1/alerts/rules`·`/active` — из vmalert HTTP API (`alerts.vmalert_url`, деф. `http://127.0.0.1:8880`), `GET /v1/alerts/history` — парсит этот logger-sink (`alerts.log_path`). **Двуязычие описаний алертов (EN/RU) — реализовано на уровне аннотаций правил** (закрыт прежний TODO): каждое правило в `rules.yml.j2` несёт `description` (EN, **каноничный**) и `description_ru` (RU-перевод); обе — статические строки. master пробрасывает обе аннотации как есть (`description_ru` — additive-поле в `/v1/alerts/{rules,active,history}`, `master.md` §6), панель выбирает текст по своей локали и **фоллбэчится на `description`** — поэтому `description_ru` опционален: свои правила self-host-операторов (панель их alertname не знает) показываются в обеих локалях текстом `description`, а RU включается добавлением одной аннотации без релиза панели. Альтернатива «ключи i18n по alertname на стороне панели» отвергнута: дублировала бы тексты правил в каталогах панели (дрейф при каждой правке правила) и не покрывала бы чужие правила. `summary` остаётся одноязычной динамической ops-строкой (Go-шаблоны `$labels`/`$value`; канал Discord/logger-sink), в панель попадает только как фоллбэк пустого `description`.
>
> **(Уточнено в мультипроекте W2 — проектное измерение алертов, tracker #955, шаг 1/3 эпика #950.)** До этого алерты были единственным экраном панели, который не сужался по выбранному проекту, — и не по недоделке панели, а из-за отсутствия измерения в источнике. Критерий «проектный алерт vs платформенный» один и объективный: **несёт ли лейбл `project` метрика, на которой построен `expr`**. Отдельного лейбла `scope:` в правила НЕ вводится — область выводится из НАЛИЧИЯ лейбла, поэтому корректно классифицируются и собственные правила self-host-операторов без всякой дисциплины разметки (тот же приём, что опциональный `description_ru`).
> - **Проектными стали:** `BufferEmptyReadyProd`/`BufferEmptyReadyNonProd` (агрегация `sum by (region)` → `sum by (region, project)`: `birdman_servers` лейбл уже несла, а агрегация его стирала — из-за чего пустой ready-буфер проекта А **маскировался** серверами проекта Б в том же регионе) и `BufferEmptyAllocFail`/`AllocationFailures` (лейбл `project` появился у `birdman_allocation_failures_total`, `expr` не менялся — лейбл доезжает сам). **Уточнение (#960):** формулировка коммита `076c6be` «алерт молчал ровно там, где должен был гореть» была шире своего доказательства — он молчал **везде** (см. блок ниже), а сужение до проекта было верным по существу, но **латентным**: разницы не было видно, пока правило не могло загореться вообще.
> - **Остались платформенными по природе** (лейбл был бы враньём): `NodeDown`, `DiskHigh*`/`ContainerdDiskHigh*` (нода общая — один хост несёт дедики РАЗНЫХ проектов, решение I6), `AgentlinkPendingStuck` (контур master↔agent), `CertExpiry` (серты линка), `BackupStale`/`BackupFailed` (бекап БД master целиком), `MasterDown` (внешний probe).
> - **Заблокированы отсутствием измерения** (не в этом шаге): `CrashLoop`, `AgentUpgradeFailed` (`birdman_events_total{kind}` — у таблицы `events` нет `project_id`, то же ограничение, из-за которого лента событий сужается клиентски) и `TickDegraded` (`birdman_server_tick_ms` эмитит агент, а `StartServer` проекта агенту не передаёт — карточка #958).
> - **Фильтр `?project=` на `/v1/alerts/{active,history}` — НЕ скрывающий** (`master.md` §6): алерт уходит, только если его `project` ЯВНО не равен запрошенному; алерт БЕЗ лейбла остаётся видимым при любом выборе. У проекта нет режима «Все» (в отличие от env), поэтому строгий фильтр спрятал бы `MasterDown`/`NodeDown`/`DiskHigh` навсегда — молча проглотить «мастер лежит» хуже, чем показать лишний алерт соседа. Прецеденты — `keepForProject` для ленты событий и платформенный срез пика CCU (решение I5/W3). `/v1/alerts/rules` не фильтруется вовсе: это каталог конфигурации, а не инстансы, и `project` там живёт внутри `expr`, а не в статических labels.
>
> **(Исправлено — мёртвые алерты буфера, tracker #960.)** `BufferEmptyReadyProd`/`BufferEmptyReadyNonProd` не могли сработать **ни при каком состоянии флота**, и это хуже отсутствующего алерта: случай «следующий игрок не получит дедик» выглядел покрытым. Причина — не в правиле, а в источнике: `birdman_servers` деривится группирующим `count`, у комбинации без строк серии **нет вовсе**, а агрегация Prometheus/MetricsQL по несуществующей серии даёт **пусто, а не 0**, поэтому `sum by (region, project) (...) == 0` не наступало никогда. Фактическим детектором работал `BufferEmptyAllocFail`, но он срабатывает **после** отказа аллокации — когда игрок уже пострадал.
> - **Починка — в метрике, не в `expr`:** master эмитит **явный ноль** ready-серии для каждого флота, который тёплый буфер действительно хочет (`fleet_configs.buffer_ready > 0`; `master/internal/metrics/metrics.go`, `collectReadyZeros`). Живые комбинации берутся из **desired state** — (project, env, region) флота, версия — его активная (пусто, если ничего не задеплоено: настроенный флот без версии — ровно то состояние, ради которого алерт и заведён). **Кардинальность** — одна серия на флот, без декартова произведения по версиям/состояниям; реальный ready-ряд нулём не задваивается (дубль labelset уронил бы весь `/metrics`).
> - **`buffer_ready = 0` нуля не получает** намеренно: флот, сознательно не держащий тёплый буфер, — не инцидент. Поэтому и `expr` не нуждается в join'е по `buffer_ready`: фильтр живёт в метрике.
> - **Нули не выдумываются при сбое БД:** если запрос счёта серверов упал или дочитался частично, «ready == 0» — неизвестность, а не ноль; ничего не эмитится, иначе икота базы превращалась бы в ложный critical.
> - **Тот же класс рядом:** `birdman_events_total{kind}` тоже DB-derived, серия вида рождалась сразу со значением `1`, а `increase()` по константе `1` даёт `0` — **самое первое событие вида было невидимо**, `CrashLoop`/`AgentUpgradeFailed` загорались только со второго. Master держит нулевую базу для алертных видов (две серии). Остальные 16 правил на `== 0`/`absent()` проверены — этого класса больше нет; `birdman_allocation_failures_total{reason,project}` (in-process CounterVec, первая ошибка после каждого рестарта master невидима по той же механике) вынесен отдельной карточкой — лечится иначе, пре-инициализацией по динамическому лейблу.
> - **Проверяется прогоном, а не рассуждением:** `infra/roles/birdman_monitoring_dev/tests/run.sh` рендерит `rules.yml.j2` дефолтами роли и гоняет `promtool test rules` (локальный бинарь или контейнер — хост не трогаем). Тесты фиксируют и дефект (без явного нуля правило молчит), и починку (с нулём — горит), и то, что буферы проектов не маскируют друг друга.
>
> **(Уточнено в v0, панель П2 — mute.)** `POST/GET/DELETE /v1/alerts/mutes` (`master.md` §6, таблица `alert_mutes`) — это **аннотация/подавление на уровне master+панели**: master помечает совпадающие алерты `muted:true` в `/v1/alerts/{active,history}`, чтобы панель их приглушала/скрывала и вёлся аудит (события `alert_muted`/`alert_unmuted`). Ограничение v0 — «vmalert продолжает фаерить, а Discord/logger-sink продолжают получать muted-алерты» — **закрыто «настоящим silence» 2/2 (tracker #245)**: mute теперь зеркалируется в alertmanager silence (блок #238 ниже), sink/Discord получают уже отфильтрованный поток; без alertmanager'а (self-host) остаётся ровно v0-семантика этого абзаца.
>
> **(Спроектировано и реализовано, «настоящий silence» — tracker #238; инфра — #244, master — #245.)** Развилка «alertmanager-контейнер vs самописный фильтр в sink/master» решена в пользу **alertmanager как безусловного звена доставки** — это не новый компонент, а снятие гейта с уже существующего в роли (образ `prom/alertmanager` запинен, шаблон и порт 9093 есть, но контейнер поднимается только при заданном `birdman_alert_discord_webhook`). Самописный фильтр отвергнут: нарушал бы принцип «ноль самописного» (пересборка matcher/expiry-семантики, которая в alertmanager батл-тестед), а Discord-путь вообще не покрывал бы (при webhook'е sink выпадает из цепочки). Целевая схема:
>
> 1. **Инфра (`birdman_monitoring_dev`) — реализовано (tracker #244)**: vmalert `-notifier.url` → **всегда** alertmanager; alert-sink — **всегда** поднят и подключён как постоянный `webhook_configs`-receiver alertmanager'а (пишет `alerts.log` как раньше), `discord_configs` добавляется в тот же receiver при заданном webhook'е. Это заодно закрыло латентную дыру прежнего webhook-пути: при включённом Discord sink выпадал из цепочки и `alerts.log` (история панели) переставал пополняться. Alertmanager получил data-volume `birdman-amdata` (персистентность silences между рестартами); `group_wait` 10s держит acceptance «NodeDown в alerts.log ≤60с» (eval 5s + for 30s ≈ 40s + 10s, расчёт зафиксирован в шаблоне). Sink нормализует alertmanager-webhook-тело (объект с полем `alerts` → список алертов) к прежнему формату строки — парсер master не менялся и остаётся обратно-совместим со старыми строками лога.
> 2. **master — зеркалирование mute → silence — реализовано (tracker #245)**: конфиг `alerts.alertmanager_url` (деф. `http://127.0.0.1:9093`, как у остальных URL стека; env `BIRDMAN_ALERTMANAGER_URL`; **пустое значение — зеркалирование выключено**, чистая v0-семантика); `alert_mutes` получил nullable `silence_id` (миграция `000016`). POST mute → `POST /api/v2/silences` (matchers `alertname` (+`region` и +`project`, если заданы — про проект см. блок #957 ниже), `endsAt` = `expires_at` mute'а, для бессрочного — далёкий горизонт +10 лет; повторный upsert обновляет существующий silence по id, сохраняется **возвращённый** id — AM может выпустить новый; `createdBy` несёт префикс-маркер `birdman:<имя ключа>`, `comment` = note), DELETE mute → `DELETE /api/v2/silence/{id}`. **Источник истины — `alert_mutes`**; зеркалирование best-effort (`master/internal/amsilence`): alertmanager недоступен (self-host без мониторинг-стека) → mute продолжает работать как v0 (UI+аудит), запрос не фейлится (лог с анти-спам-latch'ем — «AM недоступен» пишется один раз до восстановления). Reconcile-проход (на старте + ~60с) доводит silences до соответствия активным mute'ам — мигрирует существующие v0-mute'ы (silence_id NULL → создать silence), переживает потерю состояния alertmanager'а, чинит разъехавшийся `endsAt` и дешёвым orphan-sweep'ом гасит активные silences с birdman-маркером, не числящиеся ни за одним активным mute (DELETE mute при лежавшем AM); silences, созданные оператором в AM руками, не трогаются никогда.
> 3. **Панель — без изменений (реализовано именно так, tracker #245)**: API-shape mute тот же (`silence_id` — additive-поле в ответах) — mute из UI работает без правок панели; sink получает уже отфильтрованный поток, т.е. muted-алерты перестают литься в `alerts.log`/Discord, а `muted:true` в `/v1/alerts/active` остаётся (vmalert про silences не знает — приглушение в UI по-прежнему нужно).
>
> 4. **Проектное измерение mute'а — реализовано (tracker #957, шаг 3/3 эпика #950)**: `alert_mutes.project` (миграция `000018`, nullable; NULL = все проекты, как nullable `region` = все регионы) доезжает И до матчера `muted:true`, И до silence'а — иначе панель и alertmanager разошлись бы: панель показывает «заглушено», а Discord продолжает звенеть (или наоборот, silence гасит соседа). Матчер `project` добавляется в silence ровно тогда, когда проект у mute'а задан. Это заодно бесплатно воспроизводит строгую половину матчинга master'а: alertmanager считает отсутствующий лейбл пустой строкой, поэтому `project="alpha"` **не** накрывает платформенный алерт, у которого лейбла `project` нет вовсе — то же правило, что в `store.AlertMute.Matches`. Reconcile/orphan-sweep правки не потребовали: сверка «silence числится за mute'ом» идёт **по id**, а не по матчерам, поэтому два mute'а одного alertname в разных проектах — это две строки с двумя id, и обе известны свипу.
>
> Self-host (`deploy/` — мониторинг-стека нет) не ломается: без alertmanager'а mute деградирует до v0-семантики, как и остальные `alerts.*`-ручки (503/502/[]).

### Логи

Дедики — на тачках с tail/скачиванием через master (см. `agent.md` §5). master — journald (JSON). Централизованное хранилище логов дедиков — **реализовано** (Логи v1): VictoriaLogs + vector на нодах + read-only прокси через master.

> **(Уточнено в v0, Логи v1 — реализовано, ветка `logs-v1`.)** `birdman-victorialogs` (`victoriametrics/victoria-logs:v1.51.0`) в роли `birdman_monitoring_dev`: слушает **127.0.0.1:9428** (наружу ничего, как и весь стек), `-retentionPeriod` **14 дней** (`birdman_vl_retention`), volume `birdman-vldata`. На каждой ноде — `birdman-vector` (роль `birdman_agent_dev`, `timberio/vector`, host-network, свой API выключен): читает те же файлы `{log_dir}/servers/*.log`, что и live-tail (`.log.1` игнорирует — уже прочитан до ротации), лейблы `server_id`/`node`/`region`, loki-push в VL (`birdman_vl_sink_url`, дев-деф. `http://127.0.0.1:9428/insert`), диск-буфер 256MB `drop_newest` — доставка best-effort, нода никогда не блокируется на логах. Панель ходит только через master-прокси `GET /v1/logs/query` (`master.md` §6), напрямую в VL не лезет. **Агент не менялся вообще** — vector лишь читает уже существующие файлы; node-local ротация/ретенция (7д, `agent.md` §5) остаются независимой ручкой от ретенции VL. Live-tail (`/v1/servers/{id}/logs`) не тронут и от VL не зависит. **Прод-транспорт логов между регионами (итерация 5) — private overlay** (WireGuard/nebula): vector шлёт на приватный адрес центрального VL, без публичного порта наружу. **(Реализовано в итерации 5.1:** оверлей birdman `10.77.0.0/24` — роль `birdman_overlay`, userspace wireguard-go в контейнере; vector нод шлёт в `http://10.77.0.1:9428/insert` через socat-форвардер хаба.**)** Rollout на дев-бокс (VL → vector → master → приёмка) — выполнен.
>
> **Латентность хвостовой строки (разобрано, tracker #235).** Разово наблюдалась задержка последней строки затихшего стрима ~4м40с (без потерь). Батч loki-sink НЕ виноват: его дефолтный `batch.timeout_secs` — 1с (max 1MB/100k событий), готовая строка уходит в VL за ~1–2с; в `vector.yaml.j2` каденс теперь запинен явно (`batch.timeout_secs: 1` — контракт + защита от дрейфа дефолта при апгрейдах vector). Реальные держатели хвоста — выше синка, и настройками синка не лечатся: (1) незавершённая строка без `\n` — file-source vector держит её до следующего байта (апстрим vectordotdev/vector#18341), а cio-копирование агента может разорвать строку по границе чанка; (2) блочная буферизация stdout в самом контейнере дедика; (3) транзиентный сбой доставки (overlay/VL) — бесконечные ретраи с disk-буфером доставят позже без потерь. Принято как best-effort поведение (консистентно с `drop_newest` выше): хвост приедет со следующей активностью стрима/восстановлением канала, live-tail от этого не зависит. Отдельного фикса не требуется.

## 2. CI/CD (GitHub Actions)

### Репозитории и артефакты

- Monorepo `birdman`: master, agent, sdk, panel, proto, infra. Образы/бинари: `ghcr.io/<org>/birdman-master` (образ не обязателен — деплой бинарём через ansible), agent — бинарь в GH Releases (для self-upgrade), panel — статика внутри master.
- Репо игры: workflow собирает **Linux Server** → Docker-образ `ghcr.io/<org>/<game>-server:<semver>`.

### Пайплайн серверного билда игры

```
push в main:  build → push :X.Y.Z-dev.N → POST /v1/versions {env:"dev"}   (bound-dev-ключ; env.auto_deploy сам prepull→activate — /v1/deploy НЕ нужен)
tag vX.Y.Z:   build → push :X.Y.Z       → POST /v1/promote {version_id, to_env:"prod"}   (bound-prod-ключ, под GH environment с manual approval)
```

Флоу на окружениях (environments v1 §4–5, `master.md` §Окружения): **dev** — `env.auto_deploy=true`, поэтому регистрация версии сразу гонит prepull→activate (каждый пуш выкатывается без отдельного `/v1/deploy`; при burst'е активируется только новейшая, «только вперёд»). **prod** — `production=true` (авто-деплой запрещён guardrail'ом): релиз идёт **промоутом** уже проверенной dev-версии (тот же `image_ref`, provenance `promoted_from`), а не новой сборкой; промоут запускает обычный деплой-пайплайн.

Секреты — **два привязанных ключа** (§5): `BIRDMAN_DEPLOY_KEY_DEV` (скоуп deploy, bound `(game, dev)`) в обычных repo-secrets — работает на каждый пуш; `BIRDMAN_DEPLOY_KEY_PROD` (скоуп deploy, bound `(game, prod)`) в **GH environment** с обязательным manual approval — только он может промоутить в prod (dev-ключ на prod-деплой/промоут → `403`). Rollback — кнопкой в панели или `POST /v1/rollback` (скоупится env версии), CI не нужен.

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

> **(Уточнено в v0; итерация 3 — реализовано целиком.)** Матчмейкер реализует правило по умолчанию (равные MAJOR.MINOR, pre-release/build-суффиксы игнорируются) **и `compat.overrides` из конфига master**: паттерны `MAJOR[.MINOR[.PATCH]]` с wildcard `x`/`*` (например `1.4.x`), overrides аддитивны к default-правилу (окно миграции расширяет, а не сужает совместимость); клиенты с разными наборами подходящих overrides не смешиваются в один матч (override-set входит в ключ очереди). Кандидаты региона **скоупятся окружением тикета** (environments v1 §3), в порядке предпочтения: `fleet_configs.active_version` этого env (rank 0) → versions(state=`deprecated`) этого env (rank 1) — окно мультиверсий (`master.md` §5): старые клиенты матчатся на deprecated, пока она не `disabled`; клиент, совместимый с active, на deprecated не попадает; `update_required` — только когда клиент не совместим ни с одной живой версией. **Прежний средний ранг `versions(state=active, channel=prod)` умер вместе с колонкой `channel`** — active-версия окружения и так приходит через `fleet_configs.active_version` (флот теперь per (project, env, region)). Несовместимость проверяется на submit тикета и на каждом тике; клиент, чей `client_version` не парсится как semver, получает `400`.

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

> **(Уточнено в итерации 5.1 — реализована dev-форма.)** `infra/playbooks/add-node.yml` работает поверх дев-ролей: новая роль `birdman_overlay` (изолированный WireGuard-оверлей birdman: userspace `wireguard-go` + socat-форвардеры control-plane в контейнере) + генерализованная `birdman_agent_dev` (REST-вызовы регистрации — `delegate_to` master-бокса; `birdman_registry_legacy: false` — без ghcr-токена на ноде). Прод-роль-суита этого раздела (`base`/`containerd`/`birdman_agent`/…) — по-прежнему TODO до первого прод-железа; add-node-флоу, delegate_to-паттерн и оверлей-роль переедут в неё без переделки.

Ключевые параметры `base`: nftables — allow 22 (allowlist админ-IP), 443+8443 только на master, 19999/udp + 20000–29999 tcp/udp на нодах; sysctl: `net.core.rmem_max/wmem_max=8388608`, `net.netfilter.nf_conntrack_max` под UDP-нагрузку, `fs.nr_open`; секреты — ansible-vault (карве-аут: `/etc/birdman/secrets.key` — исключение, эскроу в менеджер паролей владельца, НЕ vault-в-git, даже когда прод-роли примут vault для прочих секретов; решение владельца, см. §5).

## 5. Бэкапы и восстановление

- Postgres: `pg_dump` каждый час → S3-совместимое хранилище (Backblaze/Wasabi), retention 14 дней; еженедельный тест-restore в docker (CI-джоба).

> **(Уточнено в v0, итерация 4.)** Реализованы **локальные** дампы (роль `birdman_monitoring_dev`): `birdman-pg-backup.timer` каждые 6ч → `docker exec birdman-postgres pg_dump -Fc` в `/var/lib/birdman/backups`, ротация 14 свежих; тест-restore — `/usr/local/bin/birdman-pg-restore-test` (одноразовый `postgres:16`, `pg_restore` + sanity-запрос). **Оффсайт в S3 — TODO** (бакета нет; гейт шифрования секретов теперь СНЯТ — см. ниже): когда появится бакет — `rclone copy`/`aws s3 cp` в `/usr/local/bin/birdman-pg-backup` (место помечено `TODO(offsite)`), креды в vault. Еженедельный тест-restore как CI-джоба — тоже прод-фаза (сейчас скрипт запускается вручную/по расписанию на боксе).
>
> **(Уточнено в v1, Шифрование секретов at-rest — гейт СНЯТ, ветка `secrets-encryption-v1`.)** Обратимые секреты `registries.token` и `internal_ca.key_pem` (`master.md` §1) хранятся в БД **AEAD-шифротекстом** (самоописывающийся конверт `birdman:v1:<key_id>:…`, AES-256-GCM: master шифрует значение перед INSERT и расшифровывает после SELECT). Мастер-ключ — `/etc/birdman/secrets.key` (0600, полноэнтропийные 32 байта, провижинится ansible-ролью `birdman_master_dev` один раз). **Дамп `pg_dump` по построению содержит только шифротекст; `birdman-pg-backup.sh.j2` не меняется ни на строку** — он выгружает ровно то, что лежит в БД. Ключ `/etc/birdman/secrets.key` **в дамп не входит и НИКОГДА не покидает бокс в сторону бэкап-хранилища** — ни в S3, ни рядом с дампами; эскроу-копия ключа идёт ОТДЕЛЬНЫМ каналом (менеджер паролей владельца, НЕ vault-в-git, НЕ бакет дампов; restore-runbook ниже). **Прежний гейт «оффсайт-выгрузку дампов не включать, пока секреты не зашифрованы (age/gpg) либо at-rest» — СНЯТ: оффсайт-выгрузка дампов разрешена.** Само включение S3-синка — по-прежнему отдельный follow-up (нужен бакет+креды владельца, см. `TODO(offsite)` выше): этот воркстрим снял гейт, но синк не включает.
>
> **(Уточнено в Backups v1, 2026-07-13 — таймер снесён, исполняет master.)**
> Дампы делает сам master (`internal/backup`: планировщик + `pg_dump -Fc` +
> ротация + S3-выгрузка minio-go), политика настраивается в панели
> (Админка → Бекапы), история прогонов — `backup_runs`. systemd-таймер
> `birdman-pg-backup` и его скрипт удалены ролью; `birdman-pg-restore-test`
> остался. `TODO(offsite)` ЗАКРЫТ: S3-совместимый оффсайт включается в панели
> (endpoint/bucket/ключи; секрет — AEAD at-rest). Алерты: `BackupStale`
> (нет успеха дольше 2×интервала), `BackupFailed` (ошибка прогона за последний час).
- Потеря master-тачки: новая тачка → `master.yml` плейбук → restore дампа → **заменить сгенерённый плейбуком `/etc/birdman/secrets.key` эскроу-копией и перезапустить master (ДО того как master прочитает восстановленные секреты)** → агенты переподключаются сами; матчи, шедшие во время отказа, доигрываются (сервера живы), их `match_end` попадёт в PG после восстановления. Целевой RTO ≤ 60 мин, RPO ≤ 1 ч. (Уточнено в v1, mTLS.) Внутренняя CA живёт в PG (`internal_ca`), поэтому едет в том же дампе — восстановленный master поднимает **ту же** CA, и уже выданные клиентские серты нод остаются валидны: флот реконнектится mTLS **без ре-энроллмента**. **(Уточнено в v1, Шифрование секретов at-rest.)** Runbook получает ровно ОДИН новый шаг — эскроу-ключ выше: `master.yml` на новой тачке сгенерит свежий `/etc/birdman/secrets.key`, которым существующий шифротекст дампа не читается, поэтому его надо заменить эскроу-копией (менеджер паролей владельца) до старта master против восстановленной БД — иначе master упадёт при старте с `key_id`-mismatch (громко и диагностируемо; данные не повреждаются, лечится заменой файла + рестартом). Старые pre-encryption plaintext-дампы восстановимы и без этого нюанса — startup-проход master'а зашифрует их текущим ключом. RTO ≤60 мин сохраняется при доступном оператору эскроу. Репетиция same-key реконнекта покрыта integration-тестом `master/internal/agentlink`. **(a) Restore — это полная замена БД, не аддитивная:** дамп восстанавливается в чистую БД, `internal_ca` приезжает целиком из дампа — промежуточную CA, которую `master.yml`-плейбук сминтил бы при первом старте на пустой БД, здесь не с чем путать (при доступном эскроу-ключе master стартует сразу против восстановленной `internal_ca` и новую CA не генерит). Эскроу-ключ допустимо положить в `/etc/birdman/secrets.key` **до** прогона роли — таск генерации идемпотентен по `creates:` и существующий файл не перезапишет (убирает шаг «сгенерил → заменил вручную»). **(b) break-glass (эскроу-ключ утрачен):** зашифрованные `registries.token` и `internal_ca.key_pem` невосстановимы (нет ключа — нет расшифровки AEAD) — это потеря ровно этих двух секретов, не всей БД. Лечение: удалить эти строки (`delete from registries;` + деактивировать/удалить строки `internal_ca`) и переустановить — реестры добавить заново в Админке; CA регенерируется при следующем старте master'а на пустой `internal_ca`, из-за чего **все ноды обязаны ре-энроллиться по node_token** (свежая CA → старые клиентские серты нод невалидны; оператор ре-раздаёт свежий `GET /v1/ca` агентской ролью → Enroll-by-token самолечит линк, mtls-дизайн §4). Итог честно: даталосс двух секретов, флот самовосстанавливается через enrollment (RTO тут определяется ре-энроллментом флота, не 60 мин).

## 6. Runbooks (заготовки — довести в итерации 4–5)

1. **Тачка умерла**: подтверждение (ssh/IPMI) → если труп: inventory-минус, capacity-проверка, заказ замены; карантин уже отработал сам.
2. **DDoS**: прод-тачки только OVH game / Latitude (щит включён) → если региональная деградация: null-route у поставщика, `drain` тачки, буфер переезжает на соседние; клиентам матчмейкер выдаёт другой регион.
3. **Плохой релиз**: CrashLoop-алерт → `POST /v1/rollback` (кнопка в панели) → расследование по логам умерших дедиков.
4. **Диск полон**: image GC не справился → tail логов виновника, ручной прюнинг, поднять пороги/докупить диск.
5. **Восстановление master** — см. §5.

> **(environments v1 §6г — граница скоупа гигиены образов.)** birdman чистит образы **на нодах**: ретеншн версий (`retention_keep` окружения → сверхлимитные `registered|disabled` в `disabled`) + `RemoveImage` снимает их образ с нод окружения сразу, watermark-GC диска — страховка (`agent.md` §6, `master.md` §Окружения). **Теги в самом реестре** (ghcr/gar) платформа НЕ удаляет — это ретеншн-политика реестра на стороне владельца (`actions/delete-package-versions`, lifecycle-политика GAR и т.п.). Поток dev-билдов копит теги в реестре независимо от нод — заведи там свою ретенцию.

## 7. Acceptance

- **Ит. 3**: пуш в main игры → staging-флот обновлён без рук; тег → прод после approve; 0 оборванных матчей (учение). *(истор.: «staging-флот» — ныне флот окружения `dev` с `auto_deploy`; «прод после approve» — промоут bound-prod-ключом под GH environment approval, §2.)*
- **Ит. 4**: все алерты таблицы стреляют на учениях в Discord ≤60с; логи любого умершего дедика достаются из панели/CLI; дашборд Grafana «одна тачка» и «регион» существуют.
- **Ит. 5**: add-node.yml добавляет тачку за ≤15 мин от заказа до ready-буфера; тест-restore PG проходит в CI.
