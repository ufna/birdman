import { afterEach, describe, expect, it, vi } from 'vitest';
import { pumpTextStream, streamServerLogs } from '../lib/logs';
import type { ByteReader, LogStreamStatus } from '../lib/logs';

/** Ридер из массива чанков — стрим без сети. */
function readerOf(chunks: Uint8Array[]): ByteReader {
  let i = 0;
  return {
    read: () =>
      Promise.resolve(i < chunks.length ? { done: false, value: chunks[i++] } : { done: true }),
  };
}

const enc = (s: string) => new TextEncoder().encode(s);

describe('pumpTextStream', () => {
  it('склеивает чанки в текст по мере прихода', async () => {
    const out: string[] = [];
    await pumpTextStream(readerOf([enc('line1\n'), enc('line2\n')]), (c) => out.push(c));
    expect(out.join('')).toBe('line1\nline2\n');
  });

  it('корректно декодирует многобайтовый символ, разорванный между чанками', async () => {
    // «é» = 0xC3 0xA9; рвём между байтами — потоковый декодер должен собрать.
    const parts = [new Uint8Array([0x63, 0x61, 0x66, 0xc3]), new Uint8Array([0xa9, 0x0a])];
    let acc = '';
    await pumpTextStream(readerOf(parts), (c) => {
      acc += c;
    });
    expect(acc).toBe('café\n');
  });

  it('не эмитит пустые чанки', async () => {
    const out: string[] = [];
    await pumpTextStream(readerOf([enc(''), enc('x')]), (c) => out.push(c));
    expect(out).toEqual(['x']);
  });
});

describe('streamServerLogs', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('качает тело ответа в onText и проходит статусы connecting→streaming→ended', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('hello\nworld\n', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    const statuses: LogStreamStatus[] = [];
    let text = '';
    await streamServerLogs('srv-1', {
      tail: 200,
      follow: false,
      onText: (c) => {
        text += c;
      },
      onStatus: (s) => statuses.push(s),
    });
    expect(text).toBe('hello\nworld\n');
    expect(statuses[0]).toBe('connecting');
    expect(statuses).toContain('streaming');
    expect(statuses.at(-1)).toBe('ended');
    // Верный URL: follow опущен, tail проброшен.
    expect((fetchMock.mock.calls[0] as [string])[0]).toBe('/v1/servers/srv-1/logs?tail=200');
  });

  it('401 → статус unauthorized, тело не читается', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('', { status: 401 })));
    const statuses: LogStreamStatus[] = [];
    let text = '';
    await streamServerLogs('srv-1', {
      onText: (c) => {
        text += c;
      },
      onStatus: (s) => statuses.push(s),
    });
    expect(statuses.at(-1)).toBe('unauthorized');
    expect(text).toBe('');
  });

  it('404 → статус gone (логи вычищены/reaped), не бросает', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: 'not_found' }), { status: 404 })),
    );
    const statuses: LogStreamStatus[] = [];
    let text = '';
    await streamServerLogs('gone-srv', {
      onText: (c) => {
        text += c;
      },
      onStatus: (s) => statuses.push(s),
    });
    expect(statuses.at(-1)).toBe('gone');
    expect(text).toBe('');
  });

  it('follow=1 добавляется в URL', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    await streamServerLogs('a b', { follow: true, onText: () => {} });
    expect((fetchMock.mock.calls[0] as [string])[0]).toBe('/v1/servers/a%20b/logs?follow=1');
  });
});
