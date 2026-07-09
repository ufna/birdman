import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { ReactElement } from 'react';
import { I18nProvider } from '../lib/i18n';
import { LogsPanel } from '../components/LogsPanel';

const renderEn = (ui: ReactElement) => render(<I18nProvider initialLang="en">{ui}</I18nProvider>);

function ndjsonLine(time: string, msg: string, extra: Record<string, string> = {}): string {
  return JSON.stringify({ _time: time, _msg: msg, server_id: 'srv-1', ...extra });
}

function ndjson(lines: string[]): string {
  return lines.length === 0 ? '' : lines.join('\n') + '\n';
}

/**
 * Мок fetch, различающий два эндпоинта: live-tail (`/v1/servers/{id}/logs`,
 * LogsPanel мгновенно монтирует его в Live-режиме по умолчанию — эти вызовы
 * просто "висят", чтобы не мешать) и историю (`/v1/logs/query`, отвечает
 * onQuery на каждый вызов СВОИМ Response — общий экземпляр Response нельзя
 * переиспользовать между вызовами, тело читается один раз).
 */
function mockFetch(onQuery: (callIndex: number) => Response) {
  let n = 0;
  return vi.fn().mockImplementation((url: string) => {
    if (!String(url).includes('/v1/logs/query')) return new Promise(() => {});
    n += 1;
    return Promise.resolve(onQuery(n));
  });
}

/** Вызовы именно к /v1/logs/query, в порядке — отфильтровывает игнорируемый live-tail. */
function queryCalls(fetchMock: ReturnType<typeof vi.fn>): string[] {
  return fetchMock.mock.calls.map((c) => String(c[0])).filter((u) => u.includes('/v1/logs/query'));
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('LogsPanel — сегмент Live | История', () => {
  it('дефолт — Live (LogViewer виден), запрос истории не летит', () => {
    const fetchMock = vi.fn(() => new Promise(() => {})); // live tail «висит» — connecting
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<LogsPanel serverId="srv-1" />);
    expect(screen.getByRole('button', { name: 'Live' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'History' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Live' }).getAttribute('aria-pressed')).toBe('true');
    // Маркер собственно LogViewer (tail-селектор) — Live-режим реально смонтирован байт-в-байт как раньше.
    expect(screen.getByText('tail')).toBeTruthy();
  });

  it('клик «История» → queryLogs вызван с query, содержащим {server_id="srv-1"}, строки отрисованы', async () => {
    const fetchMock = mockFetch(() => new Response(ndjson([ndjsonLine('2026-07-09T10:00:00Z', 'hello there')]), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<LogsPanel serverId="srv-1" />);

    fireEvent.click(screen.getByRole('button', { name: 'History' }));
    expect(await screen.findByText('hello there')).toBeTruthy();

    const [url] = queryCalls(fetchMock).map(decodeURIComponent);
    expect(url).toContain('/v1/logs/query?');
    expect(url).toContain('{server_id="srv-1"}');
  });

  it('unavailable/unconfigured → мягкая заметка (i18n-ключ), не ошибка', async () => {
    const fetchMock = mockFetch(() => new Response(JSON.stringify({ error: 'logs_unconfigured' }), { status: 503 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<LogsPanel serverId="srv-1" />);
    fireEvent.click(screen.getByRole('button', { name: 'History' }));
    expect(await screen.findByText("Log history isn't configured on this master (victorialogs_url is empty).")).toBeTruthy();
  });

  it('unavailable/upstream → заметка «хранилище недоступно, live работает»', async () => {
    const fetchMock = mockFetch(() => new Response(JSON.stringify({ error: 'upstream' }), { status: 502 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<LogsPanel serverId="srv-1" />);
    fireEvent.click(screen.getByRole('button', { name: 'History' }));
    expect(await screen.findByText('Log storage is unavailable right now — live tail keeps working.')).toBeTruthy();
  });

  it('пусто (ok, 0 строк) → «нет строк за период»', async () => {
    const fetchMock = mockFetch(() => new Response('', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<LogsPanel serverId="srv-1" />);
    fireEvent.click(screen.getByRole('button', { name: 'History' }));
    expect(await screen.findByText('No logs.')).toBeTruthy();
  });

  it('«показать ещё» → второй вызов несёт end (первый — нет), дозапись вниз списка', async () => {
    const page1 = Array.from({ length: 500 }, (_, i) =>
      ndjsonLine(new Date(2026, 6, 9, 10, 0, 500 - i).toISOString(), `line-${String(i)}`),
    );
    const fetchMock = mockFetch((n) =>
      n === 1
        ? new Response(ndjson(page1), { status: 200 })
        : new Response(ndjson([ndjsonLine('2026-07-09T09:00:00Z', 'older-line')]), { status: 200 }),
    );
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<LogsPanel serverId="srv-1" />);

    fireEvent.click(screen.getByRole('button', { name: 'History' }));
    expect(await screen.findByText('line-0')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Show more' })).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Show more' }));
    expect(await screen.findByText('older-line')).toBeTruthy();
    // первая страница всё ещё на месте — дозапись, а не замена.
    expect(screen.getByText('line-0')).toBeTruthy();

    const [firstUrl, secondUrl] = queryCalls(fetchMock);
    expect(firstUrl).not.toContain('end=');
    expect(secondUrl).toContain('end=');
    // кнопка исчезает — вторая страница короче limit (500).
    expect(screen.queryByRole('button', { name: 'Show more' })).toBeNull();
  });

  it('текстовый фильтр применяется по Enter (submit формы), не на каждую букву', async () => {
    const fetchMock = mockFetch(() => new Response('', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<LogsPanel serverId="srv-1" />);
    fireEvent.click(screen.getByRole('button', { name: 'History' }));
    await screen.findByText('No logs.');

    const callsBeforeTyping = queryCalls(fetchMock).length;
    const input = screen.getByPlaceholderText('Filter text…');
    fireEvent.change(input, { target: { value: 'boom' } });
    expect(queryCalls(fetchMock).length).toBe(callsBeforeTyping); // печать сама по себе не фетчит

    const form = input.closest('form');
    expect(form).toBeTruthy();
    fireEvent.submit(form as HTMLFormElement);

    await waitFor(() => {
      const calls = queryCalls(fetchMock).map(decodeURIComponent);
      expect(calls.at(-1)).toContain('"boom"');
    });
  });

  it('переключение на другой пресет диапазона перезапускает историю', async () => {
    const fetchMock = mockFetch(() => new Response('', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<LogsPanel serverId="srv-1" />);
    fireEvent.click(screen.getByRole('button', { name: 'History' }));
    await screen.findByText('No logs.');

    fireEvent.click(screen.getByRole('button', { name: '1 h' }));
    await waitFor(() => {
      expect(queryCalls(fetchMock).length).toBeGreaterThanOrEqual(2);
    });
  });
});
