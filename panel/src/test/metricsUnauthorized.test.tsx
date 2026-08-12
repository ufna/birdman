// 401 на графиках метрик: гасит поллинг, разлогинивает и не молчит на графике
// с данными (tracker #1011).
//
// #996 дал 401 честный ТЕКСТ — и только текст. Поведение вокруг него было
// сломано в трёх местах, и все три вне текстового слайса #996:
//   1. статус ошибки поднимался внутри `setSeries(prev => { if (prev === null) …`,
//      то есть на графике, успевшем нарисовать точки, 401 проходил МОЛЧА;
//   2. 403 гасил 15-секундный таймер, а 401 — нет, хотя он тоже не пройдёт сам:
//      график долбил отказ бесконечно (на «Статистике» — пятью графиками);
//   3. `invalidate()` звался ровно из одного места (`lib/live.tsx`, 401 SSE), а
//      `/v1/events/stream` аутентифицируется ОДИН раз на коннекте и живёт вечно
//      — значит кука, протухшая под живым стримом, логаута не вызывала вовсе.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render, renderHook, screen } from '@testing-library/react';
import type { ReactElement, ReactNode } from 'react';
import type { SessionInfo } from '../lib/api';
import { api } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import { ThemeProvider } from '../lib/theme';
import { onSessionExpired } from '../lib/sessionExpiry';
import { useQueryRange } from '../lib/useMetrics';
import { MetricChart } from '../components/MetricChart';

const SESSION: SessionInfo = { scopes: ['readonly'], name: 'ro' };

function renderAs(ui: ReactElement, lang: 'en' | 'ru' = 'en') {
  const value = { session: SESSION, login: async () => {}, logout: async () => {}, invalidate: () => {} };
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ThemeProvider>
        <I18nProvider initialLang={lang}>
          <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
        </I18nProvider>
      </ThemeProvider>
    );
  }
  return render(<Wrapper>{ui}</Wrapper>);
}

const UNAUTH = JSON.stringify({ error: 'unauthorized', detail: 'no active session' });

/** Матрица с одной точкой — «график уже что-то нарисовал». */
const MATRIX = JSON.stringify({
  status: 'success',
  data: { resultType: 'matrix', result: [{ metric: { __name__: 'up' }, values: [[1700000000, '7']] }] },
});

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('401 не поллится вечно (пункт 2 карточки)', () => {
  it('после 401 таймер погашен: за три периода дозапроса ни одного нового fetch', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(UNAUTH, { status: 401 })));
    vi.stubGlobal('fetch', fetchMock);

    renderHook(() => useQueryRange({ query: 'up', refreshMs: 1000 }));
    await act(async () => {
      await Promise.resolve();
    });
    const afterFirst = fetchMock.mock.calls.length;
    expect(afterFirst).toBe(1);

    await act(async () => {
      vi.advanceTimersByTime(3500);
      await Promise.resolve();
    });
    // Ровно тот замер, что в карточке: «график долбит 401 каждые 15с».
    expect(fetchMock.mock.calls.length).toBe(afterFirst);
  });

  it('транзиентную ошибку (500) поллинг, наоборот, ПРОДОЛЖАЕТ — её и надо ретраить', async () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(new Response(JSON.stringify({ error: 'internal' }), { status: 500 })),
    );
    vi.stubGlobal('fetch', fetchMock);

    renderHook(() => useQueryRange({ query: 'up', refreshMs: 1000 }));
    await act(async () => {
      await Promise.resolve();
    });
    await act(async () => {
      vi.advanceTimersByTime(2500);
      await Promise.resolve();
    });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
  });
});

