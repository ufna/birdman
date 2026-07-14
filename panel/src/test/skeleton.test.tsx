import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import type { ReactElement } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import type { Environment } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { EnvContext } from '../lib/env';
import { Stats } from '../screens/Stats';
import { Cost } from '../screens/Cost';

// Скелетоны Stats/Cost: держат раскладку на ПЕРВОЙ загрузке, на СМЕНЕ ПЕРИОДА и
// на СМЕНЕ ОКРУЖЕНИЯ — контент не «прыгает» и не показывает данные прежнего env
// как готовые. Скелетон-зона помечена role=status (aria-busy).

const renderEn = (ui: ReactElement) =>
  render(
    <I18nProvider initialLang="en">
      <Tooltip.Provider>{ui}</Tooltip.Provider>
    </I18nProvider>,
  );

const zeroStack = { unit: 'x', keys: ['dev'], points: [{ date: '2026-07-08', total: 0, values: { dev: 0 } }] };
const zeroSimple = { unit: 's', points: [{ date: '2026-07-08', value: 0 }] };

function mkOverview(days: number) {
  return {
    days,
    timezone: 'UTC',
    generated_at: `gen-${String(days)}`,
    matches_per_day: zeroStack,
    players_per_day: zeroStack,
    peak_ccu_per_day: zeroSimple,
    peak_ccu: 0,
    avg_match_duration_seconds: null,
    avg_match_duration_per_day: zeroSimple,
    version_distribution: [],
    time_to_match: { p50_seconds: null, p95_seconds: null, samples: 0, source: 'x', note: 'y' },
  };
}

function mkCost(days: number) {
  return {
    days,
    timezone: 'UTC',
    generated_at: `gen-${String(days)}`,
    slot_hours_per_day_by_region: zeroStack,
    slot_hours_per_day_by_version: zeroStack,
    slot_hours_total: 0,
    utilization: [],
    utilization_note: 'x',
  };
}

function json(body: unknown) {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

/**
 * fetch: первые `resolveCalls` запросов агрегата отвечают данными (за каждым
 * стоит days из URL — эхо реального бэка), остальные «висят» — видно
 * скелетон. По умолчанию 1 (было раньше). Stats (Task 5) теперь всегда
 * фетчит /v1/stats/overview даже в live-режиме (для fill-rate карточки),
 * поэтому его тест смены периода резолвит первые 2 вызова.
 */
function stagedFetch(dataPath: string, first: (days: number) => unknown, resolveCalls = 1) {
  let dataCalls = 0;
  return vi.fn((url: string) => {
    const u = String(url);
    // метрики (instant/range) → мягко пусто, не мешают
    if (u.includes('/v1/metrics/')) return Promise.resolve(json({ status: 'success', data: { result: [] } }));
    if (u.includes(dataPath)) {
      dataCalls += 1;
      const days = Number(new URL(u, 'http://x').searchParams.get('days'));
      if (dataCalls <= resolveCalls) return Promise.resolve(json(first(days)));
      return new Promise(() => {}); // сверх лимита — не резолвим: наблюдаем скелетон
    }
    return Promise.resolve(json({}));
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Stats — скелетон первой загрузки и смены периода', () => {
  it('первая загрузка (запрос «висит») → скелетон-зона role=status', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    renderEn(<Stats />);
    expect(screen.getByRole('status')).toBeTruthy();
    expect(screen.queryByText('Peak CCU')).toBeNull(); // тела ещё нет
  });

  it('смена периода → снова скелетон, пока новые данные не пришли', async () => {
    // Дефолт экрана — live-окно 24ч (Task 5): даже в live-режиме Stats
    // фетчит /v1/stats/overview для fill-rate карточки (days=1) — резолвим и
    // его, и следующий (клик на продуктовое 7д); третий (30д) «висит».
    vi.stubGlobal('fetch', stagedFetch('/v1/stats/overview', mkOverview, 2));
    renderEn(<Stats />);
    // Live-режим загрузился — fill-rate карточка на месте, скелетона нет.
    expect(await screen.findByText('queue → match')).toBeTruthy();
    expect(screen.queryByRole('status')).toBeNull();
    // Переключаемся на продуктовое окно 7д — второй резолвящийся вызов.
    fireEvent.click(screen.getByRole('button', { name: '7 d' }));
    expect(await screen.findByText('Peak CCU')).toBeTruthy();
    expect(screen.queryByRole('status')).toBeNull();
    // Переключаем на 30д: данные «висят» (сверх лимита resolveCalls) → скелетон.
    fireEvent.click(screen.getByRole('button', { name: '30 d' }));
    expect(await screen.findByRole('status')).toBeTruthy();
  });
});

describe('Cost — скелетон первой загрузки и смены периода', () => {
  it('первая загрузка → скелетон-зона role=status', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
    renderEn(<Cost />);
    expect(screen.getByRole('status')).toBeTruthy();
    expect(screen.queryByText('Total slot-hours')).toBeNull();
  });

  it('смена периода → снова скелетон', async () => {
    vi.stubGlobal('fetch', stagedFetch('/v1/stats/cost', mkCost));
    renderEn(<Cost />);
    expect(await screen.findByText('Total slot-hours')).toBeTruthy();
    expect(screen.queryByRole('status')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '30' }));
    expect(await screen.findByRole('status')).toBeTruthy();
  });
});

