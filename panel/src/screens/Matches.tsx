// Матчи: Live (running + pending — pending уже держит дедик, running
// появится с liba) и История с фильтрами state/region и пагинацией
// limit/offset.

import { useMemo, useState } from 'react';
import * as Tabs from '@radix-ui/react-tabs';
import type { ColumnDef } from '@tanstack/react-table';
import { api } from '../lib/api';
import type { Match, MatchState } from '../lib/api';
import { useData } from '../lib/live';
import { useServerDrawer } from '../lib/drawer';
import { useNow } from '../lib/useNow';
import { ageOf, formatAge, formatStamp, shortId } from '../lib/format';
import { DataTable } from '../components/DataTable';
import { StateBadge, toneOfMatchState } from '../components/Badge';
import { Card, CardHeader, ErrorNote, LoadingRow } from '../components/ui';

const PAGE_SIZE = 50;

export function Matches() {
  return (
    <Tabs.Root defaultValue="live" className="flex flex-col gap-4">
      <Tabs.List className="flex w-fit gap-1 rounded-lg border border-line bg-card p-1" aria-label="Матчи">
        {(['live', 'history'] as const).map((tab) => (
          <Tabs.Trigger
            key={tab}
            value={tab}
            className="rounded-md px-3 py-1 text-sm text-muted transition-colors data-[state=active]:bg-mark data-[state=active]:font-medium data-[state=active]:text-accent-ink"
          >
            {tab === 'live' ? 'Live' : 'История'}
          </Tabs.Trigger>
        ))}
      </Tabs.List>
      <Tabs.Content value="live">
        <LiveMatches />
      </Tabs.Content>
      <Tabs.Content value="history">
        <MatchHistory />
      </Tabs.Content>
    </Tabs.Root>
  );
}

