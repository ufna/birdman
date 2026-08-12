// Canonical message catalog (source of truth). ru.ts mirrors these keys 1:1
// (enforced at compile time via Record<MessageKey, string> and a parity test).
//
// RULE (100% coverage): every user-facing string in panel/src goes through
// t()/tp() — NO hardcoded UI text. Regressions are caught by the parity test
// and by src/test/no-hardcoded.test.ts (fails on Cyrillic literals outside
// locales/format). When you add UI text, add a key here first.
//
// Glossary — keep terms consistent across the whole UI:
//   Fleet=Флот · Node=Тачка · Dedic=Дедик · Match=Матч · Deploy=Деплой
//   Warm/Ready buffer=Ready-буфер · Multi-version window=Окно мультиверсий
//   Heartbeat=Heartbeat · Drain/Undrain=Drain/Undrain (kept as op names)
//
// Conventions:
//   - {name} placeholders are filled by t()/tp() params.
//   - Plural bases have .one/.few/.many/.other; tp() picks via Intl.PluralRules.
//     English only ever selects one/other, but all four forms are present to
//     keep key parity with Russian.
//   - Brand ("birdman") and API tokens (semver, ids, raw state/event codes) are
//     never shown raw as prose — but their human-readable LABELS live here
//     (state.*, event.*, scope.*). A few compact mono tokens (ref.*, logs.tail/
//     follow) are intentionally identical in both languages.

