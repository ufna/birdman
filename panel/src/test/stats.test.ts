import { describe, expect, it } from 'vitest';
import type { StackedSeries, SimpleSeries, VersionShare, RegionUtil } from '../lib/api';
import {
  niceCeil,
  seriesColor,
  shortDate,
  toSimpleColumns,
  toStackModel,
  utilizationModel,
  versionShareModel,
} from '../lib/stats';

// Фикстуры — реальные формы ответов бокса (127.0.0.1:8100, П2-бэкенд).

const matchesPerDay: StackedSeries = {
  unit: 'matches/day',
  keys: ['dev'],
  points: [
    { date: '2026-07-06', total: 0, values: { dev: 0 } },
    { date: '2026-07-07', total: 0, values: { dev: 0 } },
    { date: '2026-07-08', total: 10, values: { dev: 10 } },
  ],
};

const byVersion: StackedSeries = {
  unit: 'slot-hours',
  keys: ['0.1.0', '0.1.1', '0.1.2', '0.9.0'],
  points: [
    { date: '2026-07-07', total: 0, values: { '0.1.0': 0, '0.1.1': 0, '0.1.2': 0, '0.9.0': 0 } },
    { date: '2026-07-08', total: 0.16, values: { '0.1.0': 0.02, '0.1.1': 0.09, '0.1.2': 0.02, '0.9.0': 0.03 } },
  ],
};

const peakCcu: SimpleSeries = {
  unit: 'players (peak concurrent)',
  points: [
    { date: '2026-07-07', value: 0 },
    { date: '2026-07-08', value: 2 },
  ],
};

describe('seriesColor — стабильный слот по индексу', () => {
  it('слоты 1..8, переполнение и отрицательный → other', () => {
    expect(seriesColor(0)).toBe('var(--cat-1)');
    expect(seriesColor(7)).toBe('var(--cat-8)');
    expect(seriesColor(8)).toBe('var(--cat-other)');
    expect(seriesColor(-1)).toBe('var(--cat-other)');
  });
});

describe('niceCeil', () => {
  it('округляет вверх до «красивого» потолка', () => {
    expect(niceCeil(0)).toBe(1);
    expect(niceCeil(10)).toBe(10);
    expect(niceCeil(18)).toBe(20);
    expect(niceCeil(52.29)).toBe(60);
    expect(niceCeil(0.15)).toBeCloseTo(0.15, 6);
    expect(niceCeil(0.16)).toBeCloseTo(0.2, 6);
  });
});

describe('shortDate', () => {
  it('YYYY-MM-DD → MM-DD, мусор — как есть', () => {
    expect(shortDate('2026-07-02')).toBe('07-02');
    expect(shortDate('nope')).toBe('nope');
  });
});

describe('toStackModel', () => {
  it('раскладывает стек по ключам, цвет — по индексу, потолок — nice', () => {
    const m = toStackModel(matchesPerDay);
    expect(m.keys).toEqual(['dev']);
    expect(m.columns).toHaveLength(3);
    expect(m.max).toBe(10);
    expect(m.empty).toBe(false);
    // последний день: 10 матчей в единственном регионе, цвет cat-1
    const last = m.columns[2];
    expect(last.total).toBe(10);
    expect(last.segments).toEqual([{ key: 'dev', value: 10, color: 'var(--cat-1)' }]);
  });

  it('многосерийный стек: у каждого дня все ключи (0-filled), цвета cat-1..4', () => {
    const m = toStackModel(byVersion);
    expect(m.keys).toEqual(['0.1.0', '0.1.1', '0.1.2', '0.9.0']);
    expect(m.columns[1].segments.map((s) => s.color)).toEqual([
      'var(--cat-1)',
      'var(--cat-2)',
      'var(--cat-3)',
      'var(--cat-4)',
    ]);
    expect(m.columns[1].segments.map((s) => s.value)).toEqual([0.02, 0.09, 0.02, 0.03]);
    expect(m.max).toBeCloseTo(0.2, 6);
  });

  it('полностью нулевое окно → empty=true, max=1', () => {
    const zero: StackedSeries = {
      unit: 'matches/day',
      keys: ['dev'],
      points: [
        { date: '2026-07-07', total: 0, values: { dev: 0 } },
        { date: '2026-07-08', total: 0, values: { dev: 0 } },
      ],
    };
    const m = toStackModel(zero);
    expect(m.empty).toBe(true);
    expect(m.max).toBe(1);
  });
});

describe('toSimpleColumns', () => {
  it('одиночный ряд → один accent-сегмент на день', () => {
    const s = toSimpleColumns(peakCcu);
    expect(s.empty).toBe(false);
    expect(s.max).toBe(2);
    expect(s.columns[1].segments).toEqual([{ key: peakCcu.unit, value: 2, color: 'var(--accent)' }]);
  });

  it('нулевой ряд → empty=true', () => {
    expect(toSimpleColumns({ unit: 'seconds', points: [{ date: 'd', value: 0 }] }).empty).toBe(true);
  });
});

describe('versionShareModel', () => {
  it('доли + цвета по порядку', () => {
    const dist: VersionShare[] = [
      { version: '0.1.0', matches: 4, share: 0.4 },
      { version: '0.1.1', matches: 3, share: 0.3 },
    ];
    expect(versionShareModel(dist)).toEqual([
      { version: '0.1.0', matches: 4, share: 0.4, color: 'var(--cat-1)' },
      { version: '0.1.1', matches: 3, share: 0.3, color: 'var(--cat-2)' },
    ]);
  });
});

describe('utilizationModel', () => {
  it('free = capacity − used, прижато к ≥ 0', () => {
    const util: RegionUtil[] = [{ region: 'dev', capacity_slots: 8, allocated: 0, ready: 2, draining: 0 }];
    expect(utilizationModel(util)).toEqual([
      { region: 'dev', capacity: 8, allocated: 0, ready: 2, draining: 0, free: 6, used: 2, usedRatio: 0.25 },
    ]);
  });

  it('переподписка (used > capacity) не даёт отрицательный free', () => {
    const util: RegionUtil[] = [{ region: 'x', capacity_slots: 2, allocated: 3, ready: 1, draining: 0 }];
    const [row] = utilizationModel(util);
    expect(row.free).toBe(0);
    expect(row.used).toBe(4);
  });

  it('нулевая ёмкость → usedRatio 0 (без деления на ноль)', () => {
    const [row] = utilizationModel([{ region: 'z', capacity_slots: 0, allocated: 0, ready: 0, draining: 0 }]);
    expect(row.usedRatio).toBe(0);
  });
});
