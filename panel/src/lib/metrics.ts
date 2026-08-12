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
  | { kind: 'unavailable'; reason: UnavailableReason };

/**
 * Почему метрик сейчас нет. Три значения, и третье отличается от второго
 * ИЗВЕСТНОСТЬЮ ИСТОЧНИКА, а не степенью беды (tracker #1021):
 *   - `unconfigured` — master сказал, что `victoriametrics_url` пуст (503);
 *   - `upstream` — master сказал СВОИМ JSON'ом, что не смог сходить в VM
 *     (`ops.go` → `{"error":"upstream"}`), то есть виновник известен ТОЧНО;
 *   - `gateway` — 502/504 с НЕразбираемым телом, то есть ответил не master, а
 *     шлюз перед кем-то из них, и по одному статусу панель не знает, перед кем.
 */
export type UnavailableReason = 'unconfigured' | 'upstream' | 'gateway';

interface VMMatrixResult {
  metric?: Record<string, string>;
  values?: [number, string][];
  /** instant-запрос (`/api/v1/query`) отдаёт одну точку `value`, не `values`. */
  value?: [number, string];
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

/** Одна точка instant-вектора: подписи + значение (null — NaN/+Inf/нет данных). */
export interface VectorSample {
  labels: Record<string, string>;
  name: string;
  value: number | null;
}

/**
 * VM/Prometheus vector (`/api/v1/query`) → образцы. `value` строкой, как и в
 * matrix; NaN/±Inf → null. Чистая функция — точка тестирования (гистограммные
 * перцентили time-to-match приходят именно вектором).
 */
export function parseVector(body: unknown): VectorSample[] {
  const resp = body as VMResponse | null;
  const result = resp?.data?.result;
  if (!Array.isArray(result)) return [];
  const out: VectorSample[] = [];
  for (const item of result) {
    const labels = item.metric ?? {};
    const pair = item.value;
    let value: number | null = null;
    if (Array.isArray(pair) && pair.length >= 2) {
      const raw = Number(pair[1]);
      value = Number.isFinite(raw) ? raw : null;
    }
    out.push({ labels, name: labels.__name__ ?? '', value });
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

type Unavailable = { kind: 'unavailable'; reason: UnavailableReason };

/**
 * Общий вызов metrics-proxy: GET path → либо мягкое `unavailable` (VM не
 * настроена / апстрим лежит), либо распарсенное тело VM (валидный успех). Битый
 * PromQL и прочие жёсткие ошибки — ApiError. Разделяет query и query_range.
 */
async function fetchVM(path: string, signal?: AbortSignal): Promise<Unavailable | { kind: 'body'; body: unknown }> {
  const res = await fetch(path, { credentials: 'same-origin', signal });
  // 502/504 — мягкая недоступность НЕЗАВИСИМО от тела, и решается это ДО
  // разбора JSON (tracker #996): на этих кодах отвечает ШЛЮЗ, а он отдаёт не
  // JSON, а HTML-страницу, и `JSON.parse` бросал раньше, чем дело доходило до
  // проверки статуса (до #996 кусок этой страницы ещё и уезжал в UI текстом).
  //
  // Причина именно `gateway`, а не `upstream` (tracker #1021). Прежний
  // комментарий мотивировал ветку «самым частым в self-host случаем „VM за
  // nginx лёг“» — топологии, которой в `infra/` этого репозитория НЕТ ВОВСЕ:
  // единственный nginx стоит перед МАСТЕРОМ
  // (`birdman_master_dev/templates/nginx-panel.conf.j2`), а VictoriaMetrics
  // публикует 8428 напрямую. Но и «значит это master» сказать нельзя:
  // `victoriametrics_url` настраиваемый, продукт self-host, и оператор вправе
  // поставить шлюз и перед VM. По ОДНОМУ статусу эти два случая не различимы —
  // поэтому панель не угадывает виновника, а честно называет обоих.
  // Собственный 502 мастера сюда не попадает: он JSON, у него
  // `error:'upstream'`, и его ветка ниже знает источник точно.
  const text = await res.text();
  let body: unknown;
  let parsed = true;
  try {
    body = text === '' ? undefined : JSON.parse(text);
  } catch {
    parsed = false;
  }
  const errCode = parsed ? (body as { error?: string } | undefined)?.error : undefined;
  if (res.status === 502 || res.status === 504) {
    // Тело РАЗОБРАНО и это собственный JSON мастера (`{"error":"upstream"}`,
    // ops.go) — виновник известен ТОЧНО, это VictoriaMetrics. Не разобрано —
    // отвечал шлюз, и перед кем он стоит, по статусу не видно.
    const known = errCode === 'upstream' || errCode === 'bad_gateway';
    return { kind: 'unavailable', reason: known ? 'upstream' : 'gateway' };
  }
  // Сюда доходят только НЕ-502/504, поэтому неразобранное тело здесь всё ещё
  // жёсткая ошибка — мягкая ветка выше её уже забрала (см. #996).
  if (!parsed) throw new ApiError(res.status, 'bad_response', text.slice(0, 160));
  if (res.status === 503 && errCode === 'metrics_unconfigured') {
    return { kind: 'unavailable', reason: 'unconfigured' };
  }
  if (errCode === 'upstream' || errCode === 'bad_gateway') {
    return { kind: 'unavailable', reason: 'upstream' };
  }
  // Конверт VM разбираем ДО общей ветки `!res.ok` (tracker #996). У двух
  // апстримов РАЗНАЯ форма ошибки, и поле `error` значит в них разное: у
  // master это машинный код (`{"error":"internal","detail":"…"}`), у
  // VM/Prometheus — человеческая ПРОЗА, а код лежит в `errorType`
  // (`{"status":"error","errorType":"422","error":"cannot parse query: …"}`).
  // Настоящая VM отвечает на отвергнутый запрос НЕ 200, а 422/400/503, то есть
  // до перестановки эта проза читалась как код и уезжала в UI — ровно дефект
  // #996, только через другую дверь. Master проксирует тело VM вербатим
  // (`httpapi/ops.go`), так что различать формы обязана панель.
  const vm = body as VMResponse | undefined;
  if (vm?.status === 'error') {
    throw new ApiError(res.status, vm.errorType ?? 'metrics_error', vm.error);
  }
  if (!res.ok) {
    const e = body as { error?: string; detail?: string } | undefined;
    throw new ApiError(res.status, e?.error ?? `http_${res.status}`, e?.detail);
  }
  return { kind: 'body', body };
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
  const res = await fetchVM(path, args.signal);
  if (res.kind === 'unavailable') return res;
  return { kind: 'ok', series: parseMatrix(res.body) };
}

export interface QueryInstantArgs {
  query: string;
  /** unix-секунды (по умолчанию — «сейчас» на стороне VM). */
  time?: number;
  signal?: AbortSignal;
}

/** Результат instant-запроса: вектор образцов либо мягкая недоступность VM. */
export type InstantResult =
  | { kind: 'ok'; vector: VectorSample[] }
  | Unavailable;

/**
 * GET /v1/metrics/query — instant-вектор (histogram_quantile перцентили
 * time-to-match и т.п.). Та же мягкая деградация, что и у query_range.
 */
export async function queryInstant(args: QueryInstantArgs): Promise<InstantResult> {
  const path = `/v1/metrics/query${qs({ query: args.query, time: args.time })}`;
  const res = await fetchVM(path, args.signal);
  if (res.kind === 'unavailable') return res;
  return { kind: 'ok', vector: parseVector(res.body) };
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

/**
 * PromQL: число дедиков по состоянию (`birdman_servers{state,region,version}`)
 * — ряд утилизации во времени для Cost. Сумма по всем регионам/версиям, разбивка
 * только по состоянию (occupancy флота).
 */
export function serversByStateQuery(): string {
  return 'sum by (state) (birdman_servers)';
}

/**
 * PromQL истинного time-to-match: перцентиль по гистограмме
 * `birdman_mm_time_to_match_seconds` за выбранный период (increase бакетов за N
 * дней). Мало данных / нет матчей → VM вернёт NaN → образец с value=null, и вью
 * деградирует на прокси allocation→match_start.
 */
export function timeToMatchQuantileQuery(quantile: number, days: number): string {
  return `histogram_quantile(${quantile}, sum by (le) (increase(birdman_mm_time_to_match_seconds_bucket[${days}d])))`;
}

/**
 * PromQL-билдеры гранулярных live-панелей (Task 3, "Статистика v1", окна
 * 12ч/24ч/3д): читаются через metrics-proxy (query_range), в отличие от
 * продуктовых агрегатов /v1/stats/* выше.
 */

/** Общее число игроков онлайн (сумма по агентам делается на стороне экспортёра). */
export function playersOnlineQuery(): string {
  return 'birdman_players_online';
}

/** Общее число матчей в игре (running). */
export function matchesRunningQuery(): string {
  return 'birdman_matches_running';
}

/** Глубина очереди матчмейкера, по региону. */
export function queueDepthQuery(): string {
  return 'sum by (region)(birdman_mm_queue_depth)';
}

/**
 * Доля занятых слотов: allocated / ёмкость активных нод, значение 0..1.
 * clamp_min защищает от деления на 0, пока нет активных нод (capacity=0).
 * Строка PromQL долю не умножает — в проценты форматируется на отображении
 * (см. Unit='percent' в formatMetric ниже).
 */
export function utilizationRatioQuery(): string {
  return 'sum(birdman_servers{state="allocated"}) / clamp_min(sum(birdman_node_capacity_slots), 1)';
}

/** «Живые» состояния дедиков в стабильном порядке стека утилизации (низ→верх). */
export const UTIL_STATES = ['allocated', 'ready', 'draining', 'creating'] as const;
export type UtilStateName = (typeof UTIL_STATES)[number];

// Цвета — те же тона, что у метра-снапшота (charts.UtilBar), чтобы снапшот и
// ряд во времени читались как одно: allocated=accent, ready=good, draining=warn.
const UTIL_STATE_COLOR: Record<UtilStateName, string> = {
  allocated: 'var(--accent)',
  ready: 'var(--good)',
  draining: 'var(--warn)',
  creating: 'var(--cat-1)',
};

const UTIL_STATE_LABEL: Record<UtilStateName, MessageKey> = {
  allocated: 'stats.util.allocated',
  ready: 'stats.util.ready',
  draining: 'stats.util.draining',
  creating: 'stats.util.creating',
};

/** Ряд утилизации во времени: состояние, цвет, i18n-ключ подписи, точки. */
export interface StateSeries {
  state: UtilStateName;
  color: string;
  labelKey: MessageKey;
  points: [number, number | null][];
}

/**
 * MetricSeries[] из `sum by (state)(birdman_servers)` → ряды по «живым»
 * состояниям в стабильном порядке и цвете. Состояния вне UTIL_STATES (failed/
 * reaped) отбрасываются — это не занятость ёмкости. Состояние без точек
 * пропускается. Пустой вход → [] (вью покажет «нет данных»).
 */
export function utilizationSeriesModel(series: MetricSeries[]): StateSeries[] {
  const byState = new Map<string, [number, number | null][]>();
  for (const s of series) {
    const st = s.labels.state;
    if (st !== undefined && !byState.has(st)) byState.set(st, s.points);
  }
  const out: StateSeries[] = [];
  for (const state of UTIL_STATES) {
    const points = byState.get(state);
    if (points === undefined || points.length === 0) continue;
    out.push({ state, color: UTIL_STATE_COLOR[state], labelKey: UTIL_STATE_LABEL[state], points });
  }
  return out;
}

export type Unit = 'int' | 'ms' | 'cores' | 'bytes' | 'percent';

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
    // Доля 0..1 → проценты (ambiguity resolution #1, Task 3): PromQL отдаёт
    // ratio как есть, умножение только на отображении.
    case 'percent':
      return `${(v * 100).toFixed(0)}%`;
    default:
      return Number.isInteger(v) ? String(v) : v.toFixed(1);
  }
}

export function formatBytes(n: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}
