# Спека: master

> Один Go-процесс: матчмейкер + флот-контроллер + deploy-менеджер + REST API + панель (embed). Postgres 16. Итерации 1–3, 5 из `../05-runtime-iterations.md`.

## 1. Модель данных (Postgres, DDL v1)

```sql
create table projects (
  id         uuid primary key default gen_random_uuid(),
  slug       text unique not null,          -- 'ourgame'
  created_at timestamptz not null default now()
);

create table nodes (
  id                uuid primary key default gen_random_uuid(),
  project_id        uuid not null references projects(id),
  region            text not null,          -- 'eu', 'us-east'
  hostname          text not null,
  public_ip         inet not null,
  capacity_slots    int  not null,
  agent_version     text not null default '',
  state             text not null default 'active'
                    check (state in ('active','draining','quarantine','dead')),
  last_heartbeat_at timestamptz,
  labels            jsonb not null default '{}',
  token_hash        text not null default '',  -- (уточнено в v0) bcrypt node_token,
                                               -- пока не реализован обмен token→mTLS-серт
  created_at        timestamptz not null default now()
);

create table versions (
  id         uuid primary key default gen_random_uuid(),
  project_id uuid not null references projects(id),
  semver     text not null,                 -- '1.4.2'
  image_ref  text not null,                 -- 'ghcr.io/org/ourgame-server:1.4.2'
  channel    text not null check (channel in ('staging','prod')),
  state      text not null default 'registered'
             check (state in ('registered','prepulling','active','deprecated','disabled')),
  created_at timestamptz not null default now(),
  unique (project_id, semver, channel)
);

create table servers (
  id         uuid primary key default gen_random_uuid(),
  project_id uuid not null references projects(id),
  node_id    uuid not null references nodes(id),
  version_id uuid not null references versions(id),
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
  state        text not null default 'pending'
               check (state in ('pending','running','finished','aborted')),
  players_peak int not null default 0,
  started_at   timestamptz,
  ended_at     timestamptz,
  created_at   timestamptz not null default now()
);

create table fleet_configs (            -- desired state per (project, region)
  project_id      uuid not null references projects(id),
  region          text not null,
  active_version  uuid references versions(id),
  buffer_ready    int  not null default 2,   -- сколько ready держать
  max_servers     int  not null default 50,
  reap_ttl_min    int  not null default 180, -- добой deprecated-дедиков
  primary key (project_id, region)
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
  created_at timestamptz not null default now(),
  revoked_at timestamptz
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
      node = first-fit: active-нода региона с максимумом занятых слотов,
             где free_slots > 0 и heartbeat свежий            -- плотная упаковка
      insert servers(state='creating') + команда агенту StartServer
  surplus (только у deprecated-версий или при buffer↓) → reap старейших ready
failed-сервера: слот освобождается сам (insert нового сделает дефицит)
crash-loop: ≥3 failed одной версии на одной тачке за 10 мин → стоп созданий
            этой пары (version,node) на 15 мин + event crash_loop
```

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

- Тикет: `{ticket_id, player_id, client_version, regions: [{region, rtt_ms}]}`. Очереди **in-memory** per (region, compat-bucket). Потеря при рестарте — ок: клиент ре-квьюится (см. `sdk.md`).
- Тик каждые **500мс**: в каждой очереди собрать группы по `match_size` (конфиг per project, v0 — константа) → `allocate` → всем участникам `matched {host, port, match_id, join_token}`.
- Выбор региона: минимальный медианный rtt по группе; если очередь региона < match_size дольше `widen_after_s` (деф. 30с) — расширяем на следующий по rtt регион.
- `client_version` не входит ни в один compat-bucket активных версий → ответ `update_required`.
- `join_token` = HMAC(match_id, player_id, exp 60с) — дедик проверяет через liba (анти-«зашёл мимо матчмейкера»); v0 — опционально, включается флагом.
- Анти-дубль: один активный тикет на player_id (новый вытесняет старый).
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

## 6. Публичный REST API (сводка; OpenAPI — в `master/api/openapi.yaml`)

| Метод | Скоуп | Назначение |
|---|---|---|
| `POST /v1/matchmaking/tickets` | matchmaking | создать тикет |
| `GET /v1/matchmaking/tickets/{id}` (+`?wait=25s` long-poll / SSE `/stream`) | matchmaking | статус: queued / matched{host,port,match_id,join_token} / update_required / cancelled |
| `DELETE /v1/matchmaking/tickets/{id}` | matchmaking | отмена |
| `GET /v1/qos` | public | пинг-эндпоинты регионов `[{region, host, udp_port}]` |
| `POST /v1/allocate` | allocate | граница флота (см. §3) |
| `GET /v1/nodes` · `/v1/servers` · `/v1/matches` · `/v1/versions` | readonly | списки с фильтрами |
| `GET /v1/events/stream` (SSE) | readonly | live-лента для панели |
| `PUT /v1/fleets/{region}` | admin | buffer, max_servers, reap_ttl |
| `POST /v1/versions` | deploy | регистрация билда из CI |
| `POST /v1/deploy` · `/v1/rollback` | deploy | см. §5 |
| `POST /v1/nodes/{id}/drain` · `/undrain` | admin | вывод тачки |
| `GET /v1/servers/{id}/logs?follow=1` | admin | проксирование tail с агента |
| `GET /metrics` | локально | Prometheus-метрики master |

Аутентификация: `Authorization: Bearer <api-key>`; скоупы из таблицы `api_keys`. Клиентский matchmaking-ключ — публичный по сути (зашит в клиент), поэтому его скоуп ограничен тикетами, rate-limit per IP/player_id.

(Уточнено в v0.) Реализовано подмножество: nodes/servers/versions/fleets/events/allocate + `/healthz`, `/metrics`; скоуп `admin` включает остальные скоупы; при пустой `api_keys` master при старте генерирует admin-ключ и печатает его в лог один раз; `PUT /v1/fleets/{region}` дополнительно принимает `active_version` (deploy-менеджера ещё нет); проекты создаются неявно при первом упоминании slug в `POST /v1/nodes` / `POST /v1/versions`; SSE `/v1/events/stream` — TODO следующих итераций.

## 7. Операционное

- Один бинарь `birdman-master`; конфиг `/etc/birdman/master.yaml` (dsn, listen :443 и :8443 gRPC, tls-серты, project defaults). systemd unit с `Restart=always`. (Уточнено в v0: дев-дефолты `listen_api :8100`, `listen_grpc :8444`; env-переопределения `BIRDMAN_DSN`/`BIRDMAN_LISTEN_API`/`BIRDMAN_LISTEN_GRPC`; без сертов в конфиге — self-signed автогенерация при первом старте.)
- Graceful shutdown: стоп приёма API → дожидание in-flight (≤5с) → exit. Агенты переподключаются сами.
- QoS-эндпоинт: крошечный UDP-echo в составе агента на каждой тачке (порт 19999) — master отдаёт их список в `/v1/qos`.
- Логи master: journald, JSON-формат.

## 8. Acceptance

- **Ит. 1**: `curl /v1/allocate` <1с из warm pool; kill дедика → буфер восстановлен ≤10с; выключение тачки → карантин ≤15с, слоты пересозданы; crash-loop останавливает пересоздания.
- **Ит. 2**: два клиента получают один match {host,port} через тикеты; `update_required` для старого клиента; занятый дедик не выдаётся повторно.
- **Ит. 3**: deploy при живых матчах — 0 обрывов; rollback ≤1 мин; CI регистрирует версию и деплоит staging автоматически.
- **Ит. 5**: два региона, выбор по rtt из тикета; drain тачки — без обрывов.
