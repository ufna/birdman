// Overview: «за 5 секунд понятно, всё ли живо» (panel.md §5) — карточки
// состояния, спарклайн матчей за час, живая лента событий.

import { useMemo } from 'react';
import { api } from '../lib/api';
import type { GameServer, Match, NodeInfo, VersionInfo } from '../lib/api';
import { useData } from '../lib/live';
import { useNow } from '../lib/useNow';
import { useT, useFormat } from '../lib/i18n';
import { Card, CardHeader, ErrorNote, LiveValue, LoadingRow, StatCard } from '../components/ui';
import { EventsFeed } from '../components/EventsFeed';
import { Sparkline, bucketPerMinute } from '../components/Sparkline';

const HOUR_MS = 3_600_000;
const LIVE_SERVER_STATES = new Set(['creating', 'ready', 'allocated', 'draining']);

export function Overview() {
  const nodes = useData(() => api.listNodes(), []);
  const servers = useData(() => api.listServers(), []);
  const versions = useData(() => api.listVersions(), []);
  // Матчи за час для спарклайна + live-счётчик; лимита 1000 хватает v0.
  const matches = useData(() => api.listMatches({ limit: 1000 }), []);
  const now = useNow();

  const error = nodes.error ?? servers.error ?? versions.error ?? matches.error;
  const loading =
    nodes.data === undefined ||
    servers.data === undefined ||
    versions.data === undefined ||
    matches.data === undefined;

  if (error !== undefined && loading) {
    return (
      <ErrorNote
        error={error}
        retry={() => {
          nodes.reload();
          servers.reload();
          versions.reload();
          matches.reload();
        }}
      />
    );
  }
  if (loading) return <LoadingRow />;

  const stats = computeStats(nodes.data ?? [], servers.data ?? [], versions.data ?? [], matches.data ?? []);
  // Свежесть = самое недавнее успешное чтение среди источников (все рефетчатся
  // вместе по SSE); индикатор «обновлено N назад».
  const updatedAt = maxDefined([nodes.updatedAt, servers.updatedAt, versions.updatedAt, matches.updatedAt]);
  return <OverviewBody stats={stats} matches={matches.data ?? []} now={now} updatedAt={updatedAt} />;
}

function OverviewBody({
  stats,
  matches,
  now,
  updatedAt,
}: {
  stats: Stats;
  matches: Match[];
  now: number;
  updatedAt: number | undefined;
}) {
  const { t } = useT();
  const lastHour = useMemo(
    () => matches.filter((m) => now - new Date(m.created_at).getTime() < HOUR_MS),
    [matches, now],
  );
  const points = useMemo(() => bucketPerMinute(lastHour.map((m) => m.created_at), now), [lastHour, now]);

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center justify-end">
        <Freshness updatedAt={updatedAt} now={now} />
      </div>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-5">
        <StatCard
          label={t('ov.liveMatches')}
          value={<LiveValue value={stats.liveMatches} />}
          detail={t('ov.liveDetail', { running: stats.runningMatches, pending: stats.pendingMatches })}
        />
        <StatCard label={t('ov.playersOnline')} value={<LiveValue value={stats.playersOnline} />} detail={t('ov.playersDetail')} />
        <StatCard
          label={t('ov.readyBuffer')}
          value={<LiveValue value={stats.readyTotal} />}
          detail={chips(stats.readyByRegion) || t('ov.noReady')}
        />
        <StatCard
          label={t('ov.nodes')}
          value={
            <>
              {stats.nodesActive}
              <span className="text-base font-normal text-muted"> / {stats.nodesTotal}</span>
            </>
          }
          detail={
            stats.nodesQuarantine > 0 || stats.nodesDown > 0 ? (
              // Два отдельных красных чипа: карантин и down живут одновременно
              // (часть тачек молчит дольше node_down_after_min и уже деградировала).
              <>
                {stats.nodesQuarantine > 0 && (
                  <span className="font-medium text-dead">{t('ov.inQuarantine', { count: stats.nodesQuarantine })}</span>
                )}
                {stats.nodesQuarantine > 0 && stats.nodesDown > 0 && <span className="text-muted"> · </span>}
                {stats.nodesDown > 0 && (
                  <span className="font-medium text-dead">{t('ov.down', { count: stats.nodesDown })}</span>
                )}
              </>
            ) : (
              t('ov.allActive')
            )
          }
        />
        <StatCard
          label={t('ov.fleetVersion')}
          value={stats.fleetVersions.length > 0 ? stats.fleetVersions[0].semver : '—'}
          detail={
            stats.fleetVersions.length > 0
              ? stats.fleetVersions.map((v) => `${v.region} ${v.semver}${v.extra > 0 ? ` +${v.extra}` : ''}`).join(' · ')
              : t('ov.noLiveDedics')
          }
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-5">
        <Card className="lg:col-span-2">
          <CardHeader
            title={t('ov.matchesHour')}
            aside={<span className="tabular font-mono text-sm text-muted">{lastHour.length}</span>}
          />
          <div className="px-3 py-3">
            <Sparkline points={points} />
          </div>
        </Card>
        <Card className="lg:col-span-3">
          <CardHeader title={t('ov.recentEvents')} />
          <EventsFeed />
        </Card>
      </div>
    </div>
  );
}

