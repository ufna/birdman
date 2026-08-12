// Вывод ноды из флота (слайс C): действие есть только у неработающей ноды,
// подтверждение зовёт POST /v1/nodes/{id}/revoke, отказ мастера доходит до
// пользователя.
//
// Почему у active-ноды действия нет: ревокация оборвала бы идущие матчи, живую
// ноду выводит drain. Правило продублировано на сервере (409) — здесь
// проверяется, что панель не предлагает заведомо отказной путь.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { Environment, NodeInfo, SessionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import { EnvContext } from '../lib/env';
import { Fleet } from '../screens/Fleet';

HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

const dev: Environment = {
  project: 'game',
  name: 'dev',
  production: false,
  auto_deploy: true,
  retention_keep: 20,
  created_at: '2026-07-01T00:00:00Z',
};

const envValue = {
  environments: [dev],
  project: 'game',
  selected: null as string | null,
  setSelected: () => {},
  loading: false,
  reload: () => {},
};

const sess = (...scopes: SessionInfo['scopes']): SessionInfo => ({ scopes, name: 'k' });

function mkNode(over: Partial<NodeInfo> = {}): NodeInfo {
  return {
    id: over.id ?? 'n1',
    project: 'game',
    region: 'eu',
    env: over.env ?? 'dev',
    hostname: over.hostname ?? 'box-1',
    public_ip: '10.0.0.1',
    capacity_slots: 8,
    agent_version: '0.1.0',
    state: over.state ?? 'active',
    last_heartbeat_at: new Date().toISOString(),
    created_at: '2026-07-11T00:00:00Z',
  };
}

/** fetch-мок: GET-списки + POST /revoke с настраиваемым ответом; POST'ы
 *  записываются, чтобы проверить, что ушёл ровно один и по нужному адресу. */
function fleetFetch(nodes: NodeInfo[], posts: string[], revoke: { status: number; body: unknown }) {
  return vi.fn((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? 'GET';
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
    if (u.includes('/revoke')) {
      posts.push(`${method} ${u}`);
      return json(revoke.body, revoke.status);
    }
    if (u.includes('/v1/nodes')) return json({ nodes });
    if (u.includes('/v1/servers')) return json({ servers: [] });
    if (u.includes('/v1/versions')) return json({ versions: [] });
    if (u.includes('/v1/events')) return json({ events: [] });
    return json({});
  });
}

function renderFleet() {
  return render(
    <SessionContext.Provider
      value={{ session: sess('admin'), login: async () => {}, logout: async () => {}, invalidate: () => {} }}
    >
      <I18nProvider initialLang="en">
        <EnvContext.Provider value={envValue}>
          <Fleet />
        </EnvContext.Provider>
      </I18nProvider>
    </SessionContext.Provider>,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Fleet — вывод ноды из флота (POST /v1/nodes/{id}/revoke)', () => {
  it('у active-ноды действия нет: живую ноду выводит drain', async () => {
    vi.stubGlobal('fetch', fleetFetch([mkNode({ state: 'active' })], [], { status: 200, body: {} }));
    renderFleet();
    expect(await screen.findByRole('button', { name: 'Drain' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Retire' })).toBeNull();
  });

  it('down-нода: подтверждение шлёт ровно один POST /revoke', async () => {
    const posts: string[] = [];
    vi.stubGlobal(
      'fetch',
      fleetFetch([mkNode({ state: 'down', hostname: 'box-down' })], posts, {
        status: 200,
        body: { node: mkNode({ state: 'dead' }) },
      }),
    );
    renderFleet();

    fireEvent.click(await screen.findByRole('button', { name: 'Retire' }));
    const dialog = await screen.findByRole('alertdialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Retire' }));

    await waitFor(() => {
      expect(posts).toEqual(['POST /v1/nodes/n1/revoke']);
    });
  });

  it('409 показывается локализованным, а прозы мастера в DOM нет (tracker #1005)', async () => {
    vi.stubGlobal(
      'fetch',
      fleetFetch([mkNode({ state: 'draining', hostname: 'box-drain' })], [], {
        status: 409,
        body: { error: 'conflict', detail: 'node has live servers, drain it first' },
      }),
    );
    renderFleet();

    fireEvent.click(await screen.findByRole('button', { name: 'Retire' }));
    const dialog = await screen.findByRole('alertdialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Retire' }));

    // До #1005 сюда печатался `e.detail` — английская строка мастера в русском
    // интерфейсе. Теперь это текст каталога, а сама проза структурно не доезжает.
    expect((await screen.findByRole('alert')).textContent).toContain('State conflict');
    expect(document.body.textContent).not.toContain('live servers');
  });
});