function LiveMatches() {
  const live = useData(
    () =>
      Promise.all([
        api.listMatches({ state: 'running', limit: 500 }),
        api.listMatches({ state: 'pending', limit: 500 }),
      ]).then(([running, pending]) =>
        [...running, ...pending].sort(
          (a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime(),
        ),
      ),
    [],
  );
  const columns = useLiveColumns();

  if (live.error !== undefined && live.data === undefined) {
    return <ErrorNote error={live.error} retry={live.reload} />;
  }
  return (
    <Card>
      <CardHeader
        title="Идут сейчас"
        aside={<span className="tabular font-mono text-xs text-muted">{live.data?.length ?? 0}</span>}
      />
      {live.data === undefined ? (
        <LoadingRow />
      ) : (
        <DataTable
          columns={columns}
          data={live.data}
          rowId={(m) => m.id}
          empty="Живых матчей нет. Как только матчмейкер соберёт группу — матч появится здесь без перезагрузки."
        />
      )}
    </Card>
  );
}

function useLiveColumns(): ColumnDef<Match, unknown>[] {
  const { open } = useServerDrawer();
  return useMemo(
    () => [
      idColumn(),
      regionColumn(),
      versionColumn(),
      {
        id: 'players',
        header: 'Игроки',
        cell: ({ row }) => <span className="tabular font-mono text-xs">{row.original.server_players}</span>,
      },
      {
        id: 'duration',
        header: 'Длительность',
        cell: ({ row }) => <DurationCell m={row.original} />,
      },
      {
        id: 'server',
        header: 'Сервер',
        cell: ({ row }) => (
          <div className="font-mono text-xs">
            {row.original.host}:{row.original.port}
            {row.original.server_id !== '' && (
              <>
                {' · '}
                <button
                  type="button"
                  onClick={() => {
                    open(row.original.server_id);
                  }}
                  className="text-accent-ink underline-offset-2 hover:underline"
                  title="Открыть детали дедика"
                >
                  {shortId(row.original.server_id)}
                </button>
              </>
            )}
          </div>
        ),
      },
      stateColumn(),
    ],
    [open],
  );
}

/** Тикающая длительность живого матча (от started_at, иначе от created_at). */
function DurationCell({ m }: { m: Match }) {
  const now = useNow();
  const age = ageOf(m.started_at ?? m.created_at, now);
  return <span className="tabular font-mono text-xs">{age === null ? '—' : formatAge(age)}</span>;
}

const HISTORY_STATES: { value: MatchState | ''; label: string }[] = [
  { value: '', label: 'все состояния' },
  { value: 'pending', label: 'pending' },
  { value: 'running', label: 'running' },
  { value: 'finished', label: 'finished' },
  { value: 'aborted', label: 'aborted' },
];

function MatchHistory() {
  const [state, setState] = useState<MatchState | ''>('');
  const [region, setRegion] = useState('');
  const [page, setPage] = useState(0);

  // Регионы для фильтра — из нод (авторитетный список).
  const nodes = useData(() => api.listNodes(), []);
  const regions = useMemo(
    () => [...new Set((nodes.data ?? []).map((n) => n.region))].sort((a, b) => a.localeCompare(b)),
    [nodes.data],
  );

  // limit+1 — чтобы знать, есть ли следующая страница.
  const rows = useData(
    () =>
      api.listMatches({
        state: state === '' ? undefined : state,
        region: region === '' ? undefined : region,
        limit: PAGE_SIZE + 1,
        offset: page * PAGE_SIZE,
      }),
    [state, region, page],
  );
  const hasNext = (rows.data?.length ?? 0) > PAGE_SIZE;
  const visible = rows.data?.slice(0, PAGE_SIZE) ?? [];
  const columns = useHistoryColumns();

  const select = 'rounded-lg border border-line bg-card px-2.5 py-1.5 text-xs';
  return (
    <Card>
      <CardHeader
        title="История"
        aside={
          <div className="flex items-center gap-2">
            <select
              aria-label="Фильтр по состоянию"
              className={select}
              value={state}
              onChange={(e) => {
                setState(e.target.value as MatchState | '');
                setPage(0);
              }}
            >
              {HISTORY_STATES.map((s) => (
                <option key={s.value} value={s.value}>
                  {s.label}
                </option>
              ))}
            </select>
            <select
              aria-label="Фильтр по региону"
              className={select}
              value={region}
              onChange={(e) => {
                setRegion(e.target.value);
                setPage(0);
              }}
            >
              <option value="">все регионы</option>
              {regions.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </div>
        }
      />
      {rows.error !== undefined && rows.data === undefined ? (
        <div className="p-4">
          <ErrorNote error={rows.error} retry={rows.reload} />
        </div>
      ) : rows.data === undefined ? (
        <LoadingRow />
      ) : (
        <>
          <DataTable
            columns={columns}
            data={visible}
            rowId={(m) => m.id}
            empty={page > 0 ? 'Дальше пусто — вернитесь назад.' : 'Под эти фильтры матчей нет.'}
          />
          <footer className="flex items-center justify-between border-t border-line px-4 py-2.5">
            <span className="tabular font-mono text-xs text-muted">
              {page * PAGE_SIZE + (visible.length > 0 ? 1 : 0)}–{page * PAGE_SIZE + visible.length}
            </span>
            <div className="flex gap-2">
              <PagerButton
                disabled={page === 0}
                onClick={() => {
                  setPage((p) => Math.max(0, p - 1));
                }}
              >
                ← Новее
              </PagerButton>
              <PagerButton
                disabled={!hasNext}
                onClick={() => {
                  setPage((p) => p + 1);
                }}
              >
                Старше →
              </PagerButton>
            </div>
          </footer>
        </>
      )}
    </Card>
  );
}

function PagerButton({
  disabled,
  onClick,
  children,
}: {
  disabled: boolean;
  onClick: () => void;
  children: string;
}) {
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

function useHistoryColumns(): ColumnDef<Match, unknown>[] {
  return useMemo(
    () => [
      idColumn(),
      regionColumn(),
      versionColumn(),
      stateColumn(),
      {
        id: 'peak',
        header: 'Пик игроков',
        cell: ({ row }) => <span className="tabular font-mono text-xs">{row.original.players_peak}</span>,
      },
      {
        id: 'created',
        header: 'Создан',
        cell: ({ row }) => (
          <span className="tabular font-mono text-xs text-muted">{formatStamp(row.original.created_at)}</span>
        ),
      },
      {
        id: 'took',
        header: 'Длительность',
        cell: ({ row }) => {
          const { started_at: s, ended_at: e } = row.original;
          if (s === undefined || e === undefined) return <span className="text-xs text-muted">—</span>;
          return (
            <span className="tabular font-mono text-xs">
              {formatAge(new Date(e).getTime() - new Date(s).getTime())}
            </span>
          );
        },
      },
    ],
    [],
  );
}

// Общие колонки Live/История.
function idColumn(): ColumnDef<Match, unknown> {
  return {
    id: 'id',
    header: 'Матч',
    cell: ({ row }) => <span className="font-mono text-xs">{shortId(row.original.id)}</span>,
  };
}

function regionColumn(): ColumnDef<Match, unknown> {
  return {
    id: 'region',
    header: 'Регион',
    cell: ({ row }) => <span className="font-mono text-xs">{row.original.region}</span>,
  };
}

function versionColumn(): ColumnDef<Match, unknown> {
  return {
    id: 'version',
    header: 'Версия',
    cell: ({ row }) => <span className="font-mono text-xs">{row.original.semver}</span>,
  };
}

function stateColumn(): ColumnDef<Match, unknown> {
  return {
    id: 'state',
    header: 'Состояние',
    cell: ({ row }) => <StateBadge state={row.original.state} tone={toneOfMatchState(row.original.state)} />,
  };
}
