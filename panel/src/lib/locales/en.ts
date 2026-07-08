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
  'fleet.toggleNode': 'Show or hide dedics on {host}',
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
  'deploys.toast.deployed': 'Deploy of {semver} started',
  'deploys.toast.rolledBack': 'Rolled back to {semver}',

  // — navigation (П2) —
  'nav.stats': 'Statistics',
  'nav.cost': 'Cost',
  'nav.alerts': 'Alerts',
  'nav.access': 'Access',

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
  'alerts.col.where': 'Region · node',
  'alerts.col.started': 'Started',
  'alerts.col.ended': 'Ended',
  'alerts.col.status': 'Status',
  'alerts.history': 'Firing history',
  'alerts.noHistory': 'No alerts have fired yet.',
  'alerts.limitAria': 'How many entries to fetch',
  'alerts.lastN': 'last {count}',

  // — Alerts mute (admin sets/removes; panel suppression + audit) —
  'alerts.mute.section': 'Muted',
  'alerts.mute.semantics': 'A mute hides the alert in this panel and records an audit entry — vmalert and Discord keep firing.',
  'alerts.mute.allRegions': 'all regions',
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
} as const;

export type MessageKey = keyof typeof en;
