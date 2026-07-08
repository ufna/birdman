// Флот: таблица тачек с живым heartbeat age; раскрытие строки — дедики
// этой тачки (state, версия, игроки, порт, аптайм).

import { useMemo, useState } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import { api } from '../lib/api';
import type { ApiEvent, GameServer, NodeInfo } from '../lib/api';
import { useData } from '../lib/live';
import { canAdmin, useSession } from '../lib/session';
import { useServerDrawer } from '../lib/drawer';
import { useNow } from '../lib/useNow';
import { ageOf, heartbeatTone, shortId } from '../lib/format';
import { useT, useFormat } from '../lib/i18n';
import { DataTable } from '../components/DataTable';
import { StateBadge, toneOfNodeState, toneOfServerState } from '../components/Badge';
import { ConfirmButton } from '../components/ConfirmDialog';
import { Card, CardHeader, ErrorNote, LoadingRow } from '../components/ui';

const LIVE_SERVER_STATES = new Set(['creating', 'ready', 'allocated', 'draining']);

export function Fleet() {
  const { t, tp } = useT();
  const nodes = useData(() => api.listNodes(), []);
  const servers = useData(() => api.listServers(), []);
  const versions = useData(() => api.listVersions(), []);
  // Причина карантина — из последнего события node_quarantine ноды
  // (server-side фильтра по node_id у /v1/events нет — фильтруем клиентом).
  const events = useData(() => api.listEvents(500), []);
  const { session } = useSession();
  const mayAdmin = session != null && canAdmin(session);
  const [expandedId, setExpandedId] = useState<string | null>(null);

  const semverOf = useMemo(
    () => new Map((versions.data ?? []).map((v) => [v.id, v.semver])),
    [versions.data],
  );
  const quarantineReason = useMemo(() => latestQuarantineReasons(events.data ?? []), [events.data]);
  const serversByNode = useMemo(() => {
    const m = new Map<string, GameServer[]>();
    for (const s of servers.data ?? []) {
      const list = m.get(s.node_id) ?? [];
      list.push(s);
      m.set(s.node_id, list);
    }
    for (const list of m.values()) list.sort((a, b) => a.port - b.port);
    return m;
  }, [servers.data]);

  const sortedNodes = useMemo(
    () =>
      [...(nodes.data ?? [])].sort(
        (a, b) => a.region.localeCompare(b.region) || a.hostname.localeCompare(b.hostname),
      ),
    [nodes.data],
  );

  const columns = useMemo<ColumnDef<NodeInfo, unknown>[]>(
    () => [
      {
        id: 'host',
        header: t('col.node'),
        cell: ({ row }) => (
          <div className="min-w-36">
            <div className="font-medium">{row.original.hostname}</div>
            <div className="font-mono text-xs text-muted">
              {shortId(row.original.id)} · {row.original.public_ip}
            </div>
          </div>
        ),
      },
      { id: 'region', header: t('col.region'), cell: ({ row }) => <span className="font-mono text-xs">{row.original.region}</span> },
      {
        id: 'state',
        header: t('col.state'),
        cell: ({ row }) => (
          <div className="flex flex-col gap-1">
            <StateBadge state={row.original.state} tone={toneOfNodeState(row.original.state)} domain="node" />
            {row.original.state === 'quarantine' && quarantineReason.get(row.original.id) !== undefined && (
              <span className="max-w-40 truncate text-[11px] text-dead" title={quarantineReason.get(row.original.id)}>
                {quarantineReason.get(row.original.id)}
              </span>
            )}
            {row.original.state === 'draining' && (
              <DrainProgress
                playing={(serversByNode.get(row.original.id) ?? []).filter((s) => s.state === 'allocated').length}
              />
            )}
          </div>
        ),
      },
      {
        id: 'slots',
        header: t('col.slots'),
        cell: ({ row }) => (
          <SlotsCell
            busy={(serversByNode.get(row.original.id) ?? []).filter((s) => LIVE_SERVER_STATES.has(s.state)).length}
            total={row.original.capacity_slots}
          />
        ),
      },
      {
        id: 'heartbeat',
        header: t('col.heartbeat'),
        cell: ({ row }) => <HeartbeatCell iso={row.original.last_heartbeat_at} />,
      },
      {
        id: 'agent',
        header: t('col.agent'),
        cell: ({ row }) => (
          <span className="font-mono text-xs text-muted">
            {row.original.agent_version !== '' ? row.original.agent_version : '—'}
          </span>
        ),
      },
      {
        id: 'actions',
        header: '',
        cell: ({ row }) =>
          mayAdmin ? <NodeActions node={row.original} onDone={nodes.reload} /> : null,
      },
      {
        id: 'chevron',
        header: '',
        cell: ({ row }) => (
          <svg
            viewBox="0 0 16 16"
            aria-hidden
            className={`size-3.5 text-muted transition-transform ${expandedId === row.original.id ? 'rotate-90' : ''}`}
          >
            <path d="M6 4l4 4-4 4" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
          </svg>
        ),
      },
    ],
    [t, serversByNode, expandedId, quarantineReason, mayAdmin, nodes.reload],
  );

  const error = nodes.error ?? servers.error ?? versions.error;
  if (error !== undefined && nodes.data === undefined) {
    return <ErrorNote error={error} retry={nodes.reload} />;
  }

  return (
    <Card>
      <CardHeader
        title={t('nav.fleet')}
        aside={<span className="font-mono text-xs text-muted">{tp('fleet.nodesCount', sortedNodes.length)}</span>}
      />
      {nodes.data === undefined ? (
        <LoadingRow />
      ) : (
        <DataTable
          columns={columns}
          data={sortedNodes}
          rowId={(n) => n.id}
          empty={
            <>
              {t('fleet.emptyPre')} <span className="font-mono">POST /v1/nodes</span>
            </>
          }
          expandedId={expandedId}
          onRowClick={(n) => {
            setExpandedId((cur) => (cur === n.id ? null : n.id));
          }}
          rowLabel={(n) => t('fleet.toggleNode', { host: n.hostname })}
          renderExpanded={(n) => (
            <NodeServers servers={serversByNode.get(n.id) ?? []} semverOf={semverOf} />
          )}
        />
      )}
    </Card>
  );
}

