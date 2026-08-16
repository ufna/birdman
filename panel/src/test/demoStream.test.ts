// Стрим — единственное место демо, где панель читает не JSON, а кадры SSE.
// Проверяем ТЕМ ЖЕ парсером, которым читает панель.
import { describe, expect, it, vi } from 'vitest';
import { buildFleet } from '../demo/fleet';
import { streamResponse } from '../demo/stream';
import { createSSEParser } from '../lib/sse';

const NOW = Date.UTC(2026, 7, 16, 19, 40, 0);

describe('streamResponse', () => {
  it('сразу отдаёт хвост ленты кадрами, которые понимает парсер панели', async () => {
    const res = streamResponse(buildFleet(NOW), undefined);
    const body = res.body;
    expect(body).not.toBeNull();
    const reader = body!.getReader();
    const parse = createSSEParser();
    const { value } = await reader.read();
    const frames = parse(new TextDecoder().decode(value));
    expect(frames.length).toBeGreaterThanOrEqual(12);
    expect(frames[0].id).toEqual(expect.any(Number));
    expect(frames[0].kind).toEqual(expect.any(String));
    expect(JSON.parse(frames[0].data ?? '{}')).toMatchObject({
      id: expect.any(Number),
      kind: expect.any(String),
    });
    await reader.cancel();
  });

  it('abort закрывает стрим и не оставляет таймер', async () => {
    vi.useFakeTimers();
    const ctrl = new AbortController();
    const res = streamResponse(buildFleet(NOW), ctrl.signal, 1000);
    const reader = res.body!.getReader();
    await reader.read();
    ctrl.abort();
    expect(vi.getTimerCount()).toBe(0);
    vi.useRealTimers();
  });
});
