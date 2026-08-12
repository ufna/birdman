// Панель знает, что ключ сессии привязан (tracker #1000).
//
// До этой карточки `POST/GET /v1/session` отдавали только {scopes, name}, и
// панель объясняла ЛЮБОЙ 403 единственным, что видела, — скоупами. Привязанный
// readonly-ключ получал «нужен ключ со скоупом readonly или admin», хотя
// readonly у него ЕСТЬ: локализованный, но ложный диагноз. Плюс `useQueryRange`
// поллил отказ каждые 15с (на «Статистике» — пять графиков разом).
//
// Здесь держатся ОБЕ стороны контракта: привязанная сессия получает честный
// текст, НЕпривязанная — прежний, байт-в-байт. Регресс-тесты рядом
// (screens.test.tsx, skeleton.test.tsx) этот класс не ловят по построению: они
// всегда отвечают 200 на /v1/metrics/*.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, renderHook, screen, waitFor } from '@testing-library/react';
import type { ReactElement, ReactNode } from 'react';
import { ApiError } from '../lib/api';
import type { SessionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import { useInstantQuery, useQueryRange } from '../lib/useMetrics';
import { ConfirmButton } from '../components/ConfirmDialog';
import { ErrorNote } from '../components/ui';
import { MetricMessage } from '../components/MetricMessage';
import { LogsPanel } from '../components/LogsPanel';

// Экран флит-поиска открывает дровер дедика — мокаем так же, как logsScreen.test.tsx.
vi.mock('../lib/drawer', () => ({ useServerDrawer: () => ({ open: vi.fn() }) }));

// Импорт ПОСЛЕ vi.mock.
import { Logs } from '../screens/Logs';

/** Привязанная сессия: readonly ЕСТЬ, и отказ приходит всё равно — ровно та
 *  комбинация, на которой старый диагноз был ложным. */
const BOUND: SessionInfo = { scopes: ['readonly'], name: 'ro-bound', binding: { project: 'game', env: 'dev' } };
/** Непривязанная — поле binding master не присылает вовсе (additive). */
const UNBOUND: SessionInfo = { scopes: ['readonly'], name: 'ro-global' };

const FORBIDDEN_EN = 'Access denied: you need a key with the readonly or admin scope.';
const FORBIDDEN_RU = 'Не хватает прав: нужен ключ со скоупом readonly или admin.';

function withSession(session: SessionInfo | null, lang: 'en' | 'ru' = 'en') {
  const value = { session, login: async () => {}, logout: async () => {}, invalidate: () => {} };
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <I18nProvider initialLang={lang}>
        <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
      </I18nProvider>
    );
  };
}

function renderAs(ui: ReactElement, session: SessionInfo | null, lang: 'en' | 'ru' = 'en') {
  const Wrapper = withSession(session, lang);
  return render(<Wrapper>{ui}</Wrapper>);
}

const forbiddenError = () => new ApiError(403, 'forbidden', 'key is bound to game/dev: raw query proxy is global-key only');

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('ErrorNote: 403 объясняется по сессии, а не догадкой про скоуп', () => {
  it('привязанная сессия (EN) → называет привязку, а НЕ несуществующую нехватку readonly', () => {
    renderAs(<ErrorNote error={forbiddenError()} />, BOUND);
    expect(screen.getByText(/your key is bound to game\/dev/)).toBeTruthy();
    expect(screen.queryByText(new RegExp(FORBIDDEN_EN.slice(0, 30)))).toBeNull();
    // И никакого текста мастера в UI: правило «панель 100% EN+RU».
    expect(document.body.textContent).not.toContain('raw query proxy is global-key only');
  });

  it('привязанная сессия (RU) → тот же смысл по-русски, ключ есть в обоих каталогах', () => {
    renderAs(<ErrorNote error={forbiddenError()} />, BOUND, 'ru');
    expect(screen.getByText(/ключ привязан к game\/dev/)).toBeTruthy();
    expect(document.body.textContent).not.toContain(FORBIDDEN_RU);
    expect(document.body.textContent).not.toContain('raw query proxy is global-key only');
  });

  it('НЕпривязанная сессия → прежний текст про скоуп (поведение не менялось)', () => {
    renderAs(<ErrorNote error={forbiddenError()} />, UNBOUND);
    expect(screen.getByText(new RegExp(FORBIDDEN_EN.slice(0, 30)))).toBeTruthy();
    expect(document.body.textContent).not.toContain('is bound to');
  });

  it('сессии нет вовсе (ещё не загружена) → прежний текст, без падения на undefined', () => {
    renderAs(<ErrorNote error={forbiddenError()} />, null);
    expect(screen.getByText(new RegExp(FORBIDDEN_EN.slice(0, 30)))).toBeTruthy();
  });

  it('не-403 у привязанной сессии → общий текст с КОДОМ, подмена не растеклась (tracker #1005)', () => {
    renderAs(<ErrorNote error={new ApiError(500, 'internal', 'boom')} />, BOUND);
    // Машинный код остаётся (за него цепляется баг-репорт), detail — нет.
    expect(screen.getByText(/error internal/)).toBeTruthy();
    expect(document.body.textContent).not.toContain('boom');
    expect(document.body.textContent).not.toContain('is bound to game/dev');
  });
});

