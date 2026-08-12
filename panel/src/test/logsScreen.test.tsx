import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
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

// lib/logsHistory оборачиваем в вызывающий-настоящую-реализацию мок: по
// умолчанию queryLogs ведёт себя как обычно (через глобальный fetch, как во
// всех остальных тестах этого файла), но для теста на stale-response race
// нужно детерминированно управлять ПОРЯДКОМ резолва двух конкретных вызовов —
// с сырым mockFetch это невозможно (тело Response читается асинхронно и
// порядок resolve нельзя развести без гонки внутри самого мока).
vi.mock('../lib/logsHistory', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/logsHistory')>();
  return { ...actual, queryLogs: vi.fn(actual.queryLogs) };
});

// Импорт ПОСЛЕ vi.mock, чтобы экран подхватил замоканные lib/drawer и lib/logsHistory.
import { Logs } from '../screens/Logs';
import { queryLogs } from '../lib/logsHistory';
import type { LogsResult } from '../lib/logsHistory';

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
  vi.mocked(queryLogs).mockClear();
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

  // tracker #1076. Ради этой ветки карточка и заведена: #1007 сделал
  // logs_narrowing_unsupported ШТАТНЫМ исходом для привязанного ключа на
  // апстриме, не разбирающем extra_stream_filters, а экран показывал его
  // жёсткой ошибкой с машинным кодом. Проверяем ОБА каталога: строка должна
  // называть причину (апстрим, не логи) и опцию, которую чинить оператору.
  it('narrowing (503 logs_narrowing_unsupported) → своя строка каталога EN, не ErrorNote и не «хранилище недоступно»', async () => {
    const fetchMock = mockFetch(() => new Response(JSON.stringify({ error: 'logs_narrowing_unsupported' }), { status: 503 }));
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<Logs />);
    fireEvent.submit(screen.getByPlaceholderText('Search log text…').closest('form') as HTMLFormElement);
    expect(
      await screen.findByText(
        'Log history is hidden: this master cannot narrow the search to your project — the configured VictoriaLogs does not understand the extra_stream_filters query arg. The operator has to fix victorialogs_url (self-host docs, §4). Live tail keeps working.',
      ),
    ).toBeTruthy();
    expect(screen.queryByText('Log storage is unavailable right now — live tail keeps working.')).toBeNull();
    expect(screen.queryByText(/Couldn't load data/)).toBeNull();
  });

  it('narrowing — та же ветка в RU: живой перевод, а не английский текст мастера', async () => {
    const fetchMock = mockFetch(() => new Response(JSON.stringify({ error: 'logs_narrowing_unsupported' }), { status: 503 }));
    vi.stubGlobal('fetch', fetchMock);
    render(
      <I18nProvider initialLang="ru">
        <Logs />
      </I18nProvider>,
    );
    fireEvent.submit(screen.getByPlaceholderText('Поиск по тексту логов…').closest('form') as HTMLFormElement);
    expect(await screen.findByText(/этот master не может сузить поиск до вашего проекта/)).toBeTruthy();
    expect(screen.getByText(/victorialogs_url/)).toBeTruthy();
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

  it('гонка stale-ответа: поздний ответ на устаревший поиск A не переписывает уже применённый результат более нового B', async () => {
    // Тексты поиска и сообщений НЕ пересекаются подстрокой — иначе highlight()
    // разбивает сообщение на несколько DOM-узлов (span + mark) и getByText с
    // точным текстом ложно не находит узел ещё ДО того, как проверяется сама
    // гонка (см. RED-прогон при первой версии этого теста: непересекающиеся
    // 'a'/'line-b' всё равно совпали подстрокой «b», и findByText упал не на
    // той стадии).
    let resolveA!: (res: LogsResult) => void;
    const pendingA = new Promise<LogsResult>((resolve) => {
      resolveA = resolve;
    });
    const mocked = vi.mocked(queryLogs);
    mocked.mockImplementationOnce(() => pendingA); // A — «висит» (резолвим вручную ниже)
    mocked.mockImplementationOnce(() =>
      Promise.resolve({
        kind: 'ok',
        lines: [{ time: '2026-07-09T10:00:01Z', msg: 'result-beta', fields: {} }],
      }),
    ); // B — резолвится сразу же

    renderEn(<Logs />);
    const input = screen.getByPlaceholderText('Search log text…');
    const form = input.closest('form') as HTMLFormElement;

    fireEvent.change(input, { target: { value: 'search-a' } });
    fireEvent.submit(form); // запрос A улетел и «висит»

    fireEvent.change(input, { target: { value: 'search-b' } });
    fireEvent.submit(form); // запрос B улетел следом, резолвится немедленно

    expect(await screen.findByText('result-beta')).toBeTruthy();

    // Более старый запрос A резолвится ПОСЛЕДНИМ — без staleness-guard'а он
    // перезапишет уже показанный результат B результатом устаревшего поиска.
    await act(async () => {
      resolveA({ kind: 'ok', lines: [{ time: '2026-07-09T10:00:00Z', msg: 'result-alpha', fields: {} }] });
      await pendingA;
    });

    expect(screen.queryByText('result-alpha')).toBeNull();
    expect(screen.getByText('result-beta')).toBeTruthy();
  });
});
