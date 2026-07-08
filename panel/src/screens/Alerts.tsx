// Алерты (П2): master проксирует vmalert (правила + active) и отдаёт историю из
// alerts.log. Read-only (mute/edit — TODO, это конфиг vmalert, не состояние
// master). Секции: Активные, Правила, История срабатываний (newest-first).
// Описания алертов приходят с бэка КАК ЕСТЬ (сейчас по-русски из vmalert) —
// показываем контент как пришёл, переводим только обвязку UI.

import { useMemo, useState } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import { api } from '../lib/api';
import type { ActiveAlert, AlertEvent, AlertRule } from '../lib/api';
import { alertsUnavailable } from '../lib/alerts';
import { useAsync } from '../lib/useAsync';
import { useT, useFormat } from '../lib/i18n';
import { DataTable } from '../components/DataTable';
import { StateBadge, toneOfAlertActive, toneOfSeverity } from '../components/Badge';
import { Card, CardHeader, EmptyState, ErrorNote, LoadingRow } from '../components/ui';

const HISTORY_LIMITS = [20, 50, 100];

export function Alerts() {
  const { t } = useT();
  const [limit, setLimit] = useState(50);
  const active = useAsync(() => api.alertsActive(), []);
  const rules = useAsync(() => api.alertRules(), []);
  const history = useAsync(() => api.alertHistory(limit), [limit]);

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-lg font-semibold">{t('nav.alerts')}</h1>
        <span className="rounded border border-line px-1.5 py-0.5 text-[11px] text-muted">{t('alerts.readonly')}</span>
      </div>

      <ActiveSection active={active.data} error={active.error} loading={active.data === undefined && active.loading} reload={active.reload} />
      <RulesSection rules={rules.data} error={rules.error} loading={rules.data === undefined && rules.loading} reload={rules.reload} />
      <HistorySection
        events={history.data}
        error={history.error}
        loading={history.data === undefined && history.loading}
        reload={history.reload}
        limit={limit}
        setLimit={setLimit}
      />
    </div>
  );
}

/** Мягкая подпись для «алерты не настроены / апстрим недоступен», иначе null. */
function useSoftNote(error?: Error): string | null {
  const { t } = useT();
  const kind = alertsUnavailable(error);
  if (kind === 'unconfigured') return t('alerts.unconfigured');
  if (kind === 'upstream') return t('alerts.upstream');
  return null;
}