describe('Экран «Логи»: 403 по привязке доходит до пользователя честным', () => {
  const stub403 = () =>
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ error: 'forbidden', detail: 'key is bound to game/dev: raw query proxy is global-key only' }), {
            status: 403,
          }),
        ),
      ),
    );

  it('привязанная сессия: поиск → «ключ привязан», а не «нужен readonly»', async () => {
    stub403();
    renderAs(<Logs />, BOUND);
    fireEvent.submit(screen.getByPlaceholderText('Search log text…').closest('form') as HTMLFormElement);
    expect(await screen.findByText(/your key is bound to game\/dev/)).toBeTruthy();
    expect(document.body.textContent).not.toContain(FORBIDDEN_EN);
    expect(document.body.textContent).not.toContain('raw query proxy is global-key only');
  });

  it('непривязанная сессия: тот же 403 → прежний текст про скоуп', async () => {
    stub403();
    renderAs(<Logs />, UNBOUND);
    fireEvent.submit(screen.getByPlaceholderText('Search log text…').closest('form') as HTMLFormElement);
    expect(await screen.findByText(new RegExp(FORBIDDEN_EN.slice(0, 30)))).toBeTruthy();
  });
});

describe('ConfirmDialog: 403 на ДЕЙСТВИИ тоже объясняется честно', () => {
  // Найдено вторым независимым проходом: `confirm.err.forbidden` — тот же
  // ложный диагноз про права, только другими словами, и достижим он штатно —
  // ключ, привязанный к game/dev со скоупами readonly+deploy, видит кнопки
  // Deploy/Promote (canDeploy), а master энфорсит привязку на deploy/promote.
  const CONFIRM_FORBIDDEN_EN = "You don't have permission for this action.";

  function renderDialog(session: SessionInfo | null) {
    renderAs(
      <ConfirmButton
        label="Promote"
        title="t"
        description="d"
        confirmLabel="OK"
        onConfirm={async () => {
          throw new ApiError(403, 'forbidden', 'key is bound to game/dev');
        }}
      />,
      session,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Promote' }));
    fireEvent.click(screen.getByRole('button', { name: 'OK' }));
  }

  it('привязанная сессия → называет привязку', async () => {
    renderDialog(BOUND);
    expect(await screen.findByText(/your key is bound to game\/dev/)).toBeTruthy();
    expect(document.body.textContent).not.toContain(CONFIRM_FORBIDDEN_EN);
    expect(document.body.textContent).not.toContain('key is bound to game/dev:'); // без сырого текста мастера
  });

  it('непривязанная → прежний текст ДЕЙСТВИЯ, а не текст чтения про readonly', async () => {
    renderDialog(UNBOUND);
    expect(await screen.findByText(CONFIRM_FORBIDDEN_EN)).toBeTruthy();
    // Подставить сюда «нужен readonly или admin» было бы новой ложью:
    // действию нужен deploy/admin.
    expect(document.body.textContent).not.toContain(FORBIDDEN_EN);
  });
});

describe('MetricMessage: статус forbidden не отрисовывается пустотой', () => {
  it('привязанная сессия → тот же честный текст, что и в ErrorNote', () => {
    renderAs(<MetricMessage status="forbidden" hasData={false} height={120} />, BOUND);
    expect(screen.getByText(/your key is bound to game\/dev/)).toBeTruthy();
  });

  // Проп переименован в #996: раньше сюда приезжало `error` = ApiError.message
  // (проза мастера), теперь — только машинный код. Сырое сообщение мастера
  // подставить больше НЕЛЬЗЯ: тип его не принимает (проверку держит tsc, а
  // содержательные тексты — metricMessage.test.tsx).
  it('непривязанная → текст про скоуп, а не код ошибки', () => {
    renderAs(<MetricMessage status="forbidden" hasData={false} errorCode="forbidden" height={120} />, UNBOUND);
    expect(screen.getByText(new RegExp(FORBIDDEN_EN.slice(0, 30)))).toBeTruthy();
  });
});

describe('useQueryRange: 403 не ретраится поллингом', () => {
  /** Считает вызовы query_range и отвечает заданным статусом/телом. */
  function stubRange(status: number, body: unknown) {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(JSON.stringify(body), { status })));
    vi.stubGlobal('fetch', fetchMock);
    return fetchMock;
  }

  it('403 → ровно ОДИН запрос за минуту жизни графика, статус forbidden', async () => {
    vi.useFakeTimers();
    const fetchMock = stubRange(403, { error: 'forbidden', detail: 'key is bound to game/dev: raw query proxy is global-key only' });
    const { result } = renderHook(() => useQueryRange({ query: 'up', refreshMs: 15_000 }));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.status).toBe('forbidden');
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Четыре тика поллинга по 15с — при живом таймере было бы 5 запросов.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('погашенный поллинг ВОЗВРАЩАЕТСЯ при смене запроса — гасится таймер, а не хук навсегда', async () => {
    vi.useFakeTimers();
    const fetchMock = stubRange(403, { error: 'forbidden', detail: 'key is bound to game/dev' });
    const { rerender } = renderHook(({ q }: { q: string }) => useQueryRange({ query: q, refreshMs: 15_000 }), {
      initialProps: { q: 'up' },
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Смена запроса (оператор переключил период/график) — это НОВЫЙ запрос, и
    // он обязан уйти: 403 на прошлый запрос не приговор для следующего (после
    // серверного сужения #994 привязанный ключ снова получит свои данные).
    rerender({ q: 'up{project="game"}' });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    // …и новый отказ снова гасит поллинг, а не накапливает таймеры.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('поллинг гаснет и когда данные УЖЕ были — не только на пустом графике', async () => {
    // Дыра, найденная вторым проходом: если гасить таймер только в ветке
    // «данных ещё нет», график с данными будет долбить отказ вечно. Сегодня
    // недостижимо (гейт #990 отказывает с первого запроса), достижимо станет
    // ровно после #994 — то есть в том будущем, ради которого всё и писалось.
    vi.useFakeTimers();
    let n = 0;
    const fetchMock = vi.fn(() => {
      n += 1;
      return Promise.resolve(
        n === 1
          ? new Response(JSON.stringify({ status: 'success', data: { resultType: 'matrix', result: [{ metric: {}, values: [[1, '1']] }] } }), { status: 200 })
          : new Response(JSON.stringify({ error: 'forbidden', detail: 'key is bound to game/dev' }), { status: 403 }),
      );
    });
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useQueryRange({ query: 'up', refreshMs: 15_000 }));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.status).toBe('ok'); // данные приехали
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15_000); // второй тик → 403
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    // Данные на месте (политика «не сбрасывать при ошибке» не тронута)…
    expect(result.current.series).not.toBeNull();
    // …но добывать новые точки запрещённым запросом больше не пытаемся.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('КОНТРАСТ: 500 — транзиентная ошибка, поллинг продолжается', async () => {
    vi.useFakeTimers();
    const fetchMock = stubRange(500, { error: 'internal', detail: 'boom' });
    const { result } = renderHook(() => useQueryRange({ query: 'up', refreshMs: 15_000 }));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.status).toBe('error');
    expect(fetchMock).toHaveBeenCalledTimes(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
  });
});

