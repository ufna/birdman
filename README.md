# birdman

Платформа для хостинга dedicated-серверов наших игр (рабочее название). Модель: **для собственных игр → self-host → open source (MIT)**. Свой лёгкий рантайм без Kubernetes: master (матчмейкер + управление флотом) + agent на тачках (containerd) + SDK в дедиках + красивая админ-панель. Linux-only, session-based.

## Документы

| Документ | Что внутри |
|---|---|
| [docs/birdman-report.html](docs/birdman-report.html) | Сводный отчёт: рынок, стратегия v2, план, решения (открыть в браузере) |
| [docs/01-competitive-landscape.md](docs/01-competitive-landscape.md) | Конкурентный ландшафт на 07.2026: Hydra, Multiplay/RSG, GameLift, PlayFab, AccelByte, Edgegap, Gameye, GameFabric, i3D, Agones/OSS, ценовая карта, регионы, боли клиентов. Все факты с источниками |
| [docs/02-strategy-analysis.md](docs/02-strategy-analysis.md) | Критический анализ исходной стратегии (CTO/CEO/маркетинг). Частично устарел — см. пометку v2 в шапке |
| [docs/03-implementation-roadmap.md](docs/03-implementation-roadmap.md) | План реализации v2: архитектура, поставщики, панель, фазы 0–4, SLO/KPI, риски |
| [docs/04-agones-vs-native.html](docs/04-agones-vs-native.html) | Разбор «Agones+K8s vs свой рантайм». Итог: свой рантайм (решение 07.07.2026) |
| [docs/05-runtime-iterations.md](docs/05-runtime-iterations.md) | Итерации 0–5 до прода + трек П (панель) + что нужно к каждой итерации |
| [docs/specs/](docs/specs/README.md) | Спеки компонентов: master, agent, SDK, протоколы, панель, ops/CI/ansible |

## Ключевые решения (07.07.2026)

- **Свой рантайм, без Agones/K8s**: master + agent + SDK; ansible ставит тачки, жизнью дедиков управляет только master/agent-цикл.
- **SDK-first**: обязательная либа в дедике (lifecycle, игроки, tick-метрики), wrapper-режима нет.
- **Linux-only, session-based**: Windows и персистентные миры — вне скоупа.
- **Экономика заморожена**: ни прайса, ни биллинга — строим для себя; рыночный контекст в 01 сохраняем как бенчмарк.
- **MIT** по умолчанию; открываем код в фазе 3 («built for our games, open-sourced»).
- Прод-железо только с включённой DDoS-защитой (OVH game / Latitude).
