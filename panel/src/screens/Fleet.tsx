// Флот: таблица тачек с живым heartbeat age; раскрытие строки — дедики
// этой тачки (state, версия, игроки, порт, аптайм).

import { useMemo, useState } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import { api } from '../lib/api';
import type { GameServer, NodeInfo } from '../lib/api';
import { useData } from '../lib/live';
import { useNow } from '../lib/useNow';
import { ageOf, formatAge, heartbeatTone, shortId } from '../lib/format';
import { DataTable } from '../components/DataTable';
import { StateBadge, toneOfNodeState, toneOfServerState } from '../components/Badge';
import { Card, CardHeader, ErrorNote, LoadingRow } from '../components/ui';

const LIVE_SERVER_STATES = new Set(['creating', 'ready', 'allocated', 'draining']);

export function Fleet() {
  const nodes = useData(() => api.listNodes(), []);
  const servers = useData(() => api.listServers(), []);
  const versions = useData(() => api.listVersions(), []);
  const [expandedId, setExpandedId] = useState<string | null>(null);

  const semverOf = useMemo(
    () => new Map((versions.data ?? []).map((v) => [v.id, v.semver])),
    [versions.data],
  );
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
        header: 'Тачка',
        cell: ({ row }) => (
          <div className="min-w-36">
            <div className="font-medium">{row.original.hostname}</div>
            <div className="font-mono text-xs text-muted">
              {shortId(row.original.id)} · {row.original.public_ip}
            </div>
          </div>
        ),
      },
      { id: 'region', header: 'Регион', cell: ({ row }) => <span className="font-mono text-xs">{row.original.region}</span> },
      {
        id: 'state',
        header: 'Состояние',
        cell: ({ row }) => <StateBadge state={row.original.state} tone={toneOfNodeState(row.original.state)} />,
      },
      {
        id: 'slots',
        header: 'Слоты',
        cell: ({ row }) => (
          <SlotsCell
            busy={(serversByNode.get(row.original.id) ?? []).filter((s) => LIVE_SERVER_STATES.has(s.state)).length}
            total={row.original.capacity_slots}
          />
        ),
      },
      {
        id: 'heartbeat',
        header: 'Heartbeat',
        cell: ({ row }) => <HeartbeatCell iso={row.original.last_heartbeat_at} />,
      },
      {
        id: 'agent',
        header: 'Агент',
        cell: ({ row }) => (
          <span className="font-mono text-xs text-muted">
            {row.original.agent_version !== '' ? row.original.agent_version : '—'}
          </span>
        ),
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
    [serversByNode, expandedId],
  );

  const error = nodes.error ?? servers.error ?? versions.error;
  if (error !== undefined && nodes.data === undefined) {
    return <ErrorNote error={error} retry={nodes.reload} />;
  }

  return (
    <Card>
      <CardHeader
        title="Флот"
        aside={<span className="font-mono text-xs text-muted">{sortedNodes.length} тачек</span>}
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
              Тачек пока нет. Зарегистрируйте первую: <span className="font-mono">POST /v1/nodes</span>.
            </>
          }
          expandedId={expandedId}
          onRowClick={(n) => {
            setExpandedId((cur) => (cur === n.id ? null : n.id));
          }}
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

/** Живой возраст heartbeat: тикает каждую секунду, тон — по свежести. */
export function HeartbeatCell({ iso }: { iso?: string }) {
  const now = useNow();
  const age = ageOf(iso, now);
  const tone = heartbeatTone(age);
  const toneText = { good: 'text-good', warn: 'text-warn', dead: 'text-dead' }[tone];
  return (
    <span className={`inline-flex items-center gap-1.5 font-mono text-xs ${toneText}`}>
      <span aria-hidden className="size-1.5 rounded-full bg-current" />
      <span className="tabular">{age === null ? 'не был' : `${formatAge(age)} назад`}</span>
    </span>
  );
}

function NodeServers({ servers, semverOf }: { servers: GameServer[]; semverOf: Map<string, string> }) {
  const now = useNow();
  if (servers.length === 0) {
    return <p className="py-2 text-xs text-muted">На тачке нет дедиков.</p>;
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-line bg-card">
      <table className="w-full min-w-[560px] text-xs">
        <thead>
          <tr className="border-b border-line text-muted">
            <th className="px-3 py-2 text-left font-medium">Дедик</th>
            <th className="px-3 py-2 text-left font-medium">Состояние</th>
            <th className="px-3 py-2 text-left font-medium">Версия</th>
            <th className="px-3 py-2 text-left font-medium">Игроки</th>
            <th className="px-3 py-2 text-left font-medium">Порт</th>
            <th className="px-3 py-2 text-left font-medium">Аптайм</th>
            <th className="px-3 py-2 text-left font-medium">Матч</th>
          </tr>
        </thead>
        <tbody>
          {servers.map((s) => (
            <tr key={s.id} className="border-b border-line last:border-0">
              <td className="px-3 py-2 font-mono">{shortId(s.id)}</td>
              <td className="px-3 py-2">
                <StateBadge state={s.state} tone={toneOfServerState(s.state)} />
              </td>
              <td className="px-3 py-2 font-mono">{semverOf.get(s.version_id) ?? shortId(s.version_id)}</td>
              <td className="tabular px-3 py-2 font-mono">{s.players}</td>
              <td className="tabular px-3 py-2 font-mono">{s.port}</td>
              <td className="tabular px-3 py-2 font-mono">{formatAge(Math.max(0, now - new Date(s.created_at).getTime()))}</td>
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
