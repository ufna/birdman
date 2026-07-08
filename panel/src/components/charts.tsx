// Лёгкие графики П2 (Статистика/Cost) на обычном DOM+SVG — без внешних
// зависимостей и canvas: категориальные дневные бары зеро-филлены, тем важнее
// точный стек, 2px-зазоры между сегментами, легенда и прямые подписи (это же —
// вторичное кодирование идентичности для CVD). uPlot в проекте рисует ЛИНИИ
// живых метрик (MetricChart); дневные агрегаты — столбики, другой визуальный
// язык, для него DOM-раскладка даёт корректные темы и отсутствие
// горизонтального скролла «из коробки». Раскладка — чистые функции (charts.test).

import { useId } from 'react';
import type { ReactNode } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import { useT } from '../lib/i18n';
import type { BarColumn, UtilRow, VersionShareRow } from '../lib/stats';
import { shortDate } from '../lib/stats';

/** Индексы колонок, у которых показываем подпись оси X (≤ maxLabels, всегда последняя). */
export function axisTickIndices(n: number, maxLabels = 8): Set<number> {
  const out = new Set<number>();
  if (n <= 0) return out;
  const step = Math.max(1, Math.ceil(n / maxLabels));
  for (let i = 0; i < n; i += step) out.add(i);
  out.add(n - 1);
  return out;
}

interface BarChartProps {
  columns: BarColumn[];
  max: number;
  /** Ключи стека для легенды (регионы/версии). Легенда скрыта, если ≤ 1. */
  keys?: string[];
  keyLabel?: (key: string) => string;
  /** Формат значения (ось Y + тултип): int, slot-hours, длительность. */
  formatValue: (v: number) => string;
  height?: number;
  ariaLabel: string;
  emptyLabel: string;
  empty: boolean;
}

/** Дневной бар-чарт: один столбик на день, стек по ключам (низ→верх). */
export function BarChart({
  columns,
  max,
  keys,
  keyLabel,
  formatValue,
  height = 172,
  ariaLabel,
  emptyLabel,
  empty,
}: BarChartProps) {
  if (empty) {
    return (
      <div className="flex items-center justify-center text-center text-sm text-muted" style={{ height }}>
        {emptyLabel}
      </div>
    );
  }
  const ticks = axisTickIndices(columns.length);
  const n = columns.length;
  const gap = n > 45 ? 1 : n > 20 ? 2 : 3;
  const grid = { display: 'grid', gridTemplateColumns: `repeat(${String(n)}, minmax(0, 1fr))`, gap: `${String(gap)}px` };

  return (
    <div>
      <div className="relative" style={{ height }} role="img" aria-label={ariaLabel}>
        {/* Сетка Y: линии на max и max/2 с подписью, базовая линия на 0. */}
        {[1, 0.5].map((r) => (
          <div key={r} className="pointer-events-none absolute right-0 left-0 border-t border-line" style={{ top: `${String((1 - r) * 100)}%` }}>
            <span className="absolute -top-2 left-0 bg-card px-1 font-mono text-[10px] text-muted">{formatValue(max * r)}</span>
          </div>
        ))}
        <div className="absolute right-0 bottom-0 left-0 border-t border-line" />
        {/* Столбики. */}
        <div className="absolute inset-0" style={grid}>
          {columns.map((col) => (
            <Column key={col.date} col={col} max={max} formatValue={formatValue} keyLabel={keyLabel} />
          ))}
        </div>
      </div>
      {/* Ось X: те же треки, подписи — на выбранных днях. */}
      <div className="mt-1.5" style={grid}>
        {columns.map((col, i) => (
          <span key={col.date} className="overflow-visible text-center font-mono text-[10px] whitespace-nowrap text-muted">
            {ticks.has(i) ? shortDate(col.date) : ''}
          </span>
        ))}
      </div>
      {keys !== undefined && keys.length > 1 && (
        <SeriesLegend keys={keys} colorOf={(_k, i) => colorOfKeyIndex(columns, i)} keyLabel={keyLabel} />
      )}
    </div>
  );
}

