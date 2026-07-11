# stub-server

Минимальный «игровой сервер» birdman — наш экзампл и тестовая нагрузка итераций 0–1. Две роли:

1. **UDP-чат** — играбельный дедик без клиента: подключайся `nc -u`, играй командами.
2. **Референс liba** — полная серверная сторона SDK-контракта (`docs/specs/protocol.md` §2): NDJSON поверх unix socket, lifecycle `hello → ready → allocated → match_start → players → match_end`, drain, реконнект. Настоящая игра в итерации 2 обязана вести себя так же.

## Режимы

- **Managed** (есть `BIRDMAN_SOCKET`): полный жизненный цикл; после `match_end` процесс завершается — дедик одноразовый, слот пересоздаёт master.
- **Standalone** (нет env): та же игра без агента — аналог no-op режима SDK. Для локальной разработки.

## Env-контракт (ставит agent)

| Переменная | Что это | Дефолт |
|---|---|---|
| `BIRDMAN_PORT` | UDP-порт игры | `7777` |
| `BIRDMAN_SOCKET` | путь agent-сокета | — (standalone) |
| `BIRDMAN_SERVER_ID` | id дедика | hostname |

## UDP-протокол (текст, 1 датаграм = 1 команда)

```
PING         → PONG server=<id> players=<n> state=<ready|allocated|match|draining>
JOIN <name>  → WELCOME <name> players=<n>   (+ broadcast JOINED)
SAY <text>   → broadcast MSG <name>: <text>
LEAVE        → BYE <name>                   (+ broadcast LEFT)
```

Таймаут игрока без пакетов — 60с.

## Запуск

```bash
# локально (standalone)
docker build -t birdman-stub .
docker run --rm -p 27777:7777/udp birdman-stub
printf 'PING' | nc -u -w1 127.0.0.1 27777

# тесты (полный managed-цикл против mock-агента)
go test -race ./...
```

Образ собирает CI: `ghcr.io/ufna/birdman-stub-server` (приватный; на тачках pull через `GHCR_PULL_TOKEN`).
