// Дескрипторы окон Статистики (Task 4, "Статистика v1"): выбор окна решает
// источник данных — короткие окна (12ч/24ч/3д) читают операционные ряды
// напрямую из VictoriaMetrics через metrics-proxy (live), длинные (7д/30д)
// остаются на продуктовых агрегатах /v1/stats/* (product). Потолок — 30 дней
// (старая опция 90д снята: on-the-fly-скан ограничен, роллапы — Фаза 2).

import type { MessageKey } from './i18n';

export type StatsMode = 'live' | 'product';

export interface StatsRange {
  key: '12h' | '24h' | '3d' | '7d' | '30d';
  /** Ключ i18n подписи селектора (stats.range.*), переводится в компоненте. */
  labelKey: MessageKey;
  mode: StatsMode;
  /** Скользящее окно, мс — для live (MetricChart/UtilizationChart windowMs). */
  windowMs?: number;
  /** Период, дней — для product (api.statsOverview/statsCost). */
  days?: number;
}

const HOUR_MS = 60 * 60 * 1000;
const DAY_MS = 24 * HOUR_MS;

const RANGE_12H: StatsRange = { key: '12h', labelKey: 'stats.range.12h', mode: 'live', windowMs: 12 * HOUR_MS };
const RANGE_24H: StatsRange = { key: '24h', labelKey: 'stats.range.24h', mode: 'live', windowMs: 24 * HOUR_MS };
const RANGE_3D: StatsRange = { key: '3d', labelKey: 'stats.range.3d', mode: 'live', windowMs: 3 * DAY_MS };
const RANGE_7D: StatsRange = { key: '7d', labelKey: 'stats.range.7d', mode: 'product', days: 7 };
const RANGE_30D: StatsRange = { key: '30d', labelKey: 'stats.range.30d', mode: 'product', days: 30 };

/** Порядок селектора: 12ч/24ч/3д (live) → 7д/30д (product). */
export const STATS_RANGES: StatsRange[] = [RANGE_12H, RANGE_24H, RANGE_3D, RANGE_7D, RANGE_30D];

/** Дефолт — 24ч (live): гранулярный вид — посадочный, продуктовый обзор в один клик. */
export const DEFAULT_RANGE_KEY = '24h';

// Явно Map<string, ...>: rangeByKey принимает произвольную строку (URL/
// localStorage), а не только литералы StatsRange['key'].
const BY_KEY: Map<string, StatsRange> = new Map(STATS_RANGES.map((r) => [r.key, r]));

/** Дескриптор по ключу; неизвестный/пустой ключ → дефолт (не бросает). */
export function rangeByKey(key: string): StatsRange {
  return BY_KEY.get(key) ?? RANGE_24H;
}
