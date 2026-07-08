import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError, qs } from '../lib/api';

function mockFetch(status: number, body: unknown, contentType = 'application/json') {
  // 204 обязан быть без тела (инвариант конструктора Response).
  const text = body === undefined ? null : JSON.stringify(body);
  const fn = vi.fn().mockResolvedValue(
    new Response(text, { status, headers: { 'Content-Type': contentType } }),
  );
  vi.stubGlobal('fetch', fn);
  return fn;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('qs', () => {
  it('пропускает пустые значения и кодирует остальные', () => {
    expect(qs({ state: 'running', region: undefined, limit: 50, q: '' })).toBe(
      '?state=running&limit=50',
    );
    expect(qs({})).toBe('');
    expect(qs({ r: 'eu west' })).toBe('?r=eu%20west');
  });
});

describe('api-клиент', () => {
  it('listMatches строит query и разворачивает конверт {matches}', async () => {
    const fetchMock = mockFetch(200, { matches: [{ id: 'm1', state: 'running' }] });
    const matches = await api.listMatches({ state: 'running', limit: 50, offset: 100 });
    expect(matches).toEqual([{ id: 'm1', state: 'running' }]);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/v1/matches?state=running&limit=50&offset=100');
    expect(init.method).toBe('GET');
    expect(init.credentials).toBe('same-origin');
    // GET не несёт CSRF-заголовок.
    expect((init.headers as Record<string, string>)['X-Birdman-Csrf']).toBeUndefined();
  });

  it('ошибки API превращаются в ApiError с кодом из тела', async () => {
    mockFetch(403, { error: 'forbidden', detail: 'scope readonly required' });
    const err = await api.listNodes().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    const apiErr = err as ApiError;
    expect(apiErr.status).toBe(403);
    expect(apiErr.code).toBe('forbidden');
    expect(apiErr.detail).toBe('scope readonly required');
  });

  it('не-JSON ответ об ошибке не роняет клиента', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response('<html>bad gateway</html>', { status: 502 })),
    );
    const err = (await api.listNodes().catch((e: unknown) => e)) as ApiError;
    expect(err.status).toBe(502);
    expect(err.code).toBe('bad_response');
  });

  it('createSession шлёт api_key в теле и CSRF-заголовок', async () => {
    const fetchMock = mockFetch(200, { scopes: ['admin'], name: 'boot' });
    const s = await api.createSession('bmk_x');
    expect(s.scopes).toEqual(['admin']);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/v1/session');
    expect(init.method).toBe('POST');
    expect(init.body).toBe(JSON.stringify({ api_key: 'bmk_x' }));
    const headers = init.headers as Record<string, string>;
    expect(headers['X-Birdman-Csrf']).toBe('1');
    expect(headers['Content-Type']).toBe('application/json');
  });

  it('deleteSession переживает 204 без тела', async () => {
    const fetchMock = mockFetch(204, undefined);
    await expect(api.deleteSession()).resolves.toBeUndefined();
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(init.method).toBe('DELETE');
    expect((init.headers as Record<string, string>)['X-Birdman-Csrf']).toBe('1');
  });

  it('getMatch кодирует id в пути', async () => {
    const fetchMock = mockFetch(200, { match: { id: 'x' } });
    await api.getMatch('a/b');
    expect((fetchMock.mock.calls[0] as [string])[0]).toBe('/v1/matches/a%2Fb');
  });
});
