// Админская часть демо: алерты, ключи, реестры, бекапы и продуктовые агрегаты
// /v1/stats/*. Всё, что не выводится из флота напрямую (demo/fleet.ts), живёт
// здесь. Числа согласованы с флотом: утилизация по регионам считается из него,
// а не выдумывается отдельно — иначе Cost противоречил бы Обзору.
import type {
  ActiveAlert,
  AlertEvent,
  AlertMute,
  AlertRule,
  ApiKey,
  BackupRun,
  BackupSettings,
  RegionUtil,
  RegistryInfo,
  SimpleSeries,
  StackedSeries,
  StatsCost,
  StatsOverview,
  VersionShare,
} from '../lib/api';
import { CAPACITY_SLOTS, PROJECT, REGIONS, rng, SLOW_NODE } from './fleet';
import type { Fleet } from './fleet';

const MIN = 60_000;
const HOUR = 3_600_000;
const DAY = 24 * HOUR;

const iso = (ms: number): string => new Date(ms).toISOString();
/** YYYY-MM-DD в UTC — формат точки суточного ряда (master httpapi/stats.go). */
const day = (ms: number): string => new Date(ms).toISOString().slice(0, 10);

// --- Алерты -----------------------------------------------------------------

const TICK_DESC_EN = `p95 server tick above 30 ms on ${SLOW_NODE} for 10m`;
// i18n-allow: не копия интерфейса, а поле `description_ru` алерта — master отдаёт его ровно таким.
const TICK_DESC_RU = `p95 тика дедиков выше 30 мс на ${SLOW_NODE} десять минут подряд`; // i18n-allow

export function alertRules(): AlertRule[] {
  return [
    {
      name: 'TickDegraded',
      group: 'birdman-project',
      severity: 'warning',
      expr: 'histogram_quantile(0.95, sum by (le, node) (rate(birdman_tick_seconds_bucket[5m]))) > 0.03',
      for: '10m',
      state: 'firing',
      description: TICK_DESC_EN,
      description_ru: TICK_DESC_RU,
    },
    {
      name: 'NodeDown',
      group: 'birdman-project',
      severity: 'critical',
      expr: 'time() - birdman_node_last_heartbeat_seconds > 300',
      for: '5m',
      state: 'inactive',
      description: 'a node stopped sending heartbeats',
      description_ru: 'нода перестала слать heartbeat', // i18n-allow
    },
    {
      name: 'BackupStale',
      group: 'birdman-platform',
      severity: 'warning',
      expr: 'time() - birdman_backup_last_success_timestamp > 86400',
      for: '30m',
      state: 'inactive',
      description: 'no successful Postgres backup in the last 24h',
      description_ru: 'сутки без успешного бекапа Postgres', // i18n-allow
    },
  ];
}

export function activeAlerts(now: number): ActiveAlert[] {
  return [
    {
      name: 'TickDegraded',
      severity: 'warning',
      region: 'eu-central',
      node: SLOW_NODE,
      state: 'firing',
      active_at: iso(now - 14 * MIN),
      value: '37.4',
      description: TICK_DESC_EN,
      description_ru: TICK_DESC_RU,
      muted: false,
      project: PROJECT,
      scope: 'project',
    },
  ];
}

export function alertHistory(now: number): AlertEvent[] {
  return [
    {
      name: 'TickDegraded',
      severity: 'warning',
      region: 'eu-central',
      node: SLOW_NODE,
      startsAt: iso(now - 14 * MIN),
      endsAt: iso(now + 5 * MIN),
      description: TICK_DESC_EN,
      description_ru: TICK_DESC_RU,
      active: true,
      received_at: iso(now - 14 * MIN),
      project: PROJECT,
      scope: 'project',
    },
    {
      name: 'NodeDown',
      severity: 'critical',
      region: 'us-east',
      node: 'bm-us-02',
      startsAt: iso(now - 6 * HOUR),
      endsAt: iso(now - 6 * HOUR + 22 * MIN),
      description: 'a node stopped sending heartbeats',
      description_ru: 'нода перестала слать heartbeat', // i18n-allow
      active: false,
      received_at: iso(now - 6 * HOUR),
      project: PROJECT,
      scope: 'project',
    },
    {
      name: 'BackupStale',
      severity: 'warning',
      region: '',
      node: '',
      startsAt: iso(now - 3 * DAY),
      endsAt: iso(now - 3 * DAY + 51 * MIN),
      description: 'no successful Postgres backup in the last 24h',
      description_ru: 'сутки без успешного бекапа Postgres', // i18n-allow
      active: false,
      received_at: iso(now - 3 * DAY),
      scope: 'platform',
    },
  ];
}

