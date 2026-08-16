// Демо-роутер (src/demo/router.ts) — единственная сетевая правда демо-режима.
// Тест держит два обещания: известная ручка отвечает 200 разбираемым телом, а
// НЕизвестная кричит. Молчаливая пустая карточка на скриншоте хуже красной
// ошибки при съёмке.
import { describe, expect, it, vi } from 'vitest';
import { demoFetch } from '../demo/router';

async function get(path: string): Promise<{ status: number; body: unknown }> {
  const res = await demoFetch(path);
  const text = await res.text();
  return { status: res.status, body: text === '' ? undefined : JSON.parse(text) };
}

describe('demoFetch', () => {
  it('отдаёт admin-сессию: экран логина в демо не показывается', async () => {
    const { status, body } = await get('/v1/session');
    expect(status).toBe(200);
    expect(body).toMatchObject({ name: expect.any(String), scopes: expect.arrayContaining(['admin']) });
  });

  it('неизвестная ручка — 500 и громкий console.error', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { status } = await get('/v1/nope');
    expect(status).toBe(500);
    expect(spy).toHaveBeenCalledWith(expect.stringContaining('/v1/nope'));
    spy.mockRestore();
  });
});
