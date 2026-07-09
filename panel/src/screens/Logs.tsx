// Экран «Логи»: флит-поиск по VictoriaLogs через master-прокси /v1/logs/query
// (docs/superpowers/specs/2026-07-09-centralized-logs-design.md §4, readonly).
// Текст + простые фильтры region/node/server_id + диапазон + limit; поиск по
// Enter/кнопке (не автозапуск — форма может стоить дорого на большом флоте).
// Результаты — время + чипы region/node/server_id + сообщение с подсветкой
// совпадения; клик по строке открывает дровер дедика этого server_id (тот же
// LogsPanel с Live|История, что и в дроверах Флота/Матчей — этот экран для
// быстрого поиска ПО ВСЕМУ флоту, а не по одному дедику).

import { useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { fleetSearchQuery } from '../lib/logsql';
import { queryLogs } from '../lib/logsHistory';
import type { LogLine } from '../lib/logsHistory';
import { useServerDrawer } from '../lib/drawer';
import { useT, useFormat } from '../lib/i18n';
import type { MessageKey } from '../lib/i18n';
import { shortId } from '../lib/format';
import { Card, CardHeader, EmptyState, ErrorNote, Skeleton, SkeletonRegion } from '../components/ui';

const RANGE_PRESETS: { key: MessageKey; seconds: number }[] = [
  { key: 'logs.range.1h', seconds: 3600 },
  { key: 'logs.range.24h', seconds: 86400 },
  { key: 'logs.range.7d', seconds: 7 * 86400 },
  { key: 'logs.range.14d', seconds: 14 * 86400 },
];
const LIMITS = [100, 500, 1000];

type SearchState =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'ok'; lines: LogLine[] }
  | { status: 'soft'; reason: 'unconfigured' | 'upstream' }
  | { status: 'error'; error: Error };

export function Logs() {
  const { t } = useT();
  const [text, setText] = useState('');
  const [region, setRegion] = useState('');
  const [node, setNode] = useState('');
  const [serverId, setServerId] = useState('');
  const [rangeSec, setRangeSec] = useState(86400); // 24ч по умолчанию
  const [limit, setLimit] = useState(500);
  const [state, setState] = useState<SearchState>({ status: 'idle' });
  // Подсветка идёт по ПОСЛЕДНЕМУ выполненному поиску, а не по текущему вводу
  // (который мог измениться уже после того, как результаты пришли).
  const [appliedText, setAppliedText] = useState('');

  const run = () => {
    setState({ status: 'loading' });
    const t0 = text.trim();
    setAppliedText(t0);
    const start = Math.floor(Date.now() / 1000) - rangeSec;
    queryLogs({ query: fleetSearchQuery({ text, region, node, serverId }), start, limit })
      .then((res) => {
        if (res.kind === 'unavailable') {
          setState({ status: 'soft', reason: res.reason });
          return;
        }
        setState({ status: 'ok', lines: res.lines });
      })
      .catch((e: unknown) => {
        setState({ status: 'error', error: e instanceof Error ? e : new Error(String(e)) });
      });
  };

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    run();
  };

  const inputClass = 'rounded-lg border border-line bg-card px-2.5 py-1.5 text-xs placeholder:text-muted';
  const selectClass = 'rounded-lg border border-line bg-card px-2.5 py-1.5 text-xs';

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader title={t('nav.logs')} />
        <form onSubmit={onSubmit} className="flex flex-col gap-3 p-4">
          <input
            value={text}
            onChange={(e) => {
              setText(e.target.value);
            }}
            placeholder={t('logs.search.textPlaceholder')}
            aria-label={t('logs.search.textPlaceholder')}
            className={inputClass}
          />
          <div className="flex flex-wrap items-center gap-2">
            <input
              value={region}
              onChange={(e) => {
                setRegion(e.target.value);
              }}
              placeholder={t('col.region')}
              aria-label={t('col.region')}
              className={`${inputClass} w-28`}
            />
            <input
              value={node}
              onChange={(e) => {
                setNode(e.target.value);
              }}
              placeholder={t('col.node')}
              aria-label={t('col.node')}
              className={`${inputClass} w-28`}
            />
            <input
              value={serverId}
              onChange={(e) => {
                setServerId(e.target.value);
              }}
              placeholder={t('col.server')}
              aria-label={t('col.server')}
              className={`${inputClass} w-32`}
            />

            <div role="group" aria-label={t('logs.range.aria')} className="inline-flex overflow-hidden rounded-lg border border-line text-xs">
              {RANGE_PRESETS.map((p) => (
                <button
                  key={p.key}
                  type="button"
                  aria-pressed={rangeSec === p.seconds}
                  onClick={() => {
                    setRangeSec(p.seconds);
                  }}
                  className={`px-2 py-1.5 font-medium transition-colors ${
                    rangeSec === p.seconds ? 'bg-mark text-accent-ink' : 'text-muted hover:text-ink'
                  }`}
                >
                  {t(p.key)}
                </button>
              ))}
            </div>

            <select
              aria-label={t('logs.search.limitAria')}
              className={selectClass}
              value={limit}
              onChange={(e) => {
                setLimit(Number(e.target.value));
              }}
            >
              {LIMITS.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>

            <button
              type="submit"
              className="ml-auto rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-white transition-opacity hover:opacity-90"
            >
              {t('logs.search.submit')}
            </button>
          </div>
        </form>
      </Card>

      <Card>
        <ResultsBody state={state} query={appliedText} onRetry={run} />
      </Card>
    </div>
  );
}

