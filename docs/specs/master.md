# Спека: master

> Один Go-процесс: матчмейкер + флот-контроллер + deploy-менеджер + REST API + панель (embed). Postgres 16. Итерации 1–3, 5.

## 1. Модель данных (Postgres, DDL v1)

```sql
create table projects (
  id         uuid primary key default gen_random_uuid(),
  slug       text unique not null,          -- 'ourgame'
  match_size int  not null default 2        -- (уточнено в v0) размер матча —
             check (match_size >= 1),       -- конфиг per project (§4), правится
                                            -- через PUT /v1/projects/{slug}
  created_at timestamptz not null default now()
);

create table nodes (
  id                uuid primary key default gen_random_uuid(),
  project_id        uuid not null references projects(id),
  region            text not null,          -- 'eu', 'us-east'
  env               text not null default 'dev', -- (environments v1) нода принадлежит одному env; FK (project_id, env); перевод пустой ноды — PATCH /v1/nodes/{id}
  hostname          text not null,
  public_ip         inet not null,
  capacity_slots    int  not null,
  agent_version     text not null default '',
  state             text not null default 'active'
                    check (state in ('active','draining','quarantine','down','dead')),
  last_heartbeat_at timestamptz,
  labels            jsonb not null default '{}',
  token_hash        text not null default '',  -- bcrypt node_token (v1: recovery-кред, обмен на серт реализован)
  cert_serial       text,                       -- (v1) serial активного клиентского серта ноды
  cert_not_after    timestamptz,                -- (v1) истечение серта — admission + метрики
  enrolled_at       timestamptz,                -- (v1) первый token→cert Enroll (renewal его не трогает)
  created_at        timestamptz not null default now()
);

-- (v1, mTLS agentlink — миграция 000008) внутренняя CA в PG: переживает потерю
-- бокса вместе с дампом (restore-runbook, ops.md §5). key_pem — обратимый секрет
-- (класс риска = registries.token); (v1, secrets-encryption) хранится
-- AEAD-шифротекстом at-rest — конверт birdman:v1:<key_id>:…, ключ
-- /etc/birdman/secrets.key: master шифрует перед INSERT, расшифровывает после
-- SELECT (ops.md §5), в дампе только шифротекст. Никогда не логируется,
-- /v1/ca его не отдаёт.
create table internal_ca (
  id         uuid primary key default gen_random_uuid(),
  cert_pem   text not null,
  key_pem    text not null,
  active     bool not null default true,
  created_at timestamptz not null default now(),
  not_after  timestamptz not null
);

-- (environments v1, миграция 000013) окружение — полноценное измерение per project;
-- сид dev+prod каждому проекту (ensureProject добавляет их новому). Флаги ведут
-- политику, не имя (§Окружения).
create table environments (
  id             uuid primary key default gen_random_uuid(),
  project_id     uuid not null references projects(id) on delete cascade,
  name           text not null check (name ~ '^[a-z0-9][a-z0-9-]{0,31}$'),
  production     boolean not null default false,
  auto_deploy    boolean not null default false,
  retention_keep int not null default 0 check (retention_keep >= 0),
  created_at     timestamptz not null default now(),
  unique (project_id, name),
  check (not (production and auto_deploy)),          -- guardrail и на уровне БД
  check (name not in ('all', 'global'))              -- зарезервировано под UI/API
);

create table versions (
  id            uuid primary key default gen_random_uuid(),
  project_id    uuid not null references projects(id),
  semver        text not null,                 -- '1.4.2'
  image_ref     text not null,                 -- 'ghcr.io/org/ourgame-server:1.4.2'
  env           text not null,                 -- (environments v1) заменило channel; FK (project_id, env)→environments
  promoted_from uuid references versions(id),  -- (environments v1) provenance промоута dev→prod
  state         text not null default 'registered'
                check (state in ('registered','prepulling','active','deprecated','disabled')),
  created_at    timestamptz not null default now(),
  unique (project_id, env, semver),                       -- было (project_id, semver, channel)
  unique (id, project_id, env),                           -- опора составного FK флота
  foreign key (project_id, env) references environments (project_id, name)
);

create table servers (
  id         uuid primary key default gen_random_uuid(),
  project_id uuid not null references projects(id),
  node_id    uuid not null references nodes(id),
  version_id uuid not null references versions(id),
  env        text not null default 'dev',   -- (environments v1) env исполнения (денормализовано; берётся отсюда, НЕ join'ом к nodes — перевод ноды не переписывает историю)
  state      text not null default 'creating'
             check (state in ('creating','ready','allocated','draining','failed','reaped')),
  port       int  not null,
  players    int  not null default 0,
  tick_ms    real,
  match_id   uuid,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);
create index servers_ready_idx on servers (project_id, version_id, state);
-- (уточнено в v0) идемпотентность allocate по match_id при конкурентных запросах
create unique index servers_match_id_uidx on servers (match_id) where match_id is not null;

create table matches (
  id           uuid primary key default gen_random_uuid(),
  project_id   uuid not null references projects(id),
  server_id    uuid not null references servers(id),
  version_id   uuid not null references versions(id),
  region       text not null,
  env          text not null default 'dev',  -- (environments v1) env исполнения (денормализовано, для метрик/статистики; I6 — из строки, не из ноды)
  state        text not null default 'pending'
               check (state in ('pending','running','finished','aborted')),
  players_peak int not null default 0,
  started_at   timestamptz,
  ended_at     timestamptz,
  created_at   timestamptz not null default now()
);

create table fleet_configs (            -- desired state per (project, env, region)
  project_id      uuid not null references projects(id),
  env             text not null,             -- (environments v1) флот скоупится окружением
  region          text not null,
  active_version  uuid references versions(id),
  buffer_ready    int  not null default 2,   -- сколько ready держать
  max_servers     int  not null default 50,
  reap_ttl_min    int  not null default 180, -- добой deprecated-дедиков
  primary key (project_id, env, region),
  foreign key (project_id, env) references environments (project_id, name),
  -- (environments v1, C3) active_version обязан принадлежать тому же (project, env):
  foreign key (active_version, project_id, env) references versions (id, project_id, env)
);

create table events (                    -- аудит + лента для панели/алертов
  id      bigint generated always as identity primary key,
  ts      timestamptz not null default now(),
  kind    text not null,                 -- node_quarantine, server_failed, deploy_started...
  node_id uuid, server_id uuid, match_id uuid, version_id uuid,
  payload jsonb not null default '{}'
);
create index events_ts_idx on events (ts desc);

create table api_keys (
  id     uuid primary key default gen_random_uuid(),
  name   text not null,
  hash   text not null,                  -- bcrypt ключа
  scopes text[] not null,                -- {admin} {deploy} {matchmaking} {allocate} {readonly}
  project_id uuid references projects(id),      -- (environments v1) опциональная привязка ключа
  env    text,                                  -- строго парой с project_id (NULL/NULL = глобальный ключ)
  created_at timestamptz not null default now(),
  revoked_at timestamptz,
  check ((project_id is null) = (env is null)), -- полусвязанных ключей нет (I8)
  foreign key (project_id, env) references environments (project_id, name)
);
```

Миграции: `golang-migrate`, файлы в `master/migrations/`.

## 2. Флот-контроллер (reconcile)

