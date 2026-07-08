// Деплои: таблица версий, окно мультиверсий (active + deprecated и сколько
// дедиков каждой ещё живо), прогресс pre-pull по тачкам во время деплоя,
// кнопки Deploy/Rollback (confirm, только при скоупе deploy/admin).
// Live — через SSE (deploy_started/activated/failed/rolled_back). Всё на
// публичном API: GET /v1/versions, /v1/servers, /v1/nodes, POST /v1/deploy,
// /v1/rollback, /v1/events (seed прогресса).

import { useCallback, useEffect, useMemo, useState } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import { api } from '../lib/api';
import type { ApiEvent, GameServer, NodeInfo, VersionInfo } from '../lib/api';
import { useData, useLive } from '../lib/live';
import { canDeploy, useSession } from '../lib/session';
import { formatStamp, shortId } from '../lib/format';
import { DataTable } from '../components/DataTable';
import { StateBadge, toneOfVersionState } from '../components/Badge';
import { ConfirmButton } from '../components/ConfirmDialog';
import { Card, CardHeader, ErrorNote, LoadingRow } from '../components/ui';

const LIVE_SERVER_STATES = new Set(['creating', 'ready', 'allocated', 'draining']);
const HEARTBEAT_FRESH_MS = 30_000;

export function Deploys() {
  const versions = useData(() => api.listVersions(), []);
  const servers = useData(() => api.listServers(), []);
  const nodes = useData(() => api.listNodes(), []);
  const progress = useDeployProgress();
  const { session } = useSession();
  const mayDeploy = session != null && canDeploy(session);

  const liveByVersion = useMemo(() => countLiveByVersion(servers.data ?? []), [servers.data]);

  const error = versions.error ?? servers.error ?? nodes.error;
  if (error !== undefined && versions.data === undefined) {
    return <ErrorNote error={error} retry={versions.reload} />;
  }
  if (versions.data === undefined) return <LoadingRow />;

  const projects = groupByProject(versions.data);

  return (
    <div className="flex flex-col gap-4">
      {projects.map(({ project, versions: pv }) => (
        <ProjectDeploys
          key={project}
          project={project}
          versions={pv}
          servers={servers.data ?? []}
          nodes={nodes.data ?? []}
          liveByVersion={liveByVersion}
          progress={progress}
          mayDeploy={mayDeploy}
          reload={() => {
            versions.reload();
            servers.reload();
          }}
        />
      ))}
      {projects.length === 0 && (
        <Card>
          <CardHeader title="Деплои" />
          <div className="px-4 py-10 text-center text-sm text-muted">
            Версий пока нет. Зарегистрируйте билд: <span className="font-mono">POST /v1/versions</span>.
          </div>
        </Card>
      )}
    </div>
  );
}

function ProjectDeploys({
  project,
  versions,
  servers,
  nodes,
  liveByVersion,
  progress,
  mayDeploy,
  reload,
}: {
  project: string;
  versions: VersionInfo[];
  servers: GameServer[];
  nodes: NodeInfo[];
  liveByVersion: Map<string, number>;
  progress: Map<string, DeployProgress>;
  mayDeploy: boolean;
  reload: () => void;
}) {
  const active = versions.find((v) => v.state === 'active');
  const deprecated = versions.filter((v) => v.state === 'deprecated');
  const prepulling = versions.filter((v) => v.state === 'prepulling');
  const regionActive = useMemo(() => deriveRegionActive(servers, versions), [servers, versions]);

  const columns = useMemo<ColumnDef<VersionInfo, unknown>[]>(
    () => [
      {
        id: 'semver',
        header: 'Версия',
        cell: ({ row }) => (
          <div>
            <div className="font-mono font-medium">{row.original.semver}</div>
            <div className="font-mono text-xs text-muted">{row.original.channel}</div>
          </div>
        ),
      },
      {
        id: 'state',
        header: 'Состояние',
        cell: ({ row }) => <StateBadge state={row.original.state} tone={toneOfVersionState(row.original.state)} />,
      },
      {
        id: 'live',
        header: 'Дедиков живо',
        cell: ({ row }) => {
          const n = liveByVersion.get(row.original.id) ?? 0;
          return <span className="tabular font-mono text-xs">{n > 0 ? n : '—'}</span>;
        },
      },
      {
        id: 'created',
        header: 'Создана',
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{formatStamp(row.original.created_at)}</span>,
      },
      {
        id: 'deprecated',
        header: 'Депрекейт',
        cell: ({ row }) => (
          <span className="tabular font-mono text-xs text-muted">
            {row.original.deprecated_at !== undefined ? formatStamp(row.original.deprecated_at) : '—'}
          </span>
        ),
      },
      {
        id: 'actions',
        header: '',
        cell: ({ row }) =>
          mayDeploy ? <DeployAction version={row.original} onDone={reload} /> : null,
      },
    ],
    [liveByVersion, mayDeploy, reload],
  );

  return (
    <Card>
      <CardHeader
        title={`Проект ${project}`}
        aside={
          mayDeploy && deprecated.length > 0 ? (
            <ConfirmButton
              label="Откатить"
              tone="dead"
              title={`Откат проекта ${project}`}
              description={
                <>
                  Активную версию заменит последняя deprecated (
                  <span className="font-mono">{deprecated[0].semver}</span>). Образы уже на тачках — откат за секунды,
                  живые матчи не рвутся.
                </>
              }
              confirmLabel="Откатить"
              onConfirm={async () => {
                await api.rollback({ project });
                reload();
              }}
            />
          ) : (
            <span className="font-mono text-xs text-muted">{versions.length} версий</span>
          )
        }
      />

      <div className="grid gap-4 border-b border-line p-4 md:grid-cols-2">
        <WindowCard active={active} deprecated={deprecated} liveByVersion={liveByVersion} />
        <RegionActiveCard regionActive={regionActive} activeSemver={active?.semver} />
      </div>

      {prepulling.map((v) => (
        <PrepullBar key={v.id} version={v} progress={progress.get(v.id)} nodes={nodes} />
      ))}

      <DataTable
        columns={columns}
        data={versions}
        rowId={(v) => v.id}
        empty="Версий проекта нет."
      />
    </Card>
  );
}

