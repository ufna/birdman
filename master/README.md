# birdman-master v0

Флот-контроллер + Allocation API + матчмейкер v0 + gRPC AgentLink — итерации
1–2 из `docs/05-runtime-iterations.md`. Спека: `docs/specs/master.md`
(источник истины).

Что внутри v0:

- **Postgres 16** — единственный state store; миграции (golang-migrate, embed)
  накатываются автоматически при старте;
- **gRPC AgentLink** (`:8444`, TLS): нода создаётся заранее через
  `POST /v1/nodes` → одноразовый `node_token`; агент аутентифицирует стрим
  `Hello{node_token}`; heartbeat ~2с; per-node очередь команд с `cmd_id`/`Ack`
  и ре-отправкой при реконнекте;
- **lease-чекер**: heartbeat старше 10с → нода в `quarantine`; ещё 20с — её
  сервера → `failed`; возврат heartbeat → `active`;
- **reconcile** (тик 1с): дефицит buffer_ready → `INSERT servers(creating)` +
  `StartServer` (first-fit: самая занятая живая нода региона); излишек ready →
  `StopServer` старейших; crash-loop ≥3 failed (version,node) за 10 мин →
  пауза пары на 15 мин + событие `crash_loop`; зависшие `creating` >120с →
  `failed`;
- **Allocation API** `POST /v1/allocate` — атомарный claim через
  `FOR UPDATE SKIP LOCKED`, идемпотентность по `match_id`,
  `409 {"error":"no_capacity"}`;
- **матчмейкер v0** (`docs/specs/master.md` §4): in-memory очереди per
  (region, compat-bucket), тик 500мс, группы по `projects.match_size`
  (деф. 2, правится `PUT /v1/projects/{slug}`), регион по минимальному
  медианному rtt группы, widen на следующий по rtt регион игрока через
  `widen_after_s` (30с), TTL тикета 120с → `expired`, анти-дубль по
  player_id, совместимость client_version по major.minor (`ops.md` §3),
  `no_capacity` → тикеты ждут ретрая; join_token (HMAC) — за флагом,
  по умолчанию выключен;
- **REST** (`:8100`, Bearer API-ключи из таблицы `api_keys`, скоупы
  admin/deploy/matchmaking/allocate/readonly; bcrypt + кэш):
  - `POST /v1/matchmaking/tickets` `{player_id, client_version,
    regions:[{region,rtt_ms}][, project]}` → `{ticket_id, status}`;
  - `GET /v1/matchmaking/tickets/{id}?wait=25s` — long-poll (кап 30с):
    `queued | matched{host,port,match_id,join_token?} | update_required |
    cancelled | expired`;
  - `DELETE /v1/matchmaking/tickets/{id}` — отмена;
  - `GET /v1/qos` (публичный) → `{"qos":[{region, host, udp_port:19999}]}`
    из живых нод — сам UDP-echo появится в агенте в итерации 4, адреса уже
    правильные;
  - rate-limit матчмейкинга: 5 rps per player_id → `429 rate_limited`;
- **/metrics** (Prometheus): `birdman_servers{state,region,version}`,
  `birdman_allocation_duration_seconds`,
  `birdman_allocation_failures_total{reason}`,
  `birdman_node_heartbeat_age_seconds`, `birdman_mm_queue_depth{region}`,
  `birdman_mm_time_to_match_seconds`, `birdman_mm_tickets_total{result}`;
- **SSE** `GET /v1/events/stream` (readonly+): новые строки `events` как
  `id:`/`event: <kind>`/`data: <json>` (курсор по id, poll ~1с), keepalive
  каждые 15с, реконнект без потерь через `?after_id=` / `Last-Event-ID`;
- **матчи для панели**: `GET /v1/matches` (фильтры `state|region|project`,
  `limit/offset`, свежие первыми; в ответе semver, host:port и живые
  `server_players`) и `GET /v1/matches/{id}`;
- **cookie-сессии панели**: `POST /v1/session {api_key}` → HttpOnly Secure
  SameSite=Lax cookie (in-memory, TTL 24ч, скоупы ключа), `GET /v1/session`,
  `DELETE /v1/session`; Bearer работает как раньше; для не-GET по cookie
  обязателен заголовок `X-Birdman-Csrf: 1`;
- **встроенная админ-панель** (`/`, П0 read-only) — см. раздел «Панель».

Отложено (TODO, спеки помечены): обмен node_token → клиентский mTLS-серт,
проверка join_token на дедике (liba), compat-overrides, deploy/rollback,
drain ноды, logs-proxy.

## Конфиг

`master.example.yaml` → `master.yaml`; env-переменные `BIRDMAN_DSN`,
`BIRDMAN_LISTEN_API`, `BIRDMAN_LISTEN_GRPC`, `BIRDMAN_MM_JOIN_SECRET`
перекрывают файл. Без TLS-сертов в конфиге master при первом старте
генерирует self-signed пару в `tls.auto_cert_dir` (dev-режим; gRPC всегда
TLS). Секция `matchmaking` (tick_ms/widen_after_s/ticket_ttl_s/
default_project/join_token) — см. `master.example.yaml`; секреты в файл не
кладём — join-секрет только через env.

## Дев-запуск (docker compose)