export const en = {
  // — common —
  'common.close': 'Close',
  'common.cancel': 'Cancel',
  'common.running': 'Working…',
  'common.edit': 'Edit',
  'common.save': 'Save',
  'common.delete': 'Delete',
  'common.retry': 'Retry',
  'common.loading': 'Loading…',
  'common.port': 'port',
  'common.uptime': 'uptime',
  'common.playersCount.one': '{count} player',
  'common.playersCount.few': '{count} players',
  'common.playersCount.many': '{count} players',
  'common.playersCount.other': '{count} players',

  // — navigation —
  'nav.overview': 'Overview',
  'nav.fleet': 'Fleet',
  'nav.matches': 'Matches',
  'nav.deploys': 'Deploys',
  'nav.events': 'Events',
  'nav.sections': 'Sections',
  'nav.menu': 'Menu',

  // — accessibility —
  'a11y.skipToContent': 'Skip to content',

  // — event-stream indicator —
  'live.live': 'live',
  'live.connecting': 'connecting…',
  'live.reconnecting': 'reconnecting…',
  'live.noAccess': 'no access',
  'live.title': 'Event stream status',

  // — theme / language / session controls —
  'theme.toLight': 'Light theme',
  'theme.toDark': 'Dark theme',
  'lang.switch': 'Language',
  'shell.keyName': 'Session API key name',
  'shell.scopes': 'Session scopes',
  'shell.logout': 'Sign out',

  // — access gate (App root) —
  'app.noScope': 'The {name} key has no readonly scope — nothing to display.',
  'app.switchKey': 'Sign in with another key',

  // — login —
  'login.apiKey': 'API key',
  'login.reveal': 'Show key',
  'login.hide': 'Hide key',
  'login.checking': 'Checking…',
  'login.submit': 'Sign in',
  'login.hint':
    'You need a key with the readonly or admin scope. Master prints the first admin key to its log on first startup with an empty key table.',
  'login.err.badKey': "The key was rejected. Check the value and that it hasn't been revoked.",
  'login.err.unreachable': 'Master is unreachable. Check that the process is running.',

  // — shared UI blocks —
  'ui.err.forbidden': 'Access denied: you need a key with the readonly or admin scope.',
  // Отказ по ПРИВЯЗКЕ ключа, а не по скоупу (tracker #1000). Говорим про ключ,
  // а не «эти данные вам недоступны навсегда»: серверное сужение (#994) вернёт
  // привязанному оператору его собственные данные.
  'ui.err.boundKey': 'Request refused: your key is bound to {project}/{env} and has no access to this data.',
  'ui.err.loadFailed': "Couldn't load data: {detail}",
  // Общий словарь отказов REST-API (tracker #1005, lib/apiError.ts). Тело
  // ответа мастера сюда не попадает — только машинный код.
  'ui.err.expired': 'The session has expired — sign in again.',
  'ui.err.offline': "Couldn't reach master — check the connection.",
  'ui.err.code': 'The platform refused the request (error {code}).',
  // 400 на ФОРМЕ: мастер отверг значения. Общий ключ, а не свой у каждой
  // формы — действие оператора всюду одно: посмотреть на поля.
  'ui.err.badRequest': 'The master rejected these values — check the fields and try again.',

  // — table column headers (shared) —
  'col.match': 'Match',
  'col.region': 'Region',
  'col.version': 'Version',
  'col.state': 'State',
  'col.players': 'Players',
  'col.duration': 'Duration',
  'col.server': 'Server',
  'col.playersPeak': 'Peak players',
  'col.created': 'Created',
  'col.node': 'Node',
  'col.env': 'Env',
  'col.slots': 'Slots',
  'col.heartbeat': 'Heartbeat',
  'col.agent': 'Agent',
  'col.dedic': 'Dedic',
  'col.port': 'Port',
  'col.uptime': 'Uptime',

  // — pager —
  'pager.newer': '← Newer',
  'pager.older': 'Older →',

  // — state labels (human-readable; codes stay in the API) —
  'state.node.active': 'Active',
  'state.node.draining': 'Draining',
  'state.node.quarantine': 'Quarantine',
  'state.node.down': 'Down',
  'state.node.dead': 'Dead',
  'state.server.creating': 'Creating',
  'state.server.ready': 'Ready',
  'state.server.allocated': 'Allocated',
  'state.server.draining': 'Draining',
  'state.server.failed': 'Failed',
  'state.server.reaped': 'Reaped',
  'state.match.pending': 'Pending',
  'state.match.running': 'Running',
  'state.match.finished': 'Finished',
  'state.match.aborted': 'Aborted',
  'state.version.registered': 'Registered',
  'state.version.prepulling': 'Pre-pulling',
  'state.version.active': 'Active',
  'state.version.deprecated': 'Deprecated',
  'state.version.disabled': 'Disabled',

  // — event-kind labels (raw code kept in the badge title; unknown → code) —
  'event.node_created': 'Node registered',
  'event.node_quarantine': 'Node quarantined',
  'event.node_down': 'Node down',
  'event.node_recovered': 'Node recovered',
  'event.node_drain': 'Node draining',
  'event.node_undrain': 'Node returned',
  'event.node_enrolled': 'Node enrolled',
  'event.node_cert_renewed': 'Cert renewed',
  'event.server_failed': 'Dedic failed',
  'event.server_recovered': 'Dedic recovered',
  'event.server_drain': 'Dedic draining',
  'event.crash_loop': 'Crash loop',
  'event.allocation_failed': 'Allocation failed',
  'event.version_registered': 'Version registered',
  'event.version_disabled': 'Version disabled',
  'event.fleet_updated': 'Fleet updated',
  'event.deploy_started': 'Deploy started',
  'event.deploy_node_pulled': 'Image pulled on node',
  'event.deploy_activated': 'Deploy activated',
  'event.deploy_failed': 'Deploy failed',
  'event.deploy_rolled_back': 'Rolled back',
  'event.deploy_no_nodes': 'Deploy without nodes',
  'event.agent_upgrade': 'Agent upgrade',
  'event.agent_upgrade_succeeded': 'Agent upgraded',
  'event.agent_upgrade_failed': 'Agent upgrade failed',
  'event.registry_upserted': 'Registry saved',
  'event.registry_updated': 'Registry updated',
  'event.registry_removed': 'Registry removed',
  'event.apikey_created': 'Key created',
  'event.apikey_revoked': 'Key revoked',
  'event.apikey_purged': 'Key deleted forever',
  'event.node_env_changed': 'Node moved to env',
  'event.environment_deleted': 'Environment deleted',
  'event.version_promoted': 'Version promoted',
  'event.version_retired': 'Version retired',

  // — API-key scope labels (codes stay in the API) —
  'scope.admin': 'Admin',
  'scope.deploy': 'Deploy',
  'scope.matchmaking': 'Matchmaking',
  'scope.allocate': 'Allocate',
  'scope.readonly': 'Read-only',

  // — compact entity-ref prefixes in the event feed (mono, same in both langs) —
  'ref.node': 'node',
  'ref.srv': 'srv',
  'ref.match': 'match',

  // — Matches screen —
  'matches.tabs': 'Matches',
  'matches.tab.live': 'Live',
  'matches.tab.history': 'History',
  'matches.live.title': 'In progress now',
  'matches.live.empty':
    'No live matches. As soon as matchmaking forms a group, a match appears here without a reload.',
  'matches.history.title': 'History',
  'matches.filter.stateAria': 'Filter by state',
  'matches.filter.regionAria': 'Filter by region',
  'matches.filter.allStates': 'all states',
  'matches.filter.allRegions': 'all regions',
  'matches.history.emptyNext': 'Nothing further — go back.',
  'matches.history.emptyFilter': 'No matches for these filters.',

  // — Match detail drawer —
  'match.openDetails': 'Open match details',
  'match.sections': 'Match sections',
  'match.tab.overview': 'Overview',
  'match.tab.stats': 'Stats',
  'match.tab.logs': 'Logs',
  'match.started': 'Started',
  'match.ended': 'Ended',
  'match.statsNote': "Match stats from VictoriaMetrics over the match window, via master-proxy.",
  'match.logs.gone': "This match's logs are no longer available.",
  'match.noServer': "This match's dedicated server has been cleaned up — logs and metrics are unavailable.",
  'match.liveHint': 'Live match — updating',

  // — dedic drawer (server) —
  'server.dedic': 'Dedic',
  'server.openDetails': 'Open dedic details',
  'server.openDetailsFull': 'Open dedic details: timeline, logs, metrics',
  'server.notInList': 'not in the current dedic list (possibly reaped) — logs and metrics are available',
  'server.tab.timeline': 'Timeline',
  'server.tab.logs': 'Logs',
  'server.tab.metrics': 'Metrics',
  'server.tabs.aria': 'Dedic sections',
  'server.metricsNote':
    "This dedic's metrics from VictoriaMetrics via master-proxy. 30-minute window, refreshed every 15s.",
  'server.noEvents': 'No events for this dedic yet.',

  // — metric charts —
  'metric.aria': 'Metric chart',
  'metric.loading': 'Loading metrics…',
  // Пометка над графиком, чьи точки больше не обновляются (tracker #1011).
  'metric.stale': 'Refused — these points are no longer updating. Sign in again or check your key.',
  'metric.unconfigured': "Metrics aren't configured on this master (victoriametrics_url is empty).",
  'metric.unreachable': 'VictoriaMetrics is unavailable — no data right now.',
  // 502/504 со страницей ШЛЮЗА (tracker #1021). Виновника не называем: перед
  // master шлюз стоит в поставляемой топологии, перед VictoriaMetrics его
  // может поставить оператор, а по одному статусу они не различимы. Текст
  // обязан быть верным в ОБЕИХ и при этом говорить, что смотреть.
  'metric.gatewayDown':
    'A gateway returned an error — metrics are unavailable. Check that master and VictoriaMetrics are up.',
  // Жёсткая ошибка графика. `{code}` — машинный код (ApiError.code), а НЕ
  // сообщение мастера: до #996 сюда интерполировался `${code}: ${detail}` из
  // тела ответа, то есть английская проза мастера/VM попадала на график в
  // русском интерфейсе. Сам код не переводится намеренно — panel.md §1 п.6
  // ставит сырые коды в один ряд с semver и id.
  'metric.error': 'Metrics unavailable (error {code}).',
  'metric.err.expired': 'The session has expired — sign in again to see metrics.',
  'metric.err.offline': "Couldn't reach master — check the connection.",
  // tracker #1007: the upstream does not parse the narrowing query arg, so master
  // refuses instead of answering a project-bound key with the whole fleet.
  'metric.err.narrowing':
    'Metrics are hidden: this master cannot narrow the query to your project — the configured VictoriaMetrics does not understand the extra_label query arg. The operator has to fix victoriametrics_url (self-host docs, §4).',
  'metric.noData': 'No data for the selected period.',
  'metric.players': 'Players',
  'metric.tick': 'Tick, ms',
  'metric.cpu': 'CPU, cores',
  'metric.mem': 'Memory',

  // — log viewer (tail/follow kept as terminal-log jargon in both langs) —
  'logs.tail': 'tail',
  'logs.follow': 'follow',
  'logs.tailAria': 'How many trailing lines to fetch',
  'logs.preparing': 'Preparing…',
  'logs.download': 'Download',
  'logs.empty': 'No logs.',
  'logs.error': "Couldn't fetch logs.",
  'logs.waiting': 'Waiting for lines…',
  'logs.gone': 'Logs are no longer available.',
  'logs.connecting': 'connecting…',
  'logs.liveTail': 'live tail',
  'logs.reading': 'reading…',
  'logs.done': 'done',
  'logs.errorShort': 'error',

  // — logs Live|History segment (Task 5, "Логи v1") —
  'logs.live': 'Live',
  'logs.history': 'History',
  'logs.segmentAria': 'Log view',
  'logs.range.aria': 'Time window',
  'logs.range.1h': '1 h',
  'logs.range.24h': '24 h',
  'logs.range.7d': '7 d',
  'logs.range.14d': '14 d',
  'logs.filterPlaceholder': 'Filter text…',
  'logs.filterApply': 'Apply',
  'logs.more': 'Show more',
  'logs.unconfigured': "Log history isn't configured on this master (victorialogs_url is empty).",
  'logs.unavailable': 'Log storage is unavailable right now — live tail keeps working.',
  // tracker #1007/#1076: the upstream does not parse the narrowing query arg, so
  // master refuses instead of answering a project-bound key with the whole fleet.
  // Twin of metric.err.narrowing, with the knob and the config option of logs.
  'logs.narrowing':
    'Log history is hidden: this master cannot narrow the search to your project — the configured VictoriaLogs does not understand the extra_stream_filters query arg. The operator has to fix victorialogs_url (self-host docs, §4). Live tail keeps working.',

  // — Logs screen: fleet-wide search (Task 6, "Логи v1") —
  'logs.search.textPlaceholder': 'Search log text…',
  'logs.search.hint': 'Enter a search above and run it to see matching log lines.',
  'logs.search.submit': 'Search',
  'logs.search.limitAria': 'Result limit',
  'logs.search.empty': 'No log lines matched your search.',

  // — confirm dialog —
  'confirm.err.forbidden': "You don't have permission for this action.",
  'confirm.err.conflict': 'State conflict — the action is unavailable right now.',
  'confirm.err.generic': "Couldn't perform the action.",

  // — Fleet screen —
  'fleet.nodesCount.one': '{count} node',
  'fleet.nodesCount.few': '{count} nodes',
  'fleet.nodesCount.many': '{count} nodes',
  'fleet.nodesCount.other': '{count} nodes',
  'fleet.emptyPre': 'No nodes yet. Register the first:',
  // Вариант с проектом (мультипроект W2): после сужения голое «нет нод»
  // читалось бы как «платформа мертва». Без выбранного проекта — вариант выше.
  'fleet.emptyInProject': 'No nodes in project {project} yet. Register the first:',
  'fleet.node.noServers': 'No dedics on this node.',
  'fleet.toggleNode': 'Show or hide dedics on {host}',
  'fleet.drain.playing.one': '{count} still playing',
  'fleet.drain.playing.few': '{count} still playing',
  'fleet.drain.playing.many': '{count} still playing',
  'fleet.drain.playing.other': '{count} still playing',
  'fleet.drain.empty': 'drained',
  'fleet.hb.never': 'never',
  'fleet.revoke': 'Retire',
  'fleet.revoke.title': 'Retire {host} from the fleet?',
  'fleet.revoke.desc':
    'The node is marked dead: the master stops tracking it, its alerts go quiet and its environment stops blocking image cleanup. Match history is kept. There is no way back from the panel — a node that comes back to life will not rejoin on its own.',
  'fleet.showRetired.one': 'Show {count} retired',
  'fleet.showRetired.few': 'Show {count} retired',
  'fleet.showRetired.many': 'Show {count} retired',
  'fleet.showRetired.other': 'Show {count} retired',
  'fleet.drain': 'Drain',
  'fleet.undrain': 'Undrain',
  'fleet.drain.title': 'Take {host} out of rotation?',
  'fleet.drain.desc':
    'New dedics will not be placed, the ready buffer is reaped, and allocated ones finish their matches. You can return the node with Undrain.',
  'fleet.undrain.title': 'Return {host} to rotation?',
  'fleet.undrain.desc':
    'Reconcile will start placing dedics on this node again; the agent receives Undrain.',

  // — Overview screen —
  'ov.liveMatches': 'Live matches',
  'ov.liveDetail': 'running {running} · pending {pending}',
  'ov.playersOnline': 'Players online',
  'ov.playersDetail': 'across allocated dedics',
  'ov.readyBuffer': 'Ready buffer',
  'ov.noReady': 'no ready dedics',
  'ov.nodes': 'Nodes',
  'ov.inQuarantine': '{count} in quarantine',
  'ov.down': '{count} down',
  'ov.allActive': 'all active',
  // Ноль тачек — не «все активны» (tracker #1071): при пустом флоте quarantine и
  // down тоже нули, и карточка успокаивала оператора нового проекта ровно там,
  // где запускать дедики физически не на чем.
  'ov.noNodes': 'no nodes in this project',
  'ov.fleetVersion': 'Fleet version',
  'ov.noLiveDedics': 'no live dedics',
  'ov.matchesHour': 'Matches this hour',
  'ov.recentEvents': 'Recent events',
  'ov.sparkAria': 'Matches over the last hour, by minute',
  'ov.updated': 'updated {ago}',
  'ov.updatedNow': 'updated just now',
  'ov.liveAria': 'Live counter, updates from the event stream',

  // — Events screen —
  'events.feedUnavailable': 'Event feed is unavailable.',
  'events.none': 'No events yet.',
  'events.kindAria': 'Filter by kind',
  'events.nodeAria': 'Filter by node',
  'events.periodAria': 'Period',
  'events.windowAria': 'Feed window size',
  'events.allKinds': 'all kinds',
  'events.allNodes': 'all nodes',
  'events.window': 'window {count}',
  'events.emptyFilter': 'No events for these filters.',
  'events.eventsCount.one': '{count} event',
  'events.eventsCount.few': '{count} events',
  'events.eventsCount.many': '{count} events',
  'events.eventsCount.other': '{count} events',
  'events.ofTotal': 'of {total}',
  'events.pageOf': 'p. {page}/{pages}',
  'events.scope.platform': 'platform',
  'events.scope.platformHint':
    'Platform event: it belongs to the installation, not to a project, and stays visible whichever project is selected.',
  'period.all': 'all time',
  'period.15m': '15 minutes',
  'period.1h': '1 hour',
  'period.24h': '24 hours',

  // — Deploys screen —
  'deploys.emptyPre': 'No versions yet. Register a build:',
  'deploys.emptyInProject': 'No versions in project {project} yet. Register a build:',
  'deploys.project': 'Project {project}',
  'deploys.versionsCount.one': '{count} version',
  'deploys.versionsCount.few': '{count} versions',
  'deploys.versionsCount.many': '{count} versions',
  'deploys.versionsCount.other': '{count} versions',
  'deploys.col.liveDedics': 'Dedics live',
  'deploys.col.created': 'Created',
  'deploys.col.deprecated': 'Deprecated',
  'deploys.rollback': 'Roll back',
  'deploys.rollback.title': 'Roll back project {project}',
  'deploys.rollback.desc':
    'The active version will be replaced by the most recent deprecated one ({semver}). Images are already on the nodes — rollback takes seconds and live matches are not interrupted.',
  'deploys.warming': 'warming up…',
  'deploys.deploy': 'Deploy',
  'deploys.deploy.title': 'Deploy {semver}?',
  'deploys.deploy.desc':
    'Master will pre-pull the image on all live nodes, then atomically switch the active version. Live matches on the old version finish within the multi-version window.',
  'deploys.window': 'Multi-version window',
  'deploys.noActive': 'No active version.',
  'deploys.dedicsCount.one': '{count} dedic',
  'deploys.dedicsCount.few': '{count} dedics',
  'deploys.dedicsCount.many': '{count} dedics',
  'deploys.dedicsCount.other': '{count} dedics',
  // Недостающая ёмкость проекта (tracker #1071). Соседние карточки рисуют
  // «active 1.0.0 · 0 дедиков» и «Живых дедиков нет» — верно, но без причины;
  // баннер называет её ровно там, где вопрос и возникает.
  'deploys.noNodes.title': 'No nodes in this project — no dedic will start',
  'deploys.noNodes.body':
    'Version {semver} is active and the fleet is configured, but project {project} has no node of its own. A node belongs to exactly one project, so the scheduler has nowhere to place dedics. Register a node — on an existing box it comes up as another agent instance.',
  'deploys.noNodes.cta': 'Go to Fleet',
  'deploys.regionActive': 'Active by region',
  'deploys.noLiveDedics': 'No live dedics.',
  'deploys.emptyProject': 'No versions in this project.',
  'deploys.prepullPre': 'Warming',
  'deploys.prepullSuffix': '(pre-pull)',
  'deploys.prepull.nodes': '{pulled} / {total} nodes',
  'deploys.prepull.waiting': 'Waiting for per-node pull reports (deploy_node_pulled events)…',
  'deploys.toast.deployed': 'Deploy of {semver} started',
  // Мастер отдал warning no_live_nodes (tracker #1071): флип состоялся, но
  // катить было не на что — тост обязан сказать это в момент действия, а не
  // отрапортовать успех.
  'deploys.toast.deployedNoNodes': '{semver} is active, but the project has no nodes — no dedic will start',
  'deploys.toast.rolledBack': 'Rolled back to {semver}',

  // — Deploys screen: «How to deploy a build» card —
  'deploys.howto.title': 'How to deploy a build',
  'deploys.howto.expand': 'Show steps',
  'deploys.howto.collapse': 'Hide steps',
  'deploys.howto.step1.title': '1. Build & push the server image',
  'deploys.howto.step1.desc':
    'Build your game server image and push it to ghcr (or any OCI registry the nodes can reach). Example image_ref:',
  'deploys.howto.step1.stubNote':
    'No build of your own yet? The stub workflow publishes a placeholder image — run it manually (workflow_dispatch) with a semver `tag` input:',
  'deploys.howto.step1.pullNote': 'Pull access to ghcr is already configured on the node — nothing else to set up there.',
  'deploys.howto.step1.registryLink': 'Private image? Connect it: Admin → Registries',
  'deploys.howto.step1.registryHint': 'Private image? An admin connects it: Admin → Registries.',
  'deploys.howto.step2.title': '2. Get a deploy-scoped API key',
  'deploys.howto.step2.desc': 'The commands below authenticate with a key that has the `deploy` scope.',
  'deploys.howto.step2.adminLink': 'Create one on the Admin screen',
  'deploys.howto.step2.readonlyHint': 'An admin creates one on the Admin screen.',
  'deploys.howto.step2.secretNote': 'The secret is shown once, right after creation — save it somewhere safe.',
  'deploys.howto.key.pickLabel': 'Use an existing key',
  'deploys.howto.key.pickPlaceholder': '— none —',
  'deploys.howto.key.nameLabel': 'Key name',
  'deploys.howto.key.createButton': 'Create a deploy key & fill in',
  'deploys.howto.key.createErr': "Couldn't create the key.",
  'deploys.howto.key.secretWarning': "Copy the secret now — it won't be shown again.",
  'deploys.howto.key.hideSecret': 'Hide secret',
  'deploys.howto.key.toastCreated': 'Key {name} created',
  'deploys.howto.step3.title': '3. Register the version and deploy',
  'deploys.howto.step3.registerLabel': 'Register a new version (the env in the body is where it lands — CI usually pushes to a non-production env like dev):',
  'deploys.howto.step3.deployLabel': 'Deploy a registered version:',
  'deploys.howto.step3.localNote':
    "master only listens on the node's localhost — run these from the node itself or through an SSH tunnel (this panel's address is already a tunnel).",
  'deploys.howto.step3.tableNote': 'Once registered, the version appears in the table below — deploy it with the Deploy button.',
  'deploys.howto.copyRegister': 'Copy register command',
  'deploys.howto.copyDeploy': 'Copy deploy command',
  'deploys.howto.copied': 'Copied',

  // — navigation (П2) —
  'nav.stats': 'Statistics',
  'nav.cost': 'Cost',
  'nav.alerts': 'Alerts',
  'nav.logs': 'Logs',
  'nav.access': 'Admin',

  // — severity / key-status / alert-state labels (raw codes stay in the API) —
  'severity.critical': 'Critical',
  'severity.warning': 'Warning',
  'severity.info': 'Info',
  'keystatus.active': 'Active',
  'keystatus.revoked': 'Revoked',
  'alertstate.firing': 'Firing',
  'alertstate.resolved': 'Resolved',

  // — Statistics: range selector (Task 4, "Статистика v1") —
  'stats.range.aria': 'Time window',
  'stats.range.12h': '12 h',
  'stats.range.24h': '24 h',
  'stats.range.3d': '3 d',
  'stats.range.7d': '7 d',
  'stats.range.30d': '30 d',

  // — Statistics: live panels for granular windows (Task 5, "Статистика v1") —
  'stats.live.online': 'Players online',
  'stats.live.matchesRunning': 'Matches running',
  'stats.live.queue': 'Matchmaker queue depth',
  'stats.live.util': 'Slot utilization',
  'stats.live.utilOverTime': 'Dedics by state over time',
  'stats.live.utilOverTimeNote': 'live dedics by state',

  // — Statistics screen —
  'stats.utc': 'UTC',
  'stats.periodAria': 'Period, days',
  'stats.daysUnit': 'days',
  'stats.noData': 'No data for the selected period.',
  'stats.byRegion': 'stacked by region',
  'stats.card.matches': 'Matches',
  'stats.card.matchesDetail': 'over {days} days',
  'stats.card.peakCcu': 'Peak CCU',
  'stats.card.peakCcuDetail': 'max concurrent players',
  'stats.card.avgDuration': 'Avg match',
  'stats.card.avgDurationDetail': 'mean finished-match duration',
  'stats.card.fillRate': 'Time to match',
  'stats.card.fillRateDetail': 'p50 / p95 · n={count}',
  'stats.matchesPerDay': 'Matches per day',
  'stats.playersPerDay': 'Players per day',
  'stats.peakCcuPerDay': 'Peak CCU per day',
  'stats.peakCcuNote': 'peak concurrent players',
  'stats.avgDurationPerDay': 'Avg match duration per day',
  'stats.avgDurationNote': 'finished matches only',
  'stats.versionDist': 'Version distribution',
  'stats.versionDistNote': 'share of matches',
  'stats.fillRate': 'Matchmaker fill-rate',
  'stats.ttm.p50': 'p50',
  'stats.ttm.p95': 'p95',
  'stats.ttm.note': 'Allocation → match start (server readiness), a fill-rate proxy. Sample size: {count}.',
  'stats.ttm.source': 'source',
  'stats.ttm.srcQueue': 'queue → match',
  'stats.ttm.srcAlloc': 'allocation → start',
  'stats.ttm.srcQueueNote': 'True wait from the birdman_mm_time_to_match_seconds histogram over the period.',
  'stats.ttm.trueDegraded': 'Too few matchmaker samples in this period — using the allocation→start proxy.',
  'stats.ttm.trueUnavailable': 'Metrics proxy unavailable — histogram time-to-match is hidden.',
  'stats.ttm.trueLoading': 'Loading histogram…',
  'stats.ttm.allocUnavailable': 'Overview data unavailable — allocation→start proxy is hidden.',

  // — utilization meter (Cost) —
  'stats.util.allocated': 'allocated',
  'stats.util.ready': 'ready',
  'stats.util.draining': 'draining',
  'stats.util.creating': 'creating',
  'stats.util.free': 'free',
  'stats.util.aria': 'Slot utilization for region {region}',
  'stats.util.slots.one': '{used}/{capacity} slot',
  'stats.util.slots.few': '{used}/{capacity} slots',
  'stats.util.slots.many': '{used}/{capacity} slots',
  'stats.util.slots.other': '{used}/{capacity} slots',

  // — Cost screen —
  'cost.unit': 'slot-hours',
  'cost.total': 'Total slot-hours',
  'cost.totalDetail': '{unit} over {days} days',
  'cost.byRegion': 'Slot-hours per day · by region',
  'cost.byVersion': 'Slot-hours per day · by version',
  'cost.utilization': 'Node utilization',
  'cost.utilSnapshot': 'current snapshot',
  'cost.noUtil': 'No active nodes to report utilization.',
  'cost.utilNote':
    'Snapshot of allocated/ready/draining vs active-node capacity. Utilization over time is charted below.',
  'cost.utilOverTime': 'Utilization over time',
  'cost.utilOverTimeNote': 'live dedics by state · {hours}h window',

  // — Alerts screen (descriptions come from vmalert as-is; UI chrome only) —
  'alerts.readonly': 'rules & firing state from vmalert (read-only)',
  'alerts.sound': 'Sound on new critical',
  'alerts.critical.badge': '{count} active critical alerts',
  'alerts.active': 'Active',
  'alerts.noActive': 'No active alerts — all clear.',
  'alerts.unconfigured': 'Alerts are not configured on this master (vmalert_url is empty).',
  'alerts.upstream': 'vmalert is unavailable — no rules or firing state right now.',
  'alerts.rules': 'Rules',
  'alerts.noRules': 'No alerting rules.',
  'alerts.col.rule': 'Rule',
  'alerts.col.severity': 'Severity',
  'alerts.col.expr': 'Expression',
  'alerts.col.for': 'For',
  'alerts.col.alert': 'Alert',
  'alerts.col.where': 'Project · region · node',
  'alerts.col.started': 'Started',
  'alerts.col.ended': 'Ended',
  'alerts.col.status': 'Status',
  'alerts.history': 'Firing history',
  'alerts.noHistory': 'No alerts have fired yet.',
  'alerts.limitAria': 'How many entries to fetch',
  'alerts.lastN': 'last {count}',

  // Project scope of an alert (мультипроект, tracker #956). Narrowing by the
  // selected project is NON-HIDING: an alert only leaves the screen when its
  // project is explicitly a different one. A platform alert (no project label —
  // master down, node down, disk, certs, backups) stays visible under EVERY
  // project and says so, otherwise the operator reads "MasterDown" as a problem
  // of the project currently selected. Same framing as stats.ccu.platformWide.
  'alerts.scope.platform': 'platform',
  'alerts.scope.platformHint': 'Platform-wide alert: it belongs to no project and stays visible whichever project is selected.',
  'alerts.scope.note':
    'Narrowed to the selected project. Platform alerts (master, nodes, disks, certificates, backups) stay visible under every project.',
  'alerts.rules.platformNote':
    'The rule catalogue is platform-wide and is NOT narrowed by the project selector: a rule has no project of its own — it lives inside the expression, which may fan out into one alert per project.',

  // — Alerts mute (admin sets/removes; panel suppression + audit) —
  'alerts.mute.section': 'Muted',
  'alerts.mute.semantics': 'A mute hides the alert in this panel and records an audit entry — vmalert and Discord keep firing.',
  'alerts.mute.allRegions': 'all regions',
  'alerts.mute.allProjects': 'all projects',
  'alerts.mute.badge': 'muted',
  'alerts.mute.action': 'Mute',
  'alerts.mute.unmute': 'Unmute',
  'alerts.mute.byWhen': 'by {who} · {when}',
  'alerts.mute.expires': 'expires {when}',
  'alerts.mute.noExpiry': 'no expiry (indefinite)',
  'alerts.mute.dialog.title': 'Mute {name}',
  'alerts.mute.note': 'Note (optional)',
  'alerts.mute.notePlaceholder': 'e.g. known flapping node, ticket #123',
  'alerts.mute.expiresLabel': 'Expires',
  'alerts.mute.preset.1h': '1h',
  'alerts.mute.preset.8h': '8h',
  'alerts.mute.preset.24h': '24h',
  'alerts.mute.preset.forever': 'Indefinite',
  'alerts.mute.preset.custom': 'Custom',
  'alerts.mute.expiresPreview': 'Until {when}',
  'alerts.mute.unmute.title': 'Remove mute for {name}?',
  'alerts.mute.unmute.desc': 'The alert will show in the panel again. vmalert and Discord were never affected.',
  'alerts.mute.toast.created': 'Alert {name} muted',
  'alerts.mute.toast.removed': 'Mute removed for {name}',
  'alerts.mute.err.bad': 'The mute request was rejected (check the alert name).',
  'alerts.mute.err.gone': 'This mute no longer exists — it may have already been removed.',
  'alerts.mute.err.conflict': 'Mute conflict — reload and try again.',

  // — Access screen (admin-only) —
  'access.keys': 'API keys',
  'access.empty': 'No API keys yet.',
  'access.col.name': 'Name',
  'access.col.scopes': 'Scopes',
  'access.col.created': 'Created',
  'access.col.status': 'Status',
  'access.revoke': 'Revoke',
  'access.revoke.title': 'Revoke key {name}?',
  'access.revoke.desc': 'The key stops working immediately (cached sessions are dropped). This cannot be undone.',
  'access.err.lastAdmin': "Can't revoke the last active admin key (self-lockout).",
  'access.newKey': 'New key',
  'access.create.title': 'New API key',
  'access.create.desc': 'Pick a name and scopes. The secret is shown once, right after creation.',
  'access.create.name': 'Name',
  'access.create.namePlaceholder': 'e.g. ci-deploy',
  'access.create.scopes': 'Scopes',
  'access.create.submit': 'Create key',
  'access.create.err': "Couldn't create the key.",
  'access.secret.title': 'Key {name} created',
  'access.secret.warning': "Copy the secret now — it won't be shown again.",
  'access.secret.copy': 'Copy',
  'access.secret.copied': 'Copied',
  'access.secret.done': 'Done',
  'access.toast.created': 'Key {name} created',
  'access.toast.revoked': 'Key {name} revoked',

  // — Access screen: purge (hard-delete) a revoked key —
  'access.purge': 'Delete forever',
  'access.purge.title': 'Delete key {name} forever?',
  'access.purge.desc': 'The key row is deleted for good (audit log entries stay). This cannot be undone.',
  'access.toast.purged': 'Key {name} deleted forever',

  // — Access screen: Registries section (private registry credentials) —
  'access.registries': 'Registries',
  'access.registries.hint': 'Credentials only need pull (read) access to the registry — nothing more.',
  'access.registries.empty': 'No registries yet.',
  'access.registries.col.host': 'Host',
  'access.registries.col.type': 'Type',
  'access.registries.col.username': 'Username',
  'access.registries.col.token': 'Token',
  'access.registries.col.note': 'Note',
  'access.registries.col.updated': 'Updated',
  'access.registries.tokenMasked': '••••',
  // Registry-type dropdown labels (raw code stays in the API).
  'access.registries.type.ghcr': 'ghcr',
  'access.registries.type.gar': 'Google Artifact Registry',
  'access.registries.type.generic': 'Generic',
  // Per-type help — names where to get the credential.
  'access.registries.help.ghcr':
    'Personal access token with the read:packages scope — GitHub Settings → Developer settings → Personal access tokens.',
  'access.registries.help.gar':
    'Service-account JSON key with the Artifact Registry Reader role — GCP Console → IAM → Service Accounts → Keys. Paste the whole key file.',
  'access.registries.help.generic': 'Username and password (or token) for docker login to this registry.',
  'access.registries.add': 'Add registry',
  'access.registries.create.title': 'Add or replace a registry',
  'access.registries.create.desc':
    'Pick a type, then fill the host and credentials for a private registry. Submitting the same host again replaces its credentials — that is how you rotate a token.',
  'access.registries.create.host': 'Host',
  'access.registries.create.hostPlaceholder': 'e.g. ghcr.io',
  'access.registries.create.hostGarPlaceholder': 'REGION-docker.pkg.dev',
  'access.registries.create.username': 'Username',
  'access.registries.create.usernamePlaceholder': 'registry username',
  'access.registries.create.token': 'Token',
  'access.registries.create.tokenPlaceholder': 'personal access token',
  'access.registries.create.tokenGarPlaceholder': '{ "type": "service_account", "private_key": "…", … }',
  'access.registries.create.note': 'Note',
  'access.registries.create.notePlaceholder': 'optional note',
  'access.registries.create.submit': 'Save',
  'access.registries.create.err': "Couldn't save the registry.",
  'access.registries.edit': 'Edit',
  'access.registries.edit.title': 'Edit registry {host}',
  'access.registries.edit.desc':
    'The host is immutable — delete and re-add to change it. Leave the token empty to keep the current secret, or enter a new one to rotate it.',
  'access.registries.keepSecret': 'Leave empty to keep the current token',
  'access.registries.delete': 'Delete',
  'access.registries.delete.title': 'Delete registry {host}?',
  'access.registries.delete.desc':
    'Connected nodes lose this credential immediately — anonymous pulls may start failing for private images on this host.',
  'access.registries.toast.saved': 'Registry {host} saved',
  'access.registries.toast.updated': 'Registry {host} updated',
  'access.registries.toast.deleted': 'Registry {host} deleted',

  // --- Backups (Backups v1) ---
  'nav.backups': 'Backups',
  'backups.title': 'Backups',
  'backups.subtitle': 'Postgres dumps by the master: schedule, local retention and S3 offsite.',
  'backups.status.title': 'Status',
  'backups.status.last': 'Last backup',
  'backups.status.lastNever': 'never',
  'backups.status.size': 'Size',
  'backups.status.s3Last': 'Last S3 upload',
  'backups.status.next': 'Next scheduled',
  'backups.status.nextDisabled': 'schedule disabled',
  'backups.status.lastError': 'Last run failed',
  'backups.runNow': 'Run now',
  'backups.runNow.hint.noSettings': 'Backup settings are not loaded yet',
  'backups.runNow.hint.dirty': 'Save or revert your changes first — a manual run uses the saved settings',
  'backups.runNow.confirm.title': 'Backups are disabled',
  'backups.runNow.confirm.desc': 'Scheduled backups are turned off. Run a one-off manual backup with the saved settings?',
  'backups.toast.runStarted': 'Backup started',
  'backups.toast.runBusy': 'A backup run is already in progress',
  'backups.toast.saved': 'Backup settings saved',
  'backups.toast.s3ok': 'S3 connection OK — bucket is accessible',
  // Ошибки бекапов, попадавшие в тост сырым detail мастера (tracker #1005).
  'backups.err.save': "Couldn't save the backup settings.",
  'backups.err.run': "Couldn't start the backup run.",
  'backups.err.s3': "Couldn't reach the S3 bucket with the saved settings.",
  'backups.form.title': 'Settings',
  'backups.form.enabled': 'Scheduled backups',
  'backups.form.interval': 'Interval (hours)',
  'backups.form.retentionLocal': 'Keep local dumps',
  'backups.form.retentionHint': 'Both retentions count dumps, not days.',
  'backups.form.save': 'Save',
  'backups.form.s3.enabled': 'Upload to S3',
  'backups.form.s3.endpoint': 'Endpoint URL',
  'backups.form.s3.endpointHint': 'S3-compatible endpoint, e.g. https://s3.eu-central-003.backblazeb2.com',
  'backups.form.s3.region': 'Region',
  'backups.form.s3.bucket': 'Bucket',
  'backups.form.s3.prefix': 'Key prefix',
  'backups.form.s3.accessKey': 'Access key',
  'backups.form.s3.secretKey': 'Secret key',
  'backups.form.s3.secretKeyKeep': 'Leave empty to keep the current secret',
  'backups.form.s3.retention': 'Keep dumps in bucket',
  'backups.form.s3.test': 'Test connection',
  'backups.form.s3.testHint': 'Tests the saved configuration — save your changes first.',
  'backups.history.title': 'History',
  'backups.history.empty': 'No backup runs yet.',
  'backups.history.col.when': 'Started',
  'backups.history.col.kind': 'Kind',
  'backups.history.col.result': 'Result',
  'backups.history.col.size': 'Size',
  'backups.history.col.s3': 'S3',
  'backups.history.col.error': 'Error',
  'backups.history.kind.scheduled': 'scheduled',
  'backups.history.kind.manual': 'manual',
  'backups.history.result.running': 'running',
  'backups.history.result.ok': 'ok',
  'backups.history.result.error': 'error',
  'event.backup_failed': 'Backup failed',

  // --- Projects (мультипроект W1) ---
  // Top-level switcher: every screen shows the SELECTED project; there is no
  // "all projects" mode.
  'project.switch': 'Project',
  'project.none': 'No projects yet',
  'project.none.hint':
    'A project is created implicitly by the first node or version you register — there is nothing to create by hand.',
  'project.unavailable': 'Projects unavailable',
  'project.unavailable.hint': "Couldn't load the project list. Click to retry.",

  // --- Environments (environments v1 §8) ---
  // Global env switcher (Shell chips) — the environments OF THE SELECTED PROJECT.
  'env.switch': 'Environment',
  'scope.current': 'Current project and environment',
  'env.all': 'All',
  'env.productionTitle': 'production environment',

  // Environment list unavailable: the filter degrades to «All» and says so.
  'env.unavailable': 'Environments unavailable',
  'env.unavailable.hint':
    "Couldn't load the environment list — the env filter is off and every screen shows all data. Click to retry.",

  // Fleet: move a node to another environment (PATCH /v1/nodes/{id}).
  'fleet.moveEnv': 'Move to env…',
  'fleet.moveEnv.title': 'Move {host} to another environment',
  'fleet.moveEnv.desc':
    'The node will serve versions of the target environment. It must carry no live dedics — drain it first.',
  'fleet.moveEnv.target': 'Target environment',
  'fleet.moveEnv.current': 'current: {env}',
  'fleet.moveEnv.confirm': 'Move',
  'fleet.moveEnv.toast': '{host} moved to {env}',
  'fleet.moveEnv.err.conflict':
    'The node still carries live dedics — drain it first (Drain), then move it once the dedics are gone.',
  'fleet.moveEnv.err.generic': "Couldn't move the node.",

  // Deploys: env badge, promote, provenance, hide-disabled, env settings card.
  'deploys.hideDisabled': 'Hide disabled',
  'deploys.promote': 'Promote',
  'deploys.promote.title': 'Promote {semver} from {from}',
  'deploys.promote.desc':
    'Register {semver} in a production environment (same image, provenance kept) and deploy it there. Live matches on other environments are not affected.',
  'deploys.promote.target': 'Target environment',
  'deploys.promote.confirm': 'Promote',
  'deploys.promote.noTargets': 'No production environment to promote into — create one in Admin → Environments.',
  'deploys.promote.toast': 'Promoted {semver} → {env}',
  'deploys.provenance': 'promoted from {from}',
  'deploys.provenance.unknown': 'promoted from another environment',
  'deploys.env.settings': 'Environment settings',
  'deploys.env.settingsFor': '{env} settings',
  'deploys.env.autoDeploy': 'Auto-deploy',
  'deploys.env.autoDeployHint': 'Registering a version deploys it immediately (dev flow). Not allowed on production.',
  'deploys.env.retention': 'Keep versions',
  'deploys.env.retentionHint': '0 = unlimited. Registered/disabled versions beyond this are retired.',
  'deploys.env.save': 'Save',
  'deploys.env.toast': 'Environment {env} updated',
  'deploys.emptyEnv': 'No versions in this environment.',

  // Access: API-key (project, env) binding.
  'access.col.binding': 'Binding',
  'access.binding.global': 'global',
  'access.create.binding': 'Binding',
  'access.create.bindingHint':
    'Deploy, matchmaking and allocate keys can be scoped to one environment. Admin keys cannot be bound.',
  'access.create.bindingGlobal': 'Global (all environments)',

  // Access: Environments admin section.
  'access.projects': 'Projects',
  'access.projects.hint':
    'A project is the top-level dimension: environments, versions, fleets, nodes and keys all live inside one. The slug cannot be renamed — API keys, matchmaking and CI are tied to it.',
  'access.projects.add': 'New project',
  'access.projects.edit': 'Project {slug}',
  'access.projects.current': 'open',
  'access.projects.slug': 'Slug',
  'access.projects.slug.hint': 'Lowercase letters, digits and dashes, up to 32 characters.',
  'access.projects.matchSize': 'match size {size}',
  'access.projects.matchSize.label': 'Players per match',
  'access.projects.err.matchSize': 'Players per match must be a whole number, 1 or more.',
  'access.projects.err.save': 'Could not save the project.',
  'access.projects.err.slugTaken': 'That slug is already taken — pick another one.',
  'access.projects.toast.created': 'Project {slug} created',
  'access.projects.toast.saved': 'Project {slug} saved',
  'access.projects.toast.deleted': 'Project {slug} deleted',
  'access.projects.toast.deletedCascade':
    'Project {slug} deleted: {environments} environment(s), {versions} version(s), {keys} key(s) revoked',
  'access.projects.delete.title': 'Delete project {slug}?',
  'access.projects.delete.desc':
    'Everything inside goes with it: environments, versions, fleets, matches and their history. Bound API keys are revoked for good — CI using them stops working. This cannot be undone.',
  'access.projects.delete.usage': 'What is inside',
  'access.projects.delete.usage.environments': 'Environments',
  'access.projects.delete.usage.versions': 'Versions',
  'access.projects.delete.usage.matches': 'Matches',
  'access.projects.delete.usage.keys': 'Live keys',
  'access.projects.delete.usage.retiredNodes': 'Retired nodes',
  'access.projects.delete.nodesBlock':
    '{count} live node(s) still belong to this project. Retire or move them on the Fleet screen first.',
  'access.projects.delete.confirmLabel': 'Type the project slug to confirm',
  'access.projects.delete.err': 'Could not delete the project.',
  'access.environments': 'Environments',
  'access.environments.hint':
    'Environments are a per-project platform dimension. Behaviour follows the production flag, not the name.',
  'access.environments.empty': 'No environments yet.',
  'access.environments.col.name': 'Name',
  'access.environments.col.production': 'Production',
  'access.environments.col.autoDeploy': 'Auto-deploy',
  'access.environments.col.retention': 'Keep versions',
  'access.environments.col.created': 'Created',
  'access.environments.flag.on': 'on',
  'access.environments.flag.off': 'off',
  'access.environments.retentionUnlimited': 'unlimited',
  'access.environments.add': 'Add environment',
  'access.environments.create.title': 'Add environment',
  'access.environments.create.desc':
    'Pick a lowercase name. The production flag drives behaviour: production forbids auto-deploy and keeps versions unlimited.',
  'access.environments.create.name': 'Name',
  'access.environments.create.namePlaceholder': 'e.g. staging',
  'access.environments.create.production': 'Production environment',
  'access.environments.create.autoDeploy': 'Auto-deploy new versions',
  'access.environments.create.retention': 'Keep versions (0 = unlimited)',
  'access.environments.create.submit': 'Create',
  'access.environments.create.err': "Couldn't save the environment.",
  'access.environments.guardrail': 'Auto-deploy is only allowed when production is off.',
  'access.environments.edit': 'Edit',
  'access.environments.edit.title': 'Edit environment {name}',
  'access.environments.edit.desc': 'The name is immutable. Enabling auto-deploy requires production to be off.',
  'access.environments.delete': 'Delete',
  'access.environments.delete.title': 'Delete environment {name}?',
  'access.environments.delete.desc':
    'The environment is deleted together with everything in it. This cannot be undone.',
  'access.environments.delete.usage': 'Will be deleted:',
  'access.environments.delete.usage.versions': 'Versions',
  'access.environments.delete.usage.fleets': 'Fleets',
  'access.environments.delete.usage.matches': 'Matches',
  'access.environments.delete.usage.servers': 'Servers',
  // Плюрал: у en только one/other (0 → other), поэтому «0 API keys» — норма.
  'access.environments.delete.usage.keys.one': '{count} API key will be revoked.',
  'access.environments.delete.usage.keys.few': '{count} API keys will be revoked.',
  'access.environments.delete.usage.keys.many': '{count} API keys will be revoked.',
  'access.environments.delete.usage.keys.other': '{count} API keys will be revoked.',
  'access.environments.delete.usageErr': "Couldn't load what this environment holds.",
  // Ноды не каскадятся: их переводят в другое окружение руками (экран «Флот»).
  'access.environments.delete.nodesBlock.one':
    '{count} node still lives here. Move it to another environment first (Fleet → Move to env…).',
  'access.environments.delete.nodesBlock.few':
    '{count} nodes still live here. Move them to another environment first (Fleet → Move to env…).',
  'access.environments.delete.nodesBlock.many':
    '{count} nodes still live here. Move them to another environment first (Fleet → Move to env…).',
  'access.environments.delete.nodesBlock.other':
    '{count} nodes still live here. Move them to another environment first (Fleet → Move to env…).',
  'access.environments.delete.confirmLabel': 'Type the environment name to delete it:',
  'access.environments.delete.copy': 'Copy name',
  'access.environments.delete.copied': 'Copied',
  'access.environments.delete.err': "Couldn't delete the environment.",
  'access.environments.toast.created': 'Environment {name} created',
  'access.environments.toast.updated': 'Environment {name} updated',
  'access.environments.toast.deleted': 'Environment {name} deleted',
  'access.environments.toast.deletedCascade':
    'Environment {name} deleted: versions {versions}, fleets {fleets}, matches {matches}, servers {servers}, keys revoked {keys}',

  // Stats: peak CCU is never narrowed by the env filter, and the note says so.
  // WHICH whole it covers depends on the project scope (мультипроект W3): a
  // selected project makes it that project's peak, otherwise the platform's.
  'stats.ccu.platformWide': 'platform-wide (not filtered by env)',
  'stats.ccu.projectWide': 'across the whole project (not filtered by env)',
} as const;

export type MessageKey = keyof typeof en;
