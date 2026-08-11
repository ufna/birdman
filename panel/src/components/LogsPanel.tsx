// Лог-вкладка дровера (дедик/матч): сегмент Live | История. Live — байт-в-
// байт текущий LogViewer (не трогаем — live-tail через /v1/servers/{id}/logs).
// История — поиск по VictoriaLogs через master-прокси /v1/logs/query (lib/
// logsql.ts + lib/logsHistory.ts, Task 4): работает и для reaped/мёртвого
// дедика — нужна только serverId-строка, тогда как Live для таких дедиков
// покажет `gone` (как и раньше, см. LogViewer/lib/logs.ts).

import { useEffect, useRef, useState } from 'react';
import { serverHistoryQuery, LOG_RANGE_PRESETS } from '../lib/logsql';
import { queryLogs } from '../lib/logsHistory';
import type { LogLine } from '../lib/logsHistory';
import { useT, useFormat } from '../lib/i18n';
import { LogViewer } from './LogViewer';
import { EmptyState, ErrorNote, Skeleton, SkeletonRegion } from './ui';

type Segment = 'live' | 'history';

/** Сколько строк тянуть за страницу (и первую, и «показать ещё»). Кнопка
 *  «показать ещё» прячется, когда страница короче лимита. */
const HISTORY_LIMIT = 500;

/** initialFollow пробрасывается в Live-режим как есть (MatchDrawer хочет
 *  не-автослежение у завершённых матчей) — дефолт true совпадает с прежним
 *  поведением LogViewer байт-в-байт. */
export function LogsPanel({ serverId, initialFollow = true }: { serverId: string; initialFollow?: boolean }) {
  const { t } = useT();
  const [segment, setSegment] = useState<Segment>('live');

  const segClass = (active: boolean) =>
    `px-2.5 py-1 text-xs font-medium transition-colors ${active ? 'bg-mark text-accent-ink' : 'text-muted hover:text-ink'}`;

  return (
    <div className="flex flex-col gap-3">
      <div role="group" aria-label={t('logs.segmentAria')} className="inline-flex w-fit overflow-hidden rounded-lg border border-line">
        {(['live', 'history'] as Segment[]).map((s) => (
          <button
            key={s}
            type="button"
            aria-pressed={segment === s}
            onClick={() => {
              setSegment(s);
            }}
            className={segClass(segment === s)}
          >
            {t(s === 'live' ? 'logs.live' : 'logs.history')}
          </button>
        ))}
      </div>
      {segment === 'live' ? (
        <LogViewer serverId={serverId} initialFollow={initialFollow} />
      ) : (
        <LogsHistory serverId={serverId} />
      )}
    </div>
  );
}

type HistoryState =
  | { status: 'loading' }
  | { status: 'ok'; lines: LogLine[]; hasMore: boolean }
  | { status: 'soft'; reason: 'unconfigured' | 'upstream' }
  | { status: 'error'; error: Error };

/** История одного дедика: диапазон + текстовый фильтр (Enter/кнопка) +
 *  «показать ещё» постранично назад по времени. Живого сервера не требует —
 *  работает для reaped ровно так же, как для живого (только serverId). */
