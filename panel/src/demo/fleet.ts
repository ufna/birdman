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
  // Все ноды active. Дренящаяся нода дала бы на Обзоре пару «11 / 12» и
  // «all active» разом: панель считает подпись по отсутствию quarantine/down,
  // и на витрине это читается как противоречие. Историю слива несёт Деплой —
  // там версия 1.14.2 сливается ВИДИМО, с числом живых дедиков.
  { host: 'bm-eu-04', region: 'eu-central', env: 'prod', state: 'active' },
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
  // dev-версия ПРЯМО СЕЙЧАС разъезжается (auto_deploy у dev включён), поэтому
  // prepulling, а не active: карточка «окно мультиверсий» берёт новейшую
  // active-версию, и dev-версия с нулём дедиков забирала бы у неё место —
  // окно показывало бы «Active 1.15.1 · 0 dedics» вместо прод-пары.
  { semver: '1.15.1', env: 'dev', state: 'prepulling', ageDays: 0.4 },
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

/** Фон ленты: то, чем флот занят непрерывно. Крутится по кругу. */
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
] as const;

/**
 * Сценарий выката поверх фона: индекс в ленте (0 — самое свежее) → вид события.
 * Индексы 2…14 — семь отчётов о пре-пуле dev-версии 1.15.1, индекс 16 — её
 * старт. Порядок именно такой: панель сидирует прогресс от СТАРЫХ к новым
 * (`Deploys.tsx`, `useDeployProgress`), и `deploy_started` обязан быть СТАРШЕ
 * своих `deploy_node_pulled` — иначе он обнулит уже набранный прогресс.
 * Индексы 32/36 — завершённый выкат прод-версии 1.15.0 получасом раньше.
 */
const EVENT_SCRIPT: Record<number, string> = {
  2: 'deploy_node_pulled',
  4: 'deploy_node_pulled',
  6: 'deploy_node_pulled',
  8: 'deploy_node_pulled',
  10: 'deploy_node_pulled',
  12: 'deploy_node_pulled',
  14: 'deploy_node_pulled',
  16: 'deploy_started',
  32: 'deploy_activated',
  36: 'deploy_started',
};

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
  const warming = versionOf('1.15.1'); // греется на dev прямо сейчас
  const rolling = versionOf('1.15.0'); // выкачена в прод сутки назад
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
  // Порядок отчётов о пре-пуле, считая от свежего: он задаёт и ноду, и остаток.
  // Семь спуленных при остатке 5 дают на полосе прогресса «7 / 12 nodes».
  const pullAt = Object.keys(EVENT_SCRIPT)
    .map(Number)
    .filter((k) => EVENT_SCRIPT[k] === 'deploy_node_pulled')
    .sort((a, b) => a - b);
  const events: ApiEvent[] = Array.from({ length: 60 }, (_, i) => {
    const kind = EVENT_SCRIPT[i] ?? EVENT_KINDS[i % EVENT_KINDS.length];
    const s = servers[(i * 7) % servers.length];
    const m = matchOfServer.get(s.id) ?? matches[0];
    const ts = now - i * 40_000 - between(r, 0, 20) * 1000;
    // Пре-пул отчитывается ПО НОДЕ, а не по дедику: у каждого отчёта своя
    // тачка (панель складывает их в множество) и свой остаток.
    const pullIdx = pullAt.indexOf(i);
    const isPull = pullIdx >= 0;
    const node = isPull ? nodes[pullIdx] : (nodeById.get(s.node_id) ?? nodes[0]);
    const version = kind === 'deploy_node_pulled' || i === 16 ? warming : rolling;
    return {
      id: 4821 - i,
      ts: iso(ts),
      kind,
      project: PROJECT,
      node_id: node.id,
      ...(isPull ? {} : { server_id: s.id }),
      ...(kind.startsWith('match') ? { match_id: m.id } : {}),
      ...(kind.startsWith('deploy') ? { version_id: version.id } : {}),
      payload: isPull
        ? { semver: warming.semver, env: warming.env, hostname: node.hostname, remaining: 5 + pullIdx }
        : payloadFor(kind, s, m, node, version.semver, between(r, 214, 1180)),
    };
  });

  return { projects, environments, nodes, versions, servers, matches, events };
}

/**
 * Снапшот флота для рядов метрик (demo/series.ts): чем должен заканчиваться
 * график в точке «сейчас». Все числа НЕ зависят от `now` (раскладка по
 * состояниям задана арифметикой, игроки — семенем), поэтому флот тут строится
 * один раз и от произвольного момента.
 */
export interface Snapshot {
  playersOnline: number;
  matchesRunning: number;
  allocated: number;
  ready: number;
  draining: number;
  creating: number;
  capacitySlots: number;
  /** Сколько секунд назад версию демотнули — с этого места на стеке ступенька. */
  deprecatedAgoSec: number;
  serverById: (id: string) => { players: number; tickMs: number } | undefined;
}

let snapshotCache: Snapshot | null = null;

export function fleetSnapshot(): Snapshot {
  if (snapshotCache !== null) return snapshotCache;
  const f = buildFleet(0);
  const count = (state: GameServer['state']) => f.servers.filter((s) => s.state === state).length;
  const byId = new Map(f.servers.map((s) => [s.id, { players: s.players, tickMs: s.tick_ms ?? 14 }]));
  snapshotCache = {
    playersOnline: f.servers.filter((s) => s.state === 'allocated').reduce((a, s) => a + s.players, 0),
    matchesRunning: f.matches.filter((m) => m.state === 'running').length,
    allocated: count('allocated'),
    ready: count('ready'),
    draining: count('draining'),
    creating: count('creating'),
    capacitySlots: f.nodes.filter((n) => n.state !== 'dead').length * CAPACITY_SLOTS,
    deprecatedAgoSec: 26 * 60,
    serverById: (id) => byId.get(id),
  };
  return snapshotCache;
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
      // id матча панель печатает сама из match_id — в payload его дублировать
      // незачем, строка ленты и так узкая.
      return { players: match.players_peak, region: match.region, semver: match.semver };
    case 'match_end':
      return { duration_s: durationS, players_peak: match.players_peak, region: match.region };
    case 'server_ready':
      return { port: server.port, region: server.region, hostname: node.hostname };
    case 'server_reaped':
      return { reason: 'version deprecated', hostname: node.hostname, region: node.region };
    case 'deploy_started':
      return { semver: rollingSemver, env: 'prod', pending_nodes: 4 };
    case 'deploy_activated':
      return { semver: rollingSemver, env: 'prod', regions: 3 };
    default:
      return {};
  }
}
