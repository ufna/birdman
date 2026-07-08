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
  'ui.err.loadFailed': "Couldn't load data: {detail}",

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
  'event.node_recovered': 'Node recovered',
  'event.node_drain': 'Node draining',
  'event.node_undrain': 'Node returned',
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
  'event.agent_upgrade': 'Agent upgrade',
  'event.agent_upgrade_succeeded': 'Agent upgraded',
  'event.agent_upgrade_failed': 'Agent upgrade failed',

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
  'metric.unconfigured': "Metrics aren't configured on this master (victoriametrics_url is empty).",
  'metric.unreachable': 'VictoriaMetrics is unavailable — no data right now.',
  'metric.error': 'Metrics unavailable: {error}',
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
  'fleet.node.noServers': 'No dedics on this node.',
  'fleet.drain.playing.one': '{count} still playing',
  'fleet.drain.playing.few': '{count} still playing',
  'fleet.drain.playing.many': '{count} still playing',
  'fleet.drain.playing.other': '{count} still playing',
  'fleet.drain.empty': 'drained',
  'fleet.hb.never': 'never',
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
  'ov.allActive': 'all active',
  'ov.fleetVersion': 'Fleet version',
  'ov.noLiveDedics': 'no live dedics',
  'ov.matchesHour': 'Matches this hour',
  'ov.recentEvents': 'Recent events',
  'ov.sparkAria': 'Matches over the last hour, by minute',

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
  'period.all': 'all time',
  'period.15m': '15 minutes',
  'period.1h': '1 hour',
  'period.24h': '24 hours',

  // — Deploys screen —
  'deploys.emptyPre': 'No versions yet. Register a build:',
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
  'deploys.regionActive': 'Active by region',
  'deploys.noLiveDedics': 'No live dedics.',
  'deploys.emptyProject': 'No versions in this project.',
  'deploys.prepullPre': 'Warming',
  'deploys.prepullSuffix': '(pre-pull)',
  'deploys.prepull.nodes': '{pulled} / {total} nodes',
  'deploys.prepull.waiting': 'Waiting for per-node pull reports (deploy_node_pulled events)…',
} as const;

export type MessageKey = keyof typeof en;
