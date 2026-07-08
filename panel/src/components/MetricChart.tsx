// Переиспользуемый график метрики через metrics-proxy (panel.md §1.3, §5):
// <MetricChart query= title= unit= />. Как Sparkline на uPlot, но с осью
// времени, легендой последнего значения и аккуратными состояниями «метрики не
// настроены» (VM off) / «нет данных» / ошибка — панель не падает, а объясняет.

import { useEffect, useMemo, useRef, useState } from 'react';
import uPlot from 'uplot';
import 'uplot/dist/uPlot.min.css';
import { cssVar, useTheme } from '../lib/theme';
import { useT } from '../lib/i18n';
import { queryRange, toAlignedData, formatMetric } from '../lib/metrics';
import type { MetricRange, MetricSeries, Unit } from '../lib/metrics';

interface MetricChartProps {
  query: string;
  title: string;
  unit?: Unit;
  /** Скользящее окно, мс (по умолчанию 30 минут). Игнорируется, если задан range. */
  windowMs?: number;
  /** Явное окно [start, end?]. Если end задан — график статичен (история). */
  range?: MetricRange;
  height?: number;
  /** Период дозапроса, мс (по умолчанию 15с). */
  refreshMs?: number;
}

type Status = 'loading' | 'ok' | 'unconfigured' | 'unreachable' | 'error';

export function MetricChart({
  query,
  title,
  unit = 'int',
  windowMs = 30 * 60_000,
  range,
  height = 150,
  refreshMs = 15_000,
}: MetricChartProps) {
  const [status, setStatus] = useState<Status>('loading');
  const [series, setSeries] = useState<MetricSeries[] | null>(null);
  const [errorMsg, setErrorMsg] = useState('');

  // Статичное окно (история матча) — только один запрос, без поллинга.
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
          // Прошлые данные не сбрасываем — держим график, покажем ошибку
          // только если данных ещё не было (как страховочный поллинг useData).
          setSeries((prev) => {
            if (prev === null) {
              setErrorMsg(e instanceof Error ? e.message : String(e));
              setStatus('error');
            }
            return prev;
          });
        });
    };
    load();
    // История матча не тикает — не поллим; живое/скользящее окно дозапрашиваем.
    const timer = isStatic ? null : setInterval(load, refreshMs);
    return () => {
      active = false;
      ctrl?.abort();
      if (timer !== null) clearInterval(timer);
    };
  }, [query, windowMs, refreshMs, rangeStart, rangeEnd, isStatic]);

  const aligned = useMemo(() => toAlignedData(series ?? []), [series]);
  const hasData = aligned.x.length > 0;
  const latest = useMemo(() => lastValue(series ?? []), [series]);

  return (
    <div className="rounded-lg border border-line bg-card">
      <div className="flex items-baseline justify-between gap-3 border-b border-line px-3 py-2">
        <span className="text-xs font-medium text-muted">{title}</span>
        {status === 'ok' && hasData && (
          <span className="tabular font-mono text-xs text-ink">{formatMetric(latest, unit)}</span>
        )}
      </div>
      <div className="px-2 py-2" style={{ minHeight: height }}>
        {status === 'ok' && hasData ? (
          <Plot aligned={aligned} unit={unit} height={height} />
        ) : (
          <ChartMessage status={status} hasData={hasData} error={errorMsg} height={height} />
        )}
      </div>
    </div>
  );
}

function ChartMessage({
  status,
  hasData,
  error,
  height,
}: {
  status: Status;
  hasData: boolean;
  error: string;
  height: number;
}) {
  const { t } = useT();
  const text =
    status === 'loading'
      ? t('metric.loading')
      : status === 'unconfigured'
        ? t('metric.unconfigured')
        : status === 'unreachable'
          ? t('metric.unreachable')
          : status === 'error'
            ? t('metric.error', { error })
            : !hasData
              ? t('metric.noData')
              : '';
  return (
    <div
      className="flex items-center justify-center px-3 text-center text-xs text-muted"
      style={{ height }}
    >
      {text}
    </div>
  );
}

/** Собственно canvas uPlot: пересоздание на смену темы/высоты, setData — на данные. */
function Plot({ aligned, unit, height }: { aligned: { x: number[]; ys: (number | null)[][] }; unit: Unit; height: number }) {
  const holder = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const { theme } = useTheme();
  const { t } = useT();

  useEffect(() => {
    const el = holder.current;
    if (el === null) return;
    const accent = cssVar('--accent');
    const line = cssVar('--line');
    const muted = cssVar('--muted');
    const mono = cssVar('--font-mono') || 'monospace';
    const opts: uPlot.Options = {
      width: el.clientWidth || 320,
      height,
      legend: { show: false },
      cursor: { y: false, points: { size: 6 } },
      scales: { x: { time: true }, y: { range: (_u, _min, max) => [0, Math.max(1, max * 1.2)] } },
      axes: [
        {
          stroke: muted,
          grid: { stroke: line, width: 1 },
          ticks: { stroke: line, size: 4 },
          font: `11px ${mono}`,
        },
        {
          stroke: muted,
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          size: 48,
          font: `11px ${mono}`,
          values: (_u, splits) => splits.map((v) => formatMetric(v, unit)),
        },
      ],
      series: [
        {},
        { stroke: accent, width: 2, fill: hexWithAlpha(accent, 0.12), points: { show: false } },
      ],
    };
    const u = new uPlot(opts, [aligned.x, ...aligned.ys] as uPlot.AlignedData, el);
    plot.current = u;
    const ro = new ResizeObserver(() => {
      u.setSize({ width: el.clientWidth, height });
    });
    ro.observe(el);
    return () => {
      ro.disconnect();
      u.destroy();
      plot.current = null;
    };
    // Данные обновляет setData ниже; пересоздание — только тема/высота/unit.
  }, [theme, height, unit]);

  useEffect(() => {
    plot.current?.setData([aligned.x, ...aligned.ys] as uPlot.AlignedData);
  }, [aligned]);

  return <div ref={holder} role="img" aria-label={t('metric.aria')} />;
}

function lastValue(series: MetricSeries[]): number | null {
  for (const s of series) {
    for (let i = s.points.length - 1; i >= 0; i--) {
      const v = s.points[i][1];
      if (v !== null) return v;
    }
  }
  return null;
}

/** #RRGGBB → rgba(): заливка под линией (canvas не понимает color-mix). */
function hexWithAlpha(hex: string, alpha: number): string {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (m === null) return hex;
  const n = parseInt(m[1], 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
}
