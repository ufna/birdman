// Overview: «за 5 секунд понятно, всё ли живо» (panel.md §5) — карточки
// состояния, спарклайн матчей за час, живая лента событий.

import { useMemo } from 'react';
import { api } from '../lib/api';
import type { GameServer, Match, NodeInfo, VersionInfo } from '../lib/api';
import { useData } from '../lib/live';
import { useNow } from '../lib/useNow';
import { Card, CardHeader, ErrorNote, LoadingRow, StatCard } from '../components/ui';
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
  return <OverviewBody stats={stats} matches={matches.data ?? []} now={now} />;
}

function OverviewBody({ stats, matches, now }: { stats: Stats; matches: Match[]; now: number }) {
  const lastHour = useMemo(
    () => matches.filter((m) => now - new Date(m.created_at).getTime() < HOUR_MS),
    [matches, now],
  );
  const points = useMemo(() => bucketPerMinute(lastHour.map((m) => m.created_at), now), [lastHour, now]);

  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-5">
        <StatCard
          label="Живые матчи"
          value={stats.liveMatches}
          detail={`running ${stats.runningMatches} · pending ${stats.pendingMatches}`}
        />
        <StatCard label="Игроки онлайн" value={stats.playersOnline} detail="по allocated-дедикам" />
        <StatCard
          label="Ready-буфер"
          value={stats.readyTotal}
          detail={chips(stats.readyByRegion) || 'нет ready-дедиков'}
        />
        <StatCard
          label="Тачки"
          value={
            <>
              {stats.nodesActive}
              <span className="text-base font-normal text-muted"> / {stats.nodesTotal}</span>
            </>
          }
          detail={
            stats.nodesQuarantine > 0 ? (
              <span className="font-medium text-dead">{stats.nodesQuarantine} в карантине</span>
            ) : (
              'все активны'
            )
          }
        />
        <StatCard
          label="Версия флита"
          value={stats.fleetVersions.length > 0 ? stats.fleetVersions[0].semver : '—'}
          detail={
            stats.fleetVersions.length > 0
              ? stats.fleetVersions.map((v) => `${v.region} ${v.semver}${v.extra > 0 ? ` +${v.extra}` : ''}`).join(' · ')
              : 'нет живых дедиков'
          }
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-5">
        <Card className="lg:col-span-2">
          <CardHeader
            title="Матчи за час"
            aside={<span className="tabular font-mono text-sm text-muted">{lastHour.length}</span>}
          />
          <div className="px-3 py-3">
            <Sparkline points={points} />
          </div>
        </Card>
        <Card className="lg:col-span-3">
          <CardHeader title="Последние события" />
          <EventsFeed />
        </Card>
      </div>
    </div>
  );
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
    fleetVersions,
  };
}

function chips(byRegion: Map<string, number>): string {
  return [...byRegion.entries()]
    .sort((a, b) => a[0].localeCompare(b[0]))
    .map(([r, n]) => `${r} ${n}`)
    .join(' · ');
}
