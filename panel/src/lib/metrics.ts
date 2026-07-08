// Тонкий клиент к metrics-proxy master (GET /v1/metrics/query_range → VM,
// panel.md §1.3): панель читает графики через master, не зная про топологию
// мониторинга. Здесь — парсинг VM-ответа matrix → серии для uPlot и
// аккуратная обработка «VM не настроена» (master отдаёт 503
// metrics_unconfigured) и «нет данных».

import { ApiError, qs } from './api';
import type { MessageKey } from './i18n';

/** Одна серия matrix: подписи + точки [unix-сек, значение|null (разрыв)]. */
export interface MetricSeries {
  labels: Record<string, string>;
  /** __name__ метрики, если есть. */
  name: string;
  points: [number, number | null][];
}

/**
 * Ответ query_range: успех с сериями, либо метрики недоступны без вины панели —
 * VM не настроена на master (503 metrics_unconfigured) или настроена, но
 * недоступна (502/504 upstream). Оба случая панель показывает мягко, не
 * ошибкой. Прочие ответы (битый PromQL и т.п.) — ApiError.
 */
export type MetricsResult =
  | { kind: 'ok'; series: MetricSeries[] }
  | { kind: 'unavailable'; reason: 'unconfigured' | 'upstream' };

interface VMMatrixResult {
  metric?: Record<string, string>;
  values?: [number, string][];
}
interface VMResponse {
  status?: string;
  data?: { resultType?: string; result?: VMMatrixResult[] };
  errorType?: string;
  error?: string;
}

/**
 * VM/Prometheus matrix → серии. Значения приходят строками (`"3"`, `"NaN"`,
 * `"+Inf"`); нечисловые превращаются в null (разрыв линии в uPlot). Чистая
 * функция — точка тестирования парсера.
 */
export function parseMatrix(body: unknown): MetricSeries[] {
  const resp = body as VMResponse | null;
  const result = resp?.data?.result;
  if (!Array.isArray(result)) return [];
  const out: MetricSeries[] = [];
  for (const item of result) {
    const labels = item.metric ?? {};
    const values = Array.isArray(item.values) ? item.values : [];
    const points: [number, number | null][] = [];
    for (const pair of values) {
      if (!Array.isArray(pair) || pair.length < 2) continue;
      const t = Number(pair[0]);
      if (!Number.isFinite(t)) continue;
      const raw = Number(pair[1]);
      points.push([t, Number.isFinite(raw) ? raw : null]);
    }
    out.push({ labels, name: labels.__name__ ?? '', points });
  }
  return out;
}

/** Данные для uPlot: общий отсортированный x + по одному y на серию. */
export interface AlignedData {
  x: number[];
  ys: (number | null)[][];
}

/**
 * Выравнивает серии на общую ось времени (объединение всех временных
 * точек); там, где у серии значения нет, ставится null — uPlot рисует
 * разрыв, а не «ступеньку через весь график».
 */
export function toAlignedData(series: MetricSeries[]): AlignedData {
  const xs = new Set<number>();
  for (const s of series) for (const [t] of s.points) xs.add(t);
  const x = [...xs].sort((a, b) => a - b);
  const index = new Map(x.map((t, i) => [t, i]));
  const ys = series.map((s) => {
    const col = new Array<number | null>(x.length).fill(null);
    for (const [t, v] of s.points) {
      const i = index.get(t);
      if (i !== undefined) col[i] = v;
    }
    return col;
  });
  return { x, ys };
}

export interface QueryRangeArgs {
  query: string;
  /** unix-секунды */
  start: number;
  end: number;
  /** шаг в секундах */
  step: number;
  signal?: AbortSignal;
}

/**
 * GET /v1/metrics/query_range. Возвращает серии, либо `unconfigured`, если на
 * этом master не задан victoriametrics_url (503). Прочие ошибки — ApiError.
 */
export async function queryRange(args: QueryRangeArgs): Promise<MetricsResult> {
  const path = `/v1/metrics/query_range${qs({
    query: args.query,
    start: args.start,
    end: args.end,
    step: `${args.step}s`,
  })}`;
  const res = await fetch(path, { credentials: 'same-origin', signal: args.signal });
  const text = await res.text();
  let body: unknown;
  try {
    body = text === '' ? undefined : JSON.parse(text);
  } catch {
    throw new ApiError(res.status, 'bad_response', text.slice(0, 160));
  }
  const errCode = (body as { error?: string } | undefined)?.error;
  if (res.status === 503 && errCode === 'metrics_unconfigured') {
    return { kind: 'unavailable', reason: 'unconfigured' };
  }
  // VM настроена, но лежит/не отвечает — прокси отдаёт 502/504 upstream.
  if (res.status === 502 || res.status === 504 || errCode === 'upstream' || errCode === 'bad_gateway') {
    return { kind: 'unavailable', reason: 'upstream' };
  }
  if (!res.ok) {
    const e = body as { error?: string; detail?: string } | undefined;
    throw new ApiError(res.status, e?.error ?? `http_${res.status}`, e?.detail);
  }
  const vm = body as VMResponse | undefined;
  if (vm?.status === 'error') {
    throw new ApiError(res.status, vm.errorType ?? 'metrics_error', vm.error);
  }
  return { kind: 'ok', series: parseMatrix(body) };
}

/**
 * Окно графика в unix-секундах. end задан — статичная история (не поллим);
 * end опущен — живое окно [start, now], график дозапрашивается.
 */
export interface MetricRange {
  start: number;
  end?: number;
}

/** Дескриптор графика метрики: titleKey переводится в компоненте. */
export interface MetricQuery {
  key: string;
  titleKey: MessageKey;
  expr: string;
  unit: Unit;
}

/** PromQL для метрик одного дедика (agent metrics.go: label server_id). */
export function serverMetricQueries(serverID: string): MetricQuery[] {
  const sel = `{server_id="${serverID}"}`;
  return [
    { key: 'players', titleKey: 'metric.players', expr: `birdman_server_players${sel}`, unit: 'int' },
    { key: 'tick', titleKey: 'metric.tick', expr: `birdman_server_tick_ms${sel}`, unit: 'ms' },
    { key: 'cpu', titleKey: 'metric.cpu', expr: `rate(birdman_container_cpu_seconds_total${sel}[1m])`, unit: 'cores' },
    { key: 'mem', titleKey: 'metric.mem', expr: `birdman_container_memory_bytes${sel}`, unit: 'bytes' },
  ];
}

/** Метрики матча: только tick_ms и игроки (panel.md, фича 2). */
export function matchMetricQueries(serverID: string): MetricQuery[] {
  const sel = `{server_id="${serverID}"}`;
  return [
    { key: 'players', titleKey: 'metric.players', expr: `birdman_server_players${sel}`, unit: 'int' },
    { key: 'tick', titleKey: 'metric.tick', expr: `birdman_server_tick_ms${sel}`, unit: 'ms' },
  ];
}

export type Unit = 'int' | 'ms' | 'cores' | 'bytes';

/** Формат значения по единице измерения (для оси и легенды графика). */
export function formatMetric(v: number | null, unit: Unit): string {
  if (v === null || !Number.isFinite(v)) return '—';
  switch (unit) {
    case 'bytes':
      return formatBytes(v);
    case 'cores':
      return v.toFixed(2);
    case 'ms':
      return v >= 100 ? v.toFixed(0) : v.toFixed(1);
    default:
      return Number.isInteger(v) ? String(v) : v.toFixed(1);
  }
}

function formatBytes(n: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}
