import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../lib/api';
import { parseLogLines, queryLogs } from '../lib/logsHistory';

describe('parseLogLines', () => {
  it('парсит валидный ndjson: _time/_msg выделяются, остальное — в fields', () => {
    const text = [
      '{"_time":"2026-07-09T10:00:00Z","_msg":"hello","server_id":"s1","node":"n1","region":"dev"}',
      '{"_time":"2026-07-09T10:00:01Z","_msg":"world","server_id":"s1","node":"n1","region":"dev"}',
    ].join('\n');
    const lines = parseLogLines(text);
    expect(lines).toHaveLength(2);
    expect(lines[0]).toEqual({
      time: '2026-07-09T10:00:00Z',
      msg: 'hello',
      fields: { server_id: 's1', node: 'n1', region: 'dev' },
    });
    expect(lines[1].msg).toBe('world');
  });

  it('мусорная строка (невалидный JSON) молча пропускается', () => {
    const text = ['not json', '{"_time":"2026-07-09T10:00:00Z","_msg":"ok"}', '{broken'].join('\n');
    const lines = parseLogLines(text);
    expect(lines).toHaveLength(1);
    expect(lines[0].msg).toBe('ok');
  });

  it('валидный JSON без _time (не строка лога VL) пропускается', () => {
    expect(parseLogLines('{"_msg":"no time"}')).toEqual([]);
    expect(parseLogLines('[1,2,3]')).toEqual([]);
    expect(parseLogLines('null')).toEqual([]);
  });

  it('пустой вход (в т.ч. только переводы строк) → []', () => {
    expect(parseLogLines('')).toEqual([]);
    expect(parseLogLines('\n\n')).toEqual([]);
  });

  it('_msg отсутствует → пустая строка сообщения, остальное всё равно в fields', () => {
    const [line] = parseLogLines('{"_time":"2026-07-09T10:00:00Z","server_id":"s1"}');
    expect(line.msg).toBe('');
    expect(line.fields).toEqual({ server_id: 's1' });
  });
});

describe('queryLogs (GET /v1/logs/query)', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('200 ndjson → ok + распарсенные строки; URL несёт query/start/end/limit', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response('{"_time":"2026-07-09T10:00:00Z","_msg":"hello","server_id":"s1"}\n', { status: 200 }),
    );
    vi.stubGlobal('fetch', fetchMock);
    const res = await queryLogs({ query: '{server_id="s1"}', start: 1, end: 2, limit: 500 });
    expect(res.kind).toBe('ok');
    if (res.kind === 'ok') {
      expect(res.lines).toHaveLength(1);
      expect(res.lines[0].msg).toBe('hello');
    }
    const url = (fetchMock.mock.calls[0] as [string])[0];
    expect(url).toContain('/v1/logs/query?');
    expect(url).toContain('query=%7Bserver_id%3D%22s1%22%7D');
    expect(url).toContain('start=1');
    expect(url).toContain('end=2');
    expect(url).toContain('limit=500');
  });

  it('200 без совпадений (пустое тело) → ok с пустым списком строк', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('', { status: 200 })));
    expect(await queryLogs({ query: '*' })).toEqual({ kind: 'ok', lines: [] });
  });

  it('503 logs_unconfigured → unavailable/unconfigured, не бросает', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: 'logs_unconfigured', detail: 'victorialogs_url is not set' }), {
          status: 503,
        }),
      ),
    );
    expect(await queryLogs({ query: '*' })).toEqual({ kind: 'unavailable', reason: 'unconfigured' });
  });

  it('502 upstream → unavailable/upstream, не бросает', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: 'upstream', detail: 'connection refused' }), { status: 502 })),
    );
    expect(await queryLogs({ query: '*' })).toEqual({ kind: 'unavailable', reason: 'upstream' });
  });

  it('504 (таймаут апстрима, без тела) → unavailable/upstream', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('', { status: 504 })));
    expect(await queryLogs({ query: '*' })).toEqual({ kind: 'unavailable', reason: 'upstream' });
  });

  it('500 → бросает ApiError', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: 'internal', detail: 'boom' }), { status: 500 })),
    );
    const err = await queryLogs({ query: '*' }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(500);
  });

  it('400 bad_request (кривой limit) → бросает ApiError', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: 'bad_request', detail: 'limit must be a positive integer' }), { status: 400 })),
    );
    const err = await queryLogs({ query: '*', limit: -1 }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe('bad_request');
  });
});
