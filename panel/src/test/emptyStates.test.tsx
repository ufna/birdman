import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { ReactElement } from 'react';
import { I18nProvider } from '../lib/i18n';
import { MetricChart } from '../components/MetricChart';
import { UtilizationChart } from '../components/UtilizationChart';
import { LogViewer } from '../components/LogViewer';

// Грациозная деградация фичи 2: у старых матчей метрики/логи могли истечь —
// показываем аккуратные пустые состояния, а не спиннер/падение.

const renderEn = (ui: ReactElement) => render(<I18nProvider initialLang="en">{ui}</I18nProvider>);

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('MetricChart — пустое окно метрик матча', () => {
  it('пустая matrix (метрики истекли) → «No data», без спиннера/падения', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ status: 'success', data: { resultType: 'matrix', result: [] } }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    );
    // Статичное окно матча (range с end) — один запрос, без поллинга.
    renderEn(<MetricChart query='birdman_server_players{server_id="gone"}' title="Players" range={{ start: 100, end: 200 }} />);
    expect(await screen.findByText('No data for the selected period.')).toBeTruthy();
  });
});

describe('UtilizationChart — утилизация во времени, мягкая деградация', () => {
  it('пустая matrix (метрик по состояниям нет) → «No data», без падения', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ status: 'success', data: { resultType: 'matrix', result: [] } }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    );
    renderEn(<UtilizationChart />);
    expect(await screen.findByText('No data for the selected period.')).toBeTruthy();
  });

  it('VM не настроена (503) → подпись «метрики не настроены», не спиннер', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: 'metrics_unconfigured' }), {
          status: 503,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    );
    renderEn(<UtilizationChart />);
    expect(await screen.findByText(/Metrics aren't configured/)).toBeTruthy();
  });
});

describe('LogViewer — логи матча вычищены', () => {
  it('404 → «Logs are no longer available.», без падения', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: 'not_found' }), { status: 404 })),
    );
    renderEn(<LogViewer serverId="gone-srv" initialFollow={false} />);
    const hits = await screen.findAllByText('Logs are no longer available.');
    expect(hits.length).toBeGreaterThan(0);
  });
});
