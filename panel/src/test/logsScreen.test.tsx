import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { ReactElement } from 'react';
import { I18nProvider } from '../lib/i18n';

const openSpy = vi.fn();
// Экран открывает дровер дедика через lib/drawer (useServerDrawer) — мокаем
// хук, чтобы не тащить в тест весь DrawerProvider/ServerDrawer/API (см.
// брифу задачи 6: «пробрось/замокай так же, как в тестах существующих
// экранов» — существующих тестов на «клик → дровер» нет, поэтому выбран
// самый лёгкий вариант, явно допущенный формулировкой брифа).
vi.mock('../lib/drawer', () => ({
  useServerDrawer: () => ({ open: openSpy }),
}));

// Импорт ПОСЛЕ vi.mock, чтобы экран подхватил замоканный lib/drawer.
import { Logs } from '../screens/Logs';

const renderEn = (ui: ReactElement) => render(<I18nProvider initialLang="en">{ui}</I18nProvider>);

function ndjson(objs: Record<string, unknown>[]): string {
  return objs.length === 0 ? '' : objs.map((o) => JSON.stringify(o)).join('\n') + '\n';
}

/** Фабрика Response за каждым вызовом — тело читается один раз (fetch().text()). */
function mockFetch(respond: () => Response) {
  return vi.fn().mockImplementation(() => Promise.resolve(respond()));
}

afterEach(() => {
  vi.unstubAllGlobals();
  openSpy.mockClear();
});

describe('Logs — экран флит-поиска', () => {
  it('до первого поиска — подсказка, запрос ещё не улетал', () => {
    const fetchMock = mockFetch(() => new Response('', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<Logs />);
    expect(screen.getByText('Enter a search above and run it to see matching log lines.')).toBeTruthy();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('ввод текста + Enter → queryLogs вызван с текстом; результат виден с подсветкой; клик по строке открывает дровер дедика', async () => {
    const fetchMock = mockFetch(
      () =>
        new Response(
          ndjson([{ _time: '2026-07-09T10:00:00Z', _msg: 'boom happened here', server_id: 's1', node: 'n1', region: 'eu' }]),
          { status: 200 },
        ),
    );
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<Logs />);

    const input = screen.getByPlaceholderText('Search log text…');
    fireEvent.change(input, { target: { value: 'boom' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    expect(await screen.findByText('happened here', { exact: false })).toBeTruthy();
    const url = decodeURIComponent((fetchMock.mock.calls[0] as [string])[0]);
    expect(url).toContain('/v1/logs/query?');
    expect(url).toContain('"boom"');

    // Подсветка совпадения: ровно один <mark> с текстом запроса.
    const marks = document.querySelectorAll('mark');
    expect(marks).toHaveLength(1);
    expect(marks[0].textContent).toBe('boom');

    // Клик по строке результата открывает дровер дедика этого server_id.
    fireEvent.click(screen.getByText('s1'));
    expect(openSpy).toHaveBeenCalledWith('s1');
  });

  it('пусто (ok, 0 совпадений) → «ничего не найдено»', async () => {
    const fetchMock = mockFetch(() => new Response('', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<Logs />);
    fireEvent.submit(screen.getByPlaceholderText('Search log text…').closest('form') as HTMLFormElement);
    expect(await screen.findByText('No log lines matched your search.')).toBeTruthy();
  });

  it('unconfigured → мягкая заметка вместо ошибки (переиспользует ключ Task 5)', async () => {
    const fetchMock = mockFetch(() => new Response(JSON.stringify({ error: 'logs_unconfigured' }), { status: 503 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<Logs />);
    fireEvent.submit(screen.getByPlaceholderText('Search log text…').closest('form') as HTMLFormElement);
    expect(await screen.findByText("Log history isn't configured on this master (victorialogs_url is empty).")).toBeTruthy();
  });

  it('upstream → та же мягкая заметка недоступности, что и в дровере', async () => {
    const fetchMock = mockFetch(() => new Response(JSON.stringify({ error: 'upstream' }), { status: 502 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<Logs />);
    fireEvent.submit(screen.getByPlaceholderText('Search log text…').closest('form') as HTMLFormElement);
    expect(await screen.findByText('Log storage is unavailable right now — live tail keeps working.')).toBeTruthy();
  });

  it('жёсткая ошибка (500) → ErrorNote, не тихая заметка', async () => {
    const fetchMock = mockFetch(() => new Response(JSON.stringify({ error: 'internal', detail: 'boom' }), { status: 500 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<Logs />);
    fireEvent.submit(screen.getByPlaceholderText('Search log text…').closest('form') as HTMLFormElement);
    expect(await screen.findByText(/Couldn't load data/)).toBeTruthy();
  });

  it('фильтры region/node/server_id уходят в query как стрим-фильтры', async () => {
    const fetchMock = mockFetch(() => new Response('', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<Logs />);
    fireEvent.change(screen.getByPlaceholderText('Region'), { target: { value: 'eu' } });
    fireEvent.change(screen.getByPlaceholderText('Node'), { target: { value: 'n1' } });
    fireEvent.submit(screen.getByPlaceholderText('Search log text…').closest('form') as HTMLFormElement);
    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalled();
    });
    const url = decodeURIComponent((fetchMock.mock.calls[0] as [string])[0]);
    expect(url).toContain('{region="eu",node="n1"}');
  });
});
