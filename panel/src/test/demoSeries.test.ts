// Ряды проверяем ТЕМ ЖЕ парсером, которым их читает панель (parseMatrix/
// parseVector), — иначе тест проверял бы наши намерения, а не контракт.
import { describe, expect, it } from 'vitest';
import { matrixFor, vectorFor } from '../demo/series';
import {
  parseMatrix,
  parseVector,
  playersOnlineQuery,
  queueDepthQuery,
  serversByStateQuery,
  timeToMatchQuantileQuery,
  utilizationRatioQuery,
} from '../lib/metrics';

const END = Math.floor(Date.UTC(2026, 7, 16, 19, 40, 0) / 1000);
const START = END - 24 * 3600;
const STEP = 300;
const range = (q: string) => parseMatrix(matrixFor(q, START, END, STEP));

describe('matrixFor', () => {
  it('заполняет запрошенное окно целиком', () => {
    const s = range(playersOnlineQuery());
    expect(s.length).toBe(1);
    expect(s[0].points.length).toBe(Math.floor((END - START) / STEP) + 1);
    expect(s[0].points[0][0]).toBe(START);
    expect(s[0].points[s[0].points.length - 1][0]).toBe(END);
    expect(s[0].points.every(([, v]) => v !== null)).toBe(true);
  });

  it('у суток есть форма, а не полка', () => {
    const vs = range(playersOnlineQuery())[0].points.map(([, v]) => v ?? 0);
    expect(Math.max(...vs)).toBeGreaterThan(Math.min(...vs) * 2);
  });

  it('очередь разбита по регионам', () => {
    expect(
      range(queueDepthQuery())
        .map((s) => s.labels.region)
        .sort(),
    ).toEqual(['ap-southeast', 'eu-central', 'us-east']);
  });

  it('утилизация — доля 0..1, панель сама сделает проценты', () => {
    expect(range(utilizationRatioQuery())[0].points.every(([, v]) => (v ?? -1) > 0 && (v ?? 2) < 1)).toBe(true);
  });

  it('дедики по состояниям дают все четыре живых состояния стека', () => {
    expect(
      range(serversByStateQuery())
        .map((s) => s.labels.state)
        .sort(),
    ).toEqual(['allocated', 'creating', 'draining', 'ready']);
  });

  it('незнакомый запрос — пусто, а не ошибка', () => {
    expect(range('sum(nonsense_metric)')).toEqual([]);
  });
});

describe('vectorFor', () => {
  it('перцентили time-to-match', () => {
    expect(parseVector(vectorFor(timeToMatchQuantileQuery(0.5, 7), END))[0].value).toBeCloseTo(7.4, 1);
    expect(parseVector(vectorFor(timeToMatchQuantileQuery(0.95, 7), END))[0].value).toBeCloseTo(21.8, 1);
  });
});