/** Deploy-кнопка для строки версии: доступна для registered/deprecated. */
function DeployAction({ version, onDone }: { version: VersionInfo; onDone: () => void }) {
  if (version.state === 'prepulling') {
    return <span className="font-mono text-xs text-warn">идёт прогрев…</span>;
  }
  if (version.state !== 'registered' && version.state !== 'deprecated') return null;
  return (
    <ConfirmButton
      label="Развернуть"
      title={`Развернуть ${version.semver}?`}
      description={
        <>
          Master спрогреет образ на всех живых тачках (pre-pull), затем атомарно переключит активную версию. Живые матчи
          старой версии доигрывают в окне мультиверсий.
        </>
      }
      confirmLabel="Развернуть"
      onConfirm={async () => {
        await api.deploy(version.id);
        onDone();
      }}
    />
  );
}

/** Окно мультиверсий: active + deprecated и сколько дедиков каждой ещё живо. */
function WindowCard({
  active,
  deprecated,
  liveByVersion,
}: {
  active?: VersionInfo;
  deprecated: VersionInfo[];
  liveByVersion: Map<string, number>;
}) {
  const rows: { v: VersionInfo; role: string }[] = [];
  if (active !== undefined) rows.push({ v: active, role: 'active' });
  for (const d of deprecated) rows.push({ v: d, role: 'deprecated' });
  return (
    <div className="rounded-lg border border-line bg-paper p-3">
      <div className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">Окно мультиверсий</div>
      {rows.length === 0 ? (
        <p className="text-sm text-muted">Нет активной версии.</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {rows.map(({ v, role }) => (
            <li key={v.id} className="flex items-center justify-between gap-3">
              <span className="flex items-center gap-2">
                <StateBadge state={role} tone={toneOfVersionState(role)} />
                <span className="font-mono text-sm">{v.semver}</span>
              </span>
              <span className="tabular font-mono text-xs text-muted">{liveByVersion.get(v.id) ?? 0} дедиков</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** Активная версия по регионам — выведено из живых дедиков (GET /v1/fleets в
 *  API нет; TODO — точный per-region active_version из флит-конфигов). */
function RegionActiveCard({
  regionActive,
  activeSemver,
}: {
  regionActive: { region: string; semver: string; count: number; extra: number }[];
  activeSemver?: string;
}) {
  return (
    <div className="rounded-lg border border-line bg-paper p-3">
      <div className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">Активно по регионам</div>
      {regionActive.length === 0 ? (
        <p className="text-sm text-muted">Живых дедиков нет.</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {regionActive.map((r) => (
            <li key={r.region} className="flex items-center justify-between gap-3">
              <span className="font-mono text-sm">{r.region}</span>
              <span className="flex items-center gap-2">
                <span className={`font-mono text-sm ${r.semver === activeSemver ? 'text-good' : 'text-warn'}`}>
                  {r.semver}
                </span>
                {r.extra > 0 && <span className="font-mono text-xs text-muted">+{r.extra}</span>}
                <span className="tabular font-mono text-xs text-muted">{r.count} дедиков</span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** Прогресс pre-pull по тачкам: спрогретые ноды из событий deploy_node_pulled
 *  + remaining из последнего события; total = sprogreto + remaining, либо
 *  оценка по живым нодам, пока событий нет. */
function PrepullBar({ version, progress, nodes }: { version: VersionInfo; progress?: DeployProgress; nodes: NodeInfo[] }) {
  const pulled = progress?.pulled.size ?? 0;
  const estimate = nodes.filter(
    (n) => n.state === 'active' && n.last_heartbeat_at !== undefined && Date.now() - new Date(n.last_heartbeat_at).getTime() < HEARTBEAT_FRESH_MS,
  ).length;
  const total = progress?.remaining != null ? pulled + progress.remaining : Math.max(pulled, estimate);
  const ratio = total > 0 ? Math.min(1, pulled / total) : 0;

  return (
    <div className="border-b border-line bg-warn-bg/40 px-4 py-3">
      <div className="mb-2 flex items-center justify-between gap-3">
        <span className="text-sm">
          Прогрев <span className="font-mono font-medium">{version.semver}</span> (pre-pull)
        </span>
        <span className="tabular font-mono text-xs text-muted">
          {pulled} / {total > 0 ? total : '?'} тачек
        </span>
      </div>
      <span className="block h-1.5 w-full overflow-hidden rounded-full bg-line" aria-hidden>
        <span className="block h-full rounded-full bg-warn transition-all" style={{ width: `${String(Math.round(ratio * 100))}%` }} />
      </span>
      {pulled > 0 && (
        <div className="mt-2 flex flex-wrap gap-1">
          {[...(progress?.pulled ?? [])].map((nid) => (
            <span key={nid} className="rounded border border-line bg-card px-1.5 py-0.5 font-mono text-[11px] text-muted">
              {shortId(nid)}
            </span>
          ))}
        </div>
      )}
      {progress === undefined && (
        <p className="mt-2 text-xs text-muted">
          Ждём отчёты о прогреве по тачкам (события deploy_node_pulled)…
        </p>
      )}
    </div>
  );
}

// --- helpers ---

interface DeployProgress {
  pulled: Set<string>;
  remaining: number | null;
}

/**
 * Live-трекер pre-pull по версиям: seed из /v1/events + обновление по SSE.
 * deploy_started обнуляет прогресс версии, deploy_node_pulled добавляет тачку
 * и запоминает remaining, activated/failed/rolled_back закрывают прогресс.
 */
function useDeployProgress(): Map<string, DeployProgress> {
  const { subscribe } = useLive();
  const [map, setMap] = useState<Map<string, DeployProgress>>(new Map());

  const apply = useCallback((e: ApiEvent, m: Map<string, DeployProgress>) => {
    const vid = e.version_id;
    if (vid === undefined) return;
    switch (e.kind) {
      case 'deploy_started':
        m.set(vid, { pulled: new Set(), remaining: null });
        break;
      case 'deploy_node_pulled': {
        const cur = m.get(vid) ?? { pulled: new Set<string>(), remaining: null };
        if (e.node_id !== undefined) cur.pulled.add(e.node_id);
        const rem = e.payload.remaining;
        cur.remaining = typeof rem === 'number' ? rem : cur.remaining;
        m.set(vid, cur);
        break;
      }
      case 'deploy_activated':
      case 'deploy_failed':
      case 'deploy_rolled_back':
        m.delete(vid);
        break;
    }
  }, []);

  useEffect(() => {
    let cancelled = false;
    api
      .listEvents(1000)
      .then((list) => {
        if (cancelled) return;
        const m = new Map<string, DeployProgress>();
        for (const e of [...list].reverse()) apply(e, m); // oldest → newest
        setMap(m);
      })
      .catch(() => {
        /* прогресс просто не сидируется */
      });
    return () => {
      cancelled = true;
    };
  }, [apply]);

  useEffect(
    () =>
      subscribe((e) => {
        setMap((prev) => {
          const m = new Map(prev);
          apply(e.event, m);
          return m;
        });
      }),
    [subscribe, apply],
  );

  return map;
}

function countLiveByVersion(servers: GameServer[]): Map<string, number> {
  const m = new Map<string, number>();
  for (const s of servers) {
    if (LIVE_SERVER_STATES.has(s.state)) m.set(s.version_id, (m.get(s.version_id) ?? 0) + 1);
  }
  return m;
}

function groupByProject(versions: VersionInfo[]): { project: string; versions: VersionInfo[] }[] {
  const m = new Map<string, VersionInfo[]>();
  for (const v of versions) {
    const list = m.get(v.project) ?? [];
    list.push(v);
    m.set(v.project, list);
  }
  return [...m.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([project, vs]) => ({ project, versions: vs }));
}

/** Активная версия по регионам из живых дедиков: доминирующий semver + сколько ещё версий рядом. */
function deriveRegionActive(
  servers: GameServer[],
  versions: VersionInfo[],
): { region: string; semver: string; count: number; extra: number }[] {
  const semverOf = new Map(versions.map((v) => [v.id, v.semver]));
  const byRegion = new Map<string, Map<string, number>>();
  for (const s of servers) {
    if (!LIVE_SERVER_STATES.has(s.state)) continue;
    const sv = semverOf.get(s.version_id);
    if (sv === undefined) continue;
    const byVer = byRegion.get(s.region) ?? new Map<string, number>();
    byVer.set(sv, (byVer.get(sv) ?? 0) + 1);
    byRegion.set(s.region, byVer);
  }
  return [...byRegion.entries()]
    .map(([region, byVer]) => {
      const sorted = [...byVer.entries()].sort((a, b) => b[1] - a[1]);
      const total = sorted.reduce((acc, [, n]) => acc + n, 0);
      return { region, semver: sorted[0][0], count: total, extra: byVer.size - 1 };
    })
    .sort((a, b) => a.region.localeCompare(b.region));
}