describe('401 разлогинивает, а не оставляет панель наполовину (пункт 3 карточки)', () => {
  it('401 с метрик-проксии сообщает о протухшей сессии', async () => {
    const seen = vi.fn();
    const off = onSessionExpired(seen);
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(UNAUTH, { status: 401 }))));

    renderHook(() => useQueryRange({ query: 'up' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(seen).toHaveBeenCalled();
    off();
  });

  it('401 с ОБЫЧНОГО API (lib/api.ts) — тоже: чинится не только график', async () => {
    const seen = vi.fn();
    const off = onSessionExpired(seen);
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(UNAUTH, { status: 401 }))));

    await expect(api.listNodes()).rejects.toThrow();
    expect(seen).toHaveBeenCalled();
    off();
  });

  it('не-401 сессию НЕ трогает (иначе разлогинивал бы любой сбой)', async () => {
    const seen = vi.fn();
    const off = onSessionExpired(seen);
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(JSON.stringify({ error: 'internal' }), { status: 500 }))),
    );

    await expect(api.listNodes()).rejects.toThrow();
    expect(seen).not.toHaveBeenCalled();
    off();
  });

  it('отписка работает: после неё уведомлений нет', async () => {
    const seen = vi.fn();
    onSessionExpired(seen)();
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(UNAUTH, { status: 401 }))));
    await expect(api.listNodes()).rejects.toThrow();
    expect(seen).not.toHaveBeenCalled();
  });
});

describe('график С ДАННЫМИ не молчит про терминальный отказ (пункт 1 карточки)', () => {
  /** Первый ответ — точки, дальше 401: ровно «оператор сидит на открытой
   *  Статистике, кука протухает» — случай, который карточка называет самым
   *  нужным и в котором текста как раз не было. */
  function stubThenUnauthorized() {
    let call = 0;
    const mock = vi.fn(() => {
      call += 1;
      return Promise.resolve(
        call === 1 ? new Response(MATRIX, { status: 200 }) : new Response(UNAUTH, { status: 401 }),
      );
    });
    vi.stubGlobal('fetch', mock);
    return mock;
  }

  it('RU: точки остаются НА МЕСТЕ, но появляется пометка, что они не обновляются', async () => {
    stubThenUnauthorized();
    renderAs(<MetricChart query="up" title="Онлайн" refreshMs={1000} />, 'ru');
    await act(async () => {
      await Promise.resolve();
    });
    // Данные доехали — значение видно в шапке карточки.
    expect(await screen.findByText('7')).toBeTruthy();

    await act(async () => {
      vi.advanceTimersByTime(1200);
      await Promise.resolve();
    });
    expect(screen.getByRole('status').textContent).toContain('больше не обновляются');
    // И это НЕ подмена графика сообщением: холст uPlot на месте.
    expect(document.querySelector('.uplot')).not.toBeNull();
  });

  it('EN: тот же смысл по-английски (правило 100% EN+RU)', async () => {
    stubThenUnauthorized();
    renderAs(<MetricChart query="up" title="Online" refreshMs={1000} />, 'en');
    await act(async () => {
      await Promise.resolve();
    });
    await screen.findByText('7');
    await act(async () => {
      vi.advanceTimersByTime(1200);
      await Promise.resolve();
    });
    expect(screen.getByRole('status').textContent).toContain('no longer updating');
  });

  it('транзиентный сбой поверх работающего графика пометкой НЕ мигает', async () => {
    // Обратная сторона: 500 самопроходящий, следующий тик его починит, и
    // пугать оператора поверх живых данных не надо.
    let call = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(() => {
        call += 1;
        return Promise.resolve(
          call === 1
            ? new Response(MATRIX, { status: 200 })
            : new Response(JSON.stringify({ error: 'internal' }), { status: 500 }),
        );
      }),
    );
    renderAs(<MetricChart query="up" title="Онлайн" refreshMs={1000} />, 'ru');
    await act(async () => {
      await Promise.resolve();
    });
    await screen.findByText('7');
    await act(async () => {
      vi.advanceTimersByTime(1200);
      await Promise.resolve();
    });
    expect(screen.queryByRole('status')).toBeNull();
  });
});