export function alertMutes(now: number): AlertMute[] {
  return [
    {
      id: 'mute-1',
      alertname: 'TickDegraded',
      region: 'ap-southeast',
      project: PROJECT,
      note: 'known noisy box, replacement ordered',
      created_at: iso(now - 2 * DAY),
      expires_at: iso(now + 5 * DAY),
      created_by: 'ops',
    },
  ];
}

// --- Ключи, реестры, бекапы --------------------------------------------------

export function apiKeys(now: number): ApiKey[] {
  return [
    {
      id: 'key-ci-deploy',
      name: 'ci-deploy',
      scopes: ['deploy'],
      created_at: iso(now - 41 * DAY),
      revoked_at: null,
      project: PROJECT,
      env: 'prod',
    },
    {
      id: 'key-game-backend',
      name: 'game-backend',
      scopes: ['matchmaking'],
      created_at: iso(now - 63 * DAY),
      revoked_at: null,
    },
    {
      id: 'key-grafana',
      name: 'grafana',
      scopes: ['readonly'],
      created_at: iso(now - 63 * DAY),
      revoked_at: null,
    },
    {
      id: 'key-retired-ci',
      name: 'retired-ci',
      scopes: ['deploy'],
      created_at: iso(now - 88 * DAY),
      revoked_at: iso(now - 12 * DAY),
    },
  ];
}

export function registries(now: number): RegistryInfo[] {
  return [
    {
      id: 'reg-ghcr',
      host: 'ghcr.io',
      type: 'ghcr',
      username: 'acme-games-ci',
      note: 'nova-arena images',
      created_at: iso(now - 63 * DAY),
      updated_at: iso(now - 9 * DAY),
    },
  ];
}

export function backupSettings(now: number): BackupSettings {
  return {
    enabled: true,
    interval_hours: 6,
    retention_local: 14,
    s3_enabled: true,
    s3_endpoint: 'https://s3.eu-central-1.amazonaws.com',
    s3_region: 'eu-central-1',
    s3_bucket: 'birdman-backups-demo',
    s3_prefix: 'nova-arena/',
    s3_access_key: 'AKIADEMO0000EXAMPLE',
    has_s3_secret: true,
    retention_s3: 30,
    updated_at: iso(now - 9 * DAY),
  };
}

export function backupRuns(now: number): BackupRun[] {
  const r = rng(0x8ac0);
  const runs: BackupRun[] = [];
  for (let i = 0; i < 8; i++) {
    const started = now - 90 * MIN - i * 6 * HOUR;
    // Один сорванный прогон трое суток назад: история без единой ошибки
    // выглядит нарисованной, а оператору важно видеть, что колонка ошибок жива.
    const failed = i === 5;
    runs.push({
      id: 4120 - i,
      started_at: iso(started),
      finished_at: iso(started + (failed ? 42_000 : 96_000 + Math.floor(r() * 20_000))),
      kind: i === 2 ? 'manual' : 'scheduled',
      result: failed ? 'error' : 'ok',
      size_bytes: failed ? null : 243_000_000 + Math.floor(r() * 9_000_000),
      s3_uploaded: !failed,
      error: failed ? 's3: connection reset by peer' : '',
    });
  }
  return runs;
}

// --- Продуктовые агрегаты /v1/stats/* ---------------------------------------

/** Суточная доля дня недели: выходные тяжелее буднего дня. */
function weekFactor(ms: number): number {
  const d = new Date(ms).getUTCDay();
  return d === 0 || d === 6 ? 1.24 : 1;
}