describe('useInstantQuery: 403 не перезапрашивается по тику refetchKey', () => {
  it('смена refetchKey после 403 запроса НЕ повторяет, смена самого запроса — повторяет', async () => {
    // refetchKey в Stats — generated_at из /v1/stats/overview, а тот
    // перечитывается по всплеску SSE (дебанс 3с): на живом кластере это чаще
    // 15-секундного поллинга. Найдено вторым независимым проходом.
    const fetchMock = vi.fn(() =>
      Promise.resolve(new Response(JSON.stringify({ error: 'forbidden', detail: 'key is bound to game/dev' }), { status: 403 })),
    );
    vi.stubGlobal('fetch', fetchMock);
    const { result, rerender } = renderHook(({ q, k }: { q: string; k: string }) => useInstantQuery({ query: q, refetchKey: k }), {
      initialProps: { q: 'histogram', k: 't0' },
    });
    await waitFor(() => {
      expect(result.current.status).toBe('forbidden');
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    rerender({ q: 'histogram', k: 't1' });
    rerender({ q: 'histogram', k: 't2' });
    await act(async () => {
      await Promise.resolve();
    });
    expect(fetchMock).toHaveBeenCalledTimes(1); // три тика SSE — ни одного нового отказа
    expect(result.current.status).toBe('forbidden'); // и сообщение на экране не мигнуло на loading

    // Другой ЗАПРОС — другое дело: он обязан уйти (после #994 может и пройти).
    rerender({ q: 'histogram{project="game"}', k: 't2' });
    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(2);
    });
  });

  it('КОНТРАСТ: 500 — запрос повторяется по тику refetchKey как раньше', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(JSON.stringify({ error: 'internal', detail: 'boom' }), { status: 500 })));
    vi.stubGlobal('fetch', fetchMock);
    const { result, rerender } = renderHook(({ k }: { k: string }) => useInstantQuery({ query: 'histogram', refetchKey: k }), {
      initialProps: { k: 't0' },
    });
    await waitFor(() => {
      expect(result.current.status).toBe('error');
    });
    rerender({ k: 't1' });
    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(2);
    });
  });
});