/** Индикатор свежести: «обновлено N назад» + живая точка (дышит, пока стрим жив). */
function Freshness({ updatedAt, now }: { updatedAt: number | undefined; now: number }) {
  const { t } = useT();
  const fmt = useFormat();
  if (updatedAt === undefined) return null;
  const ageMs = Math.max(0, now - updatedAt);
  const label = ageMs < 5000 ? t('ov.updatedNow') : t('ov.updated', { ago: fmt.ago(ageMs) });
  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-muted" aria-label={t('ov.liveAria')}>
      <span aria-hidden className="size-1.5 rounded-full bg-good live-dot-on" />
      <span className="tabular">{label}</span>
    </span>
  );
}

/** Наибольшее из заданных чисел (undefined пропускаются); все пусты → undefined. */
function maxDefined(xs: (number | undefined)[]): number | undefined {
  let max: number | undefined;
  for (const x of xs) {
    if (x !== undefined && (max === undefined || x > max)) max = x;
  }
  return max;
}

interface Stats {
  liveMatches: number;
  runningMatches: number;
  pendingMatches: number;
  playersOnline: number;
  readyTotal: number;
  readyByRegion: Map<string, number>;
  nodesActive: number;
  nodesTotal: number;
  nodesQuarantine: number;
  nodesDown: number;
  fleetVersions: { region: string; semver: string; extra: number }[];
}

function computeStats(nodes: NodeInfo[], servers: GameServer[], versions: VersionInfo[], matches: Match[]): Stats {
  // «Живой» матч v0 = pending|running: pending уже занимает дедик, liba
  // переведёт его в running в следующей итерации (master.md §1).
  const running = matches.filter((m) => m.state === 'running').length;
  const pending = matches.filter((m) => m.state === 'pending').length;

  const readyByRegion = new Map<string, number>();
  let playersOnline = 0;
  const semverOf = new Map(versions.map((v) => [v.id, v.semver]));
  const regionVersions = new Map<string, Map<string, number>>();
  for (const s of servers) {
    if (s.state === 'ready') {
      readyByRegion.set(s.region, (readyByRegion.get(s.region) ?? 0) + 1);
    }
    if (s.state === 'allocated') playersOnline += s.players;
    if (LIVE_SERVER_STATES.has(s.state)) {
      const sv = semverOf.get(s.version_id);
      if (sv !== undefined) {
        const byVer = regionVersions.get(s.region) ?? new Map<string, number>();
        byVer.set(sv, (byVer.get(sv) ?? 0) + 1);
        regionVersions.set(s.region, byVer);
      }
    }
  }

  const fleetVersions = [...regionVersions.entries()]
    .map(([region, byVer]) => {
      const top = [...byVer.entries()].sort((a, b) => b[1] - a[1])[0];
      return { region, semver: top[0], extra: byVer.size - 1 };
    })
    .sort((a, b) => a.region.localeCompare(b.region));

  return {
    liveMatches: running + pending,
    runningMatches: running,
    pendingMatches: pending,
    playersOnline,
    readyTotal: [...readyByRegion.values()].reduce((a, b) => a + b, 0),
    readyByRegion,
    nodesActive: nodes.filter((n) => n.state === 'active').length,
    nodesTotal: nodes.length,
    nodesQuarantine: nodes.filter((n) => n.state === 'quarantine').length,
    // Карантин дольше node_down_after_min → мастер деградирует ноду в `down`;
    // считаем отдельно, чтобы красная сводка не гасла, когда стало хуже.
    nodesDown: nodes.filter((n) => n.state === 'down').length,
    fleetVersions,
  };
}

function chips(byRegion: Map<string, number>): string {
  return [...byRegion.entries()]
    .sort((a, b) => a[0].localeCompare(b[0]))
    .map(([r, n]) => `${r} ${n}`)
    .join(' · ');
}
