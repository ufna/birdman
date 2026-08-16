// Демо-флот: один связный срез платформы, из которого фичи читаются глазами —
// тёплый буфер, мультиверсионный выкат со сливом, три региона, два проекта.
// Всё типизировано интерфейсами lib/api.ts: расхождение с контрактом master
// краснеет на `npm run check`, а не всплывает кривым скриншотом через полгода.
//
// Раскладка по состояниям и версиям — АРИФМЕТИКА, а не сэмплинг: rng тратится
// только на шум (игроки, tick_ms, смещения времени). Иначе числа на витрине
// поплыли бы от любой правки, и переснять тот же кадр стало бы нельзя.
import type {
  ApiEvent,
  Environment,
  GameServer,
  Match,
  NodeInfo,
  ProjectInfo,
  VersionInfo,
} from '../lib/api';

/** mulberry32: детерминированный PRNG, значения [0,1). */
export function rng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export const REGIONS = ['ap-southeast', 'eu-central', 'us-east'] as const;

export const PROJECT = 'nova-arena';
export const SECOND_PROJECT = 'dune-raiders';

export interface Fleet {
  projects: ProjectInfo[];
  environments: Environment[];
  nodes: NodeInfo[];
  versions: VersionInfo[];
  servers: GameServer[];
  matches: Match[];
  events: ApiEvent[];
}

const MIN = 60_000;
const HOUR = 3_600_000;
const DAY = 24 * HOUR;

const iso = (ms: number): string => new Date(ms).toISOString();
const between = (r: () => number, lo: number, hi: number): number => lo + Math.floor(r() * (hi - lo + 1));

/** Короткий hex-идентификатор, как в проде (панель печатает его через shortId). */
function hex(r: () => number, len = 8): string {
  let s = '';
  for (let i = 0; i < len; i++) s += Math.floor(r() * 16).toString(16);
  return s;
}

/**
 * Ровное распределение `want` штук по `total` позициям: true на позиции `k`,
 * если k-я позиция «выпала» очередной штуке. Без случайности — раскладка версий
 * по дедикам обязана быть одинаковой от прогона к прогону.
 */
function spread(k: number, total: number, want: number): boolean {
  return Math.floor(((k + 1) * want) / total) !== Math.floor((k * want) / total);
}

/** Описание ноды до сборки: имя, регион, окружение, состояние. */
const NODE_PLAN: { host: string; region: string; env: string; state: NodeInfo['state'] }[] = [
  { host: 'bm-eu-01', region: 'eu-central', env: 'prod', state: 'active' },
  { host: 'bm-eu-02', region: 'eu-central', env: 'prod', state: 'active' },
  { host: 'bm-eu-03', region: 'eu-central', env: 'prod', state: 'active' },
  { host: 'bm-eu-04', region: 'eu-central', env: 'prod', state: 'draining' },
  { host: 'bm-us-01', region: 'us-east', env: 'prod', state: 'active' },
  { host: 'bm-us-02', region: 'us-east', env: 'prod', state: 'active' },
  { host: 'bm-us-03', region: 'us-east', env: 'prod', state: 'active' },
  { host: 'bm-ap-01', region: 'ap-southeast', env: 'prod', state: 'active' },
  { host: 'bm-ap-02', region: 'ap-southeast', env: 'prod', state: 'active' },
  { host: 'bm-ap-03', region: 'ap-southeast', env: 'prod', state: 'active' },
  { host: 'bm-eu-05', region: 'eu-central', env: 'dev', state: 'active' },
  { host: 'bm-us-04', region: 'us-east', env: 'dev', state: 'active' },
];

/** Ёмкость ноды в слотах. 12 нод × 10 = 120 слотов: 48 allocated дают те же
 *  40 % утилизации, что рисует ряд метрик (demo/series.ts) — снапшот и график
 *  обязаны сходиться, иначе витрина противоречит сама себе. */
export const CAPACITY_SLOTS = 10;

/** Нода с деградировавшим тиком — под активный алерт TickDegraded (demo/admin.ts). */
export const SLOW_NODE = 'bm-eu-03';

/** Версии проекта: две active живут одновременно, третья сливается. */
const VERSION_PLAN: {
  semver: string;
  env: string;
  state: VersionInfo['state'];
  ageDays: number;
  deprecatedMin?: number;
}[] = [
  { semver: '1.15.1', env: 'dev', state: 'active', ageDays: 0.4 },
  { semver: '1.15.0', env: 'prod', state: 'active', ageDays: 1.2 },
  { semver: '1.14.3', env: 'prod', state: 'active', ageDays: 6.1 },
  { semver: '1.14.2', env: 'prod', state: 'deprecated', ageDays: 11.4, deprecatedMin: 26 },
  { semver: '1.13.7', env: 'prod', state: 'disabled', ageDays: 27.8 },
];

