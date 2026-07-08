import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { ReactElement } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import { I18nProvider } from '../lib/i18n';
import { axisTickIndices, BarChart, ShareBars, UtilBar } from '../components/charts';
import { toStackModel } from '../lib/stats';
import type { StackedSeries } from '../lib/api';

const renderEn = (ui: ReactElement) =>
  render(
    <I18nProvider initialLang="en">
      <Tooltip.Provider>{ui}</Tooltip.Provider>
    </I18nProvider>,
  );

describe('axisTickIndices', () => {
  it('≤ maxLabels: подписываем все дни', () => {
    expect([...axisTickIndices(7)]).toEqual([0, 1, 2, 3, 4, 5, 6]);
  });
  it('30/90 дней: прореживаем, но последний всегда подписан', () => {
    const t30 = axisTickIndices(30);
    expect(t30.has(0)).toBe(true);
    expect(t30.has(29)).toBe(true);
    expect(t30.size).toBeLessThanOrEqual(9);
    const t90 = axisTickIndices(90);
    expect(t90.has(89)).toBe(true);
    expect(t90.size).toBeLessThanOrEqual(9);
  });
  it('пустой вход → пустой набор', () => {
    expect(axisTickIndices(0).size).toBe(0);
  });
});

const series: StackedSeries = {
  unit: 'matches/day',
  keys: ['eu', 'us'],
  points: [
    { date: '2026-07-07', total: 3, values: { eu: 2, us: 1 } },
    { date: '2026-07-08', total: 5, values: { eu: 4, us: 1 } },
  ],
};

describe('BarChart', () => {
  it('рисует бары + легенду (>1 ключа) + подпись оси', () => {
    const m = toStackModel(series);
    renderEn(
      <BarChart
        columns={m.columns}
        max={m.max}
        keys={m.keys}
        formatValue={(v) => String(v)}
        empty={m.empty}
        ariaLabel="Matches per day"
        emptyLabel="No data"
      />,
    );
    expect(screen.getByRole('img', { name: 'Matches per day' })).toBeTruthy();
    // легенда обеих серий
    expect(screen.getByText('eu')).toBeTruthy();
    expect(screen.getByText('us')).toBeTruthy();
    // подпись оси последнего дня
    expect(screen.getByText('07-08')).toBeTruthy();
  });

  it('пустое окно → показывает emptyLabel, без баров', () => {
    renderEn(
      <BarChart columns={[]} max={1} formatValue={(v) => String(v)} empty ariaLabel="Matches" emptyLabel="No data for the selected period." />,
    );
    expect(screen.getByText('No data for the selected period.')).toBeTruthy();
    expect(screen.queryByRole('img', { name: 'Matches' })).toBeNull();
  });
});

describe('ShareBars', () => {
  it('пустое распределение → «No data»', () => {
    renderEn(<ShareBars rows={[]} sharePct={(s) => `${String(s)}`} />);
    expect(screen.getByText('No data for the selected period.')).toBeTruthy();
  });
  it('строки с прямыми подписями (версия · матчи · доля)', () => {
    renderEn(
      <ShareBars
        rows={[{ version: '0.1.0', matches: 4, share: 0.4, color: 'var(--cat-1)' }]}
        sharePct={(s) => `${String(Math.round(s * 100))}%`}
      />,
    );
    expect(screen.getByText('0.1.0')).toBeTruthy();
    expect(screen.getByText('4 · 40%')).toBeTruthy();
  });
});

describe('UtilBar', () => {
  it('регион, слоты и легенда состояний', () => {
    renderEn(<UtilBar row={{ region: 'dev', capacity: 8, allocated: 0, ready: 2, draining: 0, free: 6, used: 2, usedRatio: 0.25 }} />);
    expect(screen.getByText('dev')).toBeTruthy();
    expect(screen.getByText('2/8 slots')).toBeTruthy();
    expect(screen.getByText('free')).toBeTruthy();
  });
});