function LogsHistory({ serverId }: { serverId: string }) {
  const { t } = useT();
  const fmt = useFormat();
  const [rangeSec, setRangeSec] = useState(86400); // 24ч по умолчанию
  const [text, setText] = useState('');
  const [appliedText, setAppliedText] = useState('');
  const [reloadKey, setReloadKey] = useState(0);
  const [state, setState] = useState<HistoryState>({ status: 'loading' });
  const [loadingMore, setLoadingMore] = useState(false);
  // Ошибка ИМЕННО «показать ещё»: раньше она глоталась молча и кнопка просто
  // переставала работать без объяснения (tracker #1000). Отдельно от state,
  // потому что уже показанные строки при этом остаются на экране.
  const [moreError, setMoreError] = useState<Error | null>(null);

  // Guard от гонки устаревшего ответа (тот же idiom, что screens/Logs.tsx):
  // монотонный счётчик поколений, ОБЩИЙ для основной загрузки истории и
  // loadMore — не просто булев `active` на замыкание одного эффекта, потому
  // что loadMore должен уметь узнать, что «его» поколение уже протухло, пока
  // основной эффект перезапустился (смена диапазона/фильтра/сервера).
  const seqRef = useRef(0);

  useEffect(() => {
    const seq = ++seqRef.current;
    setState({ status: 'loading' });
    setMoreError(null);
    const start = Math.floor(Date.now() / 1000) - rangeSec;
    queryLogs({ query: serverHistoryQuery(serverId, appliedText), start, limit: HISTORY_LIMIT })
      .then((res) => {
        if (seq !== seqRef.current) return; // ответ на устаревший запрос — игнорируем
        if (res.kind === 'unavailable') {
          setState({ status: 'soft', reason: res.reason });
          return;
        }
        setState({ status: 'ok', lines: res.lines, hasMore: res.lines.length >= HISTORY_LIMIT });
      })
      .catch((e: unknown) => {
        if (seq !== seqRef.current) return;
        setState({ status: 'error', error: e instanceof Error ? e : new Error(String(e)) });
      });
  }, [serverId, rangeSec, appliedText, reloadKey]);

  const loadMore = () => {
    if (state.status !== 'ok') return;
    const oldest = state.lines.at(-1);
    if (oldest === undefined) return;
    const seq = seqRef.current; // поколение ДО await — если основной эффект перезапустится, эта страница протухнет
    const start = Math.floor(Date.now() / 1000) - rangeSec;
    // VictoriaLogs' end ЭКСКЛЮЗИВЕН ([start, end)) — дробные секунды
    // собственного времени самой старой строки, БЕЗ floor и БЕЗ −1: «строго
    // старше последней показанной строки», без пропуска и дублей на границе
    // страницы (floor+(-1) молча топил строки между границей и oldest).
    const end = new Date(oldest.time).getTime() / 1000;
    setLoadingMore(true);
    setMoreError(null);
    queryLogs({ query: serverHistoryQuery(serverId, appliedText), start, end, limit: HISTORY_LIMIT })
      .then((res) => {
        setLoadingMore(false);
        if (seq !== seqRef.current) return; // диапазон/фильтр сменился, пока «показать ещё» летело — страница устарела
        if (res.kind === 'unavailable') return; // держим уже показанные строки как есть
        setState((prev) =>
          prev.status === 'ok'
            ? { status: 'ok', lines: [...prev.lines, ...res.lines], hasMore: res.lines.length >= HISTORY_LIMIT }
            : prev,
        );
      })
      .catch((e: unknown) => {
        setLoadingMore(false);
        if (seq !== seqRef.current) return; // страница протухла — её ошибка тоже
        // Уже показанные строки остаются, но отказ НАЗЫВАЕМ: молчащая кнопка
        // неотличима от сломанной панели (tracker #1000).
        setMoreError(e instanceof Error ? e : new Error(String(e)));
      });
  };

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <div role="group" aria-label={t('logs.range.aria')} className="inline-flex overflow-hidden rounded-lg border border-line text-xs">
          {LOG_RANGE_PRESETS.map((p) => (
            <button
              key={p.key}
              type="button"
              aria-pressed={rangeSec === p.seconds}
              onClick={() => {
                setRangeSec(p.seconds);
              }}
              className={`px-2 py-1 font-medium transition-colors ${
                rangeSec === p.seconds ? 'bg-mark text-accent-ink' : 'text-muted hover:text-ink'
              }`}
            >
              {t(p.key)}
            </button>
          ))}
        </div>
        <form
          className="ml-auto flex items-center gap-1.5"
          onSubmit={(e) => {
            e.preventDefault();
            setAppliedText(text);
          }}
        >
          <input
            value={text}
            onChange={(e) => {
              setText(e.target.value);
            }}
            placeholder={t('logs.filterPlaceholder')}
            aria-label={t('logs.filterPlaceholder')}
            className="rounded-md border border-line bg-card px-2 py-1 text-xs"
          />
          <button type="submit" className="rounded-md border border-line px-2 py-1 text-xs text-muted transition-colors hover:text-ink">
            {t('logs.filterApply')}
          </button>
        </form>
      </div>

      {state.status === 'loading' ? (
        <HistorySkeleton />
      ) : state.status === 'error' ? (
        <ErrorNote
          error={state.error}
          retry={() => {
            setReloadKey((k) => k + 1);
          }}
        />
      ) : state.status === 'soft' ? (
        <EmptyState>{t(state.reason === 'unconfigured' ? 'logs.unconfigured' : 'logs.unavailable')}</EmptyState>
      ) : state.lines.length === 0 ? (
        <EmptyState>{t('logs.empty')}</EmptyState>
      ) : (
        <>
          <div className="tabular flex max-h-96 flex-col gap-1 overflow-auto rounded-lg border border-line bg-paper p-3 font-mono text-xs leading-relaxed">
            {state.lines.map((l, i) => (
              <div key={`${l.time}-${String(i)}`} className="flex gap-2">
                <span className="shrink-0 text-muted">{fmt.clock(l.time)}</span>
                <span className="min-w-0 flex-1 break-all whitespace-pre-wrap">{l.msg}</span>
              </div>
            ))}
          </div>
          {moreError !== null && <ErrorNote error={moreError} retry={loadMore} />}
          {state.hasMore && (
            <button
              type="button"
              onClick={loadMore}
              disabled={loadingMore}
              className="self-center rounded-lg border border-line px-3 py-1 text-xs text-muted transition-colors hover:text-ink disabled:opacity-50"
            >
              {loadingMore ? t('common.loading') : t('logs.more')}
            </button>
          )}
        </>
      )}
    </div>
  );
}

/** Скелетон списка строк на время первой загрузки/смены диапазона-фильтра. */
function HistorySkeleton() {
  return (
    <SkeletonRegion>
      <div className="flex flex-col gap-1.5 rounded-lg border border-line bg-paper p-3">
        {Array.from({ length: 6 }).map((_, i) => (
          <Skeleton key={i} className="h-3 w-full" />
        ))}
      </div>
    </SkeletonRegion>
  );
}
