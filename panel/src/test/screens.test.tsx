import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { ReactElement } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import { I18nProvider } from '../lib/i18n';
import { Stats } from '../screens/Stats';
import { Cost } from '../screens/Cost';

// Смоук полной композиции экранов Статистика/Cost на реальных формах ответов
// бокса (карточки + графики + метр утилизации не падают, данные размаплены).

const renderEn = (ui: ReactElement) =>
  render(
    <I18nProvider initialLang="en">
      <Tooltip.Provider>{ui}</Tooltip.Provider>
    </I18nProvider>,
  );

const overview = {
  days: 7,
  timezone: 'UTC',
  generated_at: '2026-07-08T14:39:09Z',
  matches_per_day: {
    unit: 'matches/day',
    keys: ['dev'],
    points: [
      { date: '2026-07-07', total: 0, values: { dev: 0 } },
      { date: '2026-07-08', total: 10, values: { dev: 10 } },
    ],
  },
  players_per_day: {
    unit: 'players/day (sum players_peak)',
    keys: ['dev'],
    points: [
      { date: '2026-07-07', total: 0, values: { dev: 0 } },
      { date: '2026-07-08', total: 18, values: { dev: 18 } },
    ],
  },
  peak_ccu_per_day: {
    unit: 'players (peak concurrent)',
    points: [
      { date: '2026-07-07', value: 0 },
      { date: '2026-07-08', value: 2 },
    ],
  },
  peak_ccu: 2,
  avg_match_duration_seconds: 52.29,
  avg_match_duration_per_day: {
    unit: 'seconds',
    points: [
      { date: '2026-07-07', value: 0 },
      { date: '2026-07-08', value: 52.29 },
    ],
  },
  version_distribution: [
    { version: '0.1.0', matches: 4, share: 0.4 },
    { version: '0.1.1', matches: 3, share: 0.3 },
  ],
  time_to_match: {
    p50_seconds: 15.54,
    p95_seconds: 80.58,
    samples: 10,
    source: 'matches.created_at→started_at',
    note: 'fill-rate proxy',
  },
};

const cost = {
  days: 7,
  timezone: 'UTC',
  generated_at: '2026-07-08T14:39:10Z',
  slot_hours_per_day_by_region: {
    unit: 'slot-hours',
    keys: ['dev'],
    points: [
      { date: '2026-07-07', total: 0, values: { dev: 0 } },
      { date: '2026-07-08', total: 0.15, values: { dev: 0.15 } },
    ],
  },
  slot_hours_per_day_by_version: {
    unit: 'slot-hours',
    keys: ['0.1.0', '0.1.1'],
    points: [
      { date: '2026-07-07', total: 0, values: { '0.1.0': 0, '0.1.1': 0 } },
      { date: '2026-07-08', total: 0.11, values: { '0.1.0': 0.02, '0.1.1': 0.09 } },
    ],
  },
  slot_hours_total: 0.15,
  utilization: [{ region: 'dev', capacity_slots: 8, allocated: 0, ready: 2, draining: 0 }],
  utilization_note: 'current snapshot',
};

function stubJSON(body: unknown) {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })));
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Stats — полная композиция', () => {
  it('карточки, графики и распределение по версиям рендерятся', async () => {
    stubJSON(overview);
    renderEn(<Stats />);
    expect(await screen.findByText('Peak CCU')).toBeTruthy();
    expect(screen.getByText('Time to match')).toBeTruthy();
    expect(screen.getByRole('img', { name: 'Matches per day' })).toBeTruthy();
    expect(screen.getByRole('img', { name: 'Peak CCU per day' })).toBeTruthy();
    expect(screen.getByText('0.1.0')).toBeTruthy(); // версия в распределении
  });
});

describe('Cost — полная композиция', () => {
  it('итог слото-часов, стек-графики и метр утилизации рендерятся', async () => {
    stubJSON(cost);
    renderEn(<Cost />);
    expect(await screen.findByText('Total slot-hours')).toBeTruthy();
    expect(screen.getByRole('img', { name: 'Slot-hours per day · by region' })).toBeTruthy();
    expect(screen.getByText('Node utilization')).toBeTruthy();
    expect(screen.getByText('2/8 slots')).toBeTruthy(); // утилизация региона dev
  });
});
