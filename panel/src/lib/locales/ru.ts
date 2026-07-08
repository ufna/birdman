// Русский перевод. Ключи 1:1 с en.ts (тип Record<MessageKey, string> не даст
// разойтись: пропуск ключа или лишний ключ — ошибка компиляции; плюс есть
// runtime-тест паритета). Тон — как в текущем UI. Глоссарий — см. en.ts.

import type { MessageKey } from './en';

export const ru: Record<MessageKey, string> = {
  // — общее —
  'common.close': 'Закрыть',
  'common.cancel': 'Отмена',
  'common.running': 'Выполняем…',
  'common.retry': 'Повторить',
  'common.loading': 'Загрузка…',
  'common.port': 'порт',
  'common.uptime': 'аптайм',
  'common.playersCount.one': '{count} игрок',
  'common.playersCount.few': '{count} игрока',
  'common.playersCount.many': '{count} игроков',
  'common.playersCount.other': '{count} игрока',

  // — навигация —
  'nav.overview': 'Обзор',
  'nav.fleet': 'Флот',
  'nav.matches': 'Матчи',
  'nav.deploys': 'Деплои',
  'nav.events': 'События',
  'nav.sections': 'Разделы',

  // — индикатор стрима событий —
  'live.live': 'live',
  'live.connecting': 'подключение…',
  'live.reconnecting': 'переподключение…',
  'live.noAccess': 'нет доступа',
  'live.title': 'Состояние стрима событий',

  // — тема / язык / сессия —
  'theme.toLight': 'Светлая тема',
  'theme.toDark': 'Тёмная тема',
  'lang.switch': 'Язык',
  'shell.keyName': 'Имя API-ключа сессии',
  'shell.logout': 'Выйти',

  // — гейт доступа (корень) —
  'app.noScope': 'У ключа {name} нет скоупа readonly — показывать нечего.',
  'app.switchKey': 'Войти другим ключом',

  // — логин —
  'login.apiKey': 'API-ключ',
  'login.reveal': 'Показать ключ',
  'login.hide': 'Скрыть ключ',
  'login.checking': 'Проверяем…',
  'login.submit': 'Войти',
  'login.hint':
    'Нужен ключ со скоупом readonly или admin. Первый admin-ключ master печатает в лог при старте с пустой таблицей ключей.',
  'login.err.badKey': 'Ключ не подошёл. Проверьте значение и что он не отозван.',
  'login.err.unreachable': 'Master недоступен. Проверьте, что процесс запущен.',

  // — общие блоки UI —
  'ui.err.forbidden': 'Не хватает прав: нужен ключ со скоупом readonly или admin.',
  'ui.err.loadFailed': 'Не удалось загрузить данные: {detail}',

  // — заголовки колонок (общие) —
  'col.match': 'Матч',
  'col.region': 'Регион',
  'col.version': 'Версия',
  'col.state': 'Состояние',
  'col.players': 'Игроки',
  'col.duration': 'Длительность',
  'col.server': 'Сервер',
  'col.playersPeak': 'Пик игроков',
  'col.created': 'Создан',
  'col.node': 'Тачка',
  'col.slots': 'Слоты',
  'col.heartbeat': 'Heartbeat',
  'col.agent': 'Агент',
  'col.dedic': 'Дедик',
  'col.port': 'Порт',
  'col.uptime': 'Аптайм',

  // — пагинатор —
  'pager.newer': '← Новее',
  'pager.older': 'Старше →',

  // — подписи состояний (коды остаются в API) —
  'state.node.active': 'Активна',
  'state.node.draining': 'Опустошается',
  'state.node.quarantine': 'Карантин',
  'state.node.dead': 'Мертва',
  'state.server.creating': 'Создаётся',
  'state.server.ready': 'Готов',
  'state.server.allocated': 'В игре',
  'state.server.draining': 'Опустошается',
  'state.server.failed': 'Упал',
  'state.server.reaped': 'Снят',
  'state.match.pending': 'Ожидает',
  'state.match.running': 'Идёт',
  'state.match.finished': 'Завершён',
  'state.match.aborted': 'Прерван',
  'state.version.registered': 'Зарегистрирована',
  'state.version.prepulling': 'Прогрев',
  'state.version.active': 'Активна',
  'state.version.deprecated': 'Депрекейт',
  'state.version.disabled': 'Отключена',

  // — экран «Матчи» —
  'matches.tabs': 'Матчи',
  'matches.tab.live': 'Live',
  'matches.tab.history': 'История',
  'matches.live.title': 'Идут сейчас',
  'matches.live.empty':
    'Живых матчей нет. Как только матчмейкер соберёт группу — матч появится здесь без перезагрузки.',
  'matches.history.title': 'История',
  'matches.filter.stateAria': 'Фильтр по состоянию',
  'matches.filter.regionAria': 'Фильтр по региону',
  'matches.filter.allStates': 'все состояния',
  'matches.filter.allRegions': 'все регионы',
  'matches.history.emptyNext': 'Дальше пусто — вернитесь назад.',
  'matches.history.emptyFilter': 'Под эти фильтры матчей нет.',

  // — дровер деталей матча —
  'match.openDetails': 'Открыть детали матча',
  'match.sections': 'Разделы матча',
  'match.tab.overview': 'Обзор',
  'match.tab.stats': 'Статистика',
  'match.tab.logs': 'Логи',
  'match.started': 'Начало',
  'match.ended': 'Конец',
  'match.statsNote': 'Статистика матча из VictoriaMetrics за окно матча, через master-proxy.',
  'match.logs.gone': 'Логи этого матча больше недоступны.',
  'match.noServer': 'Дедик этого матча уже вычищен — логи и метрики недоступны.',
  'match.liveHint': 'Живой матч — обновляется',

  // — дровер дедика —
  'server.dedic': 'Дедик',
  'server.openDetails': 'Открыть детали дедика',
  'server.openDetailsFull': 'Открыть детали дедика: таймлайн, логи, метрики',
  'server.notInList': 'нет в текущем списке дедиков (возможно, reaped) — логи и метрики доступны',
  'server.tab.timeline': 'Таймлайн',
  'server.tab.logs': 'Логи',
  'server.tab.metrics': 'Метрики',
  'server.tabs.aria': 'Разделы дедика',
  'server.metricsNote':
    'Метрики этого дедика из VictoriaMetrics через master-proxy. Окно 30 минут, обновление каждые 15с.',
  'server.noEvents': 'Событий по этому дедику пока нет.',

  // — графики метрик —
  'metric.aria': 'График метрики',
  'metric.loading': 'Загрузка метрик…',
  'metric.unconfigured': 'Метрики не настроены на этом master (victoriametrics_url пуст).',
  'metric.unreachable': 'VictoriaMetrics недоступна — данных сейчас нет.',
  'metric.error': 'Метрики недоступны: {error}',
  'metric.noData': 'Нет данных за выбранный период.',
  'metric.players': 'Игроки',
  'metric.tick': 'Tick, мс',
  'metric.cpu': 'CPU, ядра',
  'metric.mem': 'Память',

  // — просмотр логов —
  'logs.tailAria': 'Сколько последних строк тянуть',
  'logs.preparing': 'Готовим…',
  'logs.download': 'Скачать',
  'logs.empty': 'Логов нет.',
  'logs.error': 'Не удалось получить логи.',
  'logs.waiting': 'Ждём строки…',
  'logs.gone': 'Логи больше недоступны.',
  'logs.connecting': 'подключение…',
  'logs.liveTail': 'live tail',
  'logs.reading': 'читаем…',
  'logs.done': 'готово',
  'logs.errorShort': 'ошибка',

  // — диалог подтверждения —
  'confirm.err.forbidden': 'Недостаточно прав для этого действия.',
  'confirm.err.conflict': 'Конфликт состояния — действие сейчас недоступно.',
  'confirm.err.generic': 'Не удалось выполнить действие.',

  // — экран «Флот» —
  'fleet.nodesCount.one': '{count} тачка',
  'fleet.nodesCount.few': '{count} тачки',
  'fleet.nodesCount.many': '{count} тачек',
  'fleet.nodesCount.other': '{count} тачки',
  'fleet.emptyPre': 'Тачек пока нет. Зарегистрируйте первую:',
  'fleet.node.noServers': 'На тачке нет дедиков.',
  'fleet.drain.playing.one': '{count} доигрывает',
  'fleet.drain.playing.few': '{count} доигрывают',
  'fleet.drain.playing.many': '{count} доигрывают',
  'fleet.drain.playing.other': '{count} доигрывают',
  'fleet.drain.empty': 'опустошена',
  'fleet.hb.never': 'не был',
  'fleet.drain': 'Drain',
  'fleet.undrain': 'Undrain',
  'fleet.drain.title': 'Вывести {host} из ротации?',
  'fleet.drain.desc':
    'Новые дедики размещаться не будут, ready-буфер реапится, а allocated доигрывают свои матчи. Тачку можно вернуть кнопкой Undrain.',
  'fleet.undrain.title': 'Вернуть {host} в ротацию?',
  'fleet.undrain.desc':
    'Reconcile снова начнёт размещать дедики на этой тачке, агенту уйдёт Undrain.',

  // — экран «Обзор» —
  'ov.liveMatches': 'Живые матчи',
  'ov.liveDetail': 'running {running} · pending {pending}',
  'ov.playersOnline': 'Игроки онлайн',
  'ov.playersDetail': 'по allocated-дедикам',
  'ov.readyBuffer': 'Ready-буфер',
  'ov.noReady': 'нет ready-дедиков',
  'ov.nodes': 'Тачки',
  'ov.inQuarantine': '{count} в карантине',
  'ov.allActive': 'все активны',
  'ov.fleetVersion': 'Версия флита',
  'ov.noLiveDedics': 'нет живых дедиков',
  'ov.matchesHour': 'Матчи за час',
  'ov.recentEvents': 'Последние события',
  'ov.sparkAria': 'Матчи за последний час, по минутам',

  // — экран «События» —
  'events.feedUnavailable': 'Лента событий недоступна.',
  'events.none': 'Событий ещё не было.',
  'events.kindAria': 'Фильтр по kind',
  'events.nodeAria': 'Фильтр по тачке',
  'events.periodAria': 'Период',
  'events.windowAria': 'Размер окна ленты',
  'events.allKinds': 'все kind',
  'events.allNodes': 'все тачки',
  'events.window': 'окно {count}',
  'events.emptyFilter': 'Под эти фильтры событий нет.',
  'events.eventsCount.one': '{count} событие',
  'events.eventsCount.few': '{count} события',
  'events.eventsCount.many': '{count} событий',
  'events.eventsCount.other': '{count} события',
  'events.ofTotal': 'из {total}',
  'events.pageOf': 'стр. {page}/{pages}',
  'period.all': 'всё время',
  'period.15m': '15 минут',
  'period.1h': '1 час',
  'period.24h': '24 часа',

  // — экран «Деплои» —
  'deploys.emptyPre': 'Версий пока нет. Зарегистрируйте билд:',
  'deploys.project': 'Проект {project}',
  'deploys.versionsCount.one': '{count} версия',
  'deploys.versionsCount.few': '{count} версии',
  'deploys.versionsCount.many': '{count} версий',
  'deploys.versionsCount.other': '{count} версии',
  'deploys.col.liveDedics': 'Дедиков живо',
  'deploys.col.created': 'Создана',
  'deploys.col.deprecated': 'Депрекейт',
  'deploys.rollback': 'Откатить',
  'deploys.rollback.title': 'Откат проекта {project}',
  'deploys.rollback.desc':
    'Активную версию заменит последняя deprecated ({semver}). Образы уже на тачках — откат за секунды, живые матчи не рвутся.',
  'deploys.warming': 'идёт прогрев…',
  'deploys.deploy': 'Развернуть',
  'deploys.deploy.title': 'Развернуть {semver}?',
  'deploys.deploy.desc':
    'Master спрогреет образ на всех живых тачках (pre-pull), затем атомарно переключит активную версию. Живые матчи старой версии доигрывают в окне мультиверсий.',
  'deploys.window': 'Окно мультиверсий',
  'deploys.noActive': 'Нет активной версии.',
  'deploys.dedicsCount.one': '{count} дедик',
  'deploys.dedicsCount.few': '{count} дедика',
  'deploys.dedicsCount.many': '{count} дедиков',
  'deploys.dedicsCount.other': '{count} дедика',
  'deploys.regionActive': 'Активно по регионам',
  'deploys.noLiveDedics': 'Живых дедиков нет.',
  'deploys.emptyProject': 'Версий проекта нет.',
  'deploys.prepullPre': 'Прогрев',
  'deploys.prepullSuffix': '(pre-pull)',
  'deploys.prepull.nodes': '{pulled} / {total} тачек',
  'deploys.prepull.waiting': 'Ждём отчёты о прогреве по тачкам (события deploy_node_pulled)…',
};
