# Спеки birdman

> Дата: 07.07.2026 (спеки поддерживаются в актуальном состоянии, пометки «уточнено в v0»). Спеки — источник истины для реализации. Расхождение кода со спекой = баг документации: чиним спеку в том же PR, что и код. Язык кода и идентификаторов — английский, доков — русский.
>
> Раздел «Что нужно для старта (итерация 0)» ниже — исторический (итерации 0–4 уже выполнены).

## Индекс

| Спека | Что покрывает | Итерации |
|---|---|---|
| [architecture.md](architecture.md) | Компоненты, потоки, ключевые решения, глоссарий, нефункциональные цели | все |
| [master.md](master.md) | Схема Postgres, reconcile, allocation, матчмейкер v0, deploy-менеджер, публичный REST API | 1–3, 5 |
| [agent.md](agent.md) | containerd, порты, супервизия дедиков, логи, image GC, self-upgrade, конфиг | 0–1, 3–4 |
| [protocol.md](protocol.md) | gRPC master↔agent (mTLS), NDJSON liba↔agent (unix socket), версионирование | 1–2 |
| [sdk.md](sdk.md) | UE-плагин + core-либа: API поверхность, no-op режим, клиентский матчмейкинг, заморозка контракта | 2 |
| [panel.md](panel.md) | Экраны П0–П2, данные и эндпоинты, стек, встраивание в master | трек П |
| [ops.md](ops.md) | Метрики, алерты, логи, CI/CD, версионная конвенция, ansible, runbooks, бэкапы | 3–5 |

## Структура репозитория (план)

```
birdman/
  master/     # Go: матчмейкер + флот + API (+ go:embed панели)
  agent/      # Go: нода-агент
  sdk/
    core/     # C++17 core-либа (без UE-зависимостей)
    unreal/   # UE-плагин (Birdman)
  examples/
    stub-server/  # референс-дедик: UDP-чат + серверная сторона liba-контракта (есть)
  panel/      # React + TS (SPA)
  proto/      # protobuf master↔agent + JSON-схемы liba↔agent
  infra/      # ansible: inventories, playbooks, roles
  docs/       # исследование, планы, спеки
```

## Что нужно для старта (итерация 0)

1. **Одна тестовая тачка**: Ubuntu 24.04 LTS (или 22.04), root/sudo по SSH, ≥4 ядра, ≥8 GB RAM, ≥100 GB диск, публичный IP; открытые порты: 22 (SSH), 443 и 8443 (master, когда до него дойдём), 20000–29999 TCP+UDP (дедики). Для итерации 0 любой дедик/VPS ок; прод (итерация 5) — только OVH game-линейка / Latitude (включённый DDoS).
2. **Linux server build нашей игры** + инструкция запуска (порт/аргументы), либо доступ к репо игры/CI. Если билда пока нет — итерации 0–1 идут на stub-сервере (UDP echo на Go), настоящая игра подключается в итерации 2.
3. **GitHub**: репозиторий (по умолчанию — этот, monorepo) + включённый GHCR и токен с правом push пакетов.
4. Не блокеры, понадобятся позже: Discord webhook (итерация 4), вторая тачка (итерация 5), фронтенд-инженер или +3–4 недели (трек П), ~0.5 инженера команды игры (итерации 2–3).

## Дефолты (если не возразили — действуют)

Go 1.24+ (master/agent, static binaries), Ubuntu 24.04 LTS, containerd без Docker-демона, Postgres 16, REST+SSE для публичного API, gRPC+mTLS для master↔agent, NDJSON поверх unix socket для liba↔agent, React+TypeScript+Vite для панели, VictoriaMetrics+Grafana+vmalert для наблюдаемости, ansible для сетапа тачек, GitHub Actions для CI.
