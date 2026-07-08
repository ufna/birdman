// Чистые вью-модели для экранов Статистика/Cost (П2). Ответы master
// (lib/api.ts) → структуры, которые прямо рисует BarChart/ShareBars/UtilBar.
// Всё зеро-филлится на бэке (пустые дни = 0), поэтому здесь только раскладка
// и назначение цветов серий. Чистые функции — точка тестирования (stats.test).

import type { RegionUtil, SimpleSeries, StackedSeries, VersionShare } from './api';

/** Число слотов категориальной палитры (--cat-1..N в tokens.css). */
export const CATEGORICAL_SLOTS = 8;

/**
 * Цвет серии по её СТАБИЛЬНОМУ индексу (позиция ключа в отсортированном наборе
 * — цвет следует за сущностью, не за рангом; фильтр периода не перекрашивает
 * оставшиеся серии). >8 серий → нейтральный «прочее». Возвращает css-переменную
 * (графики в обычном DOM, canvas тут не нужен).
 */
export function seriesColor(index: number): string {
  if (index < 0 || index >= CATEGORICAL_SLOTS) return 'var(--cat-other)';
  return `var(--cat-${String(index + 1)})`;
}

/** Один сегмент колонки: ключ (регион/версия), значение, цвет серии. */
export interface StackSegment {
  key: string;
  value: number;
  color: string;
}

/** Колонка стек-графика: дата, итог за день и сегменты в порядке ключей (низ→верх). */
export interface BarColumn {
  date: string;
  total: number;
  segments: StackSegment[];
}

export interface StackModel {
  keys: string[];
  columns: BarColumn[];
  /** «Красивый» потолок оси Y (> 0 всегда). */
  max: number;
  unit: string;
  /** true, если все итоги нулевые (окно без данных). */
  empty: boolean;
}

/** Округляет потолок оси вверх до «красивого» числа (1/2/2.5/…/10 × 10^k). */
export function niceCeil(v: number): number {
  if (!Number.isFinite(v) || v <= 0) return 1;
  const pow = Math.pow(10, Math.floor(Math.log10(v)));
  const n = v / pow;
  const step = [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10].find((s) => n <= s + 1e-9) ?? 10;
  return Number((step * pow).toPrecision(2));
}

/** StackedSeries (ответ master) → модель для BarChart. Цвет — по индексу ключа. */
export function toStackModel(series: StackedSeries): StackModel {
  const colorByKey = new Map(series.keys.map((k, i) => [k, seriesColor(i)]));
  const columns: BarColumn[] = series.points.map((p) => ({
    date: p.date,
    total: p.total,
    segments: series.keys.map((k) => ({
      key: k,
      value: p.values[k] ?? 0,
      color: colorByKey.get(k) ?? 'var(--cat-other)',
    })),
  }));
  const peak = series.points.reduce((m, p) => Math.max(m, p.total), 0);
  return { keys: series.keys, columns, max: niceCeil(peak), unit: series.unit, empty: peak === 0 };
}

/** SimpleSeries → колонки с одним сегментом заданного цвета (по умолчанию accent). */
export function toSimpleColumns(series: SimpleSeries, color = 'var(--accent)'): {
  columns: BarColumn[];
  max: number;
  unit: string;
  empty: boolean;
} {
  const columns: BarColumn[] = series.points.map((p) => ({
    date: p.date,
    total: p.value,
    segments: [{ key: series.unit, value: p.value, color }],
  }));
  const peak = series.points.reduce((m, p) => Math.max(m, p.value), 0);
  return { columns, max: niceCeil(peak), unit: series.unit, empty: peak === 0 };
}

/** Строка распределения по версиям: доля + цвет (совпадает со слотом версии). */
export interface VersionShareRow {
  version: string;
  matches: number;
  share: number;
  color: string;
}

export function versionShareModel(dist: VersionShare[]): VersionShareRow[] {
  return dist.map((d, i) => ({ version: d.version, matches: d.matches, share: d.share, color: seriesColor(i) }));
}

/** Утилизация региона: занятость слотов по состояниям + свободный остаток. */
export interface UtilRow {
  region: string;
  capacity: number;
  allocated: number;
  ready: number;
  draining: number;
  /** capacity − (allocated+ready+draining), прижато к ≥ 0. */
  free: number;
  used: number;
  /** used / capacity в 0..1 (0 при нулевой ёмкости). */
  usedRatio: number;
}

export function utilizationModel(util: RegionUtil[]): UtilRow[] {
  return util.map((u) => {
    const used = u.allocated + u.ready + u.draining;
    const free = Math.max(0, u.capacity_slots - used);
    return {
      region: u.region,
      capacity: u.capacity_slots,
      allocated: u.allocated,
      ready: u.ready,
      draining: u.draining,
      free,
      used,
      usedRatio: u.capacity_slots > 0 ? used / u.capacity_slots : 0,
    };
  });
}

/** "2026-07-08" → "07-08" (компактная подпись оси X). Невалидное → как есть. */
export function shortDate(date: string): string {
  const m = /^\d{4}-(\d{2})-(\d{2})$/.exec(date);
  return m ? `${m[1]}-${m[2]}` : date;
}
