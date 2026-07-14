// Deploys × environments v1 §8: дефолт-скрытие disabled (тоггл), Promote-диалог
// (тело POST /v1/promote), бейдж env, карточка настроек env (PATCH). Провайдеры
// как в access.test: I18nProvider + Session + инъекция EnvContext; fetch — мок.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ReactNode } from 'react';
import type { Environment, SessionInfo, VersionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import { EnvContext } from '../lib/env';
import { Deploys } from '../screens/Deploys';

// Radix Dialog дёргает эти API при фокусе — в jsdom их нет.
HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

const dev: Environment = { project: 'game', name: 'dev', production: false, auto_deploy: true, retention_keep: 20, created_at: '2026-07-01T00:00:00Z' };
const prod: Environment = { project: 'game', name: 'prod', production: true, auto_deploy: false, retention_keep: 0, created_at: '2026-07-01T00:00:00Z' };

const mkVer = (over: Partial<VersionInfo>): VersionInfo => ({
  id: over.id ?? 'v1',
  project: 'game',
  semver: over.semver ?? '1.0.0',
  image_ref: over.image_ref ?? 'ghcr.io/x/game:1.0.0',
  env: over.env ?? 'dev',
  state: over.state ?? 'active',
  created_at: over.created_at ?? '2026-07-10T00:00:00Z',
  deprecated_at: over.deprecated_at,
  promoted_from: over.promoted_from,
});

const sess = (...scopes: SessionInfo['scopes']): SessionInfo => ({ scopes, name: 'k' });

const baseEnv = {
  environments: [dev, prod],
  project: 'game',
  selected: null as string | null,
  setSelected: () => {},
  loading: false,
  reload: () => {},
};

afterEach(() => {
  vi.unstubAllGlobals();
});

interface Recorded {
  url: string;
  method: string;
  body: unknown;
}

/** fetch-мок: GET-списки из versions; POST /v1/promote|deploy и PATCH env — 2xx.
 *  Пишет все не-GET вызовы в `calls` для проверки тел. */
function deploysFetch(versions: VersionInfo[], calls: Recorded[]) {
  return vi.fn((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? 'GET';
    if (method !== 'GET') {
      calls.push({ url: u, method, body: init?.body !== undefined ? JSON.parse(String(init.body)) : undefined });
    }
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
    if (u.includes('/v1/promote')) return json({ version: mkVer({ id: 'vp', env: 'prod' }), deploy: { state: 'prepulling', pending_nodes: 1 } }, 202);
    if (u.includes('/v1/deploy')) return json({ deploy: { state: 'prepulling', pending_nodes: 1 } });
    if (u.includes('/v1/environments/')) return json({ environment: dev });
    if (u.includes('/v1/versions')) return json({ versions });
    if (u.includes('/v1/servers')) return json({ servers: [] });
    if (u.includes('/v1/nodes')) return json({ nodes: [] });
    if (u.includes('/v1/events')) return json({ events: [] });
    return json({});
  });
}

function renderDeploys(value: Parameters<typeof EnvContext.Provider>[0]['value'], ui?: ReactNode) {
  return render(
    <SessionContext.Provider value={{ session: sess('admin'), login: async () => {}, logout: async () => {}, invalidate: () => {} }}>
      <I18nProvider initialLang="en">
        <EnvContext.Provider value={value}>{ui ?? <Deploys navigate={() => {}} />}</EnvContext.Provider>
      </I18nProvider>
    </SessionContext.Provider>,
  );
}

describe('Deploys — дефолт-скрытие disabled (M11)', () => {
  const versions = [
    mkVer({ id: 'v-act', semver: '1.0.0', state: 'active' }),
    mkVer({ id: 'v-old', semver: '0.9.0', state: 'disabled', created_at: '2026-07-01T00:00:00Z' }),
  ];

  it('по умолчанию disabled-версия скрыта; тоггл её показывает', async () => {
    vi.stubGlobal('fetch', deploysFetch(versions, []));
    renderDeploys(baseEnv);
    // Дождались загрузки (активная версия в таблице), disabled скрыта по умолчанию.
    await screen.findAllByText('1.0.0');
    expect(screen.queryByText('0.9.0')).toBeNull();
    // Снимаем «скрывать отключённые» → disabled-версия появляется.
    fireEvent.click(screen.getByRole('checkbox'));
    await waitFor(() => {
      expect(screen.getByText('0.9.0')).toBeTruthy();
    });
  });
});

describe('Deploys — Promote-диалог (тело POST /v1/promote)', () => {
  it('Promote у dev-версии → выбор prod-цели → POST {version_id, to_env}', async () => {
    const calls: Recorded[] = [];
    vi.stubGlobal('fetch', deploysFetch([mkVer({ id: 'v-act', semver: '1.0.0', state: 'active', env: 'dev' })], calls));
    renderDeploys(baseEnv);

    // Кнопка Promote есть у non-production версии.
    fireEvent.click(await screen.findByRole('button', { name: 'Promote' }));
    // В диалоге цель по умолчанию — prod; подтверждаем.
    const dialog = screen.getByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Promote' }));

    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('/v1/promote'))).toBe(true);
    });
    const promote = calls.find((c) => c.url.includes('/v1/promote'));
    expect(promote?.method).toBe('POST');
    expect(promote?.body).toEqual({ version_id: 'v-act', to_env: 'prod' });
  });

  it('у production-версии кнопки Promote нет', async () => {
    vi.stubGlobal('fetch', deploysFetch([mkVer({ id: 'v-p', semver: '2.0.0', state: 'active', env: 'prod' })], []));
    renderDeploys({ ...baseEnv, selected: 'prod' });
    await screen.findAllByText('2.0.0'); // active-версия видна и в окне, и в таблице
    expect(screen.queryByRole('button', { name: 'Promote' })).toBeNull();
  });

  // follow-up p5: при пустом/недоступном списке окружений isProdEnv() слеп
  // («всё не production») — раньше Promote вылезал даже у prod-версии и открывал
  // диалог без целей.
  it('список окружений пуст/недоступен → Promote скрыт (и у prod-, и у dev-версии)', async () => {
    const versions = [
      mkVer({ id: 'v-p', semver: '2.0.0', state: 'active', env: 'prod' }),
      mkVer({ id: 'v-d', semver: '1.0.0', state: 'deprecated', env: 'dev' }),
    ];
    vi.stubGlobal('fetch', deploysFetch(versions, []));
    renderDeploys({ ...baseEnv, environments: [] });
    await screen.findAllByText('2.0.0');
    expect(screen.queryByRole('button', { name: 'Promote' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Deploy' })).toBeTruthy(); // deploy при этом жив
  });
});

describe('Deploys — provenance promoted_from', () => {
  it('показывает «promoted from <env>/<semver>» по источнику', async () => {
    const versions = [
      mkVer({ id: 'src', semver: '1.0.0', state: 'deprecated', env: 'dev' }),
      mkVer({ id: 'dst', semver: '1.0.0', state: 'active', env: 'prod', promoted_from: 'src' }),
    ];
    vi.stubGlobal('fetch', deploysFetch(versions, []));
    renderDeploys({ ...baseEnv, selected: 'prod' });
    // Текст с префиксом «↳ » — матчим подстроку регуляркой.
    expect(await screen.findByText(/promoted from dev\/1\.0\.0/)).toBeTruthy();
  });
});

describe('Deploys — карточка настроек env (PATCH)', () => {
  it('admin меняет retention выбранного env → PATCH /v1/environments/{project}/{env}', async () => {
    const calls: Recorded[] = [];
    vi.stubGlobal('fetch', deploysFetch([mkVer({ id: 'v-act', semver: '1.0.0', state: 'active', env: 'dev' })], calls));
    renderDeploys({ ...baseEnv, selected: 'dev' });

    const retention = await screen.findByDisplayValue('20'); // dev.retention_keep
    fireEvent.change(retention, { target: { value: '5' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('/v1/environments/game/dev'))).toBe(true);
    });
    const patch = calls.find((c) => c.url.includes('/v1/environments/game/dev'));
    expect(patch?.method).toBe('PATCH');
    expect(patch?.body).toMatchObject({ retention_keep: 5, auto_deploy: true });
  });
});
