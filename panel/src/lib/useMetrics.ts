// React-хуки над metrics-proxy (lib/metrics.ts): дозапрос query_range/instant с
// поллингом и аккуратными статусами «VM не настроена / недоступна / нет данных /
// ошибка». Вынесено из MetricChart, чтобы переиспользовать в графике утилизации
// (Cost) и истинном time-to-match (Stats) — единая механика поллинга и статусов.

import { useEffect, useState } from 'react';
import { queryRange, queryInstant } from './metrics';
import type { MetricRange, MetricSeries, VectorSample } from './metrics';

/** Статус загрузки графика: до данных — loading; мягкая недоступность VM — не ошибка. */
export type MetricStatus = 'loading' | 'ok' | 'unconfigured' | 'unreachable' | 'error';

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
          setSeries((prev) => {
            if (prev === null) {
              setError(e instanceof Error ? e.message : String(e));
              setStatus('error');
            }
            return prev;
          });
        });
    };
    load();
    const timer = isStatic ? null : setInterval(load, refreshMs);
    return () => {
      active = false;
      ctrl?.abort();
      if (timer !== null) clearInterval(timer);
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
 */
export function useInstantQuery({ query, refetchKey, enabled = true }: UseInstantQueryOpts): InstantState {
  const [status, setStatus] = useState<MetricStatus>('loading');
  const [vector, setVector] = useState<VectorSample[] | null>(null);

  useEffect(() => {
    if (!enabled) return;
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
      .catch(() => {
        if (!active || ctrl.signal.aborted) return;
        setStatus('error');
        setVector(null);
      });
    return () => {
      active = false;
      ctrl.abort();
    };
  }, [query, refetchKey, enabled]);

  return { status, vector };
}
