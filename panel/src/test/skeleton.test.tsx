import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import type { ReactElement } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import { I18nProvider } from '../lib/i18n';
import { Stats } from '../screens/Stats';
import { Cost } from '../screens/Cost';

// Скелетоны Stats/Cost: держат раскладку на ПЕРВОЙ загрузке и на СМЕНЕ ПЕРИОДА —
// контент не «прыгает». Скелетон-зона помечена role=status (aria-busy).

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

/** fetch: первый запрос агрегата отвечает данными, второй «висит» — видно скелетон. */
function stagedFetch(dataPath: string, first: (days: number) => unknown) {
  let dataCalls = 0;
  return vi.fn((url: string) => {
    const u = String(url);
    // метрики (instant/range) → мягко пусто, не мешают
    if (u.includes('/v1/metrics/')) return Promise.resolve(json({ status: 'success', data: { result: [] } }));
    if (u.includes(dataPath)) {
      dataCalls += 1;
      const days = Number(new URL(u, 'http://x').searchParams.get('days'));
      if (dataCalls === 1) return Promise.resolve(json(first(days)));
      return new Promise(() => {}); // второй период — не резолвим: наблюдаем скелетон
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
    vi.stubGlobal('fetch', stagedFetch('/v1/stats/overview', mkOverview));
    renderEn(<Stats />);
    // Первый период загрузился — тело на месте, скелетона нет.
    expect(await screen.findByText('Peak CCU')).toBeTruthy();
    expect(screen.queryByRole('status')).toBeNull();
    // Переключаем период на 90: данные за 90 «висят» → показываем скелетон.
    fireEvent.click(screen.getByRole('button', { name: '90' }));
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
