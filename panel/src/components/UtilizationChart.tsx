// Утилизация тачек ВО ВРЕМЕНИ (Cost, panel.md §6): дедики по состоянию за
// период через metrics-proxy query_range (`sum by (state)(birdman_servers)`).
// Линия на состояние (allocated/ready/draining/creating) — те же тона, что у
// метра-снапшота. Дополняет снапшот: видно, как буфер и занятость менялись.
// Мягкая деградация: VM не настроена/недоступна/нет данных → подпись, не падение.

import { useEffect, useMemo, useRef } from 'react';
import uPlot from 'uplot';
import 'uplot/dist/uPlot.min.css';
import { cssVar, useTheme } from '../lib/theme';
import { useT } from '../lib/i18n';
import { toAlignedData, formatMetric, serversByStateQuery, utilizationSeriesModel } from '../lib/metrics';
import type { StateSeries } from '../lib/metrics';
import { useQueryRange } from '../lib/useMetrics';
import { MetricMessage } from './MetricMessage';

/** Период по умолчанию для окна утилизации — 6 часов (dev-данные разрежены). */
const DEFAULT_WINDOW_MS = 6 * 60 * 60_000;

export function UtilizationChart({
  windowMs = DEFAULT_WINDOW_MS,
  height = 180,
  refreshMs = 30_000,
}: {
  windowMs?: number;
  height?: number;
  refreshMs?: number;
}) {
  const { t } = useT();
  const query = serversByStateQuery();
  const { status, series, errorCode } = useQueryRange({ query, windowMs, refreshMs });

  const states = useMemo(() => utilizationSeriesModel(series ?? []), [series]);
  const aligned = useMemo(
    () => toAlignedData(states.map((s) => ({ labels: {}, name: s.state, points: s.points }))),
    [states],
  );
  const hasData = states.length > 0 && aligned.x.length > 0;

  return (
    <div className="p-4">
      {status === 'ok' && hasData ? (
        <>
          <Plot aligned={aligned} states={states} height={height} />
          <ul className="mt-3 flex flex-wrap gap-x-4 gap-y-1.5">
            {states.map((s) => (
              <li key={s.state} className="flex items-center gap-1.5 text-xs text-muted">
                <span aria-hidden className="size-2.5 rounded-[3px]" style={{ background: s.color }} />
                <span>{t(s.labelKey)}</span>
                <span className="tabular font-mono text-ink">{lastValue(s)}</span>
              </li>
            ))}
          </ul>
        </>
      ) : (
        <MetricMessage status={status} hasData={hasData} errorCode={errorCode} height={height} />
      )}
    </div>
  );
}

/** Последнее непустое значение ряда (для легенды). */
function lastValue(s: StateSeries): string {
  for (let i = s.points.length - 1; i >= 0; i--) {
    const v = s.points[i][1];
    if (v !== null) return String(v);
  }
  return '—';
}

/** uPlot: ось времени + линия на состояние своим тоном. Пересоздание — тема/высота. */
function Plot({
  aligned,
  states,
  height,
}: {
  aligned: { x: number[]; ys: (number | null)[][] };
  states: StateSeries[];
  height: number;
}) {
  const holder = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const { theme } = useTheme();
  const { t } = useT();
  // Цвета серий читаем из CSS-переменных (canvas не понимает var()).
  const colors = states.map((s) => resolveVar(s.color));
  // Ключ по ЗНАЧЕНИЮ, а не по идентичности: `colors` — новый массив каждый
  // рендер, в deps он бы пересоздавал график постоянно. Отдельной переменной
  // (а не выражением прямо в deps), чтобы список зависимостей оставался
  // статически проверяемым — иначе линтер не видит, что там.
  const colorsKey = colors.join(',');

  useEffect(() => {
    const el = holder.current;
    if (el === null) return;
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
        { stroke: muted, grid: { stroke: line, width: 1 }, ticks: { stroke: line, size: 4 }, font: `11px ${mono}` },
        {
          stroke: muted,
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          size: 40,
          font: `11px ${mono}`,
          values: (_u, splits) => splits.map((v) => formatMetric(v, 'int')),
        },
      ],
      series: [{}, ...colors.map((c) => ({ stroke: c, width: 2, points: { show: false } }))],
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
    // Данные обновляет setData ниже; пересоздание — тема/высота/набор серий.
    // eslint-disable-next-line react-hooks/exhaustive-deps -- aligned.x/ys здесь только начальные данные canvas'а (дальше setData из эффекта ниже), а `colors` отслеживается по значению через colorsKey: набор серий меняется вместе с ним, идентичность массива — нет.
  }, [theme, height, colorsKey]);

  useEffect(() => {
    plot.current?.setData([aligned.x, ...aligned.ys] as uPlot.AlignedData);
  }, [aligned]);

  return <div ref={holder} role="img" aria-label={t('cost.utilOverTime')} />;
}

/** var(--x) → фактический цвет темы для canvas. */
function resolveVar(v: string): string {
  const m = /^var\((--[\w-]+)\)$/.exec(v.trim());
  return m ? cssVar(m[1]) || '#888' : v;
}