Цикл каждые **1с** (один воркер на project×region, advisory lock в PG на случай двух master'ов по ошибке):

```
desired  = fleet_configs (active_version + версии в окне мультиверсий, buffer_ready)
observed = servers where state in (creating, ready) group by version
for each version в окне:
  deficit = buffer - count(creating+ready) → создать deficit серверов:
      node = спред: наименее занятая active-нода региона первой,
             где free_slots > 0 и heartbeat свежий            -- анти-аффинити
      insert servers(state='creating') + команда агенту StartServer
  surplus (только у deprecated-версий или при buffer↓) → reap старейших ready
failed-сервера: слот освобождается сам (insert нового сделает дефицит)
crash-loop: ≥3 failed одной версии на одной тачке за 10 мин → стоп созданий
            этой пары (version,node) на 15 мин + event crash_loop
```

**Размещение буфера — анти-аффинити (follow-ups итерации 5 §1):** дефицит раскладывается на наименее занятые active-ноды региона первыми (`order by used asc`), растягивая warm-буфер по железу; нода на полной ёмкости (`used = capacity_slots`) из размещения исключается вовсе. Плотная упаковка (bin-pack, busier-node-first) **отвергнута учением 5.2** — спред снижает blast-radius отказа одной ноды (buffer_ready=2 над двумя пустыми нодами → 1+1, а не 2+0).

Создание сервера — это команда агенту; переход `creating → ready` делает только heartbeat от агента (после `ready` от liba). (Уточнено в v0: `creating` без прогресса от агента дольше 120с → `failed` + событие — самолечение после потери StartServer/рестарта master; репорт `pulling` от агента обновляет `updated_at` и таймер не срабатывает.)

## 3. Allocation API (граница матчмейкер ↔ флот)

`POST /v1/allocate {project, region, version_id?, match_id}` → `{server_id, host, port}` | `409 no_capacity`

```sql
with c as (
  select s.id from servers s
  join nodes n on n.id = s.node_id
  where s.project_id = $1 and s.state = 'ready'
    and s.version_id = coalesce($2, s.version_id)
    and n.region = $3 and n.state = 'active'
    and n.last_heartbeat_at > now() - interval '10 seconds'
  order by s.created_at
  limit 1
  for update of s skip locked
)
update servers set state='allocated', match_id=$4, updated_at=now()
where id = (select id from c)
returning id, node_id, port;
```

Свойства: атомарно, p95 <50мс, тысячи/с. Идемпотентность: повторный запрос с тем же `match_id` возвращает уже выданный сервер. `no_capacity` → матчмейкер держит тикеты и ретраит (буфер тем временем восстанавливается reconcile'ом).

## 4. Матчмейкер v0

- Тикет: `{ticket_id, player_id, client_version, regions: [{region, rtt_ms}]}`. Очереди **in-memory** per (region, compat-bucket). Потеря при рестарте — ок: клиент ре-квьюится (см. `sdk.md`). (Уточнено в v0: статусы тикета `queued | matched | update_required | cancelled | expired`; TTL тикета в очереди — конфиг `ticket_ttl_s`, деф. 120с → `expired`; терминальные тикеты живут в памяти ещё ~2×TTL для поздних GET.)
- Тик каждые **500мс**: в каждой очереди собрать группы по `match_size` (конфиг per project — колонка `projects.match_size`, деф. 2, правится `PUT /v1/projects/{slug}`; уточнено в v0) → `allocate` (внутренний вызов store с новым `match_id` uuid, регионом и пиненой версией) → всем участникам `matched {host, port, match_id, join_token?}`. `no_capacity` → тикеты остаются в очереди, ретрай следующим тиком (буфер тем временем поднимает reconcile); заодно инкрементится `birdman_allocation_failures_total{no_capacity}` — алерт BufferEmpty видит и внутренние отказы.
- Выбор региона: минимальный медианный rtt по группе (группа — старейшие `match_size` тикетов, FIFO; тай-брейк — имя региона); если очередь региона < match_size дольше `widen_after_s` (деф. 30с) — расширяем на следующий по rtt регион игрока (ещё один регион за каждый следующий интервал).
- `client_version` не входит ни в один compat-bucket активных версий → ответ `update_required` (проверка на submit и на каждом тике — смена активной версии не оставляет несовместимые тикеты висеть). (Уточнено в v0: правило по умолчанию major.minor из `ops.md` §3, overrides-таблица — позже; «активные версии» региона = `fleet_configs.active_version` окружения тикета (+ окно deprecated того же env — §Окружения; прежний ранг `versions(state=active, channel=prod)` снят вместе с `channel`), пока deploy-менеджер не переводит state — см. §5; при полном отсутствии активных версий тикеты ждут в очереди до TTL, а не получают `update_required`.)
- `join_token` = HMAC(match_id, player_id, exp 60с) — дедик проверяет через liba (анти-«зашёл мимо матчмейкера»); v0 — опционально, включается флагом (`matchmaking.join_token.enabled`, секрет — env `BIRDMAN_MM_JOIN_SECRET`; выдача реализована, проверка на дедике — TODO liba). Это **авторизация захода на дедик, а не аутентификация игрока**, и пока никем не проверяется: агент фрейм `verify_token` не шлёт, liba его игнорирует как неизвестный тип — гарантией считать нельзя (`architecture.md`, «Модель доверия»).
- Анти-дубль: один активный тикет на player_id (новый вытесняет старый → `cancelled`); ключ — (project, env, player_id), окружения игрока независимы.
- `player_id` — **непрозрачная строка от вызывающего**, master ей доверяет и нигде её не персистит; аутентификации игрока в birdman нет. Предусловие: тикеты заводит держатель `matchmaking`-ключа = бэкенд игры, не игровой клиент (ключ даёт право на ЛЮБОЙ `player_id`, а также чтение/отмену чужого тикета по `ticket_id`). Целиком — `architecture.md`, «Модель доверия (trust boundaries)».
- (Уточнено в v0.) Проект тикета: поле `project` опционально — по умолчанию `matchmaking.default_project` из конфига либо единственный проект в БД; успешная аллокация пишет строку в `matches` (state `pending`).
- Метрики: `birdman_mm_queue_depth{region,env}` (по лучшему региону тикета и окружению; environments v1 §7), `birdman_mm_time_to_match_seconds` (histogram), `birdman_mm_tickets_total{result}`.
- Явно вне v0: скиллы, пати, бэкфилл, реконнект в матч.

## 5. Deploy-менеджер (мягкий деплой)

Состояния версии: `registered → prepulling → active → deprecated → disabled`.

```
POST /v1/deploy {version_id}:
  1. version.state = prepulling; всем активным тачкам региона — PrePull(image)
  2. все тачки отчитались pulled (или таймаут 15 мин → abort + event)
  3. транзакция: старая active → deprecated; новая → active; fleet_config.active_version = new
  4. reconcile сам: поднимает буфер новой; deprecated-дедики новых матчей не получают
  5. reap deprecated: players==0 → stop; либо по reap_ttl_min — Drain с дедлайном (liba получает drain)
POST /v1/rollback: шаг 3 в обратную сторону (образы уже на тачках) — секунды
```

Окно мультиверсий: буферы считаются для **обеих** версий (active — полный buffer, deprecated — min(2, buffer)); учитывать при capacity-планировании тачек. Матчмейкер в окне матчит старых клиентов на deprecated, если политика `compat` разрешает (см. `ops.md` §Версии).

> **(Уточнено в v0, итерация 3 — deploy-менеджер реализован.)**
> - Состояния версий — **project-global**; на проект одновременно живёт максимум **одна** deprecated: при флипе более старые deprecated уходят в `disabled` (событие `version_disabled`). `disabled` недоступна для деплоя и матчинга.
> - Флип демотирует не только `state=active`, но и версию, на которую указывает `fleet_configs.active_version`, оставаясь `registered` (bootstrap через `PUT /v1/fleets` до появления deploy-менеджера) — иначе старый билд выпал бы из окна и его живые матчи получили бы drain немедленно.
> - **Окно закрывает `reap_ttl_min`**: по его истечении (от `versions.deprecated_at`, максимум по флитам проекта) deprecated → `disabled` — reconcile реапит её ready-буфер, живым матчам шлёт per-server `DrainServer{deadline_s=300}` (protocol.md §1; ровно один раз — сервер помечается `draining`), матчмейкер перестаёт её предлагать (старые клиенты получают `update_required`).
> - PrePull-таргеты — active-ноды с heartbeat <30с в регионах флитов проекта; нет живых нод → флип сразу (образам некуда греться). Repeated `POST /v1/deploy` идемпотентен (prepulling → отчёт о прогрессе, active → no-op); одновременно prepull'ится максимум одна версия проекта; `failed` PullReport абортит деплой сразу. Стейт ожидания prepull — in-memory, маркер — `versions.state=prepulling`: после рестарта master деплой резюмируется с новым 15-мин таймаутом (PrePull идемпотентен и дёшев на тёплом кэше).
> - `POST /v1/rollback {project?, region?}`: project можно опустить при единственном проекте; `region` ограничивает переключение `fleet_configs.active_version` этим регионом (состояния версий остаются глобальными); наружу — событие `deploy_rolled_back`.
> - События: `deploy_started`, `deploy_node_pulled`, `deploy_activated`, `deploy_failed`, `deploy_rolled_back`, `version_disabled`, `server_drain`. Метрики: `birdman_deploy_prepull_seconds` (histogram), `birdman_versions{project,env,state}`.
> - `PUT /v1/fleets/{region}` больше не сбрасывает `active_version` при отсутствии поля (nil → оставить как есть): владелец флипов — deploy-менеджер, прямое назначение осталось как bootstrap/ops-override.

## Окружения (environments v1)

> Миграция `000013_environments` + сиды в `ensureProject`. Окружение — **полноценное измерение** платформы, не лейбл: колонка `env` у versions/fleets/nodes/servers/matches, active-версия и окно deprecated скоупятся per (project, env). Прежний `versions.channel` (staging/prod) снят — он был **зрелостью билда, а не размещением**. Стейт-машина версий (§5) не изменилась — изменился только скоуп.

**Таблица `environments` (per-project) — флаги ведут политику, не имя:**

| Колонка | Смысл |
|---|---|
| `name` | имя (`^[a-z0-9][a-z0-9-]{0,31}$`, кроме `all`/`global`); иммутабельно |
| `production` (bool) | политика prod: авто-деплой запрещён, ретеншн-дефолт ∞, алерты critical |
| `auto_deploy` (bool) | регистрация версии сразу prepull→activate |
| `retention_keep` (int≥0) | сколько версий держать сверх окна; 0 = ∞ |

Сид каждому проекту: `dev` (production=false, auto_deploy=true, keep=20) + `prod` (production=true, auto_deploy=false, keep=0). **Guardrail-инвариант `auto_deploy=true ТОЛЬКО при production=false`** — CHECK в БД (`check (not (production and auto_deploy))`), валидация API и панель. FK-нарушения (нет такого env) в CreateVersion/UpsertFleet/CreateAPIKey мапятся в понятный `400 «no such environment <project>/<env>»`, не сырой 500.

**env как измерение (§1 DDL):**
- **versions:** unique (project, env, semver); `promoted_from` — provenance промоута; составной unique (id, project, env) — опора для FK флота.
- **fleet_configs:** PK (project, env, region); `active_version` обязан принадлежать тому же (project, env) — составной FK `fleet_active_version_env_fk` закрывает «dev-флоту prod-версию» на уровне БД.
- **nodes:** нода принадлежит ровно одному env; новые входят как `dev` (никогда prod неявно); перевод — явный `PATCH /v1/nodes/{id} {env}` (любой стейт кроме `dead`, без живых servers; иначе 409; событие `node_env_changed {from,to}`).
- **servers/matches:** денормализованный `env` строки исполнения — env ВСЕГДА из этой колонки, НИКОГДА join'ом к nodes (I6: перевод ноды не переписывает историю).
- **api_keys:** опциональная привязка `(project_id, env)` — строго парой (`check ((project_id is null) = (env is null))`); NULL/NULL = глобальный ключ (существующие работают как раньше).

**Удаление окружения (снимает прежний тупик «использованный env неудаляем», I10).** `DELETE /v1/environments/{project}/{name}` сносит окружение ВМЕСТЕ с содержимым — одной транзакцией, в порядке FK: `matches` → `servers` → `fleet_configs` (сначала `active_version = null`) → `versions` (у версий-потомков из других env обнуляется `promoted_from` — self-FK) → `match_stats_daily` → привязанные `api_keys` (**revoke**, не удаление: ключ отзывается и потому не может «стать глобальным»; привязку снимает FK `api_keys_env_fk`, утраченная пара сохраняется в событии `apikey_revoked {project, env, reason: environment_deleted}`) → строка `environments`. Событие `environment_deleted {project, name, production, versions, fleets, matches, servers, api_keys_revoked}` пишется в той же транзакции. **Ноды не каскадятся никогда:** предусловие — ноль нод в окружении (иначе 409), иначе живой агент остался бы с `node_id` в несуществующем env; побочно это гарантирует отсутствие живых серверов (guard перевода ноды их не пускает). Непустое окружение требует подтверждения — тело `{"confirm":"<name>"}` с ТОЧНЫМ именем (иначе 400); пустое (никогда не использованное) удаляется без confirm (204, как раньше).

**Стейт-машина per (project, env):** инвариант «одна active + одно окно deprecated» — per (project, env). `ActivateVersion` флипает строго внутри (project, env) — dev-флип не трогает prod-active; старшие deprecated → disabled. `DisableExpiredDeprecated` берёт `max(reap_ttl_min)` флотов **именно этого env** версии. Reconcile `PlanFleet` идёт по (project, env, region), **все шесть выборок серверов** скоупятся `s.env` (dev-проход не реапит prod-серверы того же региона); advisory-lock `project:env:region`. Deploy-менеджер: prepull-цели — active-ноды `env=version.env`; in-flight-ключ и busy-check — per (project, env); `HandlePullReport` матчит job по **(image_ref, нода ∈ pending)** — параллельные деплои одного ref в dev и prod (промоут!) не съедают отчёты друг друга.

**Промоут (`POST /v1/promote {version_id, to_env}`, scope deploy):** регистрирует версию в to_env (тот же project/semver/image_ref, `promoted_from`, state registered) + немедленный деплой; откат — существующий rollback. Идемпотентен: та же (project, to_env, semver) с **тем же** image_ref в registered → переиспользуется; с иным ref → 409. Привязанный ключ — только в свой env. **Поведение при занятом env:** если в (project, to_env) уже идёт деплой — промоут возвращает **409** (как у deploy), но версия УЖЕ зарегистрирована в to_env; после завершения текущего деплоя **повторите промоут** — он идемпотентно переиспользует registered-строку и запустит деплой. (Цепочка «только вперёд» — механизм auto_deploy-окружений; production-env авто-деплоя не имеет по guardrail'у — автоматического отложенного флипа прода не происходит, осознанно.)

**Авто-деплой — «только вперёд»:** мастер держит per (project, env) отметку `last_attempted` (created_at+id последней версии, которую пытался катить авто-деплой). Регистрация в auto_deploy-env: нет in-flight деплоя (project, env) → BeginDeploy сразу (+отметка); иначе версия ждёт registered. На **любом** завершении деплоя (activate из HandlePullReport, мгновенный activate при нуле живых нод, expire-таймаут, failed-report, abort) менеджер деплоит **новейшую registered строго новее `last_attempted`** (tie-break по id — CI-burst в одну секунду). Упавшая версия НЕ ретраится (вечный цикл битого образа исключён) — её вытеснит следующий билд либо ручной deploy. **Burst-грань:** событие `deploy_started` в авто-пути несёт `{"auto": true, "skipped": N}` — сколько промежуточных версий пропущено (при burst'е активируется только последняя). `Resume` при рестарте восстанавливает и in-flight, и цепочку (включая версию, зависшую prepulling без job'а после ошибки PrePullTargets).

**Ретеншн (`RetireVersions`, субтик реконсайл-лупа ~60с):** для env с `retention_keep>0` версии `registered|disabled` ранжируются по created_at desc; `registered` за позицией keep **и** старше 1ч → `disabled` (единственный путь registered→disabled; active/prepulling/deprecated не ранжируются и не трогаются; 1ч-защита от гонки с auto-deploy-очередью). Событие `version_retired {semver, env}`. Образ снятой версии снимается с нод окружения командой `RemoveImage` (`protocol.md` §1, `agent.md` §6), watermark-GC — страховка.

**Снятие образов — два такта (Фаза D 14.07.2026 показала, что одного мало).** (1) *Быстрый путь:* RemoveImage уходит немедленно при ЛЮБОМ переходе версии в `disabled` (флип-демоут, TTL, ретеншн) — fire-and-forget, без маркера. Но disabled-переход происходит ровно тогда, когда серверы этой версии ещё дренятся (grace 30с), поэтому агент типично отвечает `busy` («образ занят живым контейнером») и ничего не удаляет — повторить команду в старом дизайне было некому. (2) *Сходящийся sweep* (миграция `000015`, колонка `versions.image_cleanup_at`): тот же ~60с субтик выбирает версии, у которых `state='disabled'` **и** маркер пуст **и** **нет живых серверов версии** (`creating|ready|allocated|draining`) **и** образ не держит не-disabled версия того же (project, env) **и** в окружении нет `down`-ноды (batch-гигиена: окружение, заблокированное упавшей нодой, не выедает `limit 200` у здоровых; короткий офлайн до `node_down_after_min` держит HasSession-гейт) — и шлёт `RemoveImage` нодам окружения. Выборка ограничена `limit 200` — остаток доберётся следующим проходом.

> **Маркер — ПО ОТЧЁТАМ агента, а не по факту отправки.** Раньше sweep штамповал `image_cleanup_at` вслепую: протокол не нёс результата RemoveImage (`Ack` подтверждает лишь ПРИЁМ команды), поэтому пропущенное удаление — `busy` (образ под дренящимся контейнером) или `error` — терялось навсегда: версия выпадала из выборки, повторить было некому, образ доживал до watermark-GC. Теперь агент отвечает `ImageReport{removed|absent|busy|error}` (`protocol.md` §1), и маркер ставится, только когда **каждая** целевая нода окружения подтвердила, что образа у неё нет (`removed`|`absent` — равносильны). `busy`/`error` → маркера нет → версия остаётся в выборке → следующий субтик шлёт снова (дубль идемпотентен). Ожидания живут in-memory: рестарт мастера их теряет, но и маркера у версии нет, так что следующий субтик просто переотправит — сходимость по построению. Наблюдаемость: `birdman_image_removals_total{status}` (`ops.md` §1) — флот, где удаления стабильно возвращаются `busy`/`error`, теперь видно.
>
> **Офлайн-ноды: sweep не шлёт им НИЧЕГО.** Он повторитель (приходит к непомеченной версии каждые 60с), а команда офлайн-ноде лишь паркуется в in-memory очереди хаба — за сутки офлайна там копились бы тысячи дубликатов, вываливающихся на агента разом при реконнекте. Поэтому: нет живой сессии хоть у одной целевой ноды окружения → версия пропускается целиком (ни команды, ни маркера). Терять нечего — образ с офлайн-ноды всё равно не снять; вернётся нода — субтик отработает штатно. Не вернётся никогда — `dead` ставит ТОЛЬКО ручная ревокация (о пропаже кричит NodeDown-алерт); до неё долго-офлайн нода уходит в `down` (lease-checker после `node_down_after_min`), и её окружение выпадает уже из sweep-выборки (условие выше), а диск живых нод страхует watermark-GC. HasSession-гейт при этом остаётся боевым для короткого офлайна (сессия упала, `down` ещё не наступил) и как защита от гонки «выборка↔статус ноды». Быстрый путь остаётся fire-and-forget (одна отправка на disabled-переход, не повторитель) — его команда штатно доедет из очереди на реконнекте.

**Привязка ключей (enforcement, `requireScopeEnv`):** для скоупов deploy/matchmaking/allocate привязанный ключ обязан совпасть с (project, env) целевого ресурса (versions/deploy/promote/rollback — по env версии/цели; тикеты — project+env берутся/валидируются из привязки на создании **и сверяются с (project, env) самого тикета на чтении и отмене**, tracker #963; allocate — по env флота). Несовпадение → `403 {"detail":"key is bound to <project>/<env>"}` — **кроме чтения/отмены тикета: там `404` тем же телом, что и у несуществующего `ticket_id`** (403 назвал бы чужой тикет существующим, то есть раздал бы оракул по uuid); гейт стоит ДО rate-limit'а, который ключуется по `player_id` тикета, то есть по жертве. Привязанный ключ также **дефолтит project** (не только валидирует). NULL-привязка = глобальный ключ. Привязка несовместима со скоупом `admin` (400).

**BIRDMAN_ENV в дедике:** существующий map `StartServer.env` (`protocol.md` §1) несёт `BIRDMAN_ENV=<name>` — игровой сервер знает своё окружение (конфиги/аналитика). Ноль диффов proto/liba.

## 6. Публичный REST API (сводка; OpenAPI — в `master/api/openapi.yaml`)

| Метод | Скоуп | Назначение |
|---|---|---|
| `POST /v1/matchmaking/tickets` | matchmaking | создать тикет |
| `GET /v1/matchmaking/tickets/{id}` (+`?wait=25s` long-poll / SSE `/stream`) | matchmaking | статус: queued / matched{host,port,match_id,join_token} / update_required / cancelled; привязанный ключ — только тикеты своей (project, env), чужой → `404` как несуществующий (tracker #963) |
| `DELETE /v1/matchmaking/tickets/{id}` | matchmaking | отмена; та же проверка привязки, что и на чтении (чужой тикет → `404`, отмена не проходит) |
| `GET /v1/qos?env=&project=` | public | пинг-эндпоинты нод env `[{region, host, udp_port}]`; без env — единственный env с активными нодами, иначе `400 env_required` (environments v1 §3); `project` НЕ валидируется — единственное исключение среди чтений, ручка публичная (правило под таблицей) |
| `POST /v1/allocate` | allocate | граница флота (см. §3); `env?` резолвится (явное → привязка ключа → единственный env с ready → `409`) |
| `GET /v1/nodes` · `/v1/servers` · `/v1/matches` · `/v1/versions` | readonly | списки с фильтрами; все четыре принимают `?project=` (мультипроект W2 — панель сужает по выбранному проекту на сервере; слаг валидируется, опечатка → 400, см. правило под таблицей), `/v1/nodes` и `/v1/versions` дополнительно `?env=`; пустой фильтр = всё, как раньше. `/v1/nodes` (v1) отдаёт аддитивные cert-поля `cert_serial`, `cert_not_after`, `enrolled_at` (nullable). События (`/v1/events`) проектом НЕ сужаются: в таблице нет `project_id`, панель убирает лишь события явно чужого проекта |
| `GET /v1/ca` | readonly | публичный PEM-бандл активных внутренних CA (`text/plain`) — для ansible (кладёт `master-ca.pem` на ноды) и отладки; приватный ключ CA неоткуда прочитать by construction (mTLS v1, `protocol.md` §Auth) |
| `GET /v1/events/stream` (SSE) | readonly | live-лента для панели |
| `PUT /v1/fleets/{region}` | admin | buffer, max_servers, reap_ttl; **`env` в теле обязателен** (environments v1 §2; валидация active_version×env — БД-FK + 400) |
| `GET /v1/projects` | readonly | список проектов `[{id, slug, match_size, created_at}]`, старейший первым — источник селектора проекта в панели (мультипроект W1). Readonly по замыслу: панель обязана показывать, в каком проекте работает, и readonly-сессии тоже |
| `PUT /v1/projects/{slug}` | admin | match_size проекта (уточнено в v0) |
| `POST /v1/projects` | admin | явное создание проекта: 201, `409` на занятом слаге (в отличие от идемпотентного PUT, который молча перезаписал бы чужой `match_size`). Событие `project_created` |
| `GET /v1/projects/{slug}/usage` | admin | состав проекта (окружения/версии/флоты/живые ноды/матчи/серверы/ключи) — панель показывает его ДО подтверждения удаления |
| `DELETE /v1/projects/{slug}` | admin | удаление каскадом, тело `{"confirm":"<slug>"}`. `409`, пока есть ЖИВЫЕ ноды (выведенные каскадятся); `204` для пустого проекта; `400` без точного confirm; ключи проекта отзываются необратимо. Событие `project_deleted` |
| `POST /v1/versions` | deploy | регистрация билда из CI; **`env` обязателен** (заменил `channel`); привязанный ключ — только свой env |
| `POST /v1/deploy` · `/v1/rollback` | deploy | см. §5; rollback скоупится env версии — `env` обязателен при >1 env с окном deprecated, иначе sole-fallback |
| `POST /v1/promote` | deploy | промоут `{version_id, to_env}` → регистрация в to_env (тот же image_ref, `promoted_from`) + деплой; идемпотентен; привязанный ключ — только в свой env (см. §Окружения) |
| `GET /v1/environments?project=` | readonly | список окружений `[{name, production, auto_deploy, retention_keep, created_at}]`; `project` валидируется (опечатка → 400, а не пустой список), пустой резолвится в единственный проект, при нескольких — `400 project is required` |
| `GET /v1/environments/{project}/{name}/usage` | readonly | состав окружения `{versions, fleets, nodes, servers, matches, api_keys}` — что снесёт удаление (панель показывает его в диалоге) |
| `POST /v1/environments` · `PATCH /v1/environments/{project}/{name}` | admin | CRUD окружений; guardrail production×auto_deploy → 400 |
| `DELETE /v1/environments/{project}/{name}` | admin | удаление ВМЕСТЕ с содержимым (versions/fleets/matches/servers каскадом, привязанные ключи → revoke; событие `environment_deleted`). Предусловие: **ноль нод** (иначе 409 — сначала переведите их, `PATCH /v1/nodes/{id}`). Непустое окружение требует тела `{"confirm":"<name>"}` с ТОЧНЫМ именем (иначе 400) → `200 {deleted:{…}}`; пустое удаляется без confirm → 204 |
| `PATCH /v1/nodes/{id} {env}` | admin | перевод ноды в env (любой стейт кроме `dead`, без живых servers; иначе 409; событие `node_env_changed`) |
| `POST /v1/nodes/{id}/drain` · `/undrain` | admin | вывод тачки |
| `POST /v1/nodes/{id}/revoke` | admin | вывод ноды из флота навсегда: `state=dead`, событие `node_revoked`. 409, если есть живые серверы (живую ноду выводит drain). Идемпотентно. Строка ноды остаётся — на неё ссылается история серверов |
| `GET /v1/servers/{id}/logs?follow=&tail=` | readonly | проксирование tail с агента (уточнено в v0: readonly — панель показывает логи) |
| `POST /v1/agent-upgrade` | admin | self-upgrade агента(ов): `{url,sha256,version,node_id?}` (уточнено в v0) |
| `GET /v1/metrics/query` · `/query_range` | readonly | read-only прокси к VictoriaMetrics для панели (уточнено в v0) |
| `GET /v1/logs/query` | readonly | read-only прокси к VictoriaLogs для панели: история/поиск логов по флоту (Логи v1) |
| `GET/POST /v1/apikeys` · `DELETE /v1/apikeys/{id}` (`?purge=true` — hard-delete revoked, Реестры v1) | admin | управление API-ключами (панель «Админка»; секрет — один раз) |
| `GET/POST /v1/registries` · `PATCH`/`DELETE /v1/registries/{id}` | admin | приватные registry-креды для pull агентов (Реестры v1 + v2: тип ghcr/gar/generic, PATCH-edit; токен write-only, раздача агентам по agentlink — `protocol.md` §1) |
| `GET /v1/stats/overview` · `/v1/stats/cost` (`?days=N&project=&env=`) | readonly | агрегаты статистики/cost-view (панель П2; rollup-backed, потолок 30д). `project` (мультипроект W3) сужает ВСЁ, включая пик CCU; `env` — всё, КРОМЕ пика (решение I5). Опечатка в любом из двух → 400, а не молча пустой ряд |
| `GET /v1/alerts/rules` · `/history` · `/active` (`?project=` — только `/history`·`/active`) | readonly | правила/история/активные алерты vmalert (панель П2). `project` (мультипроект W2, tracker #955) сужает **не скрывающе**: уходит только алерт ЯВНО чужого проекта, платформенный (без лейбла) виден всегда; сам слаг валидируется (tracker #961) — опечатка даёт 400, а не экран, неотличимый от «всё спокойно». `rules` — каталог конфигурации, не фильтруется |
| `POST/GET /v1/alerts/mutes` · `DELETE /v1/alerts/mutes/{id}` | admin/readonly/admin | mute алертов (панель П2; источник истины — master, зеркалируется в alertmanager silence best-effort) |
| `POST/GET/DELETE /v1/session` | — (login по API-ключу) | cookie-сессия панели (уточнено в v0) |
| `GET /metrics` | локально | Prometheus-метрики master |

**Привязка ключа на чтении (tracker #974).** `GET /v1/matches/{id}` энфорсит привязку `(project, env)` ключа: привязанный ключ ЛЮБОГО скоупа (включая `readonly`) читает только матчи своей пары, чужой → `403 key is bound to <project>/<env>`. Исторически энфорсмент стоял лишь на deploy/matchmaking/allocate, и матч чужого проекта отдавался по uuid молча — при том, что листинг с #961 уже валидирует `?project=`. Глобальный ключ и сессия панели, как и раньше, видят всё. Матч теперь отдаёт своё окружение полем `env` (колонка была в схеме с environments v1, в API не выходила — без неё привязку не проверить).

**Валидация `?project=` — ЕДИНОЕ правило API (tracker #961).** Слаг проекта, пришедший в `?project=` на **аутентифицированном чтении**, ВСЕГДА валидируется по БД: неизвестный → `400 {"error":"bad_request","detail":"no such project <slug>"}` (общий вход — `httpapi.projectFilter`). Пустой параметр = вся платформа, как до мультипроекта. Правило заведено потому, что до него его не было: листинги W2 (`/v1/{nodes,servers,versions,matches}`) и алерты (#955) слаг не проверяли, `/v1/stats/*` (W3) проверял — API отвечал на один и тот же параметр двумя способами, и каждая следующая волна выбирала наугад. Больнее всего расхождение било по алертам: там пустой экран — ЖЕЛАННОЕ состояние, поэтому «алертов нет» и «я опечатался в проекте» выглядели одинаково и оба радовали. **400, а не 404:** сам ресурс (список/статистика) существует, плох ВВОД в query-параметре — тот же класс, что `?days=`/`?limit=`/`?state=`. Оракулом существования чужих сущностей ручка при этом не становится: все они readonly-скоупа, а `GET /v1/projects` того же скоупа и так перечисляет все слаги. Панель опечатку послать не может по построению (`resolveProject` отдаёт только слаг из ответа `/v1/projects`, а удаления проектов в API нет вовсе) — 400 адресован человеку с curl'ом и скриптам. **Исключений ровно три, каждое по своей причине:** (1) `GET /v1/qos` — ручка ПУБЛИЧНАЯ, 400 «no such project» раздал бы оракул слагов кому угодно, аргумент «у вызывающего и так есть readonly» там не работает → resolve-or-empty; (2) `GET /v1/alerts/rules` — фильтр не принимает вовсе (каталог конфигурации); (3) `project` в ТЕЛЕ `POST /v1/alerts/mutes` — матчер хранимого правила, а не фильтр над данными (см. ниже). Добавляешь `?project=` на новую ручку — либо валидируй, либо впиши сюда четвёртое исключение с причиной. **`?env=` сведён к тому же правилу (tracker #971).** Общий вход — `httpapi.scopeFilter` (там же, где `projectFilter`): при заданном `project` имя окружения проверяется В ЭТОМ проекте (пара, как у deploy/versions/promote), без проекта — достаточно существования имени хоть у одного проекта. Опечатка → `400 no such environment …` одинаково на `/v1/stats/*`, `/v1/nodes` и `/v1/versions`. До этого валидировала только статистика, а листинги молча отдавали пустой список — тот же класс, что был у `?project=` до #961.

Аутентификация: `Authorization: Bearer <api-key>`; скоупы из таблицы `api_keys`. Скоуп `matchmaking` ограничен тремя ручками тикетов, но внутри них ограничен слабо: любой `player_id`, чтение и отмена любого тикета по `ticket_id` — владения тикетом нет вовсе (v0). Границей служит только привязка ключа к (project, env), и она проверяется на всех трёх ручках (tracker #963; до него — лишь на создании, `httpapi/matchmaking.go`): привязанный ключ работает только с тикетами своей пары, чужой отвечает `404` неотличимо от несуществующего. Глобальный (NULL/NULL) ключ, как и раньше, достаёт до всех проектов. Поэтому ключ рассчитан на **бэкенд игры**, а не на игровой клиент: вшить его в клиент = раздать общий секрет и потерять доверие к `player_id` (rate-limit — 5 rps per player_id, in-memory, это глушилка абьюза, не граница безопасности; per-IP лимита в master нет). Разбор границ — `architecture.md`, «Модель доверия (trust boundaries)».

(Уточнено в v0.) Реализовано подмножество: nodes/servers/versions/fleets/projects/events/allocate + matchmaking-тикеты (long-poll `?wait=`, кап 30с), `/v1/qos`, `/healthz`, `/metrics`; **итерация 3: `POST /v1/deploy` (202 prepulling / 200 active) и `POST /v1/rollback`** — см. §5; скоуп `admin` включает остальные скоупы; при пустой `api_keys` master при старте генерирует admin-ключ и печатает его в лог один раз; `PUT /v1/fleets/{region}` дополнительно принимает `active_version` (bootstrap/ops-override; флипы делает deploy-менеджер); проекты создаются неявно при первом упоминании slug в `POST /v1/nodes` / `POST /v1/versions` / `PUT /v1/projects/{slug}`; rate-limit матчмейкинга — 5 rps per player_id, in-memory token bucket → `429 rate_limited` (`protocol.md` §3); `GET /v1/qos` отдаёт живые ноды (active, heartbeat <30с) с `udp_port:19999` — сам UDP-echo приезжает в агенте в итерации 4; ошибки — плоский JSON `{"error","detail"}`, не RFC 7807.

(Уточнено в v0, панель П0.) **SSE `/v1/events/stream` реализован** (скоуп readonly+): новые строки `events` стримятся как `id:`/`event: <kind>`/`data: <json>` по курсору `events.id` (poll PG ~1с), keepalive-коммент каждые 15с, реконнект без потерь через `?after_id=` или `Last-Event-ID`. **`GET /v1/matches`** — read-model с join (semver, host, port, живые `server_players`), фильтры `state|region|project`, `limit/offset`, сортировка `created_at desc`; **`GET /v1/matches/{id}`**. **Сессии панели:** `POST /v1/session {api_key}` → HttpOnly Secure SameSite=Lax cookie (in-memory store, TTL 24ч, скоупы ключа), `GET /v1/session` → `{scopes,name}`, `DELETE /v1/session`; auth-middleware принимает Bearer ИЛИ cookie, для не-GET по cookie обязателен заголовок `X-Birdman-Csrf: 1`. **Панель встроена в бинарь** (`go:embed`, `internal/panelui`): `/` и `/assets/*` — статика с SPA-fallback, `/v1/*` без маршрута — JSON 404; без сборки панели бинарь отдаёт placeholder (сборка — `panel/build.sh`, детали в `master/README.md`).

(Уточнено в v0, итерация 4 — реализовано.) Операционные эндпоинты (тело — `internal/httpapi/ops.go`, `server.go` тронут минимально: только маршруты): **`POST /v1/nodes/{id}/drain` · `/undrain`** (admin) — `node.state=draining`, событие `node_drain`/`node_undrain`, агенту команда `Drain`/`Undrain`; reconcile (`PlanFleet`) реапит ready-серверы на draining-нодах (warm pool переезжает на active-ноды), allocated доигрывают. **`POST /v1/nodes/{id}/revoke`** (admin) — единственный путь в `dead`: автоматика доводит молчащую ноду только до `down`. Ревокация значит «бокса больше нет»: окружение перестаёт блокировать sweep снятия образов (его выборка смотрит `state='down'`), метрики ноды перестают эмититься (`state <> 'dead'`), и панель убирает её из списка. Агенту ничего не шлётся, воскрешения нет — Hello поднимает состояние только из `quarantine|down`. **`GET /v1/servers/{id}/logs?follow=&tail=`** (readonly, не admin — панель P1 показывает логи) — `TailLogs` агенту, проксирование chunked-стрима `LogChunk` через `agentlink.LogRouter`; работает для reaped/failed (логи и `.gz` живут на ноде ретенцию); отмена tail при дисконнекте клиента. **`POST /v1/agent-upgrade`** (admin) — `UpgradeAgent` ноде(ам), событие `agent_upgrade`; watchdog через 70с сверяет re-Hello `agent_version` → `agent_upgrade_succeeded`/`agent_upgrade_failed` (вход алерта AgentUpgradeFailed). **`GET /v1/metrics/query` · `/query_range`** (readonly) — тонкий прокси к VictoriaMetrics (`metrics.victoriametrics_url` в конфиге, деф. `http://127.0.0.1:8428`; пусто → 503). Новые метрики (`ops.md` §1): `birdman_events_total{kind}` (DB-derived counter, append-only — переживает рестарт), `birdman_matches_running`, `birdman_players_online`.

(Уточнено в v0, панель П2 — реализовано; тело — `internal/httpapi/{apikeys,stats,alerts}.go`, `store/{apikeys,stats,alerts}.go`; `server.go` тронут минимально: только маршруты + сеттеры `WithAlertsSources`/`WithSilenceMirror`. Mute алертов — таблица `alert_mutes` (миграция `000004`; `silence_id` — `000016`; `project` — `000018`), зеркалирование в alertmanager — `internal/amsilence`.)

- **`GET /v1/apikeys`** (admin) → `{id,name,scopes,created_at,revoked_at}` **без секрета**. **`POST /v1/apikeys {name,scopes[]}`** — валидирует `scopes ⊆ {admin,deploy,matchmaking,allocate,readonly}` (дедуп/сортировка), возвращает секрет **один раз** (`{key, secret}`; хранится только bcrypt), событие `apikey_created` (без секрета в payload). **`DELETE /v1/apikeys/{id}`** — revoke (проставляет `revoked_at`, идемпотентно), событие `apikey_revoked`; **запрет отзыва последнего активного admin-ключа → 409** `last_admin_key` (защита от self-lockout, проверка+запись в одной транзакции `FOR UPDATE`). Отзыв инвалидирует не только БД-путь (`AuthAPIKey` фильтрует `revoked_at is null`), но и **in-memory кэш аутентификатора и cookie-сессии** этого ключа → ключ перестаёт работать сразу, а не через TTL кэша (5м).
- (Реестры v1.) **`DELETE /v1/apikeys/{id}?purge=true`** (`purge=1` тоже принимается) — hard-delete строки, но **только для уже отозванного** ключа: `204` на revoked; **`409 not_revoked`** на активном (purge никогда не отзывает сам от своего имени — тот же роут дважды не эскалирует revoke в перманентное удаление); `404` на неизвестном id или уже запурженном (ретрай purge идемпотентен по эффекту, а не по коду — второй вызов отвечает 404, не повторным 204). Обычный `DELETE` без `?purge=` — поведение байт-в-байт прежнее (регресс-тест). Событие **`apikey_purged`** (payload `{name}`, без хэшей и id) — согласуется с `apikey_created`/`apikey_revoked`, события отозванного ключа в `events` остаются нетронутыми. Purge, как и revoke, вызывает `invalidateKey(id)` в auth-кэше (defense-in-depth — ключ уже отвергнут БД-путём, но кэш/сессии подчищаются тоже).
- (Реестры v1 — реализовано; тело — `internal/httpapi/registries.go`, `store/registries.go`; раздача агентам — `internal/agentlink/{hub,service}.go`.) **`GET /v1/registries`** (admin) → `{registries:[{id,host,username,note,created_at,updated_at}]}` — **без токенов**: секрет неоткуда прочитать, `store.ListRegistryCreds` (единственный читатель токена) используется только сборкой agentlink-снапшота, никогда HTTP. **`POST /v1/registries {host,username,token,note?}`** — upsert по нормализованному host (`on conflict (host) do update`): повторный POST того же host целиком заменяет username/token/note — единственный способ ротации токена, «поменять только note» без токена формой не предусмотрено. Host валидируется (`store.NormalizeRegistryHost`): непустой, без схемы/пути, lowercase, **`docker.io`/`index.docker.io` отклоняются** `400 bad_request` (агент не смог бы host-match'ить их exact-match против `registry-1.docker.io`, в который их резолвит containerd) — ответ на успех `201 {registry:{…без token}}`. **`DELETE /v1/registries/{id}`** → `204`/`404`/`400` (не-uuid). События аудита `registry_upserted`/`registry_removed` — payload `{host,username}`, токен нигде не логируется и не попадает в события. После любого успешного изменения — раздача полного снапшота всем подключённым агентам по agentlink (`SetRegistries`, `protocol.md` §1; `Service.BroadcastRegistries`, подключён через хук `WithRegistriesHook` в `cmd/birdman-master/main.go`), плюс тот же снапшот каждому агенту при (пере)подключении — до реплея pending-команд (`agentlink/hub.go`, `attach`), чтобы реплеенный StartServer/PrePull приватного образа не обогнал креды.
- (Реестры **v2** — типизированные реестры + edit.) У записи появился **`type ∈ {ghcr,gar,generic}`** (миграция `000010`, бэкфилл `ghcr.io→ghcr`). `GET`/`POST`-ответы несут `type`. **Мастер нормализует любой тип в docker-basic-auth `(username, secret)` на записи** (`store.ValidateRegistry`): `gar` → форсит `username=_json_key` и требует, чтобы токен был JSON-ключом сервис-аккаунта (`{"type":"service_account","private_key":…}`); `ghcr` → host обязан быть `ghcr.io`; `gar` → host матчит `*-docker.pkg.dev`/`gcr.io`; `generic` → правила v1 (`docker.io` отклоняется). Поэтому **`ListRegistryCreds`/`SetRegistries`/agent/proto не меняются** — тип агенту не уходит, замороженный контракт цел. **`PATCH /v1/registries/{id} {type?,username?,token?,note?}`** (admin) → `200 {registry}` (без токена): host в теле **игнорируется** (неизменяем — сменить host = delete+add), **пустой/отсутствующий `token` → секрет не трогается** (AEAD-шифротекст остаётся байт-в-байт), заданный → ротация; смена типа на `gar` без нового токена отвергается (`_json_key`+JSON не натянуть на непрочитанный шифротекст); `404`/`400`. Событие **`registry_updated`** (payload `{host,type,username}`, без токена). Секрет по-прежнему AEAD-шифруется at-rest (Шифрование секретов v1).
- **`GET /v1/stats/overview?days=N`** (readonly, **N=1..30**, деф. 7 — потолок 30д = retention VM = горизонт роллапов; выше → 400) — агрегаты по дням (UTC, пустые периоды → нули, не пропуски; каждый ряд с единицей): `matches_per_day`/`players_per_day` (стек по региону; players = Σ`players_peak` — прокси), `peak_ccu_per_day` + `peak_ccu` (sweep-line пик одновременных `players_peak`), `avg_match_duration_seconds` (+ per-day), `version_distribution` (доли по semver), `time_to_match` p50/p95. **`GET /v1/stats/cost?days=N`** — `slot_hours_per_day_by_region`/`_by_version` (слото-часы = суммарное allocated-время дедиков из `matches [started_at,ended_at]`, разбито по UTC-дням) + `slot_hours_total` + `utilization` (снапшот allocated/ready/draining vs capacity active-нод по регионам). **Rollup-backed:** ответ = материализованные роллапы (`match_stats_daily` по дню×region×semver×env×**project** + `match_ccu_daily` по дню×**project**, где строка `project=''` — платформенный пик И маркер «день посчитан»; миграция 000017) за иммутабельные дни `[axis0..today-2]` + **живой пересчёт хвоста ≤2д** (сегодня/вчера из сырых `matches`, чтобы данные всегда свежие без ожидания джобы). Живой хвост предполагает, что матч «оседает» (завершается) в пределах этих ≤2 дней от старта — верно для сессионных dedik-матчей (минуты); матч, всё ещё идущий в момент, когда его день старта уходит из хвоста в иммутабельный диапазон роллапов, замораживает `avg_match_duration`/`players_peak` этого дня до следующего бэкфилла. Роллапы держит `statsrollup`-джоба (бэкфилл 30д на старте + тикер `stats_rollup_interval`, деф. 2м, пересчёт последних 2 UTC-дней). Датасет — только матчи, которые реально стартовали (`started_at is not null`). **Проектное измерение (мультипроект W3):** `?project=` сужает и роллапы, и живой хвост, включая пик CCU — проекты непересекающиеся тенанты, поэтому «пик под выбранным проектом» обязан быть пиком этого проекта (в отличие от env, где ёмкость флота общая и пик остаётся платформенным, I5). Пик НЕ аддитивен, поэтому проектные срезы считаются отдельным проходом, а не делением платформенного. Исторические строки, которые бэкфилл миграции не смог атрибутировать однозначно (один semver в двух проектах в один день), несут `project=''` и при заданном `?project=` ИСКЛЮЧАЮТСЯ: подмешивание засчитало бы одни матчи каждому тенанту. Последние 30 дней самолечатся — `statsrollup.Backfill` пересчитывает их из сырых матчей при каждом старте master. **Семантика занятости (occupancy, по спеке):** slot-hours и peak CCU считают **внутриоконную занятость** — долгий матч, начавшийся ДО окна, но идущий в нём, вносит свои слото-часы/игроков в дни окна (matches/players_peak/version_distribution по-прежнему по дню СТАРТА). `time_to_match` p50/p95 (перцентили неаддитивны → не в роллапе) читается узким запросом `matches.created_at→started_at` за окно; истинное «время в очереди» (ticket submit→matched) — гистограмма `birdman_mm_time_to_match_seconds` через metrics-proxy. Утилизация во времени — через metrics-proxy (`birdman_servers` / `birdman_node_capacity_slots`, `query_range`); в master — только снапшот.
- **`GET /v1/alerts/rules`** (readonly) → правила vmalert (имя, группа, severity, выражение, for, state, description), нормализованы из vmalert `GET /api/v1/rules` (recording-правила отфильтрованы). **`GET /v1/alerts/active`** → активные (state=firing) из vmalert `GET /api/v1/alerts`. **`GET /v1/alerts/history?limit=N`** → нормализованный список `{name,severity,region,node,startsAt,endsAt,description,active,received_at}` из лог-сина алертов (JSON-строки alertmanager-v2 `{received_at, alerts:[…]}`; битые строки пропускаются, поддержан и одиночный alert-объект в строке; `active` — по `endsAt`: пусто/zero/в будущем → true). Источники — в конфиге `alerts` (`vmalert_url` деф. `http://127.0.0.1:8880`, env `BIRDMAN_VMALERT_URL`; `log_path` деф. `/var/log/birdman/alerts.log`). Деградация: пусто `vmalert_url` → 503, недоступен → 502, нет лог-файла → пустой список (не 500). **v0: чтение — read-only** (edit правил vmalert вне скоупа master), но **mute-аннотации — state master** (ниже). Описания пробрасываются как есть из аннотаций правил: `description` (EN, каноничный) и additive-поле `description_ru` (`omitempty`; без фоллбэка на summary — фоллбэк на `description` делает панель) — во всех трёх ответах (rules/active/history); двуязычие — конфиг vmalert (аннотации правил), см. `ops.md` §1. В `/v1/alerts/active` и `/v1/alerts/history` каждому алерту добавлено поле `muted:bool` — есть ли активное mute-правило с совпадающим `alertname` И (region правила null ИЛИ == region алерта) И (project правила null ИЛИ == project алерта, **строго** — см. tracker #957 ниже). **Проектное измерение (мультипроект W2, tracker #955):** те же два ответа несут additive-поля `project` (лейбл алерта; пусто = платформенный) и `scope` (`"project"` при непустом `project`, иначе `"platform"` — выводится master'ом, чтобы панель не повторяла правило «пустой лейбл = платформенный»). Область определяется НАЛИЧИЕМ лейбла, отдельного лейбла `scope:` в правила vmalert не вводится — так корректно классифицируются и свои правила self-host-оператора (`ops.md` §1). Обе ручки принимают **`?project=`**, и фильтр **НЕ скрывающий**: алерт выпадает, только если его `project` ЯВНО не равен запрошенному, а алерт без лейбла остаётся видимым при любом выборе. Строгий фильтр отвергнут: у проекта нет режима «Все» (в отличие от `env`), поэтому он спрятал бы `MasterDown`/`NodeDown`/`DiskHigh` НАВСЕГДА — молча проглотить «мастер лежит» хуже, чем показать лишний алерт соседа (прецеденты — `keepForProject` ленты событий и платформенный срез пика CCU, решение I5/W3). В `/history` фильтр применяется ДО отсечения по `limit` — иначе `?limit=N&project=X` возвращал бы горстку записей, съеденных соседями. **Слаг ВАЛИДИРУЕТСЯ по БД (tracker #961)** — по общему правилу выше; «не скрывающий» и «валидируемый» — два разных вопроса, и на оба ответ «да»: опечатка больше не притворяется режимом «видны только платформенные», который на ЭТОМ экране неотличим от желанного «всё спокойно», а для существующего проекта платформенные алерты по-прежнему видны все. Проверка идёт ДО обращения к vmalert и ДО чтения лог-сина (опечатка не тратит апстрим и не превращается в 502/пустой список), но ПОСЛЕ `503 alerts_unconfigured`: «на этом мастере алертов нет вовсе» — факт более фундаментальный, чем плохой ввод. **`/v1/alerts/rules` не фильтруется вовсе:** это каталог конфигурации, а не инстансы алертов — у правила проекта нет, его `expr` разворачивается в серию на проект, и `project` живёт внутри текста запроса, а не в статических labels.
- **`POST /v1/alerts/mutes {alertname, region?, project?, note?, expires_at?}`** (admin) → `201 {mute:{id,alertname,region,project,note,created_at,expires_at,created_by,silence_id}}`. Валидация: `alertname` непустой; `expires_at` (если есть) — RFC3339 **в будущем**; `region`/`project`/`expires_at` пустые/отсутствуют → null («все регионы»/«все проекты»/«бессрочно»). `created_by` = name ключа сессии; событие `alert_muted` (payload без секретов, несёт `project`). **Дедуп — upsert:** повторный mute того же активного `(alertname, region, project)` (оба матча null-aware) обновляет `note`/`expires_at` у существующей строки (не плодит дубли, не 409), возвращая всё те же 201 с итоговым mute — POST заодно работает как «продлить/изменить». **Проектное измерение (мультипроект, tracker #957, миграция `000018`):** `project` — nullable-колонка `alert_mutes`, NULL = все проекты, ровно как nullable `region` = все регионы; `project` **НЕ валидируется по БД** — и с tracker #961 это уже НЕ «как на чтениях» (там валидируется), а третье именованное исключение из общего правила: здесь project — МАТЧЕР хранимого правила, а не фильтр над существующими данными. Три причины: соседний `region` валидировать нечем в принципе (реестра регионов нет), и проверять одну половину цели, не проверяя вторую, — новое расхождение; панель подставляет сюда ЛЕЙБЛ алерта из мониторинг-стека, а не слаг из БД-селектора, так что алерт со старым лейблом иначе стал бы незаглушаемым; промах ВИДЕН — mute остаётся строкой в `GET /v1/alerts/mutes` с проектом на виду, в отличие от невидимо суженной выдачи. **Матч по проекту — СТРОГИЙ, сознательно наоборот к не скрывающему `?project=`:** проектный mute НЕ кроет платформенный алерт (у которого `project` пуст). Иначе оператор проекта А, заглушивший `MasterDown` «у себя», ронял бы сигнал и проекту Б, который об этом не знает; чтобы заглушить платформенный алерт, mute ставится **без проекта** — явно и видимо. Обе стороны выбирают одну и ту же безопасную: показать лишний алерт лучше, чем спрятать; заглушить на один меньше лучше, чем на один больше. **`GET /v1/alerts/mutes`** (readonly) → `{mutes:[…]}` только активные (expires_at null или в будущем), newest-first; `?all=1` — включая истёкшие. **`DELETE /v1/alerts/mutes/{id}`** (admin) → `204` при реальном удалении (событие `alert_unmuted`), `404` на несуществующий/уже удалённый id, `400` на не-uuid; идемпотентно по эффекту (конечное состояние — «не muted»), 404 отличает реальный unmute от no-op и не плодит дублей аудита. **Семантика mute — настоящий silence (`ops.md` §1, tracker #238/#245): источник истины — `alert_mutes`, зеркалирование в alertmanager best-effort** (`internal/amsilence`): POST/upsert mute → `POST {alerts.alertmanager_url}/api/v2/silences` (matchers `alertname` (+`region` и +`project`, если заданы; null — матчера нет, silence кроет все регионы/проекты), `endsAt=expires_at`, бессрочный — далёкий горизонт +10 лет; при существующем `silence_id` он передаётся в теле — AM обновляет silence на месте, сохраняется возвращённый id; `createdBy="birdman:<имя ключа>"` — префикс-маркер, `comment=note`), DELETE mute → `DELETE /api/v2/silence/{id}`. Полученный id — в `alert_mutes.silence_id` (миграция `000016`; additive-поле, панель не менялась; в самом 201-ответе может быть ещё null — best-effort, виден следующим GET'ом). **Недоступный alertmanager НЕ фейлит запрос** (те же 201/204, лог) — mute деградирует до v0-аннотации (панель+аудит), self-host без мониторинг-стека работает как раньше; пустой `alerts.alertmanager_url` (деф. `http://127.0.0.1:9093`, env `BIRDMAN_ALERTMANAGER_URL`) — зеркалирование выключено. **Reconcile** (на старте master + ~60с): mute без `silence_id` получает silence (миграция старых v0-mute'ов), потерянный/истёкший silence перевыпускается (переживает потерю состояния AM), `endsAt`-дрейф чинится; orphan-sweep гасит активные silences с birdman-маркером без соответствующего mute (чужие не трогаются). `muted:true` в `/active`/`/history` остаётся — vmalert про silences не знает, приглушение в UI по-прежнему нужно; sink/Discord получают уже отфильтрованный alertmanager'ом поток.

(Уточнено в v0, Логи v1 — реализовано, ветка `logs-v1`; тело — `internal/httpapi/ops.go` `handleLogsQuery`.) **`GET /v1/logs/query`** (readonly) — тонкий read-only прокси к VictoriaLogs (`victorialogs_url` + `/select/logsql/query`): параметры `query` (LogsQL, passthrough — та же модель доверия, что и сырой PromQL в metrics-proxy), `start`, `end` пробрасываются как есть. **`limit` зажимается на master**: не задан → деф. **1000**; задан, но не положительное целое → `400 bad_request` (апстрим не вызывается); больше 10000 → молча клампится до **10000**. Таймаут запроса к VL — 15с. Деградация зеркальна metrics-proxy: пустой `metrics.victorialogs_url` → `503 logs_unconfigured`, недоступный/упавший апстрим (включая таймаут) → `502 upstream`. Конфиг — секция `metrics` (та же, что и `victoriametrics_url`): `victorialogs_url`, деф. `http://127.0.0.1:9428`, env-override `BIRDMAN_VL_URL`; пусто → фича мягко выключена. Content-Type и тело ответа проксируются от VL как есть (ndjson построчно: `_time`, `_msg`, стрим-поля `server_id`/`node`/`region`). Live-tail (`GET /v1/servers/{id}/logs`) этим прокси не затронут и от VL не зависит.

## 7. Операционное

- Один бинарь `birdman-master`; конфиг `/etc/birdman/master.yaml` (dsn, listen :443 и :8443 gRPC, tls-серты, project defaults, `secrets_key_file`). systemd unit с `Restart=always`. (Уточнено в v0: дев-дефолты `listen_api :8100`, `listen_grpc :8444`; env-переопределения `BIRDMAN_DSN`/`BIRDMAN_LISTEN_API`/`BIRDMAN_LISTEN_GRPC`.) (Уточнено в v1, secrets-encryption §2.) `secrets_key_file` (деф. `/etc/birdman/secrets.key`, 0600, провижинится ролью `birdman_master_dev`) — мастер-ключ шифрования обратимых секретов at-rest (`registries.token`/`internal_ca.key_pem`), **обязателен**: без валидного ключа master не стартует (fail-loud). Env-оверрайды: `BIRDMAN_SECRETS_KEY_FILE` (путь, замещает yaml-значение) и `BIRDMAN_SECRETS_KEY` (значение base64 для dev/CI — при заданном значении оно **выигрывает** у файла; ровно один источник доходит до загрузчика ключа).
- (Уточнено в v1, mTLS agentlink.) Внутренняя CA живёт в Postgres (`internal_ca`, миграция 000008; master генерит её под advisory-lock при первом старте — ECDSA P-256, TTL 10 лет). Без внешних `tls.cert_file/key_file` master **выпускает себе server-лист от этой CA при старте** (в память, TTL 90 дней, hot-rotate за 14 дней до истечения через `GetCertificate`) — self-signed автоген (`EnsureServerCert`) выведен из эксплуатации. gRPC-листенер `:8444`: `ClientCAs` = активные CA, `ClientAuth: VerifyClientCertIfGiven` (Enroll обязан работать до выдачи серта); строгость Session — конфиг `agentlink_auth: token|mixed|mtls` (env `BIRDMAN_AGENTLINK_AUTH`, деф. `mixed`; `protocol.md` §Auth). Наблюдаемость: `birdman_agentlink_sessions{auth="mtls|token"}`, `birdman_tls_cert_expiry_timestamp_seconds{cert="ca|server"}`, `birdman_node_cert_expiry_timestamp_seconds{node}`, `birdman_agentlink_registries_withheld_total`.
- Graceful shutdown: стоп приёма API → дожидание in-flight (≤5с) → exit. Агенты переподключаются сами.
- QoS-эндпоинт: крошечный UDP-echo в составе агента на каждой тачке (порт 19999) — master отдаёт их список в `/v1/qos`.
- Логи master: journald, JSON-формат.

## 8. Acceptance

- **Ит. 1**: `curl /v1/allocate` <1с из warm pool; kill дедика → буфер восстановлен ≤10с; выключение тачки → карантин ≤15с, слоты пересозданы; crash-loop останавливает пересоздания.
- **Ит. 2**: два клиента получают один match {host,port} через тикеты; `update_required` для старого клиента; занятый дедик не выдаётся повторно.
- **Ит. 3**: deploy при живых матчах — 0 обрывов; rollback ≤1 мин; CI регистрирует версию и деплоит staging автоматически. *(истор.: «staging» здесь — ныне окружение `dev` с `auto_deploy`, §Окружения.)*
- **Ит. 5**: два региона, выбор по rtt из тикета; drain тачки — без обрывов.
