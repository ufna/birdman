import { describe, expect, it } from 'vitest';
import { ageOf, formatAge, heartbeatTone, shortId, summarizePayload } from '../lib/format';

describe('formatAge', () => {
  it('секунды', () => {
    expect(formatAge(0)).toBe('0с');
    expect(formatAge(3_000)).toBe('3с');
    expect(formatAge(59_900)).toBe('59с');
  });
  it('минуты с ведущим нулём секунд', () => {
    expect(formatAge(75_000)).toBe('1м 15с');
    expect(formatAge(600_000)).toBe('10м 00с');
  });
  it('часы и дни без секундного шума', () => {
    expect(formatAge(3_720_000)).toBe('1ч 2м');
    expect(formatAge(90 * 3_600_000)).toBe('3д 18ч');
  });
  it('рассинхрон часов прижимается к нулю', () => {
    expect(formatAge(-5_000)).toBe('0с');
  });
});

describe('heartbeatTone', () => {
  it('свежий — good, стареющий — warn, протухший — dead', () => {
    expect(heartbeatTone(2_000)).toBe('good');
    expect(heartbeatTone(7_000)).toBe('warn');
    expect(heartbeatTone(12_000)).toBe('dead');
  });
  it('нода без heartbeat — dead', () => {
    expect(heartbeatTone(null)).toBe('dead');
  });
});

describe('ageOf', () => {
  it('возраст от ISO-метки', () => {
    const now = Date.parse('2026-07-07T12:00:10Z');
    expect(ageOf('2026-07-07T12:00:00Z', now)).toBe(10_000);
  });
  it('нет метки или мусор — null', () => {
    expect(ageOf(undefined, 0)).toBeNull();
    expect(ageOf('not-a-date', 0)).toBeNull();
  });
});

describe('shortId', () => {
  it('обрезает uuid до 8 символов', () => {
    expect(shortId('a2f0c9d4-1111-2222-3333-444455556666')).toBe('a2f0c9d4');
    expect(shortId('short')).toBe('short');
  });
});

describe('summarizePayload', () => {
  it('плоские пары и вложенный JSON', () => {
    expect(summarizePayload({ region: 'eu', reason: 'no_capacity' })).toBe(
      'region=eu · reason=no_capacity',
    );
    expect(summarizePayload({ labels: { gpu: true } })).toBe('labels={"gpu":true}');
  });
  it('ограничивает длину', () => {
    const s = summarizePayload({ k: 'x'.repeat(300) }, 40);
    expect(s.length).toBe(40);
    expect(s.endsWith('…')).toBe(true);
  });
});