```sh
docker compose -f master/dev-compose.yml up --build -d
docker compose -f master/dev-compose.yml logs master | grep api_key   # admin-ключ, печатается ОДИН раз
curl -s localhost:8100/healthz
```

При пустой таблице `api_keys` master создаёт admin-ключ и печатает его в лог
один раз — сохраните его сразу. Дальше обычный флоу:

```sh
KEY=bmk_...   # admin-ключ из лога

# 1. зарегистрировать ноду → одноразовый node_token для конфига агента
curl -s -X POST localhost:8100/v1/nodes -H "Authorization: Bearer $KEY" \
  -d '{"project":"game","region":"eu","hostname":"n1","public_ip":"203.0.113.10","capacity_slots":8}'

# 2. зарегистрировать версию билда
curl -s -X POST localhost:8100/v1/versions -H "Authorization: Bearer $KEY" \
  -d '{"project":"game","semver":"1.0.0","image_ref":"ghcr.io/org/game:1.0.0","channel":"staging"}'

# 3. включить warm pool региона
curl -s -X PUT localhost:8100/v1/fleets/eu -H "Authorization: Bearer $KEY" \
  -d '{"project":"game","active_version":"<version_id>","buffer_ready":2}'

# 4. аллокация (после того как агент поднял ready-сервера)
curl -s -X POST localhost:8100/v1/allocate -H "Authorization: Bearer $KEY" \
  -d '{"project":"game","region":"eu","match_id":"<uuid>"}'

# 5. (опционально) размер матча проекта — по умолчанию 2
curl -s -X PUT localhost:8100/v1/projects/game -H "Authorization: Bearer $KEY" \
  -d '{"match_size":2}'
```

Проекты создаются неявно при первом упоминании slug'а в `POST /v1/nodes` /
`POST /v1/versions` / `PUT /v1/projects/{slug}` (уточнено в v0).

## Панель

Админ-панель (П0, read-only: Overview / Флот / Матчи, обе темы, live через
SSE) — SPA из `panel/`, **встроенная в бинарь**: `panel/build.sh` собирает
её (npm ci + tsc + vite build) и кладёт в `master/internal/panelui/static`,
а `internal/panelui` отдаёт этот каталог через `go:embed` с `/` и
`/assets/*` (SPA-fallback на `index.html`, `/v1/*` остаётся JSON-API).

Как это не ломает сборку без node:

- в git закоммичен только якорь `static/.gitkeep` (сам каталог в
  `.gitignore`) — `go build` и `master/test.sh` работают на машине без
  node: бинарь без панели отдаёт placeholder-страницу с подсказкой;
- `panel/build.sh` использует локальный node ≥20, а если его нет —
  `docker node:22`;
- `./master/build.sh` собирает панель автоматически; `SKIP_PANEL=1
  ./master/build.sh` — пропустить.

Вход в панель: API-ключ со скоупом `readonly` или `admin` (форма логина →
`POST /v1/session` → HttpOnly-cookie на 24ч; сессии в памяти master и не
переживают его рестарт — панель просто вернёт на логин). Дев-режим фронта:
`cd panel && npm run dev` (vite dev server проксирует `/v1` на
`localhost:8100`); тесты панели: `npm test` (vitest) + `npm run check`
(tsc), CI — `.github/workflows/panel.yml`.

## Матчмейкинг: mmcli

`mmcli` — клиент матчмейкинга для acceptance-прогонов и отладки (второй
бинарь `master/dist/mmcli`, собирается `build.sh`). Создаёт тикет,
long-poll'ит его чанками по 25с и печатает JSON результата + `host:port`
последней строкой (exit 0 — matched, 1 — иной исход, 2 — usage):

```sh
mmcli --master http://localhost:8100 --key $KEY request \
  --player p1 --version 1.0.0 --region eu:5
# {"..."} + строка "203.0.113.10:20001" при матче
```

`--region NAME:RTT_MS` повторяемый (rtt клиент меряет по `GET /v1/qos`);
`--project` нужен только если проектов несколько; `--timeout` — общий
дедлайн ожидания (деф. 120с). Ключ — со скоупом `matchmaking` (или admin).
Два запущенных `mmcli request` c разными `--player` в одном регионе дают
обоим один и тот же `host:port` и `match_id` — acceptance итерации 2.

## Тесты

Go на хосте не нужен — всё в docker:

```sh
./master/test.sh                 # postgres:16 + golang:1.24, go test -race ./...
./master/test.sh -run TestAllocate -v
```

Интеграционные тесты берут Postgres в таком порядке: `BIRDMAN_TEST_DSN` из
env (так работают test.sh и CI c service-контейнером) → сами поднимают
`postgres:16` через docker CLI (запуск `go test` прямо на хосте с docker) →
иначе `t.Skip`. Каждый тест получает отдельную БД с миграциями.

## Сборка

```sh
./master/build.sh                # панель + master/dist/{birdman-master,mmcli}
SKIP_PANEL=1 ./master/build.sh   # только бинари (в них будет placeholder)
```

Бинари — linux/amd64, static, CGO off; панель встраивается в
`birdman-master` (см. раздел «Панель»).

## Генерация proto

См. `proto/README.md` (docker + buf, версии пинованы). Сгенерированный код
закоммичен; CI пересборку не делает.
