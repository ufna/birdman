import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { ReactElement } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import { I18nProvider } from '../lib/i18n';
import { ThemeProvider } from '../lib/theme';
import type { NodeInfo } from '../lib/api';
import { Overview } from '../screens/Overview';

// Спарклайн (uPlot) не относится к «Nodes»-карточке и падает на null-canvas
// jsdom — глушим сам компонент, оставляя чистую функцию bucketPerMinute живой.
vi.mock('../components/Sparkline', async () => {
  const actual = await vi.importActual<typeof import('../components/Sparkline')>('../components/Sparkline');
  return { ...actual, Sparkline: () => null };
});

// Красная сводка Overview («Nodes»-карточка): нода, деградировавшая из quarantine
// в down (молчит дольше node_down_after_min), должна ОСТАВАТЬСЯ в красном чипе, а
// не гаснуть в «all active» ровно когда стало хуже (Important финального ревью).

const renderEn = (ui: ReactElement) =>
  render(
    <ThemeProvider>
      <I18nProvider initialLang="en">
        <Tooltip.Provider>{ui}</Tooltip.Provider>
      </I18nProvider>
    </ThemeProvider>,
  );

function mkNode(id: string, state: NodeInfo['state']): NodeInfo {
  return {
    id,
    project: 'dev',
    region: 'dev',
    env: 'dev',
    hostname: `${id}.dev`,
    public_ip: '10.0.0.1',
    capacity_slots: 8,
    agent_version: '0.1.0',
    state,
    created_at: '2026-07-11T00:00:00Z',
  };
}

/** Стаб fetch по URL: /v1/nodes отдаёт заданный флот, прочее — пусто. */
function stubFleet(nodes: NodeInfo[]) {
  const json = (body: unknown) =>
    new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      const u = String(url);
      if (u.includes('/v1/nodes')) return Promise.resolve(json({ nodes }));
      if (u.includes('/v1/servers')) return Promise.resolve(json({ servers: [] }));
      if (u.includes('/v1/versions')) return Promise.resolve(json({ versions: [] }));
      if (u.includes('/v1/matches')) return Promise.resolve(json({ matches: [] }));
      if (u.includes('/v1/events')) return Promise.resolve(json({ events: [] }));
      return Promise.resolve(json({}));
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Overview — красный чип недоступных (down) нод', () => {
  it('down-нода даёт красный чип «1 down», а не гаснет в «all active»', async () => {
    stubFleet([mkNode('a', 'active'), mkNode('b', 'down')]);
    renderEn(<Overview />);
    expect(await screen.findByText('1 down')).toBeTruthy();
    // Красный чип НЕ подменяется зелёным «all active», когда есть недоступная нода.
    expect(screen.queryByText('all active')).toBeNull();
  });

  it('quarantine и down показаны рядом отдельными чипами', async () => {
    stubFleet([mkNode('a', 'quarantine'), mkNode('b', 'down'), mkNode('c', 'down')]);
    renderEn(<Overview />);
    expect(await screen.findByText('1 in quarantine')).toBeTruthy();
    expect(screen.getByText('2 down')).toBeTruthy();
  });

  it('всё живо → «all active», без чипов quarantine/down', async () => {
    stubFleet([mkNode('a', 'active'), mkNode('b', 'active')]);
    renderEn(<Overview />);
    expect(await screen.findByText('all active')).toBeTruthy();
    expect(screen.queryByText(/down/)).toBeNull();
    expect(screen.queryByText(/in quarantine/)).toBeNull();
  });
});