// --- смена ОКРУЖЕНИЯ → скелетон (follow-up p3) ---
// Ответ /v1/stats/* не несёт env, поэтому «готовность» проверяет и период, и
// запрошенный env: иначе на переключении чипа мгновение видны цифры ПРЕЖНЕГО
// окружения как готовые.

const dev: Environment = { project: 'game', name: 'dev', production: false, auto_deploy: true, retention_keep: 20, created_at: '2026-07-01T00:00:00Z' };
const prod: Environment = { project: 'game', name: 'prod', production: true, auto_deploy: false, retention_keep: 0, created_at: '2026-07-01T00:00:00Z' };

const envValue = (selected: string | null) => ({
  environments: [dev, prod],
  project: 'game',
  selected,
  setSelected: () => {},
  loading: false,
  reload: () => {},
});

const withEnv = (selected: string | null, ui: ReactElement) => (
  <I18nProvider initialLang="en">
    <Tooltip.Provider>
      <EnvContext.Provider value={envValue(selected)}>{ui}</EnvContext.Provider>
    </Tooltip.Provider>
  </I18nProvider>
);

/** fetch: запрос агрегата БЕЗ ?env= резолвится, с ?env=… — «висит» (наблюдаем
 *  скелетон нового окружения, а не готовые данные прежнего). */
function envStagedFetch(dataPath: string, make: (days: number) => unknown) {
  return vi.fn((url: string) => {
    const u = String(url);
    if (u.includes('/v1/metrics/')) return Promise.resolve(json({ status: 'success', data: { result: [] } }));
    if (u.includes(dataPath)) {
      const params = new URL(u, 'http://x').searchParams;
      if (params.get('env') !== null) return new Promise(() => {}); // данные нового env ещё в пути
      return Promise.resolve(json(make(Number(params.get('days')))));
    }
    return Promise.resolve(json({}));
  });
}

describe('Cost — скелетон при смене окружения', () => {
  it('смена env → скелетон, данные прежнего env не показываются', async () => {
    vi.stubGlobal('fetch', envStagedFetch('/v1/stats/cost', mkCost));
    const { rerender } = render(withEnv(null, <Cost />));
    expect(await screen.findByText('Total slot-hours')).toBeTruthy();
    expect(screen.queryByRole('status')).toBeNull();

    rerender(withEnv('prod', <Cost />));
    expect(await screen.findByRole('status')).toBeTruthy();
    expect(screen.queryByText('Total slot-hours')).toBeNull();
  });
});

describe('Stats — скелетон при смене окружения', () => {
  it('смена env в продуктовом режиме → скелетон, пока не пришли данные нового env', async () => {
    vi.stubGlobal('fetch', envStagedFetch('/v1/stats/overview', mkOverview));
    const { rerender } = render(withEnv(null, <Stats />));
    // Переходим в продуктовый режим (7д) — там и живёт /v1/stats/overview-тело.
    fireEvent.click(screen.getByRole('button', { name: '7 d' }));
    expect(await screen.findByText('Peak CCU')).toBeTruthy();
    expect(screen.queryByRole('status')).toBeNull();

    rerender(withEnv('prod', <Stats />));
    expect(await screen.findByRole('status')).toBeTruthy();
    expect(screen.queryByText('Peak CCU')).toBeNull();
  });
});
