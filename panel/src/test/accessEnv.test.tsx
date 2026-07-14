// Access × environments v1 §5/§8: привязка ключа (project, env) в теле POST
// /v1/apikeys, гейт bindable-скоупов (canBindScopes), и админка «Окружения»
// (создание с guardrail production×auto_deploy, тело POST /v1/environments,
// удаление непустого окружения — состав из /usage + подтверждение вводом имени).

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ReactNode } from 'react';
import type { Environment, EnvironmentUsage, Scope, SessionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import { EnvContext } from '../lib/env';
import { Access, BINDABLE_SCOPES, canBindScopes } from '../screens/Access';

HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

const dev: Environment = { project: 'game', name: 'dev', production: false, auto_deploy: true, retention_keep: 20, created_at: '2026-07-01T00:00:00Z' };
const prod: Environment = { project: 'game', name: 'prod', production: true, auto_deploy: false, retention_keep: 0, created_at: '2026-07-01T00:00:00Z' };

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

// --- чистый хелпер ---

describe('canBindScopes', () => {
  it('bindable-скоупы (deploy/mm/allocate) без admin → true; admin или отсутствие → false', () => {
    expect(BINDABLE_SCOPES).toEqual(['deploy', 'matchmaking', 'allocate']);
    expect(canBindScopes(new Set<Scope>(['deploy']))).toBe(true);
    expect(canBindScopes(new Set<Scope>(['matchmaking']))).toBe(true);
    expect(canBindScopes(new Set<Scope>(['deploy', 'admin']))).toBe(false); // admin несовместим
    expect(canBindScopes(new Set<Scope>(['readonly']))).toBe(false); // не bindable
    expect(canBindScopes(new Set<Scope>())).toBe(false);
  });
});

// --- рендер ---

interface Recorded {
  url: string;
  method: string;
  body: unknown;
}

const noUsage: EnvironmentUsage = { versions: 0, fleets: 0, nodes: 0, servers: 0, matches: 0, api_keys: 0 };

function accessFetch(calls: Recorded[], environments: Environment[], usage: EnvironmentUsage = noUsage) {
  return vi.fn((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? 'GET';
    if (method !== 'GET') {
      calls.push({ url: u, method, body: init?.body !== undefined ? JSON.parse(String(init.body)) : undefined });
    }
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
    // GET /v1/environments/{p}/{n}/usage — состав окружения для диалога удаления.
    if (u.includes('/usage')) return json({ usage });
    if (u.includes('/v1/environments')) {
      if (method === 'POST') return json({ environment: dev }, 201);
      if (method === 'DELETE') {
        return json({
          deleted: {
            name: 'dev',
            production: false,
            versions: usage.versions,
            fleets: usage.fleets,
            matches: usage.matches,
            servers: usage.servers,
            api_keys_revoked: usage.api_keys,
          },
        });
      }
      return json({ environments });
    }
    if (u.includes('/v1/apikeys')) {
      if (method === 'POST') {
        const b = JSON.parse(String(init?.body)) as Record<string, unknown>;
        return json({ key: { id: 'new', name: b.name, scopes: b.scopes, created_at: '2026-07-10T00:00:00Z', revoked_at: null, project: b.project, env: b.env }, secret: 'bmk_x' }, 201);
      }
      return json({ apikeys: [] });
    }
    if (u.includes('/v1/registries')) return json({ registries: [] });
    return json({});
  });
}

function renderAccess(calls: Recorded[], environments: Environment[] = [dev, prod], usage: EnvironmentUsage = noUsage) {
  vi.stubGlobal('fetch', accessFetch(calls, environments, usage));
  const value = { ...baseEnv, environments };
  const ui: ReactNode = (
    <SessionContext.Provider value={{ session: sess('admin'), login: async () => {}, logout: async () => {}, invalidate: () => {} }}>
      <I18nProvider initialLang="en">
        <EnvContext.Provider value={value}>
          <Access />
        </EnvContext.Provider>
      </I18nProvider>
    </SessionContext.Provider>
  );
  return render(ui);
}

describe('Access — привязка ключа (project, env) в теле POST /v1/apikeys', () => {
  it('deploy-ключ с выбранным env → тело несёт project+env', async () => {
    const calls: Recorded[] = [];
    renderAccess(calls);

    fireEvent.click(await screen.findByRole('button', { name: 'New key' }));
    const dialog = screen.getByRole('dialog');
    fireEvent.change(within(dialog).getByPlaceholderText('e.g. ci-deploy'), { target: { value: 'ci-dev' } });
    fireEvent.click(within(dialog).getByLabelText('Deploy'));
    // Привязка: селект (project · env) — выбираем dev.
    fireEvent.change(within(dialog).getByRole('combobox'), { target: { value: 'dev' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create key' }));

    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('/v1/apikeys') && c.method === 'POST')).toBe(true);
    });
    const post = calls.find((c) => c.url.includes('/v1/apikeys') && c.method === 'POST');
    expect(post?.body).toMatchObject({ name: 'ci-dev', scopes: ['deploy'], project: 'game', env: 'dev' });
  });

  it('глобальный ключ (Global) → тело без project/env', async () => {
    const calls: Recorded[] = [];
    renderAccess(calls);

    fireEvent.click(await screen.findByRole('button', { name: 'New key' }));
    const dialog = screen.getByRole('dialog');
    fireEvent.change(within(dialog).getByPlaceholderText('e.g. ci-deploy'), { target: { value: 'ci-global' } });
    fireEvent.click(within(dialog).getByLabelText('Deploy'));
    // Оставляем «Global» (дефолт селекта).
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create key' }));

    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('/v1/apikeys') && c.method === 'POST')).toBe(true);
    });
    const post = calls.find((c) => c.url.includes('/v1/apikeys') && c.method === 'POST');
    expect(post?.body).not.toHaveProperty('project');
    expect(post?.body).not.toHaveProperty('env');
  });
});

