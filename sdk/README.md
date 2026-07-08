# birdman SDK

Серверная сторона liba-контракта (`docs/specs/protocol.md` §2) для дедиков +
заготовка UE-плагина. Спека — `docs/specs/sdk.md`.

```
sdk/core/       C++17 статическая либа birdman_core (POSIX + stdlib, ноль зависимостей)
sdk/mockagent/  Go CLI: локальный «агент» для отладки интеграции игры
sdk/example/    референс-«игра» на core (UDP + полный lifecycle) — цель smoke-теста
sdk/unreal/     UE-плагин Birdman (DRAFT, не компилировался — см. его README)
sdk/scripts/    smoke.sh — example против mockagent end-to-end
```

## Замороженный контракт

**`sdk/core/include/birdman/birdman.h` — контракт v0, заморожен 08.07.2026**
(конец «начала» итерации 2 по `docs/05-runtime-iterations.md`). Дальше —
semver и только аддитивные изменения: новые методы, новые поля `Config` с
безопасными дефолтами, новые поля событий. Менять/удалять существующее — v2.
Версия уходит агенту в `hello.sdk_version` (`birdman-cpp/0.1.0`).

API-обзор (полные доки — в самом заголовке):

```cpp
birdman::ServerLink link;
birdman::Config cfg;                         // kDispatch (дефолт) или kPoll
cfg.on_allocated = ...;                      // AllocatedEvent{match_id, players_expected, metadata}
cfg.on_drain_requested = ...;                // DrainEvent{deadline_seconds, reason}
bool managed = link.Init(cfg);               // env BIRDMAN_* нет → no-op режим, всё безопасно

link.NotifyReady();                          // ≤30с от старта процесса
link.SetPlayerCount(n);                      // при каждом изменении
link.NotifyMatchStart();
link.ReportMetric("tick_ms", 16.6);          // ≤1/с на имя (коалесится)
link.NotifyMatchEnd(MatchResult::kCompleted);// после — процесс выходит сам
link.PollCallbacks();                        // только в kPoll: раз в тик игры
link.Shutdown();
```

Модель колбэков: **по умолчанию `kDispatch`** — события приходят из
внутреннего I/O-потока сразу (обработчик обязан быть thread-safe; UE-плагин
перебрасывает их на game thread через `AsyncTask`). Опционально **`kPoll`** —
события копятся в очереди, игра забирает `PollCallbacks()` на своём тике и
получает их в своём потоке без единого лока (см. `sdk/example/main.cpp`).

Транспорт (игре не виден): реконнект с бэкоффом (0.1с → 2с) навсегда;
на каждом коннекте `hello` + реплей состояния (`ready`, `players`);
`match_start`/`match_end` при обрыве копятся в кольце на 256 фреймов;
входящие реплеи `allocated`/`drain` от агента дедуплицируются; `ping`→`pong`;
неизвестные типы фреймов игнорируются (forward-compat).

## Сборка и тесты

Нужны cmake ≥3.16 и любой C++17-компилятор; Go 1.24 — только для
mockagent/smoke (без Go smoke-тест сам отключается).

```sh
cmake -S sdk -B sdk/build -DCMAKE_BUILD_TYPE=Release
cmake --build sdk/build -j
ctest --test-dir sdk/build --output-on-failure
```

Тесты: юниты JSON, полный lifecycle против in-process mock-агента, no-op
режим, реконнект+реплей, кольцевой буфер (и его переполнение), rate-limit
метрик, ping/pong, poll-режим, конкурентность, **golden wire-тесты** против
канонических фреймов `protocol.md` §2 (сравнение распарсенного JSON) и smoke
`example↔mockagent` (полный цикл ready→allocate→match_end→exit 0).

Санитайзеры: `-DBIRDMAN_SANITIZE=thread|address` (CI гоняет TSAN-джобу).
На macOS (Darwin 25.x) санитайзер-рантаймы Apple clang неработоспособны
(TSAN падает на старте, ASAN виснет) — гоняйте их в docker/CI (linux);
проверено: полный набор тестов зелёный под TSAN на linux/gcc.

Нет cmake/Go на машине — полный прогон в docker:

```sh
docker run --rm -v "$PWD":/w -w /w golang:1.24-bookworm sh -c \
  'apt-get update -qq && apt-get install -y -qq cmake g++ >/dev/null &&
   cmake -S sdk -B /tmp/b && cmake --build /tmp/b -j &&
   ctest --test-dir /tmp/b --output-on-failure'
```

## Локальная отладка интеграции (mockagent)

```sh
cd sdk/mockagent && go build -o /tmp/mockagent .
/tmp/mockagent -socket /tmp/agent.sock
# в соседнем терминале — ваша игра:
BIRDMAN_SOCKET=/tmp/agent.sock BIRDMAN_SERVER_ID=dev-1 BIRDMAN_PORT=7777 ./your-server
# в stdin mockagent: allocate m-1 2 | drain 30 deploy | ping
```

mockagent повторяет поведение настоящего агента: печатает все фреймы liba,
реплеит последний `allocated`/`drain` при реконнекте, пингует раз в 10с.
`BIRDMAN_SDK_DEBUG=1` включает диагностику SDK в stderr игры.

## Проверено против реального агента (08.07.2026)

Контракт `sdk/core` валидирован не только против mockagent, но и против
НАСТОЯЩЕГО `birdman-agent` на дев-тачке. `sdk/example` собран в Docker-образ
(`sdk/example/Dockerfile`, CI-выпуск `.github/workflows/sdk.yml` →
`ghcr.io/ufna/birdman-sdk-example:<tag>` по `gh workflow run sdk -f tag=…`),
задеплоен как обычная версия через флот и провёл реальный матч 2 игроков
end-to-end (`ready → allocated → match_start → players → match_end → reaped`),
0 потерянных датаграмм. Это ровно тот путь, которым поедет UE-игра — см.
`docs/05-runtime-iterations.md`, «Итерация 2 · SDK-валидация». Значит: чтобы
поставить игру на платформу, достаточно собрать её сервер с этим SDK в
Docker-образ и зарегистрировать версию — платформенная сторона доказана.
