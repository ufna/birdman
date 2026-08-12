// Fleet × environments v1: перевод ноды в другое окружение из UI
// (PATCH /v1/nodes/{id} {env}, admin) — диалог с выбором цели, тело запроса,
// внятный текст на 409 («сначала drain»), гейты (dead-нода, пустой список env).

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { Environment, NodeInfo, SessionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import { EnvContext } from '../lib/env';
import { Fleet } from '../screens/Fleet';

// Radix Dialog дёргает эти API при фокусе — в jsdom их нет.
HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

const dev: Environment = { project: 'game', name: 'dev', production: false, auto_deploy: true, retention_keep: 20, created_at: '2026-07-01T00:00:00Z' };
const staging: Environment = { project: 'game', name: 'staging', production: false, auto_deploy: false, retention_keep: 5, created_at: '2026-07-01T00:00:00Z' };
const prod: Environment = { project: 'game', name: 'prod', production: true, auto_deploy: false, retention_keep: 0, created_at: '2026-07-01T00:00:00Z' };

const baseEnv = {
  environments: [dev, staging, prod],
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

interface Recorded {
  url: string;
  method: string;
  body: unknown;
}

/** fetch-мок Флота: GET-списки; PATCH /v1/nodes/{id} отвечает заданным статусом
 *  (200 по умолчанию). Все не-GET вызовы пишутся в `calls` — проверяем тело. */
function fleetFetch(nodes: NodeInfo[], calls: Recorded[], patch: { status: number; body: unknown } = { status: 200, body: { node: mkNode({ env: 'prod' }) } }) {
  return vi.fn((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? 'GET';
    if (method !== 'GET') {
      calls.push({ url: u, method, body: init?.body !== undefined ? JSON.parse(String(init.body)) : undefined });
    }
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
    if (method === 'PATCH') return json(patch.body, patch.status);
    if (u.includes('/v1/nodes')) return json({ nodes });
    if (u.includes('/v1/servers')) return json({ servers: [] });
    if (u.includes('/v1/versions')) return json({ versions: [] });
    if (u.includes('/v1/events')) return json({ events: [] });
    return json({});
  });
}

function renderFleet(value: Parameters<typeof EnvContext.Provider>[0]['value']) {
  return render(
    <SessionContext.Provider value={{ session: sess('admin'), login: async () => {}, logout: async () => {}, invalidate: () => {} }}>
      <I18nProvider initialLang="en">
        <EnvContext.Provider value={value}>
          <Fleet />
        </EnvContext.Provider>
      </I18nProvider>
    </SessionContext.Provider>,
  );
}

const moveButton = () => screen.findByRole('button', { name: /Move to env/ });

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Fleet — перевод ноды в другое окружение (PATCH /v1/nodes/{id})', () => {
  it('клик → диалог → выбор env → PATCH-тело {env:"prod"}', async () => {
    const calls: Recorded[] = [];
    vi.stubGlobal('fetch', fleetFetch([mkNode({ id: 'n1', env: 'dev' })], calls));
    renderFleet(baseEnv);

    fireEvent.click(await moveButton());
    const dialog = screen.getByRole('dialog');
    // Цели — все окружения проекта, КРОМЕ текущего (dev).
    const select = within(dialog).getByRole('combobox');
    expect([...select.querySelectorAll('option')].map((o) => o.value)).toEqual(['staging', 'prod']);

    fireEvent.change(select, { target: { value: 'prod' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Move' }));

    await waitFor(() => {
      expect(calls.length).toBeGreaterThan(0);
    });
    expect(calls[0].method).toBe('PATCH');
    expect(calls[0].url).toContain('/v1/nodes/n1');
    expect(calls[0].body).toEqual({ env: 'prod' });
  });

  it('409 conflict → инлайн-текст: сначала выведи ноду из ротации (drain)', async () => {
    const calls: Recorded[] = [];
    vi.stubGlobal(
      'fetch',
      fleetFetch([mkNode({ id: 'n1', env: 'dev' })], calls, {
        status: 409,
        body: { error: 'conflict', detail: 'node n1 has live servers, drain it first: conflict' },
      }),
    );
    renderFleet(baseEnv);

    fireEvent.click(await moveButton());
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Move' }));

    const alert = await screen.findByRole('alert');
    // Текст СВОЙ у поверхности (fleet.moveEnv.err.conflict), а не проза мастера.
    expect(alert.textContent).toMatch(/drain/i);
    expect(document.body.textContent).not.toContain('node n1 has live servers');
    expect(screen.getByRole('dialog')).toBeTruthy(); // диалог не закрылся — ошибку видно
  });

  it('400 → «проверьте поля», а НЕ detail мастера (tracker #1005)', async () => {
    const calls: Recorded[] = [];
    vi.stubGlobal(
      'fetch',
      fleetFetch([mkNode({ id: 'n1', env: 'dev' })], calls, {
        status: 400,
        body: { error: 'bad_request', detail: 'env is required' },
      }),
    );
    renderFleet(baseEnv);

    fireEvent.click(await moveButton());
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Move' }));

    expect((await screen.findByRole('alert')).textContent).toContain('rejected these values');
    expect(document.body.textContent).not.toContain('env is required');
  });

  // Выведенная нода скрыта по умолчанию (иначе «убрать её» из панели по-прежнему
  // невозможно), но не исчезает бесследно: чекбокс возвращает её, и действий у
  // неё по-прежнему нет — мастер откажет и в переводе, и в дрейне.
  it('dead-нода скрыта, чекбокс её возвращает — и действий у неё нет', async () => {
    vi.stubGlobal('fetch', fleetFetch([mkNode({ id: 'n1', env: 'dev', state: 'dead', hostname: 'dead-1' })], []));
    renderFleet(baseEnv);

    const toggle = await screen.findByRole('checkbox', { name: /Show 1 retired/ });
    expect(screen.queryByText('dead-1')).toBeNull();

    fireEvent.click(toggle);
    expect(await screen.findByText('dead-1')).toBeTruthy();
    expect(screen.queryByRole('button', { name: /Move to env/ })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Drain' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retire' })).toBeNull();
  });

  it('список окружений пуст/недоступен → переводить некуда, кнопки нет', async () => {
    vi.stubGlobal('fetch', fleetFetch([mkNode({ id: 'n1', env: 'dev' })], []));
    renderFleet({ ...baseEnv, environments: [] });
    expect(await screen.findByText('box-1')).toBeTruthy();
    expect(screen.queryByRole('button', { name: /Move to env/ })).toBeNull();
    expect(screen.getByRole('button', { name: 'Drain' })).toBeTruthy(); // дрейн при этом жив
  });
});
