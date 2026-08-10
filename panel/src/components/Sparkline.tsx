// Спарклайн «матчи за час» на uPlot: одна серия в акценте, тонкая линия,
// лёгкая заливка, без осей и легенды — подпись и итог несёт карточка.

/* eslint-disable react-refresh/only-export-components -- bucketPerMinute — чистая раскладка по минутным корзинам, тестируется без рендера. Правило про гранулярность Fast Refresh в dev-сервере, не про корректность; разносить файл по модулям ради него дороже, чем оно стоит. Политика — в eslint.config.js. */

import { useEffect, useRef } from 'react';
import uPlot from 'uplot';
import 'uplot/dist/uPlot.min.css';
import { cssVar, useTheme } from '../lib/theme';
import { useT } from '../lib/i18n';

export interface SparkPoint {
  /** unix-время начала минутной корзины, сек */
  t: number;
  v: number;
}

export function Sparkline({ points, height = 72 }: { points: SparkPoint[]; height?: number }) {
  const holder = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const { theme } = useTheme(); // пересоздание при смене темы: canvas не умеет var()
  const { t } = useT();

  useEffect(() => {
    const el = holder.current;
    if (el === null) return;

    const accent = cssVar('--accent');
    const line = cssVar('--line');
    const opts: uPlot.Options = {
      width: el.clientWidth || 300,
      height,
      legend: { show: false },
      cursor: { y: false, points: { size: 6 } },
      scales: { x: { time: true }, y: { range: (_u, _min, max) => [0, Math.max(4, max * 1.25)] } },
      axes: [
        { show: false },
        {
          side: 1,
          stroke: cssVar('--muted'),
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          size: 28,
          font: `11px ${cssVar('--font-mono') || 'monospace'}`,
          incrs: [1, 2, 5, 10, 20, 50, 100],
        },
      ],
      series: [
        {},
        {
          stroke: accent,
          width: 2,
          fill: hexWithAlpha(accent, 0.14),
          points: { show: false },
        },
      ],
    };
    const data: uPlot.AlignedData = [points.map((p) => p.t), points.map((p) => p.v)];
    const u = new uPlot(opts, data, el);
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
    // Данные не в deps: их обновляет setData ниже, пересоздание —
    // только на смену темы/высоты.
    // eslint-disable-next-line react-hooks/exhaustive-deps -- см. выше: `points` здесь только начальные данные canvas'а, дальше их везёт setData. В deps они означали бы пересоздание uPlot на каждом обновлении ленты матчей.
  }, [theme, height]);

  useEffect(() => {
    plot.current?.setData([points.map((p) => p.t), points.map((p) => p.v)]);
  }, [points]);

  return <div ref={holder} role="img" aria-label={t('ov.sparkAria')} />;
}

/** #RRGGBB → rgba(): заливка под линией (canvas не понимает color-mix). */
function hexWithAlpha(hex: string, alpha: number): string {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (m === null) return hex;
  const n = parseInt(m[1], 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
}

/** Минутные корзины за последний час из created_at матчей. */
export function bucketPerMinute(createdAts: string[], nowMs: number): SparkPoint[] {
  const minute = 60_000;
  const end = Math.floor(nowMs / minute) * minute;
  const start = end - 59 * minute;
  const counts = new Array<number>(60).fill(0);
  for (const iso of createdAts) {
    const t = new Date(iso).getTime();
    if (Number.isNaN(t) || t < start || t >= end + minute) continue;
    counts[Math.floor((t - start) / minute)] += 1;
  }
  return counts.map((v, i) => ({ t: (start + i * minute) / 1000, v }));
}
