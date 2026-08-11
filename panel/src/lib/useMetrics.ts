// React-хуки над metrics-proxy (lib/metrics.ts): дозапрос query_range/instant с
// поллингом и аккуратными статусами «VM не настроена / недоступна / нет данных /
// ошибка». Вынесено из MetricChart, чтобы переиспользовать в графике утилизации
// (Cost) и истинном time-to-match (Stats) — единая механика поллинга и статусов.

import { useEffect, useRef, useState } from 'react';
import { ApiError } from './api';
import { queryRange, queryInstant } from './metrics';
import type { MetricRange, MetricSeries, VectorSample } from './metrics';

/**
 * Статус загрузки графика: до данных — loading; мягкая недоступность VM — не
 * ошибка. `forbidden` — отказ доступа (403): отдельно от `error`, потому что
 * (а) его причину панель называет честно по сессии, без текста мастера, и
 * (б) он НЕ транзиентен, поэтому поллинг на нём останавливается.
 */
export type MetricStatus = 'loading' | 'ok' | 'unconfigured' | 'unreachable' | 'forbidden' | 'error';

/** 403 — не транзиентная ошибка: ни нехватка скоупа, ни привязка ключа сама не
 *  пройдёт, и повтор через 15с даёт только новый отказ (tracker #1000). */
function isForbidden(e: unknown): boolean {
  return e instanceof ApiError && e.status === 403;
}

export interface QueryRangeState {
  status: MetricStatus;
  series: MetricSeries[] | null;
  error: string;
}

interface UseQueryRangeOpts {
  query: string;
  /** Скользящее окно, мс (по умолчанию 30 минут). Игнорируется, если задан range. */
  windowMs?: number;
  /** Явное окно [start, end?]; end задан — статичная история (не поллим). */
  range?: MetricRange;
  /** Период дозапроса, мс (по умолчанию 15с). */
  refreshMs?: number;
}

/**
 * Дозапрос query_range с поллингом. Статичное окно (range.end задан) — один
 * запрос без поллинга. Прошлые данные при ошибке не сбрасываем: держим график,
 * ошибку показываем только пока данных ещё не было.
 */
export function useQueryRange({ query, windowMs = 30 * 60_000, range, refreshMs = 15_000 }: UseQueryRangeOpts): QueryRangeState {
  const [status, setStatus] = useState<MetricStatus>('loading');
  const [series, setSeries] = useState<MetricSeries[] | null>(null);
  const [error, setError] = useState('');

  const rangeStart = range?.start;
  const rangeEnd = range?.end;
  const isStatic = rangeStart !== undefined && rangeEnd !== undefined;

  useEffect(() => {
    let active = true;
    let ctrl: AbortController | null = null;
    // Таймер держим в переменной эффекта, а не только в cleanup: на 403 его
    // надо погасить ИЗ обработчика ошибки. До #1000 на «Статистике» пять
    // графиков продолжали долбить отказ каждые 15с на одну привязанную сессию.
    // Это не про rate-limit (nginx панели даёт 30 r/s при burst 60, пять
    // запросов в 15с — 0.33 r/s), а про бессмысленный трафик и поток 403 в
    // логах мастера, в котором тонет настоящая ошибка.
    let timer: ReturnType<typeof setInterval> | null = null;
    const stopPolling = () => {
      if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
    };
    const load = () => {
      ctrl?.abort();
      ctrl = new AbortController();
      const end = rangeEnd ?? Math.floor(Date.now() / 1000);
      const start = rangeStart ?? end - Math.floor(windowMs / 1000);
      const step = Math.max(15, Math.round((end - start) / 150));
      queryRange({ query, start, end, step, signal: ctrl.signal })
        .then((res) => {
          if (!active) return;
          if (res.kind === 'unavailable') {
            setStatus(res.reason === 'unconfigured' ? 'unconfigured' : 'unreachable');
            return;
          }
          setSeries(res.series);
          setStatus('ok');
        })
        .catch((e: unknown) => {
          if (!active || ctrl?.signal.aborted) return;
          // Останавливаем ВСЕГДА, а не только когда данных ещё не было: уже
          // показанный график остаётся на месте, но добывать новые точки
          // запрещённым запросом бессмысленно.
          const forbidden = isForbidden(e);
          if (forbidden) stopPolling();
          setSeries((prev) => {
            if (prev === null) {
              setError(e instanceof Error ? e.message : String(e));
              setStatus(forbidden ? 'forbidden' : 'error');
            }
            return prev;
          });
        });
    };
    load();
    // load() выше синхронно только СТАРТУЕТ запрос; его .catch не может
    // выполниться до этой строки, так что гасить нечего раньше присвоения.
    if (!isStatic) timer = setInterval(load, refreshMs);
    return () => {
      active = false;
      ctrl?.abort();
      stopPolling();
    };
  }, [query, windowMs, refreshMs, rangeStart, rangeEnd, isStatic]);

  return { status, series, error };
}

export interface InstantState {
  status: MetricStatus;
  vector: VectorSample[] | null;
}

interface UseInstantQueryOpts {
  query: string;
  /** Ключ ре-запроса (напр. период/лайв-тик): смена → новый запрос. */
  refetchKey?: string | number;
  enabled?: boolean;
}

/**
 * Одноразовый instant-запрос (перечитывается при смене query/refetchKey). Для
 * истинного time-to-match: перцентили гистограммы за период. Недоступность VM —
 * мягкий статус, не бросок.
 *
 * 403 здесь тоже НЕ ретраится, и это не теория: `refetchKey` в Stats — это
 * `generated_at` из `/v1/stats/overview` (`screens/Stats.tsx`), а тот
 * перечитывается по всплеску SSE с дебансом 3с (`lib/live.tsx`), то есть на
 * живом кластере чаще, чем 15-секундный поллинг `useQueryRange`. Запомнить
 * отказ надо ИМЕННО для запроса: смена `query` (другой период/другая метрика) —
 * новый запрос, он обязан уйти.
 */
export function useInstantQuery({ query, refetchKey, enabled = true }: UseInstantQueryOpts): InstantState {
  const [status, setStatus] = useState<MetricStatus>('loading');
  const [vector, setVector] = useState<VectorSample[] | null>(null);
  // Запрос, на котором прилетел 403. Ref, а не state: он не влияет на рендер и
  // не должен сам вызывать перезапуск эффекта.
  const forbiddenQuery = useRef<string | null>(null);

  useEffect(() => {
    if (!enabled) return;
    if (forbiddenQuery.current === query) {
      // Тот же самый запрос уже получил 403; refetchKey сменился (тик SSE) —
      // повторять отказ нечего. Статус уже 'forbidden', сообщение на экране.
      return;
    }
    let active = true;
    const ctrl = new AbortController();
    setStatus('loading');
    queryInstant({ query, signal: ctrl.signal })
      .then((res) => {
        if (!active) return;
        if (res.kind === 'unavailable') {
          setStatus(res.reason === 'unconfigured' ? 'unconfigured' : 'unreachable');
          setVector(null);
          return;
        }
        setVector(res.vector);
        setStatus('ok');
      })
      .catch((e: unknown) => {
        if (!active || ctrl.signal.aborted) return;
        const forbidden = isForbidden(e);
        if (forbidden) forbiddenQuery.current = query;
        setStatus(forbidden ? 'forbidden' : 'error');
        setVector(null);
      });
    return () => {
      active = false;
      ctrl.abort();
    };
  }, [query, refetchKey, enabled]);

  return { status, vector };
}