function SlotsCell({ busy, total }: { busy: number; total: number }) {
  const ratio = total > 0 ? Math.min(1, busy / total) : 0;
  return (
    <div className="flex items-center gap-2">
      <span className="tabular font-mono text-xs">
        {busy}/{total}
      </span>
      <span className="h-1.5 w-16 overflow-hidden rounded-full bg-line" aria-hidden>
        <span
          className={`block h-full rounded-full ${ratio >= 1 ? 'bg-warn' : 'bg-accent'}`}
          style={{ width: `${String(Math.round(ratio * 100))}%` }}
        />
      </span>
    </div>
  );
}

/** Drain/Undrain тачки (admin, confirm). stopPropagation — чтобы клик по
 *  кнопке не разворачивал строку. */
function NodeActions({ node, onDone }: { node: NodeInfo; onDone: () => void }) {
  const { t } = useT();
  if (node.state === 'dead') return null;
  const draining = node.state === 'draining';
  return (
    <div
      onClick={(e) => {
        e.stopPropagation();
      }}
      className="flex justify-end"
    >
      {draining ? (
        <ConfirmButton
          label={t('fleet.undrain')}
          title={t('fleet.undrain.title', { host: node.hostname })}
          description={t('fleet.undrain.desc')}
          confirmLabel={t('fleet.undrain')}
          onConfirm={async () => {
            await api.undrainNode(node.id);
            onDone();
          }}
        />
      ) : (
        <ConfirmButton
          label={t('fleet.drain')}
          tone="dead"
          title={t('fleet.drain.title', { host: node.hostname })}
          description={t('fleet.drain.desc')}
          confirmLabel={t('fleet.drain')}
          onConfirm={async () => {
            await api.drainNode(node.id);
            onDone();
          }}
        />
      )}
    </div>
  );
}

/** Индикация опустошения draining-ноды: сколько allocated ещё доигрывает. */
function DrainProgress({ playing }: { playing: number }) {
  const { t, tp } = useT();
  return (
    <span className="font-mono text-[11px] text-warn">
      {playing > 0 ? tp('fleet.drain.playing', playing) : t('fleet.drain.empty')}
    </span>
  );
}

/** node_id → причина последнего карантина (payload.reason события node_quarantine). */
function latestQuarantineReasons(events: ApiEvent[]): Map<string, string> {
  const m = new Map<string, string>();
  // events отсортированы новыми вперёд — первое совпадение и есть последнее.
  for (const e of events) {
    if (e.kind !== 'node_quarantine' || e.node_id === undefined || m.has(e.node_id)) continue;
    const reason = e.payload.reason ?? e.payload.detail;
    if (typeof reason === 'string') m.set(e.node_id, reason);
  }
  return m;
}

/** Живой возраст heartbeat: тикает каждую секунду, тон — по свежести. */
export function HeartbeatCell({ iso }: { iso?: string }) {
  const now = useNow();
  const { t } = useT();
  const fmt = useFormat();
  const age = ageOf(iso, now);
  const tone = heartbeatTone(age);
  const toneText = { good: 'text-good', warn: 'text-warn', dead: 'text-dead' }[tone];
  return (
    <span className={`inline-flex items-center gap-1.5 font-mono text-xs ${toneText}`}>
      <span aria-hidden className="size-1.5 rounded-full bg-current" />
      <span className="tabular">{age === null ? t('fleet.hb.never') : fmt.ago(age)}</span>
    </span>
  );
}

function NodeServers({ servers, semverOf }: { servers: GameServer[]; semverOf: Map<string, string> }) {
  const now = useNow();
  const { open } = useServerDrawer();
  const { t } = useT();
  const fmt = useFormat();
  if (servers.length === 0) {
    return <p className="py-2 text-xs text-muted">{t('fleet.node.noServers')}</p>;
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-line bg-card">
      <table className="w-full min-w-[560px] text-xs">
        <thead>
          <tr className="border-b border-line text-muted">
            <th className="px-3 py-2 text-left font-medium">{t('col.dedic')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.state')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.version')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.players')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.port')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.uptime')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.match')}</th>
          </tr>
        </thead>
        <tbody>
          {servers.map((s) => (
            <tr
              key={s.id}
              onClick={() => {
                open(s.id);
              }}
              className="cursor-pointer border-b border-line transition-colors last:border-0 hover:bg-paper"
              title={t('server.openDetailsFull')}
            >
              <td className="px-3 py-2 font-mono text-accent-ink underline-offset-2 hover:underline">{shortId(s.id)}</td>
              <td className="px-3 py-2">
                <StateBadge state={s.state} tone={toneOfServerState(s.state)} domain="server" />
              </td>
              <td className="px-3 py-2 font-mono">{semverOf.get(s.version_id) ?? shortId(s.version_id)}</td>
              <td className="tabular px-3 py-2 font-mono">{s.players}</td>
              <td className="tabular px-3 py-2 font-mono">{s.port}</td>
              <td className="tabular px-3 py-2 font-mono">{fmt.age(Math.max(0, now - new Date(s.created_at).getTime()))}</td>
              <td className="px-3 py-2 font-mono text-muted">
                {s.match_id !== undefined ? shortId(s.match_id) : '—'}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
