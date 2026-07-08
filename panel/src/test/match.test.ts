import { describe, expect, it } from 'vitest';
import type { Match } from '../lib/api';
import { isMatchOver, matchDurationMs, matchMetricRange } from '../lib/match';

function match(over: Partial<Match>): Match {
  return {
    id: 'm1',
    project: 'game',
    region: 'dev',
    state: 'finished',
    version_id: 'v1',
    semver: '0.1.1',
    server_id: 'srv-1',
    host: '10.0.0.1',
    port: 20000,
    server_players: 0,
    players_peak: 2,
    created_at: '2026-07-08T14:29:20Z',
    ...over,
  };
}

describe('matchDurationMs', () => {
  it('finished: ended − started', () => {
    const m = match({
      state: 'finished',
      started_at: '2026-07-08T14:30:00Z',
      ended_at: '2026-07-08T14:33:00Z',
    });
    // окно now не влияет на завершённый матч
    expect(matchDurationMs(m, Date.parse('2026-07-08T20:00:00Z'))).toBe(3 * 60_000);
  });

  it('running: тикает от started_at до now', () => {
    const m = match({ state: 'running', started_at: '2026-07-08T14:30:00Z', ended_at: undefined });
    const now = Date.parse('2026-07-08T14:30:45Z');
    expect(matchDurationMs(m, now)).toBe(45_000);
  });

  it('pending без started_at — от created_at', () => {
    const m = match({ state: 'pending', started_at: undefined, ended_at: undefined, created_at: '2026-07-08T14:29:20Z' });
    const now = Date.parse('2026-07-08T14:29:30Z');
    expect(matchDurationMs(m, now)).toBe(10_000);
  });

  it('битые даты → null; рассинхрон часов прижимается к нулю', () => {
    expect(matchDurationMs(match({ started_at: 'nope', created_at: 'nope' }), 0)).toBeNull();
    const m = match({ state: 'running', started_at: '2026-07-08T14:30:00Z', ended_at: undefined });
    expect(matchDurationMs(m, Date.parse('2026-07-08T14:29:00Z'))).toBe(0);
  });
});

describe('matchMetricRange (окно метрик)', () => {
  it('finished: статичное [started, ended] в unix-секундах', () => {
    const m = match({
      state: 'finished',
      started_at: '2026-07-08T14:30:00Z',
      ended_at: '2026-07-08T14:33:00Z',
    });
    const r = matchMetricRange(m);
    expect(r).not.toBeNull();
    expect(r?.start).toBe(Math.floor(Date.parse('2026-07-08T14:30:00Z') / 1000));
    expect(r?.end).toBe(Math.ceil(Date.parse('2026-07-08T14:33:00Z') / 1000));
  });

  it('running: открытый конец (end не задан → живое окно)', () => {
    const m = match({ state: 'running', started_at: '2026-07-08T14:30:00Z', ended_at: undefined });
    const r = matchMetricRange(m);
    expect(r?.start).toBe(Math.floor(Date.parse('2026-07-08T14:30:00Z') / 1000));
    expect(r?.end).toBeUndefined();
  });

  it('server_id вычищен → null (метрики не запрашиваем)', () => {
    expect(matchMetricRange(match({ server_id: '' }))).toBeNull();
  });
});

describe('isMatchOver', () => {
  it('finished/aborted или наличие ended_at — завершён', () => {
    expect(isMatchOver(match({ state: 'finished', ended_at: '2026-07-08T14:33:00Z' }))).toBe(true);
    expect(isMatchOver(match({ state: 'aborted', ended_at: undefined }))).toBe(true);
    expect(isMatchOver(match({ state: 'running', ended_at: undefined }))).toBe(false);
    expect(isMatchOver(match({ state: 'pending', ended_at: undefined }))).toBe(false);
  });
});