/** Цвет ключа по его индексу — берём из первого дня (у всех дней порядок ключей один). */
function colorOfKeyIndex(columns: BarColumn[], i: number): string {
  return columns[0]?.segments[i]?.color ?? 'var(--cat-other)';
}

function Column({
  col,
  max,
  formatValue,
  keyLabel,
}: {
  col: BarColumn;
  max: number;
  formatValue: (v: number) => string;
  keyLabel?: (key: string) => string;
}) {
  const nonZero = col.segments.filter((s) => s.value > 0);
  const single = col.segments.length === 1;
  return (
    <Tooltip.Root>
      <Tooltip.Trigger asChild>
        <div className="flex h-full flex-col justify-end" tabIndex={0}>
          <div className="flex flex-col-reverse gap-[2px]" style={{ height: `${String((col.total / max) * 100)}%` }}>
            {col.segments.map((s) => (
              <div
                key={s.key}
                className="min-h-0 first:rounded-b-none last:rounded-t-[3px]"
                style={{ height: col.total > 0 ? `${String((s.value / col.total) * 100)}%` : '0%', background: s.color }}
              />
            ))}
          </div>
        </div>
      </Tooltip.Trigger>
      <Tooltip.Portal>
        <Tooltip.Content
          sideOffset={6}
          className="z-50 rounded-md border border-line bg-card px-2.5 py-1.5 text-xs text-ink shadow-md"
        >
          <div className="mb-1 font-mono text-[11px] text-muted">{col.date}</div>
          {single ? (
            <div className="tabular font-mono">{formatValue(col.total)}</div>
          ) : (
            <div className="flex flex-col gap-0.5">
              {nonZero.length === 0 ? (
                <span className="tabular font-mono">{formatValue(0)}</span>
              ) : (
                nonZero.map((s) => (
                  <div key={s.key} className="flex items-center justify-between gap-3">
                    <span className="flex items-center gap-1.5">
                      <span aria-hidden className="size-2 rounded-[2px]" style={{ background: s.color }} />
                      {keyLabel ? keyLabel(s.key) : s.key}
                    </span>
                    <span className="tabular font-mono">{formatValue(s.value)}</span>
                  </div>
                ))
              )}
              <div className="mt-0.5 flex items-center justify-between gap-3 border-t border-line pt-0.5 text-muted">
                <span>Σ</span>
                <span className="tabular font-mono">{formatValue(col.total)}</span>
              </div>
            </div>
          )}
        </Tooltip.Content>
      </Tooltip.Portal>
    </Tooltip.Root>
  );
}

/** Легенда серий: цветной чип + подпись ключа. */
export function SeriesLegend({
  keys,
  colorOf,
  keyLabel,
}: {
  keys: string[];
  colorOf: (key: string, i: number) => string;
  keyLabel?: (key: string) => string;
}) {
  return (
    <ul className="mt-3 flex flex-wrap gap-x-4 gap-y-1.5">
      {keys.map((k, i) => (
        <li key={k} className="flex items-center gap-1.5 text-xs text-muted">
          <span aria-hidden className="size-2.5 rounded-[3px]" style={{ background: colorOf(k, i) }} />
          <span className="font-mono">{keyLabel ? keyLabel(k) : k}</span>
        </li>
      ))}
    </ul>
  );
}

/** Горизонтальные бары долей по версиям (прямые подписи: версия · матчи · %). */
export function ShareBars({ rows, sharePct }: { rows: VersionShareRow[]; sharePct: (share: number) => string }) {
  const { t } = useT();
  if (rows.length === 0) {
    return <div className="py-8 text-center text-sm text-muted">{t('stats.noData')}</div>;
  }
  return (
    <ul className="flex flex-col gap-2.5">
      {rows.map((r) => (
        <li key={r.version} className="flex items-center gap-3">
          <span aria-hidden className="size-2.5 shrink-0 rounded-[3px]" style={{ background: r.color }} />
          <span className="w-14 shrink-0 font-mono text-xs">{r.version}</span>
          <span className="h-2 flex-1 overflow-hidden rounded-full bg-line" aria-hidden>
            <span className="block h-full rounded-full" style={{ width: `${String(Math.round(r.share * 100))}%`, background: r.color }} />
          </span>
          <span className="tabular w-24 shrink-0 text-right font-mono text-xs text-muted">
            {r.matches} · {sharePct(r.share)}
          </span>
        </li>
      ))}
    </ul>
  );
}

