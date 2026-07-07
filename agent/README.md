# birdman-agent

Нода-агент birdman, **v0 = итерация 0** (`docs/05-runtime-iterations.md`): супервизия одного игрового дедика через containerd в режиме `run-once`. Master'а ещё нет — gRPC-линк, heartbeat и восстановление карты серверов приходят в итерации 1. Спеки: `docs/specs/agent.md`, `docs/specs/protocol.md` §2.

Что внутри v0: ensure/pull образа (включая приватный GHCR), пул host-портов, запуск контейнера (host network, cgroup-лимиты, env-контракт `BIRDMAN_*`, bind-mount per-server сокета), UDS-сервер liba-протокола (NDJSON), стейт-машина `pulling → starting → ready → allocated → draining → stopped|failed`, grace 30с до `ready`, логи дедика, graceful stop по SIGTERM.

## Сборка

Go на хосте не нужен — только docker:

```bash
./build.sh          # → dist/birdman-agent (static ELF linux/amd64, stripped)
file dist/birdman-agent
# ELF 64-bit LSB executable, x86-64, statically linked
```

Версия зашивается из `git describe` (`birdman-agent version`).

## Конфиг

`/etc/birdman/agent.yaml` — v0 читает подмножество спеки §10 (неизвестные ключи, например `master_addr`/`node_token` из полного конфига, игнорируются — форвард-совместимость):

```yaml
region: "eu"
capacity_slots: 24                    # v0 не использует
port_range: [20000, 29999]            # дефолт, если не задан
limits_default: { cpu_millis: 3500, mem_mb: 4096 }
log_dir: /var/log/birdman
data_dir: /var/lib/birdman
registry_auth:                        # для pull приватного GHCR
  username: "ufna"
  token_file: /etc/birdman/ghcr.token # токен ТОЛЬКО в файле, не в конфиге/коде
```

## Запуск

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
# unit (пул портов, конфиг, UDS против фейкового liba-клиента, стейт-машина/grace)
docker run --rm -v "$PWD":/src -w /src golang:1.24 sh -c "go vet ./... && go test -race ./..."

# интеграция с containerd — на Linux-тачке с демоном (скипается в обычном прогоне):
go test -race -tags integration ./internal/runtime/
```

Интеграционный тест параметризуется env: `BIRDMAN_TEST_CONTAINERD`, `BIRDMAN_TEST_IMAGE`, `BIRDMAN_TEST_SOCKDIR` (см. шапку `internal/runtime/integration_test.go`). Прогнан против реального containerd 1.7 (Docker Desktop VM); там же прогнаны E2E run-once: полный матч-цикл со stub-server (UDP-игрок, `match_end completed`, exit 0), SIGTERM-стоп, grace-таймаут (exit 137), drain.

## Принятые решения (v0)

1. **Клиентская библиотека — `github.com/containerd/containerd` v1.7.x, не `/v2`.** Целевые тачки — Ubuntu 24.04 с docker 28.x, т.е. демон containerd **1.7.x**; клиент той же линии исключает сюрпризы совместимости (v2-клиент рассчитан на демоны 2.x: transfer-service pull, sandbox API). Переход на `/v2` — осознанно, вместе с апгрейдом демона на тачках. Примечание: `runtime-spec` запинен на v1.1.0 (v1.3 ломает компиляцию containerd 1.7).
2. **EnsureImage вместо голого pull**: сначала локальный образ (спека §3 «ensure image» — PrePull-семантика итерации 3), иначе pull; авторизация GHCR через resolver с creds из `token_file`.
3. **Логи дедика** пишет агент (cio-стримы → файл). TODO итерация 1: shim-side лог (`cio.LogFile`/LogURI), чтобы рестарт агента не рвал поток; ротация 100MB×2 + gzip (спека §5) — тоже TODO, в v0 нет.
4. **Сокет 0666**: identity дедика = его сокет (protocol.md §2), других процессов на тачке нет доверенных/недоверенных градаций в v0; образы игр бывают non-root (наш stub — distroless **nonroot**), им нужен connect.
5. **exit-watch через `task.Wait`**, заармленный `context.WithoutCancel`: канал выхода обязан пережить отмену сигнального контекста, иначе при SIGTERM агент получает фиктивный `context canceled` вместо реального кода (поймано E2E-тестом). OOM-детект через events — итерация 1 (там он нужен для Event master'у).
6. **Пул портов in-memory** (спека §2: durable-состояния на агенте нет). Чужой процесс на порту из пула проявится как «игра не забиндилась» → нет `ready` → failed по grace.
7. **`oom_score_adj=500`** дедикам (агент 0) — при memory pressure ядро убивает дедики раньше агента (спека §3).
8. **env-контракт**: `BIRDMAN_PORT`, `BIRDMAN_SERVER_ID`, `BIRDMAN_SOCKET=/birdman/agent.sock`, `BIRDMAN_REGION` (спека §3). Labels контейнера `birdman/server-id|port|image` — под восстановление карты в итерации 1.
9. **Реконнект liba**: агент реплеит последние `allocated`/`drain` (protocol.md §2); ping каждые 10с; тишина >15с при `allocated` — warning в stderr (heartbeat-эскалация — итерация 1).

Отложено (по плану, не долг): gRPC-линк с master, восстановление карты по labels, heartbeat 2с, OOM-события, image GC, self-upgrade, UDP-echo, метрики 9101, ротация логов; ansible-плейбук тачки — отдельная задача итерации 0.
