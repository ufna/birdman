# birdman-master v0

Флот-контроллер + Allocation API + gRPC AgentLink — итерация 1 из
`docs/05-runtime-iterations.md`. Спека: `docs/specs/master.md` (источник истины).

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
- **REST** (`:8100`, Bearer API-ключи из таблицы `api_keys`, скоупы
  admin/deploy/allocate/readonly; bcrypt + кэш);
- **/metrics** (Prometheus): `birdman_servers{state,region,version}`,
  `birdman_allocation_duration_seconds`,
  `birdman_allocation_failures_total{reason}`,
  `birdman_node_heartbeat_age_seconds`.

Отложено (TODO, спеки помечены): обмен node_token → клиентский mTLS-серт,
SSE `/v1/events/stream`, матчмейкер, deploy/rollback, drain ноды, logs-proxy.

## Конфиг

`master.example.yaml` → `master.yaml`; env-переменные `BIRDMAN_DSN`,
`BIRDMAN_LISTEN_API`, `BIRDMAN_LISTEN_GRPC` перекрывают файл. Без TLS-сертов
в конфиге master при первом старте генерирует self-signed пару в
`tls.auto_cert_dir` (dev-режим; gRPC всегда TLS).

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
```

Проекты создаются неявно при первом упоминании slug'а в `POST /v1/nodes` /
`POST /v1/versions` (уточнено в v0).

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
./master/build.sh   # → master/dist/birdman-master (linux/amd64, static, CGO off)
```

## Генерация proto

См. `proto/README.md` (docker + buf, версии пинованы). Сгенерированный код
закоммичен; CI пересборку не делает.
