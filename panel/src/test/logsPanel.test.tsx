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
/** onQuery может вернуть Response синхронно ЛИБО отдать «зависший» промис
 *  (для тестов гонки stale-ответа — вызов должен разрешиться позже, вручную). */
function mockFetch(onQuery: (callIndex: number) => Response | Promise<Response>) {
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

/** Значение одного query-параметра из (возможно percent-encoded) URL вызова. */
function paramValue(url: string, key: string): string | null {
  return new URLSearchParams(url.split('?')[1] ?? '').get(key);
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
    // Самая старая строка страницы 1 — с ДРОБНОЙ секундой: end следующего
    // запроса должен быть ровно её getTime()/1000 (без floor, без −1) — см.
    // Fix 1 финального ревью: VictoriaLogs' end эксклюзивен ([start, end)),
    // floor+(-1) на границе страницы молча топит строки (до ~2с при частом
    // потоке — например, крэш-вывод).
    const oldestTime = '2026-07-09T10:00:05.789Z';
    const page1 = Array.from({ length: 500 }, (_, i) =>
      ndjsonLine(i === 499 ? oldestTime : new Date(2026, 6, 9, 10, 0, 500 - i).toISOString(), `line-${String(i)}`),
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
    // Точное значение: дробные секунды оригинального времени самой старой
    // строки — НЕ floor, НЕ минус одна секунда.
    const expectedEnd = new Date(oldestTime).getTime() / 1000;
    expect(paramValue(secondUrl, 'end')).toBe(String(expectedEnd));
    // кнопка исчезает — вторая страница короче limit (500).
    expect(screen.queryByRole('button', { name: 'Show more' })).toBeNull();
  });

  it('«показать ещё» устарело (диапазон сменился, пока запрос летел) → устаревшая страница НЕ дописывается', async () => {
    let resolveStale!: (r: Response) => void;
    const stale = new Promise<Response>((resolve) => {
      resolveStale = resolve;
    });
    const page1 = Array.from({ length: 500 }, (_, i) =>
      ndjsonLine(new Date(2026, 6, 9, 10, 0, 500 - i).toISOString(), `line-${String(i)}`),
    );
    const fetchMock = mockFetch((n) => {
      if (n === 1) return new Response(ndjson(page1), { status: 200 });
      if (n === 2) return stale; // «показать ещё» — зависает, разрешим вручную позже
      return new Response(ndjson([ndjsonLine('2026-07-09T11:00:00Z', 'fresh-after-range-change')]), { status: 200 });
    });
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<LogsPanel serverId="srv-1" />);

    fireEvent.click(screen.getByRole('button', { name: 'History' }));
    expect(await screen.findByText('line-0')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Show more' })); // call #2 — зависает
    await waitFor(() => {
      expect(queryCalls(fetchMock).length).toBe(2);
    });

    // Диапазон меняется, пока «показать ещё» ещё в полёте — свежая загрузка (call #3).
    fireEvent.click(screen.getByRole('button', { name: '1 h' }));
    expect(await screen.findByText('fresh-after-range-change')).toBeTruthy();

    // Устаревший «показать ещё» долетает ПОСЛЕДНИМ — его страница не должна примешаться к новому состоянию.
    resolveStale(new Response(ndjson([ndjsonLine('2026-07-09T08:00:00Z', 'stale-should-not-appear')]), { status: 200 }));
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByText('stale-should-not-appear')).toBeNull();
    expect(screen.getByText('fresh-after-range-change')).toBeTruthy();
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