function ActiveSection({ active, error, loading, reload }: { active?: ActiveAlert[]; error?: Error; loading: boolean; reload: () => void }) {
  const { t } = useT();
  const fmt = useFormat();
  const soft = useSoftNote(error);
  return (
    <Card>
      <CardHeader
        title={t('alerts.active')}
        aside={active !== undefined ? <span className="tabular font-mono text-xs text-muted">{active.length}</span> : undefined}
      />
      {loading ? (
        <LoadingRow />
      ) : soft !== null ? (
        <EmptyState>{soft}</EmptyState>
      ) : error !== undefined && active === undefined ? (
        <div className="p-4">
          <ErrorNote error={error} retry={reload} />
        </div>
      ) : active === undefined || active.length === 0 ? (
        <EmptyState>{t('alerts.noActive')}</EmptyState>
      ) : (
        <ul className="flex flex-col divide-y divide-line">
          {active.map((a, i) => (
            <li key={`${a.name}-${a.node}-${String(i)}`} className="flex flex-wrap items-start justify-between gap-3 px-4 py-3">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <StateBadge state={a.severity} tone={toneOfSeverity(a.severity)} domain="severity" />
                  <span className="font-mono text-sm font-medium">{a.name}</span>
                </div>
                {a.description !== '' && <p className="mt-1 text-xs text-muted">{a.description}</p>}
              </div>
              <div className="text-right font-mono text-xs text-muted">
                <div>{[a.region, a.node].filter((x) => x !== '').join(' · ') || '—'}</div>
                <div className="mt-0.5">{fmt.stamp(a.active_at)}</div>
              </div>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

function RulesSection({ rules, error, loading, reload }: { rules?: AlertRule[]; error?: Error; loading: boolean; reload: () => void }) {
  const { t } = useT();
  const soft = useSoftNote(error);
  const columns = useMemo<ColumnDef<AlertRule, unknown>[]>(
    () => [
      {
        id: 'name',
        header: t('alerts.col.rule'),
        cell: ({ row }) => (
          <div className="min-w-40">
            <div className="font-mono text-sm font-medium">{row.original.name}</div>
            <div className="font-mono text-[11px] text-muted">{row.original.group}</div>
          </div>
        ),
      },
      {
        id: 'severity',
        header: t('alerts.col.severity'),
        cell: ({ row }) => <StateBadge state={row.original.severity} tone={toneOfSeverity(row.original.severity)} domain="severity" />,
      },
      {
        id: 'expr',
        header: t('alerts.col.expr'),
        cell: ({ row }) => (
          <code className="block max-w-[22rem] truncate font-mono text-xs text-muted" title={row.original.expr}>
            {row.original.expr}
          </code>
        ),
      },
      {
        id: 'for',
        header: t('alerts.col.for'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{row.original.for}</span>,
      },
    ],
    [t],
  );

  return (
    <Card>
      <CardHeader
        title={t('alerts.rules')}
        aside={rules !== undefined ? <span className="tabular font-mono text-xs text-muted">{rules.length}</span> : undefined}
      />
      {loading ? (
        <LoadingRow />
      ) : soft !== null ? (
        <EmptyState>{soft}</EmptyState>
      ) : error !== undefined && rules === undefined ? (
        <div className="p-4">
          <ErrorNote error={error} retry={reload} />
        </div>
      ) : (
        <DataTable columns={columns} data={rules ?? []} rowId={(r) => `${r.group}/${r.name}`} empty={t('alerts.noRules')} />
      )}
    </Card>
  );
}

function HistorySection({
  events,
  error,
  loading,
  reload,
  limit,
  setLimit,
}: {
  events?: AlertEvent[];
  error?: Error;
  loading: boolean;
  reload: () => void;
  limit: number;
  setLimit: (n: number) => void;
}) {
  const { t } = useT();
  const fmt = useFormat();
  const columns = useMemo<ColumnDef<AlertEvent, unknown>[]>(
    () => [
      {
        id: 'name',
        header: t('alerts.col.alert'),
        cell: ({ row }) => (
          <div className="min-w-48 max-w-[24rem]">
            <span className="font-mono text-sm font-medium">{row.original.name}</span>
            {row.original.description !== '' && <p className="truncate text-[11px] text-muted" title={row.original.description}>{row.original.description}</p>}
          </div>
        ),
      },
      {
        id: 'severity',
        header: t('alerts.col.severity'),
        cell: ({ row }) => <StateBadge state={row.original.severity} tone={toneOfSeverity(row.original.severity)} domain="severity" />,
      },
      {
        id: 'where',
        header: t('alerts.col.where'),
        cell: ({ row }) => (
          <span className="font-mono text-xs text-muted">{[row.original.region, row.original.node].filter((x) => x !== '').join(' · ') || '—'}</span>
        ),
      },
      {
        id: 'started',
        header: t('alerts.col.started'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{stamp(fmt, row.original.startsAt)}</span>,
      },
      {
        id: 'ended',
        header: t('alerts.col.ended'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{row.original.active ? '—' : stamp(fmt, row.original.endsAt)}</span>,
      },
      {
        id: 'status',
        header: t('alerts.col.status'),
        cell: ({ row }) => (
          <StateBadge state={row.original.active ? 'firing' : 'resolved'} tone={toneOfAlertActive(row.original.active)} domain="alertstate" />
        ),
      },
    ],
    [t, fmt],
  );

  const select = 'rounded-lg border border-line bg-card px-2.5 py-1.5 text-xs';
  return (
    <Card>
      <CardHeader
        title={t('alerts.history')}
        aside={
          <select
            aria-label={t('alerts.limitAria')}
            className={select}
            value={limit}
            onChange={(e) => {
              setLimit(Number(e.target.value));
            }}
          >
            {HISTORY_LIMITS.map((n) => (
              <option key={n} value={n}>
                {t('alerts.lastN', { count: n })}
              </option>
            ))}
          </select>
        }
      />
      {loading ? (
        <LoadingRow />
      ) : error !== undefined && events === undefined ? (
        <div className="p-4">
          <ErrorNote error={error} retry={reload} />
        </div>
      ) : (
        <DataTable columns={columns} data={events ?? []} rowId={(_e) => rowKey(_e)} empty={t('alerts.noHistory')} />
      )}
    </Card>
  );
}

// startsAt/received_at делают строку уникальной среди повторных доставок.
function rowKey(e: AlertEvent): string {
  return `${e.name}|${e.node}|${e.startsAt}|${e.received_at ?? ''}`;
}

// Пустой/зеро-таймстамп (alertmanager "0001-…") → «—», иначе локальный штамп.
function stamp(fmt: ReturnType<typeof useFormat>, iso: string): string {
  if (iso === '' || iso.startsWith('0001')) return '—';
  return fmt.stamp(iso);
}
