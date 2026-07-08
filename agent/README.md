# birdman-agent

Нода-агент birdman, **итерации 1–4** (`docs/05-runtime-iterations.md`): демон `run` под управлением master (gRPC AgentLink) + локальный `run-once` из итерации 0. Спеки: `docs/specs/agent.md`, `docs/specs/protocol.md` §1–2.

Что внутри: gRPC bidi-линк с master (Hello с восстановленной картой, heartbeat 2с c NodeStats, команды Start/Stop/Allocate/PrePull/DrainServer c `Ack{cmd_id}` и идемпотентностью, реконнект с бэкоффом 1с→30с), восстановление карты серверов из containerd-labels после рестарта (живые дедики агент-рестарт переживают), ensure/pull образа (включая приватный GHCR), пул host-портов, запуск контейнера (host network, cgroup-лимиты, env-контракт `BIRDMAN_*`, ro bind-mount per-server каталога с сокетом), UDS-сервер liba-протокола (NDJSON), стейт-машина `pulling → starting → ready → allocated → draining → stopped|failed`, grace 30с до `ready`, shim-side логи дедика (`cio.LogFile` — переживают рестарт агента), graceful stop по SIGTERM (дедиков не трогает).

Итерация 4 (наблюдаемость + операционка): **`/metrics` на `127.0.0.1:9101`** (Prometheus text: `birdman_agent_up`, `birdman_agent_servers{state}`, `birdman_server_players/tick_ms{server_id}`, per-container cpu/mem из cgroups v2, диск `data_dir`, занятость пула портов); **UDP QoS-echo на `:19999`** (≤64б, публичный — клиенты меряют rtt); **ротация логов дедиков** (100MB×2, gzip после stop, ретенция 7 дней, фоновая уборка); **image GC** (диск >80% → LRU неиспользуемых образов кроме active/prepulling/deprecated; >90% → отказ StartServer + событие `disk_full`); **TailLogs** (стрим `LogChunk` master'у, в т.ч. из `.gz` для умерших дедиков); **node Drain/Undrain** (draining в heartbeat, новые StartServer отклоняет); **self-upgrade** (`UpgradeAgent{url,sha256,version}` → скачать → sha256 → atomic rename → чистый выход, systemd поднимает новый бинарь; живые дедики переживают — нужен `Restart=always` в unit).

## Сборка

Go на хосте не нужен — только docker (монтируется корень репо: `replace ../proto`):

```bash
./build.sh          # → dist/birdman-agent (static ELF linux/amd64, stripped)
file dist/birdman-agent
# ELF 64-bit LSB executable, x86-64, statically linked
```

Версия зашивается из `git describe` (`birdman-agent version`).

## Конфиг

`/etc/birdman/agent.yaml` (спека §10; неизвестные ключи игнорируются — форвард-совместимость):

```yaml
region: "dev"
capacity_slots: 8
port_range: [20000, 29999]            # дефолт, если не задан
limits_default: { cpu_millis: 3500, mem_mb: 4096 }
log_dir: /var/log/birdman
data_dir: /var/lib/birdman
registry_auth:                        # для pull приватного GHCR
  username: "ufna"
  token_file: /etc/birdman/ghcr.token # токен ТОЛЬКО в файле, не в конфиге/коде
# --- master-линк (режим run) ---
master_addr: "127.0.0.1:8444"
node_token_file: /etc/birdman/node.token  # из POST /v1/nodes; файл 0600
tls_insecure: true                    # ТОЛЬКО dev: self-signed master;
                                      # прод — tls_ca_file: /path/ca.pem
```

## Запуск (демон, итерации 1–2)

```bash
birdman-agent run --config /etc/birdman/agent.yaml
```

- коннект к `master_addr` (TLS), Hello{node_token + карта серверов} — при каждом (ре)коннекте;
- heartbeat каждые 2с: NodeStats (cpu/mem/disk/load из /proc+statfs) + все живые дедики;
- команды master: StartServer (порт из пула или заданный), StopServer{grace}, AllocateServer (итерация 2: `allocated{match_id, players_expected}` → liba, `ready → allocated`), PrePull (+PullReport), DrainServer (итерация 3: `ready|allocated → draining` + liba-фрейм `drain{deadline_s, reason}`, без сигналов — дедик доигрывает и выходит сам; фрейм реплеится при реконнекте liba), node-level Drain/UpgradeAgent/TailLogs — Ack + TODO;
- каждая команда подтверждается `Ack{cmd_id}`; повторный `cmd_id` (ре-доставка) не исполняется повторно; повторный StartServer знакомого server_id — no-op;
- события ready/failed/match_start/match_end — ServerEvent'ами (переживают реконнект в outbox-очереди);
- SIGTERM → закрыть стрим и выйти; дедики продолжают жить, следующий старт агента восстанавливает карту по labels (`birdman/state`, `birdman/match-id`).

## Запуск (run-once, итерация 0)

```bash
birdman-agent run-once \
  --config /etc/birdman/agent.yaml \
  --image ghcr.io/ufna/birdman-stub-server:latest \
  [--port 21777] [--allocate m-42] [--drain-after 60]
```

- события (стейт-переходы, players, match_start/end) — в **stdout**; диагностика — в stderr;
- `--allocate` шлёт `allocated{match_id}` после `ready` (играет за master);
- `--drain-after SEC` шлёт `drain{deadline_s:30}` через SEC секунд после `ready`;
- логи дедика: `{log_dir}/servers/{server_id}.log` (stdout/stderr контейнера + `log`-фреймы liba);
- код выхода агента = код выхода контейнера (ошибки агента → 1; нет `ready` за 30с → failed, SIGKILL, обычно 137);
- SIGTERM/SIGINT агенту → SIGTERM контейнеру → grace 30с → SIGKILL.

## Тесты

```bash
# unit (линк bufconn против фейкового master-стрима, менеджер с фейковым
# рантаймом, пул портов, конфиг, UDS против фейкового liba, стейт-машина)
docker run --rm -v "$PWD/..":/src -w /src/agent -e GOFLAGS=-buildvcs=false \
  golang:1.24 sh -c "go vet ./... && go test -race ./..."

# интеграция с containerd — на Linux-тачке с демоном (скипается в обычном прогоне):
go test -race -tags integration ./internal/runtime/
```

Интеграционный тест параметризуется env: `BIRDMAN_TEST_CONTAINERD`, `BIRDMAN_TEST_IMAGE`, `BIRDMAN_TEST_SOCKDIR` (см. шапку `internal/runtime/integration_test.go`). Прогнан против реального containerd 1.7 (Docker Desktop VM); там же прогнаны E2E run-once: полный матч-цикл со stub-server (UDP-игрок, `match_end completed`, exit 0), SIGTERM-стоп, grace-таймаут (exit 137), drain.

## Принятые решения (v0)

1. **Клиентская библиотека — `github.com/containerd/containerd` v1.7.x, не `/v2`.** Целевые тачки — Ubuntu 24.04 с docker 28.x, т.е. демон containerd **1.7.x**; клиент той же линии исключает сюрпризы совместимости (v2-клиент рассчитан на демоны 2.x: transfer-service pull, sandbox API). Переход на `/v2` — осознанно, вместе с апгрейдом демона на тачках. Примечание: `runtime-spec` запинен на v1.1.0 (v1.3 ломает компиляцию containerd 1.7).
2. **EnsureImage вместо голого pull**: сначала локальный образ (спека §3 «ensure image» — PrePull-семантика итерации 3), иначе pull; авторизация GHCR через resolver с creds из `token_file`.
3. **Логи дедика**: демон — shim-side (`cio.LogFile`, переживают рестарт агента); run-once — cio-стримы через процесс агента. Ротация 100MB×2 + gzip (спека §5) — TODO.
4. **Сокет 0666 в per-server каталоге, каталог монтируется ro** (итерация 1): бинд каталога вместо файла — иначе после рестарта агента контейнер видел бы замороженный старый inode и liba не могла бы реконнектиться; connect(2) к сокету работает и на ro-mount (S_ISSOCK не подпадает под запрет записи). Identity дедика = его сокет (protocol.md §2); образы игр бывают non-root (наш stub — distroless **nonroot**), им нужен connect.
5. **exit-watch через `task.Wait`**, заармленный `context.WithoutCancel`: канал выхода обязан пережить отмену сигнального контекста, иначе при SIGTERM агент получает фиктивный `context canceled` вместо реального кода (поймано E2E-тестом). OOM-детект через containerd events — TODO (сейчас OOM виден как failed c кодом 137).
6. **Пул портов in-memory** (спека §2: durable-состояния на агенте нет). Чужой процесс на порту из пула проявится как «игра не забиндилась» → нет `ready` → failed по grace.
7. **`oom_score_adj=500`** дедикам (агент 0) — при memory pressure ядро убивает дедики раньше агента (спека §3).
8. **env-контракт**: `BIRDMAN_PORT`, `BIRDMAN_SERVER_ID`, `BIRDMAN_SOCKET=/birdman/agent.sock`, `BIRDMAN_REGION` (спека §3). Labels контейнера `birdman/server-id|port|image|state|match-id` — источник восстановления карты (`state`/`match-id` агент обновляет при переходах).
9. **Реконнект liba**: агент реплеит последние `allocated`/`drain` (protocol.md §2); ping каждые 10с; тишина >15с при `allocated` — warning в лог (эскалация в heartbeat-стейт — итерация 2).
10. **Ack = «команда принята»** (итерация 1): агент подтверждает cmd_id сразу после регистрации команды, исход доносят ServerEvent'ы и heartbeat (при ре-доставке cmd_id из кэша последних 1024 — только повторный Ack). Событ/PullReport-очередь (outbox) переживает реконнекты; перед отправкой событий уходит свежий heartbeat — master видит консистентный стейт (порт до ready).
11. **Доставка `allocated` дедику** (итерация 2): команда `AllocateServer{server_id, match_id, players_expected}` (protocol.md §1, аддитивно) → агент шлёт liba UDS-фрейм `allocated{match_id, players_expected}`, переводит `ready → allocated` и пишет match-id в labels (переживает рестарт агента). Фрейм кэшируется UDS-сервером и реплеится реконнектящейся liba; повторная команда (реплей master'а после рестарта агента) идемпотентна — только пере-доставка фрейма и labels. Штатный конец одноразового дедика: `match_end` → exit 0 → `stopped` (master делает `reaped`, никакого crash-loop).

Отложено (по плану, не долг): полный node-drain-цикл и self-upgrade (итерация 4; per-server DrainServer уже реализован в итерации 3), OOM-события, image GC, UDP-echo 19999, метрики 9101, ротация логов.
