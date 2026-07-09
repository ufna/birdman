// Дескрипторы окон Статистики (Task 4, "Статистика v1"): 12ч/24ч/3д — live
// (VictoriaMetrics напрямую), 7д/30д — product (/v1/stats/* агрегаты).
// Потолок — 30 дней (90д снят).

import { describe, expect, it } from 'vitest';
import { DEFAULT_RANGE_KEY, STATS_RANGES, rangeByKey } from '../lib/statsRange';

describe('STATS_RANGES / rangeByKey', () => {
  it('12h/24h/3d — live; 7d/30d — product; потолок 30д', () => {
    expect(STATS_RANGES.map((r) => r.key)).toEqual(['12h', '24h', '3d', '7d', '30d']);
    expect(rangeByKey('12h').mode).toBe('live');
    expect(rangeByKey('12h').windowMs).toBe(12 * 60 * 60 * 1000);
    expect(rangeByKey('30d').mode).toBe('product');
    expect(rangeByKey('30d').days).toBe(30);
  });

  it('windowMs верен для всех live-окон (12ч/24ч/3д)', () => {
    expect(rangeByKey('24h').mode).toBe('live');
    expect(rangeByKey('24h').windowMs).toBe(24 * 60 * 60 * 1000);
    expect(rangeByKey('3d').mode).toBe('live');
    expect(rangeByKey('3d').windowMs).toBe(3 * 24 * 60 * 60 * 1000);
  });

  it('days верен для всех product-окон (7д/30д), 90д больше нет в списке', () => {
    expect(rangeByKey('7d').days).toBe(7);
    expect(rangeByKey('30d').days).toBe(30);
    expect(STATS_RANGES.some((r) => (r.key as string) === '90d')).toBe(false);
  });

  it('дефолт — 24h (live)', () => {
    expect(DEFAULT_RANGE_KEY).toBe('24h');
    expect(rangeByKey(DEFAULT_RANGE_KEY).mode).toBe('live');
  });

  it('неизвестный/пустой ключ → fallback на дефолт, не бросает', () => {
    expect(rangeByKey('nope')).toEqual(rangeByKey(DEFAULT_RANGE_KEY));
    expect(rangeByKey('')).toEqual(rangeByKey(DEFAULT_RANGE_KEY));
  });

  it('у каждого дескриптора — непустой labelKey из каталога stats.range.*', () => {
    for (const r of STATS_RANGES) {
      expect(r.labelKey.startsWith('stats.range.')).toBe(true);
    }
  });
});
