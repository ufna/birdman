// Матчи: Live (running + pending — pending уже держит дедик, running
// появится с liba) и История с фильтрами state/region и пагинацией
// limit/offset. Клик по строке (в обеих вкладках) открывает дровер деталей
// матча (статы + логи).

import { useMemo, useState } from 'react';
import * as Tabs from '@radix-ui/react-tabs';
import type { ColumnDef } from '@tanstack/react-table';
import { api } from '../lib/api';
import type { Match, MatchState } from '../lib/api';
import { useData } from '../lib/live';
import { useMatchDrawer, useServerDrawer } from '../lib/drawer';
import { useNow } from '../lib/useNow';
import { versionColor } from '../lib/stats';
import { ageOf, shortId } from '../lib/format';
import { useT, useFormat } from '../lib/i18n';
import type { I18nContextValue, MessageKey } from '../lib/i18n';
import { DataTable } from '../components/DataTable';
import { StateBadge, toneOfMatchState } from '../components/Badge';
import { Card, CardHeader, ErrorNote, LoadingRow } from '../components/ui';

const PAGE_SIZE = 50;

type T = I18nContextValue['t'];

export function Matches() {
  const { t } = useT();
  return (
    <Tabs.Root defaultValue="live" className="flex flex-col gap-4">
      <Tabs.List className="flex w-fit gap-1 rounded-lg border border-line bg-card p-1" aria-label={t('matches.tabs')}>
        {(['live', 'history'] as const).map((tab) => (
          <Tabs.Trigger
            key={tab}
            value={tab}
            className="rounded-md px-3 py-1 text-sm text-muted transition-colors data-[state=active]:bg-mark data-[state=active]:font-medium data-[state=active]:text-accent-ink"
          >
            {tab === 'live' ? t('matches.tab.live') : t('matches.tab.history')}
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
  const { t } = useT();
  const { open: openMatch } = useMatchDrawer();
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
        title={t('matches.live.title')}
        aside={<span className="tabular font-mono text-xs text-muted">{live.data?.length ?? 0}</span>}
      />
      {live.data === undefined ? (
        <LoadingRow />
      ) : (
        <DataTable
          columns={columns}
          data={live.data}
          rowId={(m) => m.id}
          onRowClick={(m) => {
            openMatch(m.id);
          }}
          empty={t('matches.live.empty')}
        />
      )}
    </Card>
  );
}

function useLiveColumns(): ColumnDef<Match, unknown>[] {
  const { t } = useT();
  const { open: openServer } = useServerDrawer();
  return useMemo(
    () => [
      idColumn(t),
      regionColumn(t),
      versionColumn(t),
      {
        id: 'players',
        header: t('col.players'),
        cell: ({ row }) => <span className="tabular font-mono text-xs">{row.original.server_players}</span>,
      },
      {
        id: 'duration',
        header: t('col.duration'),
        cell: ({ row }) => <DurationCell m={row.original} />,
      },
      {
        id: 'server',
        header: t('col.server'),
        cell: ({ row }) => (
          <div className="font-mono text-xs">
            {row.original.host}:{row.original.port}
            {row.original.server_id !== '' && (
              <>
                {' · '}
                <button
                  type="button"
                  onClick={(e) => {
                    e.stopPropagation(); // не открывать дровер матча
                    openServer(row.original.server_id);
                  }}
                  className="text-accent-ink underline-offset-2 hover:underline"
                  title={t('server.openDetails')}
                >
                  {shortId(row.original.server_id)}
                </button>
              </>
            )}
          </div>
        ),
      },
      stateColumn(t),
    ],
    [t, openServer],
  );
}

/** Тикающая длительность живого матча (от started_at, иначе от created_at). */
function DurationCell({ m }: { m: Match }) {
  const now = useNow();
  const fmt = useFormat();
  const age = ageOf(m.started_at ?? m.created_at, now);
  return <span className="tabular font-mono text-xs">{age === null ? '—' : fmt.age(age)}</span>;
}

const HISTORY_STATE_VALUES: (MatchState | '')[] = ['', 'pending', 'running', 'finished', 'aborted'];

function MatchHistory() {
  const { t } = useT();
  const { open: openMatch } = useMatchDrawer();
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
        title={t('matches.history.title')}
        aside={
          <div className="flex items-center gap-2">
            <select
              aria-label={t('matches.filter.stateAria')}
              className={select}
              value={state}
              onChange={(e) => {
                setState(e.target.value as MatchState | '');
                setPage(0);
              }}
            >
              {HISTORY_STATE_VALUES.map((s) => (
                <option key={s} value={s}>
                  {s === '' ? t('matches.filter.allStates') : t(`state.match.${s}` as MessageKey)}
                </option>
              ))}
            </select>
            <select
              aria-label={t('matches.filter.regionAria')}
              className={select}
              value={region}
              onChange={(e) => {
                setRegion(e.target.value);
                setPage(0);
              }}
            >
              <option value="">{t('matches.filter.allRegions')}</option>
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
            onRowClick={(m) => {
              openMatch(m.id);
            }}
            empty={page > 0 ? t('matches.history.emptyNext') : t('matches.history.emptyFilter')}
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
                {t('pager.newer')}
              </PagerButton>
              <PagerButton
                disabled={!hasNext}
                onClick={() => {
                  setPage((p) => p + 1);
                }}
              >
                {t('pager.older')}
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
  const { t } = useT();
  const fmt = useFormat();
  return useMemo(
    () => [
      idColumn(t),
      regionColumn(t),
      versionColumn(t),
      stateColumn(t),
      {
        id: 'peak',
        header: t('col.playersPeak'),
        cell: ({ row }) => <span className="tabular font-mono text-xs">{row.original.players_peak}</span>,
      },
      {
        id: 'created',
        header: t('col.created'),
        cell: ({ row }) => (
          <span className="tabular font-mono text-xs text-muted">{fmt.stamp(row.original.created_at)}</span>
        ),
      },
      {
        id: 'took',
        header: t('col.duration'),
        cell: ({ row }) => {
          const { started_at: s, ended_at: e } = row.original;
          if (s === undefined || e === undefined) return <span className="text-xs text-muted">—</span>;
          return (
            <span className="tabular font-mono text-xs">{fmt.age(new Date(e).getTime() - new Date(s).getTime())}</span>
          );
        },
      },
    ],
    [t, fmt],
  );
}

// Общие колонки Live/История.
function idColumn(t: T): ColumnDef<Match, unknown> {
  return {
    id: 'id',
    header: t('col.match'),
    cell: ({ row }) => <span className="font-mono text-xs">{shortId(row.original.id)}</span>,
  };
}

function regionColumn(t: T): ColumnDef<Match, unknown> {
  return {
    id: 'region',
    header: t('col.region'),
    cell: ({ row }) => <span className="font-mono text-xs">{row.original.region}</span>,
  };
}

function versionColumn(t: T): ColumnDef<Match, unknown> {
  return {
    id: 'version',
    header: t('col.version'),
    cell: ({ row }) => <VersionTag semver={row.original.semver} />,
  };
}

/** Версия с единым цветом-меткой (хэш semver) — совпадает со Stats/Cost. */
export function VersionTag({ semver }: { semver: string }) {
  return (
    <span className="inline-flex items-center gap-1.5 font-mono text-xs">
      <span aria-hidden className="size-2 shrink-0 rounded-[2px]" style={{ background: versionColor(semver) }} />
      {semver}
    </span>
  );
}

function stateColumn(t: T): ColumnDef<Match, unknown> {
  return {
    id: 'state',
    header: t('col.state'),
    cell: ({ row }) => <StateBadge state={row.original.state} tone={toneOfMatchState(row.original.state)} domain="match" />,
  };
}