function ResultsBody({ state, query, onRetry }: { state: SearchState; query: string; onRetry: () => void }) {
  const { t } = useT();
  if (state.status === 'idle') return <EmptyState>{t('logs.search.hint')}</EmptyState>;
  if (state.status === 'loading') return <ResultsSkeleton />;
  if (state.status === 'error') {
    return (
      <div className="p-4">
        <ErrorNote error={state.error} retry={onRetry} />
      </div>
    );
  }
  if (state.status === 'soft') {
    return <EmptyState>{t(state.reason === 'unconfigured' ? 'logs.unconfigured' : 'logs.unavailable')}</EmptyState>;
  }
  if (state.lines.length === 0) return <EmptyState>{t('logs.search.empty')}</EmptyState>;
  return <ResultRows lines={state.lines} query={query} />;
}

function ResultsSkeleton() {
  return (
    <SkeletonRegion>
      <div className="flex flex-col gap-2 p-4">
        {Array.from({ length: 6 }).map((_, i) => (
          <Skeleton key={i} className="h-4 w-full" />
        ))}
      </div>
    </SkeletonRegion>
  );
}

function ResultRows({ lines, query }: { lines: LogLine[]; query: string }) {
  const { open } = useServerDrawer();
  const fmt = useFormat();
  return (
    <ul className="divide-y divide-line">
      {lines.map((l, i) => {
        const sid = l.fields.server_id;
        const clickable = sid !== undefined && sid !== '';
        return (
          <li
            key={`${l.time}-${String(i)}`}
            onClick={clickable ? () => { open(sid); } : undefined}
            role={clickable ? 'button' : undefined}
            tabIndex={clickable ? 0 : undefined}
            onKeyDown={
              clickable
                ? (e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      open(sid);
                    }
                  }
                : undefined
            }
            className={`flex flex-wrap items-start gap-2 px-4 py-2.5 text-xs ${clickable ? 'cursor-pointer hover:bg-paper' : ''}`}
          >
            <span className="tabular shrink-0 font-mono text-muted">{fmt.stamp(l.time)}</span>
            <span className="flex shrink-0 flex-wrap gap-1">
              {l.fields.region !== undefined && <Chip>{l.fields.region}</Chip>}
              {l.fields.node !== undefined && <Chip>{l.fields.node}</Chip>}
              {clickable && <Chip mono>{shortId(sid)}</Chip>}
            </span>
            <span className="min-w-0 flex-1 font-mono">{highlight(l.msg, query)}</span>
          </li>
        );
      })}
    </ul>
  );
}

function Chip({ children, mono = false }: { children: string; mono?: boolean }) {
  return (
    <span className={`rounded border border-line px-1.5 py-0.5 text-[10px] tracking-wide text-muted uppercase ${mono ? 'font-mono normal-case' : ''}`}>
      {children}
    </span>
  );
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** Оборачивает КАЖДОЕ вхождение query в <mark> (регистронезависимо). Пустой
 *  query (поиск был только по фильтрам, без текста) — сообщение как есть. */
function highlight(msg: string, query: string): ReactNode {
  if (query === '') return msg;
  const parts = msg.split(new RegExp(`(${escapeRegExp(query)})`, 'ig'));
  if (parts.length === 1) return msg;
  return parts.map((part, i) => (i % 2 === 1 ? <mark key={i}>{part}</mark> : <span key={i}>{part}</span>));
}