describe('LogsPanel: «показать ещё» больше не глотает отказ молча', () => {
  function ndjsonLine(time: string, msg: string): string {
    return JSON.stringify({ _time: time, _msg: msg, server_id: 'srv-1' });
  }

  it('403 на второй странице → ErrorNote с честной причиной, уже показанные строки на месте', async () => {
    const page1 = Array.from({ length: 500 }, (_, i) => ndjsonLine(new Date(2026, 6, 9, 10, 0, 500 - i).toISOString(), `line-${String(i)}`));
    let n = 0;
    const fetchMock = vi.fn((url: string) => {
      if (!String(url).includes('/v1/logs/query')) return new Promise(() => {}); // live-tail висит
      n += 1;
      return Promise.resolve(
        n === 1
          ? new Response(page1.join('\n') + '\n', { status: 200 })
          : new Response(JSON.stringify({ error: 'forbidden', detail: 'key is bound to game/dev: raw query proxy is global-key only' }), {
              status: 403,
            }),
      );
    });
    vi.stubGlobal('fetch', fetchMock);
    renderAs(<LogsPanel serverId="srv-1" />, BOUND);

    fireEvent.click(screen.getByRole('button', { name: 'History' }));
    expect(await screen.findByText('line-0')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Show more' }));
    expect(await screen.findByText(/your key is bound to game\/dev/)).toBeTruthy();
    // Первая страница НЕ снесена: ошибка страницы — не ошибка всей истории.
    expect(screen.getByText('line-0')).toBeTruthy();
    // И никакого сырого текста мастера.
    await waitFor(() => {
      expect(document.body.textContent).not.toContain('raw query proxy is global-key only');
    });
  });

  it('УСТАРЕВШИЙ отказ «показать ещё» не всплывает поверх свежей загрузки', async () => {
    // Guard добавлен этим же коммитом и вторым проходом отмечен как непокрытый:
    // без него ошибка страницы, протухшей из-за смены диапазона, перетирала бы
    // уже показанный свежий результат.
    let rejectStale!: (e: unknown) => void;
    const stale = new Promise<Response>((_, reject) => {
      rejectStale = reject;
    });
    const page1 = Array.from({ length: 500 }, (_, i) => ndjsonLine(new Date(2026, 6, 9, 10, 0, 500 - i).toISOString(), `line-${String(i)}`));
    let n = 0;
    const fetchMock = vi.fn((url: string) => {
      if (!String(url).includes('/v1/logs/query')) return new Promise(() => {});
      n += 1;
      if (n === 1) return Promise.resolve(new Response(page1.join('\n') + '\n', { status: 200 }));
      if (n === 2) return stale; // «показать ещё» — зависает, отвергнем вручную
      return Promise.resolve(new Response(ndjsonLine('2026-07-09T11:00:00Z', 'fresh-after-range-change') + '\n', { status: 200 }));
    });
    vi.stubGlobal('fetch', fetchMock);
    renderAs(<LogsPanel serverId="srv-1" />, BOUND);

    fireEvent.click(screen.getByRole('button', { name: 'History' }));
    expect(await screen.findByText('line-0')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Show more' }));

    // Диапазон меняется, пока «показать ещё» ещё в полёте.
    fireEvent.click(screen.getByRole('button', { name: '1 h' }));
    expect(await screen.findByText('fresh-after-range-change')).toBeTruthy();

    // Протухший запрос падает ПОСЛЕДНИМ — его ошибка не должна появиться.
    rejectStale(new ApiError(403, 'forbidden', 'key is bound to game/dev'));
    await waitFor(() => {
      expect(screen.getByText('fresh-after-range-change')).toBeTruthy();
    });
    expect(screen.queryByText(/your key is bound to game\/dev/)).toBeNull();
  });
});
