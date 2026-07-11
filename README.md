# birdman

Платформа для хостинга dedicated-серверов наших игр (рабочее название). Модель: **для собственных игр → self-host → open source (MIT)**. Свой лёгкий рантайм без Kubernetes: master (матчмейкер + управление флотом) + agent на тачках (containerd) + SDK в дедиках + красивая админ-панель. Linux-only, session-based.

**Статус (08.07.2026):** итерации 0–4 + вся админ-панель (П0–П2) + C++ SDK построены и приняты на живой дев-тачке; в `main`, CI зелёный. Осталось то, что требует внешних ресурсов (вторая тачка, билд игры, Discord/S3/uptime). Репо пока приватный (**pre-OSS**), открываем в фазе 3. **Точка входа для продолжения — [docs/STATUS.md](docs/STATUS.md).**

**Запустить у себя (self-host):** [docs/self-host.md](docs/self-host.md) — от `git clone` до первого матча: master в Docker (`deploy/`), ввод игровой ноды одной командой (`infra/add-node.sh`), выкат версии и матч (`mmcli`).

## Документы

| Документ | Что внутри |
|---|---|
| [docs/STATUS.md](docs/STATUS.md) | **Статус реализации (handoff): что готово, что на боксе, как получить доступ, что дальше. Читать первым.** |
| [docs/self-host.md](docs/self-host.md) | **Self-host-квикстарт: master (`deploy/`) → первая нода (`add-node.sh`) → версия и первый матч (`mmcli`). От `git clone` до матча, каждый сниппет проверяем.** |
| [docs/birdman-report.html](docs/birdman-report.html) | Сводный отчёт: рынок, стратегия v2, план, решения (открыть в браузере) |
| [docs/01-competitive-landscape.md](docs/01-competitive-landscape.md) | Конкурентный ландшафт на 07.2026: Hydra, Multiplay/RSG, GameLift, PlayFab, AccelByte, Edgegap, Gameye, GameFabric, i3D, Agones/OSS, ценовая карта, регионы, боли клиентов. Все факты с источниками |
| [docs/02-strategy-analysis.md](docs/02-strategy-analysis.md) | Критический анализ исходной стратегии (CTO/CEO/маркетинг). Частично устарел — см. пометку v2 в шапке |
| [docs/03-implementation-roadmap.md](docs/03-implementation-roadmap.md) | План реализации v2: архитектура, поставщики, панель, фазы 0–4, SLO/KPI, риски |
| [docs/04-agones-vs-native.html](docs/04-agones-vs-native.html) | Разбор «Agones+K8s vs свой рантайм». Итог: свой рантайм (решение 07.07.2026) |
| [docs/05-runtime-iterations.md](docs/05-runtime-iterations.md) | Итерации 0–5 до прода + трек П (панель); у 0–4 проставлены отметки «Выполнено» с фактами |
| [docs/specs/](docs/specs/README.md) | Спеки компонентов: master, agent, SDK, протоколы, панель, ops/CI/ansible (поддерживаются в актуальном состоянии, пометки «уточнено в v0») |

## Ключевые решения (07.07.2026)

- **Свой рантайм, без Agones/K8s**: master + agent + SDK; ansible ставит тачки, жизнью дедиков управляет только master/agent-цикл.
- **SDK-first**: обязательная либа в дедике (lifecycle, игроки, tick-метрики), wrapper-режима нет.
- **Linux-only, session-based**: Windows и персистентные миры — вне скоупа.
- **Экономика заморожена**: ни прайса, ни биллинга — строим для себя; рыночный контекст в 01 сохраняем как бенчмарк.
- **MIT** по умолчанию; открываем код в фазе 3 («built for our games, open-sourced»).
- Прод-железо только с включённой DDoS-защитой (OVH game / Latitude).