/** Стек по ключам: total + разбивка, все ключи присутствуют (0-filled). */
function stacked(
  days: number,
  now: number,
  unit: string,
  keys: string[],
  base: number,
  weights: number[],
  seed: number,
): StackedSeries {
  const r = rng(seed);
  const points = [];
  for (let i = days - 1; i >= 0; i--) {
    const ms = now - i * DAY;
    const total = Math.round(base * weekFactor(ms) * (0.88 + r() * 0.24) * (1 + (days - i) / (days * 12)));
    const values: Record<string, number> = {};
    let rest = total;
    keys.forEach((k, j) => {
      const v = j === keys.length - 1 ? rest : Math.round(total * weights[j]);
      values[k] = v;
      rest -= v;
    });
    points.push({ date: day(ms), total, values });
  }
  return { unit, keys, points };
}

function simple(days: number, now: number, unit: string, base: number, spreadPct: number, seed: number): SimpleSeries {
  const r = rng(seed);
  const points = [];
  for (let i = days - 1; i >= 0; i--) {
    const ms = now - i * DAY;
    points.push({ date: day(ms), value: Math.round(base * weekFactor(ms) * (1 - spreadPct + r() * spreadPct * 2)) });
  }
  return { unit, points };
}

/** Доли версий в матчах периода. Абсолютные числа считаются от итога ряда
 *  matches_per_day — иначе экран сообщал бы «107 164 матча за 30 дней» и тут же
 *  расписывал по версиям 30 157. */
const VERSION_SHARES: { version: string; share: number }[] = [
  { version: '1.14.3', share: 0.71 },
  { version: '1.15.0', share: 0.18 },
  { version: '1.14.2', share: 0.11 },
];

function versionDistribution(totalMatches: number): VersionShare[] {
  let rest = totalMatches;
  return VERSION_SHARES.map((v, i) => {
    const matches = i === VERSION_SHARES.length - 1 ? rest : Math.round(totalMatches * v.share);
    rest -= matches;
    return { version: v.version, matches, share: v.share };
  });
}

export function statsOverview(days: number, now: number): StatsOverview {
  const matchesPerDay = stacked(days, now, 'matches', [...REGIONS], 3180, [0.24, 0.44, 0.32], 0x51a1);
  return {
    days,
    timezone: 'UTC',
    generated_at: iso(now),
    matches_per_day: matchesPerDay,
    players_per_day: stacked(days, now, 'players', [...REGIONS], 24_600, [0.23, 0.45, 0.32], 0x51a2),
    peak_ccu_per_day: simple(days, now, 'players', 764, 0.09, 0x51a3),
    peak_ccu: 812,
    avg_match_duration_seconds: 618,
    avg_match_duration_per_day: simple(days, now, 'seconds', 618, 0.05, 0x51a4),
    version_distribution: versionDistribution(matchesPerDay.points.reduce((a, p) => a + p.total, 0)),
    time_to_match: {
      p50_seconds: 7.4,
      p95_seconds: 21.8,
      samples: 18_422,
      source: 'histogram',
      note: '',
    },
  };
}

/** Утилизация регионов — прямо из флота, чтобы Cost не спорил с Обзором. */
export function regionUtilization(fleet: Fleet): RegionUtil[] {
  return [...REGIONS].map((region) => {
    const nodes = fleet.nodes.filter((n) => n.region === region && n.state !== 'dead');
    const servers = fleet.servers.filter((s) => s.region === region);
    const count = (state: string) => servers.filter((s) => s.state === state).length;
    return {
      region,
      capacity_slots: nodes.length * CAPACITY_SLOTS,
      allocated: count('allocated'),
      ready: count('ready'),
      draining: count('draining'),
    };
  });
}

export function statsCost(days: number, now: number, fleet: Fleet): StatsCost {
  const byRegion = stacked(days, now, 'slot-hours', [...REGIONS], 1970, [0.24, 0.44, 0.32], 0xc051);
  return {
    days,
    timezone: 'UTC',
    generated_at: iso(now),
    slot_hours_per_day_by_region: byRegion,
    slot_hours_per_day_by_version: stacked(days, now, 'slot-hours', ['1.14.2', '1.14.3', '1.15.0'], 1970, [0.11, 0.71, 0.18], 0xc052),
    slot_hours_total: byRegion.points.reduce((a, p) => a + p.total, 0),
    utilization: regionUtilization(fleet),
    utilization_note: 'snapshot of live dedicated servers against active node capacity',
  };
}
