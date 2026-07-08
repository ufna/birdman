// Детали дедика (дровер справа): стейт-таймлайн из событий, live-логи +
// скачивание, графики метрик через metrics-proxy. Открывается по клику на
// дедик во Флоте/Матчах (lib/drawer.tsx). Всё — на публичном API:
// GET /v1/servers (клиентский поиск по id — отдельного GET /v1/servers/{id}
// в master нет), /v1/events (клиентский фильтр по server_id), /v1/servers/
// {id}/logs, metrics-proxy.

import { useEffect, useMemo, useState } from 'react';
import * as Dialog from '@radix-ui/react-dialog';
import * as Tabs from '@radix-ui/react-tabs';
import { api } from '../lib/api';
import type { ApiEvent, GameServer } from '../lib/api';
import { useData, useLive } from '../lib/live';
import { useNow } from '../lib/useNow';
import { serverMetricQueries } from '../lib/metrics';
import { formatAge, formatClock, shortId, summarizePayload } from '../lib/format';
import { StateBadge, toneOfEventKind, toneOfServerState } from './Badge';
import { MetricChart } from './MetricChart';
import { LogViewer } from './LogViewer';
import { EmptyState, LoadingRow } from './ui';

export function ServerDrawer({ serverId, onClose }: { serverId: string | null; onClose: () => void }) {
  return (
    <Dialog.Root
      open={serverId !== null}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed top-0 right-0 z-50 flex h-screen w-[min(42rem,100vw)] flex-col border-l border-line bg-card shadow-2xl focus:outline-none"
        >
          {serverId !== null ? (
            <DrawerBody serverId={serverId} />
          ) : (
            <Dialog.Title className="sr-only">Дедик</Dialog.Title>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function DrawerBody({ serverId }: { serverId: string }) {
  const servers = useData(() => api.listServers(), []);
  const versions = useData(() => api.listVersions(), []);
  const now = useNow();

  const server = useMemo(
    () => (servers.data ?? []).find((s) => s.id === serverId),
    [servers.data, serverId],
  );
  const semver = useMemo(() => {
    if (server === undefined) return undefined;
    return (versions.data ?? []).find((v) => v.id === server.version_id)?.semver;
  }, [versions.data, server]);

  const queries = useMemo(() => serverMetricQueries(serverId), [serverId]);

  return (
    <>
      <header className="flex items-start justify-between gap-3 border-b border-line px-5 py-4">
        <div className="min-w-0">
          <Dialog.Title className="flex items-center gap-2 text-base font-semibold">
            <span className="font-mono">Дедик {shortId(serverId)}</span>
            {server !== undefined && <StateBadge state={server.state} tone={toneOfServerState(server.state)} />}
          </Dialog.Title>
          <p className="mt-1 truncate font-mono text-xs text-muted">
            {server !== undefined ? <ServerMeta server={server} semver={semver} now={now} /> : 'нет в текущем списке дедиков (возможно, reaped) — логи и метрики доступны'}
          </p>
        </div>
        <Dialog.Close asChild>
          <button
            type="button"
            aria-label="Закрыть"
            className="shrink-0 rounded-lg border border-line p-1.5 text-muted hover:text-ink"
          >
            <svg viewBox="0 0 16 16" className="size-4" aria-hidden>
              <path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
            </svg>
          </button>
        </Dialog.Close>
      </header>

      <Tabs.Root defaultValue="timeline" className="flex min-h-0 flex-1 flex-col">
        <Tabs.List className="flex gap-1 border-b border-line px-5 py-2" aria-label="Разделы дедика">
          {[
            { v: 'timeline', l: 'Таймлайн' },
            { v: 'logs', l: 'Логи' },
            { v: 'metrics', l: 'Метрики' },
          ].map((t) => (
            <Tabs.Trigger
              key={t.v}
              value={t.v}
              className="rounded-md px-3 py-1 text-sm text-muted transition-colors data-[state=active]:bg-mark data-[state=active]:font-medium data-[state=active]:text-accent-ink"
            >
              {t.l}
            </Tabs.Trigger>
          ))}
        </Tabs.List>

        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
          <Tabs.Content value="timeline">
            <ServerTimeline serverId={serverId} />
          </Tabs.Content>
          <Tabs.Content value="logs">
            <LogViewer serverId={serverId} />
          </Tabs.Content>
          <Tabs.Content value="metrics">
            <p className="mb-3 text-xs text-muted">
              Метрики этого дедика из VictoriaMetrics через master-proxy. Окно 30 минут, обновление каждые 15с.
            </p>
            <div className="grid gap-3 sm:grid-cols-2">
              {queries.map((q) => (
                <MetricChart key={q.key} query={q.expr} title={q.title} unit={q.unit} />
              ))}
            </div>
          </Tabs.Content>
        </div>
      </Tabs.Root>
    </>
  );
}

function ServerMeta({ server, semver, now }: { server: GameServer; semver?: string; now: number }) {
  const uptime = formatAge(Math.max(0, now - new Date(server.created_at).getTime()));
  return (
    <>
      node {shortId(server.node_id)} · {server.region} · v{semver ?? shortId(server.version_id)} · порт {server.port} ·{' '}
      {server.players} игроков · аптайм {uptime}
    </>
  );
}

const TIMELINE_CAP = 100;

/** Стейт-таймлайн дедика: события из /v1/events с client-фильтром по server_id
 *  (server-side фильтра по server_id в API нет) + live-дополнение из SSE. */
function ServerTimeline({ serverId }: { serverId: string }) {
  const { subscribe } = useLive();
  const [events, setEvents] = useState<ApiEvent[] | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setEvents(null);
    setFailed(false);
    // Тянем широкое окно и фильтруем клиентом — kind/server_id фильтров у
    // /v1/events нет (TODO: серверные фильтры, panel.md §3).
    api
      .listEvents(1000)
      .then((list) => {
        if (!cancelled) setEvents(list.filter((e) => e.server_id === serverId));
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, [serverId]);

  useEffect(
    () =>
      subscribe((e) => {
        if (e.event.server_id !== serverId) return;
        setEvents((prev) => {
          const base = prev ?? [];
          if (base.some((x) => x.id === e.id)) return base;
          return [e.event, ...base].slice(0, TIMELINE_CAP);
        });
      }),
    [subscribe, serverId],
  );

  if (failed) return <EmptyState>Лента событий недоступна.</EmptyState>;
  if (events === null) return <LoadingRow />;
  if (events.length === 0) return <EmptyState>Событий по этому дедику пока нет.</EmptyState>;

  return (
    <ol className="relative ml-2 border-l border-line">
      {events.map((e) => (
        <li key={e.id} className="relative flex items-start gap-3 py-2 pl-5">
          <span aria-hidden className="absolute top-3.5 -left-[5px] size-2 rounded-full bg-accent" />
          <span className="tabular shrink-0 pt-0.5 font-mono text-xs text-muted">{formatClock(e.ts)}</span>
          <StateBadge state={e.kind} tone={toneOfEventKind(e.kind)} />
          {Object.keys(e.payload).length > 0 && (
            <span className="min-w-0 flex-1 truncate pt-0.5 text-xs text-muted">{summarizePayload(e.payload)}</span>
          )}
        </li>
      ))}
    </ol>
  );
}