/** Метр утилизации региона: allocated/ready/draining vs ёмкость (состояния-тона). */
export function UtilBar({ row }: { row: UtilRow }) {
  const { t, tp } = useT();
  const pct = (v: number) => (row.capacity > 0 ? `${String(Math.round((v / row.capacity) * 100))}%` : '0%');
  const segs: { key: string; v: number; cls: string; labelKey: 'stats.util.allocated' | 'stats.util.ready' | 'stats.util.draining' }[] = [
    { key: 'allocated', v: row.allocated, cls: 'bg-accent', labelKey: 'stats.util.allocated' },
    { key: 'ready', v: row.ready, cls: 'bg-good', labelKey: 'stats.util.ready' },
    { key: 'draining', v: row.draining, cls: 'bg-warn', labelKey: 'stats.util.draining' },
  ];
  return (
    <div>
      <div className="mb-1.5 flex items-baseline justify-between gap-3">
        <span className="font-mono text-sm">{row.region}</span>
        <span className="tabular font-mono text-xs text-muted">
          {tp('stats.util.slots', row.capacity, { used: row.used, capacity: row.capacity })}
        </span>
      </div>
      <div className="flex h-2.5 gap-[2px] overflow-hidden rounded-full bg-line" role="img" aria-label={t('stats.util.aria', { region: row.region })}>
        {segs.map((s) => (s.v > 0 ? <span key={s.key} className={s.cls} style={{ width: pct(s.v) }} /> : null))}
      </div>
      <div className="mt-1.5 flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-muted">
        {segs.map((s) => (
          <span key={s.key} className="inline-flex items-center gap-1.5">
            <span aria-hidden className={`size-1.5 rounded-full ${s.cls}`} />
            {t(s.labelKey)} <span className="tabular font-mono text-ink">{s.v}</span>
          </span>
        ))}
        <span className="inline-flex items-center gap-1.5">
          <span aria-hidden className="size-1.5 rounded-full bg-line" />
          {t('stats.util.free')} <span className="tabular font-mono text-ink">{row.free}</span>
        </span>
      </div>
    </div>
  );
}

/** Сегментированный переключатель периода (7 / 30 / 90 дней). */
export function PeriodSelect({ value, onChange, options }: { value: number; onChange: (v: number) => void; options: number[] }) {
  const { t } = useT();
  return (
    <div role="group" aria-label={t('stats.periodAria')} className="inline-flex overflow-hidden rounded-lg border border-line text-xs">
      {options.map((o) => {
        const active = o === value;
        return (
          <button
            key={o}
            type="button"
            onClick={() => {
              onChange(o);
            }}
            aria-pressed={active}
            className={`px-2.5 py-1 font-mono font-medium transition-colors ${active ? 'bg-mark text-accent-ink' : 'text-muted hover:text-ink'}`}
          >
            {o}
          </button>
        );
      })}
      <span className="border-l border-line px-2 py-1 text-muted">{t('stats.daysUnit')}</span>
    </div>
  );
}

/** Заголовок секции внутри карточки статистики (подпись + опц. правый элемент). */
export function ChartHeading({ title, note, aside }: { title: string; note?: string; aside?: ReactNode }) {
  const labelId = useId();
  return (
    <div className="mb-3 flex items-baseline justify-between gap-3">
      <div>
        <h3 id={labelId} className="text-sm font-semibold">
          {title}
        </h3>
        {note !== undefined && <p className="mt-0.5 text-xs text-muted">{note}</p>}
      </div>
      {aside}
    </div>
  );
}
