// События: полная лента GET /v1/events с фильтрами (kind, node, период) и
// пагинацией + live-prepend через SSE. У /v1/events серверных фильтров и
// offset нет (только limit) — фильтры и пагинация клиентские поверх окна,
// размер окна регулируется селектором «окно» (TODO: серверные фильтры/
// keyset-пагинация, panel.md §3).

import { useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { api } from '../lib/api';
import type { ApiEvent, NodeInfo } from '../lib/api';
import { useData, useLive } from '../lib/live';
import { useServerDrawer } from '../lib/drawer';
import { formatStamp, shortId, summarizePayload } from '../lib/format';
import { StateBadge, toneOfEventKind } from '../components/Badge';
import { Card, CardHeader, EmptyState, ErrorNote, LoadingRow } from '../components/ui';

const PAGE_SIZE = 50;
const WINDOWS = [200, 500, 1000];
const PERIODS: { value: string; label: string; ms: number }[] = [
  { value: 'all', label: 'всё время', ms: 0 },
  { value: '15m', label: '15 минут', ms: 15 * 60_000 },
  { value: '1h', label: '1 час', ms: 3_600_000 },
  { value: '24h', label: '24 часа', ms: 86_400_000 },
];

// Известные kind'ы (models.go master) — на случай пустой ленты дропдаун не пуст.
const KNOWN_KINDS = [
  'node_created', 'node_quarantine', 'node_recovered', 'node_drain', 'node_undrain',
  'server_failed', 'server_recovered', 'crash_loop', 'allocation_failed',
  'version_registered', 'fleet_updated', 'deploy_started', 'deploy_node_pulled',
  'deploy_activated', 'deploy_failed', 'deploy_rolled_back', 'version_disabled',
  'server_drain', 'agent_upgrade', 'agent_upgrade_succeeded', 'agent_upgrade_failed',
];

export function Events() {
  const { subscribe } = useLive();
  const nodes = useData(() => api.listNodes(), []);

  const [limit, setLimit] = useState(500);
  const [reloadKey, setReloadKey] = useState(0);
  const [all, setAll] = useState<ApiEvent[] | null>(null);
  const [failed, setFailed] = useState(false);

  const [kind, setKind] = useState('');
  const [node, setNode] = useState('');
  const [period, setPeriod] = useState('all');
  const [page, setPage] = useState(0);

  // Загрузка окна ленты (перезагрузка при смене размера окна или по «Повторить»).
  useEffect(() => {
    let cancelled = false;
    setAll(null);
    setFailed(false);
    api
      .listEvents(limit)
      .then((list) => {
        if (!cancelled) setAll(list);
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, [limit, reloadKey]);

  // Live-prepend новых событий (dedup по id), с капом в размер окна.
  useEffect(
    () =>
      subscribe((e) => {
        setAll((prev) => {
          if (prev === null) return prev;
          if (prev.some((x) => x.id === e.id)) return prev;
          return [e.event, ...prev].slice(0, limit);
        });
      }),
    [subscribe, limit],
  );

  const kinds = useMemo(() => {
    const set = new Set(KNOWN_KINDS);
    for (const e of all ?? []) set.add(e.kind);
    return [...set].sort((a, b) => a.localeCompare(b));
  }, [all]);

  const filtered = useMemo(() => {
    const periodMs = PERIODS.find((p) => p.value === period)?.ms ?? 0;
    const cutoff = periodMs > 0 ? Date.now() - periodMs : 0;
    return (all ?? []).filter((e) => {
      if (kind !== '' && e.kind !== kind) return false;
      if (node !== '' && e.node_id !== node) return false;
      if (cutoff > 0 && new Date(e.ts).getTime() < cutoff) return false;
      return true;
    });
  }, [all, kind, node, period]);

  const pageCount = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const clampedPage = Math.min(page, pageCount - 1);
  const visible = filtered.slice(clampedPage * PAGE_SIZE, clampedPage * PAGE_SIZE + PAGE_SIZE);

  const resetPage = () => {
    setPage(0);
  };
  const select = 'rounded-lg border border-line bg-card px-2.5 py-1.5 text-xs';

  return (
    <Card>
      <CardHeader
        title="События"
        aside={
          <div className="flex flex-wrap items-center gap-2">
            <select aria-label="Фильтр по kind" className={select} value={kind} onChange={(e) => { setKind(e.target.value); resetPage(); }}>
              <option value="">все kind</option>
              {kinds.map((k) => (
                <option key={k} value={k}>{k}</option>
              ))}
            </select>
            <select aria-label="Фильтр по тачке" className={select} value={node} onChange={(e) => { setNode(e.target.value); resetPage(); }}>
              <option value="">все тачки</option>
              {(nodes.data ?? []).map((n: NodeInfo) => (
                <option key={n.id} value={n.id}>{n.hostname} · {shortId(n.id)}</option>
              ))}
            </select>
            <select aria-label="Период" className={select} value={period} onChange={(e) => { setPeriod(e.target.value); resetPage(); }}>
              {PERIODS.map((p) => (
                <option key={p.value} value={p.value}>{p.label}</option>
              ))}
            </select>
            <select aria-label="Размер окна ленты" className={select} value={limit} onChange={(e) => { setLimit(Number(e.target.value)); resetPage(); }}>
              {WINDOWS.map((n) => (
                <option key={n} value={n}>окно {n}</option>
              ))}
            </select>
          </div>
        }
      />
      {failed && all === null ? (
        <div className="p-4">
          <ErrorNote error={new Error('лента событий недоступна')} retry={() => { setReloadKey((k) => k + 1); }} />
        </div>
      ) : all === null ? (
        <LoadingRow />
      ) : visible.length === 0 ? (
        <EmptyState>Под эти фильтры событий нет.</EmptyState>
      ) : (
        <>
          <EventRows events={visible} />
          <footer className="flex items-center justify-between border-t border-line px-4 py-2.5">
            <span className="tabular font-mono text-xs text-muted">
              {filtered.length} событий{filtered.length !== (all.length) ? ` из ${all.length}` : ''} · стр. {clampedPage + 1}/{pageCount}
            </span>
            <div className="flex gap-2">
              <PagerButton disabled={clampedPage === 0} onClick={() => { setPage(Math.max(0, clampedPage - 1)); }}>← Новее</PagerButton>
              <PagerButton disabled={clampedPage >= pageCount - 1} onClick={() => { setPage(clampedPage + 1); }}>Старше →</PagerButton>
            </div>
          </footer>
        </>
      )}
    </Card>
  );
}

function EventRows({ events }: { events: ApiEvent[] }) {
  const { open } = useServerDrawer();
  return (
    <ul className="divide-y divide-line">
      {events.map((e) => (
        <li key={e.id} className="flex items-start gap-3 px-4 py-2.5">
          <span className="tabular shrink-0 pt-0.5 font-mono text-xs text-muted">{formatStamp(e.ts)}</span>
          <StateBadge state={e.kind} tone={toneOfEventKind(e.kind)} />
          <span className="min-w-0 flex-1 pt-0.5 text-xs">
            <span className="text-muted">{refsOf(e, open)}</span>
            {Object.keys(e.payload).length > 0 && <span className="text-ink/80">{summarizePayload(e.payload)}</span>}
          </span>
        </li>
      ))}
    </ul>
  );
}

function refsOf(e: ApiEvent, openServer: (id: string) => void): ReactNode {
  const parts: ReactNode[] = [];
  if (e.node_id !== undefined) parts.push(<span key="n" className="font-mono">node {shortId(e.node_id)}</span>);
  if (e.server_id !== undefined) {
    const sid = e.server_id;
    parts.push(
      <button
        key="s"
        type="button"
        onClick={() => { openServer(sid); }}
        className="font-mono text-accent-ink underline-offset-2 hover:underline"
      >
        srv {shortId(sid)}
      </button>,
    );
  }
  if (e.match_id !== undefined) parts.push(<span key="m" className="font-mono">match {shortId(e.match_id)}</span>);
  if (parts.length === 0) return null;
  return (
    <>
      {parts.map((p, i) => (
        <span key={i}>{i > 0 ? ' · ' : ''}{p}</span>
      ))}
      {' · '}
    </>
  );
}

function PagerButton({ disabled, onClick, children }: { disabled: boolean; onClick: () => void; children: string }) {
  return (
    <button
      type="button"
      disabled={disabled}
      onClick={onClick}
      className="rounded-lg border border-line px-2.5 py-1 text-xs text-muted transition-colors hover:text-ink disabled:opacity-40 disabled:hover:text-muted"
    >
      {children}
    </button>
  );
}