describe('Access — админка Окружения (create + guardrail)', () => {
  it('создание не-production env → POST /v1/environments с телом', async () => {
    const calls: Recorded[] = [];
    renderAccess(calls);

    fireEvent.click(await screen.findByRole('button', { name: 'Add environment' }));
    const dialog = screen.getByRole('dialog');
    fireEvent.change(within(dialog).getByPlaceholderText('e.g. staging'), { target: { value: 'staging' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));

    await waitFor(() => {
      expect(calls.some((c) => c.url.endsWith('/v1/environments') && c.method === 'POST')).toBe(true);
    });
    const post = calls.find((c) => c.url.endsWith('/v1/environments') && c.method === 'POST');
    expect(post?.body).toMatchObject({ project: 'game', name: 'staging', production: false, auto_deploy: false });
  });

  it('guardrail: отметка production глушит и снимает auto_deploy', async () => {
    renderAccess([]);
    fireEvent.click(await screen.findByRole('button', { name: 'Add environment' }));
    const dialog = screen.getByRole('dialog');
    const auto = within(dialog).getByLabelText('Auto-deploy new versions');
    fireEvent.click(auto); // включаем авто-деплой
    expect((auto as HTMLInputElement).checked).toBe(true);
    fireEvent.click(within(dialog).getByLabelText('Production environment'));
    // production ⇒ auto_deploy снят и заблокирован (guardrail).
    expect((auto as HTMLInputElement).checked).toBe(false);
    expect((auto as HTMLInputElement).disabled).toBe(true);
  });
});

// --- Удаление окружения ВМЕСТЕ с содержимым (запрос владельца) ---

/** Открывает диалог удаления первой строки таблицы окружений (dev). */
async function openDeleteDialog(): Promise<HTMLElement> {
  const rows = await screen.findAllByRole('button', { name: 'Delete' });
  fireEvent.click(rows[0]);
  return screen.findByRole('alertdialog');
}

const confirmLabel = 'Type the environment name to delete it:';

describe('Окружения — удаление непустого (состав + подтверждение вводом имени)', () => {
  it('диалог зовёт /usage и показывает состав (что снесём и сколько ключей отзовём)', async () => {
    const calls: Recorded[] = [];
    renderAccess(calls, [dev, prod], { versions: 3, fleets: 1, nodes: 0, servers: 2, matches: 5, api_keys: 1 });

    const dialog = await openDeleteDialog();
    await waitFor(() => {
      expect(within(dialog).getByTestId('env-usage-versions').textContent).toContain('3');
    });
    expect(within(dialog).getByTestId('env-usage-fleets').textContent).toContain('1');
    expect(within(dialog).getByTestId('env-usage-matches').textContent).toContain('5');
    expect(within(dialog).getByTestId('env-usage-servers').textContent).toContain('2');
    // Ключи — отдельной строкой (плюрал): «1 API key will be revoked.»
    expect(within(dialog).getByText(/1 API key will be revoked/)).toBeTruthy();
  });

  it('«Удалить» глухая, пока имя не введено ТОЧНО; точное имя → DELETE с {confirm}', async () => {
    const calls: Recorded[] = [];
    renderAccess(calls, [dev, prod], { versions: 2, fleets: 1, nodes: 0, servers: 0, matches: 0, api_keys: 0 });

    const dialog = await openDeleteDialog();
    const del = await within(dialog).findByRole('button', { name: 'Delete' });
    const input = within(dialog).getByLabelText(confirmLabel);
    expect((del as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(input, { target: { value: 'de' } }); // неполное имя
    expect((del as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(input, { target: { value: 'dev ' } }); // лишний пробел = не совпало
    expect((del as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(input, { target: { value: 'DEV' } }); // регистр важен
    expect((del as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(input, { target: { value: 'dev' } });
    expect((del as HTMLButtonElement).disabled).toBe(false);

    fireEvent.click(del);
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'DELETE')).toBe(true);
    });
    const req = calls.find((c) => c.method === 'DELETE');
    expect(req?.url).toContain('/v1/environments/game/dev');
    expect(req?.body).toEqual({ confirm: 'dev' });
  });

  it('ноды в окружении → блокирующее предупреждение, удаление недоступно', async () => {
    const calls: Recorded[] = [];
    renderAccess(calls, [dev, prod], { versions: 1, fleets: 1, nodes: 2, servers: 0, matches: 0, api_keys: 0 });

    const dialog = await openDeleteDialog();
    await waitFor(() => {
      expect(within(dialog).getByText(/2 nodes still live here/)).toBeTruthy();
    });
    // Шага подтверждения нет вовсе, кнопка удаления глухая.
    expect(within(dialog).queryByLabelText(confirmLabel)).toBeNull();
    const del = within(dialog).getByRole('button', { name: 'Delete' });
    expect((del as HTMLButtonElement).disabled).toBe(true);
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
  });

  it('кнопка копирования кладёт имя окружения в буфер', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    renderAccess([], [dev, prod], { versions: 1, fleets: 0, nodes: 0, servers: 0, matches: 0, api_keys: 0 });

    const dialog = await openDeleteDialog();
    // aria-label кнопки стабилен («Copy name»), меняется только видимая подпись.
    const copy = await within(dialog).findByRole('button', { name: 'Copy name' });
    fireEvent.click(copy);
    expect(writeText).toHaveBeenCalledWith('dev');
    await waitFor(() => {
      expect(copy.textContent).toBe('Copied');
    });
  });
});