/** Сколько дедиков какого состояния в срезе (спека витрины §2). */
const SERVER_PLAN: { state: GameServer['state']; count: number }[] = [
  { state: 'allocated', count: 48 },
  { state: 'ready', count: 24 },
  { state: 'draining', count: 6 },
  { state: 'creating', count: 2 },
];

/** Ядра ленты событий: kind + шаблон payload. Крутятся по кругу. */
const EVENT_KINDS = [
  'match_start',
  'match_end',
  'server_ready',
  'match_start',
  'match_end',
  'server_reaped',
  'match_start',
  'match_end',
  'server_ready',
  'node_drained',
  'deploy_started',
  'deploy_activated',
] as const;

export function buildFleet(now: number): Fleet {
  const r = rng(0xb17d); // одно семя на весь флот

  // --- Проекты и окружения -------------------------------------------------
  const projects: ProjectInfo[] = [
    // 16 игроков в матче: столько же держит верхнюю границу players_peak у
    // дедиков ниже. Размер матча и заполненность обязаны сходиться — иначе на
    // скриншоте матч на 10 мест показывает 15 игроков.
    { id: hex(r), slug: PROJECT, match_size: 16, created_at: iso(now - 96 * DAY) },
    { id: hex(r), slug: SECOND_PROJECT, match_size: 6, created_at: iso(now - 31 * DAY) },
  ];
  const environments: Environment[] = projects.flatMap((p) => [
    {
      project: p.slug,
      name: 'prod',
      production: true,
      auto_deploy: false,
      retention_keep: 10,
      created_at: p.created_at,
    },
    {
      project: p.slug,
      name: 'dev',
      production: false,
      auto_deploy: true,
      retention_keep: 5,
      created_at: p.created_at,
    },
  ]);

  // --- Ноды ----------------------------------------------------------------
  const nodes: NodeInfo[] = NODE_PLAN.map((n, i) => ({
    id: hex(r),
    project: PROJECT,
    region: n.region,
    env: n.env,
    hostname: n.host,
    public_ip: `203.0.113.${11 + i}`,
    capacity_slots: CAPACITY_SLOTS,
    agent_version: '0.9.4',
    state: n.state,
    last_heartbeat_at: iso(now - between(r, 2, 9) * 1000),
    created_at: iso(now - (18 + i * 3) * DAY),
  }));
  const prodNodes = nodes.filter((n) => n.env === 'prod');

  // --- Версии --------------------------------------------------------------
  const versions: VersionInfo[] = VERSION_PLAN.map((v) => ({
    id: hex(r),
    project: PROJECT,
    semver: v.semver,
    image_ref: `ghcr.io/acme-games/nova-arena:${v.semver}`,
    env: v.env,
    state: v.state,
    created_at: iso(now - v.ageDays * DAY),
    ...(v.deprecatedMin !== undefined ? { deprecated_at: iso(now - v.deprecatedMin * MIN) } : {}),
  }));
  const versionOf = (semver: string): VersionInfo => {
    const v = versions.find((x) => x.semver === semver);
    if (v === undefined) throw new Error(`demo: нет версии ${semver}`);
    return v;
  };
  const rolling = versionOf('1.15.0'); // катится
  const stable = versionOf('1.14.3'); // держит флот
  const draining = versionOf('1.14.2'); // сливается

  // --- Дедики --------------------------------------------------------------
  // Слив (6 штук) целиком на версии 1.14.2 — иначе окно мультиверсий на
  // Деплоях покажет слив пустым. Остальные 74 делятся 14/60 между катящейся и
  // держащей версиями: доля новой версии ≈ 18 % и читается на глаз.
  const states: GameServer['state'][] = SERVER_PLAN.flatMap((p) =>
    Array.from({ length: p.count }, () => p.state),
  );
  const nonDrainTotal = states.filter((s) => s !== 'draining').length;
  let k = 0;
  const servers: GameServer[] = states.map((state, i) => {
    const node = prodNodes[i % prodNodes.length];
    const version =
      state === 'draining' ? draining : spread(k++, nonDrainTotal, 14) ? rolling : stable;
    const slow = node.hostname === SLOW_NODE;
    const createdAgo = between(r, 4, 90) * MIN;
    return {
      id: hex(r),
      node_id: node.id,
      version_id: version.id,
      region: node.region,
      state,
      port: 30000 + i,
      players: state === 'allocated' ? between(r, 10, 14) : 0,
      tick_ms: slow ? between(r, 34, 41) : between(r, 11, 19),
      created_at: iso(now - createdAgo),
      updated_at: iso(now - between(r, 3, 120) * 1000),
    };
  });
  const allocated = servers.filter((s) => s.state === 'allocated');
  const idle = servers.filter((s) => s.state === 'ready');
  const semverOf = new Map(versions.map((v) => [v.id, v.semver]));
  const hostOf = new Map(nodes.map((n) => [n.id, n.public_ip]));

  // --- Матчи ---------------------------------------------------------------
  // 48 идущих (по одному на allocated-дедик), 96 доигранных за последний час
  // (они наполняют спарклайн Обзора) и 2 в очереди.
  const matches: Match[] = [];
  for (const s of allocated) {
    const created = now - between(r, 1, 40) * MIN;
    const m: Match = {
      id: hex(r),
      project: PROJECT,
      region: s.region,
      state: 'running',
      version_id: s.version_id,
      semver: semverOf.get(s.version_id) ?? '',
      server_id: s.id,
      host: hostOf.get(s.node_id) ?? '',
      port: s.port,
      server_players: s.players,
      players_peak: s.players + between(r, 0, 2),
      started_at: iso(created + 3000),
      created_at: iso(created),
    };
    matches.push(m);
    s.match_id = m.id;
  }
  for (let i = 0; i < 96; i++) {
    const s = idle[i % idle.length];
    const created = now - between(r, 11, 59) * MIN;
    const ended = Math.min(now - 30_000, created + between(r, 5, 11) * MIN);
    matches.push({
      id: hex(r),
      project: PROJECT,
      region: s.region,
      state: 'finished',
      version_id: s.version_id,
      semver: semverOf.get(s.version_id) ?? '',
      server_id: s.id,
      host: hostOf.get(s.node_id) ?? '',
      port: s.port,
      server_players: 0,
      players_peak: between(r, 8, 10),
      started_at: iso(created + 4000),
      ended_at: iso(ended),
      created_at: iso(created),
    });
  }
  for (let i = 0; i < 2; i++) {
    const s = idle[(i + 3) % idle.length];
    const created = now - between(r, 5, 40) * 1000;
    matches.push({
      id: hex(r),
      project: PROJECT,
      region: s.region,
      state: 'pending',
      version_id: s.version_id,
      semver: semverOf.get(s.version_id) ?? '',
      server_id: s.id,
      host: hostOf.get(s.node_id) ?? '',
      port: s.port,
      server_players: 0,
      players_peak: 0,
      created_at: iso(created),
    });
  }

  // --- Лента событий -------------------------------------------------------
  // 60 штук за последние 40 минут, id убывает: панель показывает свежие сверху.
  // Каждое событие собирается вокруг ОДНОГО дедика: нода и матч берутся из
  // него, а не выбираются независимо. Иначе строка ленты противоречит сама
  // себе — `region=us-east` рядом с `hostname=bm-eu-03`.
  const nodeById = new Map(nodes.map((n) => [n.id, n]));
  const matchOfServer = new Map(matches.map((m) => [m.server_id, m]));
  const drainingNode = nodes.find((n) => n.state === 'draining') ?? nodes[0];
  const events: ApiEvent[] = Array.from({ length: 60 }, (_, i) => {
    const kind = EVENT_KINDS[i % EVENT_KINDS.length];
    const s = servers[(i * 7) % servers.length];
    const node = kind === 'node_drained' ? drainingNode : (nodeById.get(s.node_id) ?? nodes[0]);
    const m = matchOfServer.get(s.id) ?? matches[0];
    const ts = now - i * 40_000 - between(r, 0, 20) * 1000;
    return {
      id: 4821 - i,
      ts: iso(ts),
      kind,
      project: PROJECT,
      node_id: node.id,
      server_id: s.id,
      ...(kind.startsWith('match') ? { match_id: m.id } : {}),
      ...(kind.startsWith('deploy') ? { version_id: rolling.id } : {}),
      payload: payloadFor(kind, s, m, node, rolling.semver, between(r, 214, 1180)),
    };
  });

  return { projects, environments, nodes, versions, servers, matches, events };
}

/** Полезная нагрузка события — то, что панель печатает справа в ленте. */
function payloadFor(
  kind: string,
  server: GameServer,
  match: Match,
  node: NodeInfo,
  rollingSemver: string,
  durationS: number,
): Record<string, unknown> {
  switch (kind) {
    case 'match_start':
      return { match: match.id, players: match.players_peak, region: match.region };
    case 'match_end':
      return { match: match.id, duration_s: durationS, players_peak: match.players_peak };
    case 'server_ready':
      return { port: server.port, region: server.region, hostname: node.hostname };
    case 'server_reaped':
      return { reason: 'version deprecated', hostname: node.hostname, region: node.region };
    case 'node_drained':
      return { hostname: node.hostname, region: node.region, live_servers: 6 };
    case 'deploy_started':
      return { semver: rollingSemver, env: 'prod', pending_nodes: 4 };
    case 'deploy_activated':
      return { semver: rollingSemver, env: 'prod', regions: 3 };
    default:
      return {};
  }
}
