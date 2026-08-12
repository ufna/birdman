// Реестры v2 («Админка»: раздел «Реестры»): типизированный CRUD-вью поверх
// GET/POST/PATCH/DELETE /v1/registries (docs/superpowers/specs/
// 2026-07-10-registries-v2-design.md §4). Рендерим весь экран Access (секция
// живёт там же, что «API-ключи»). Покрываем: дропдаун типа переключает поля
// (gar → textarea, без username), add по типу, edit-карандаш (host read-only,
// keep-secret PATCH vs ротация), per-type подсказки, ошибку валидации с бэка,
// колонку «Тип» и то, что токен никогда не рендерится.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { RegistryInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { Access } from '../screens/Access';

// Radix (Dialog/AlertDialog) дёргает эти API при фокусе — в jsdom их нет.
HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

const JSON_HDR = { 'Content-Type': 'application/json' };

const renderEn = () =>
  render(
    <I18nProvider initialLang="en">
      <Access />
    </I18nProvider>,
  );

const noKeys = { apikeys: [] };

function reg(over: Partial<RegistryInfo> = {}): RegistryInfo {
  return {
    id: 'r1',
    host: 'ghcr.io',
    type: 'ghcr',
    username: 'bot',
    note: 'main',
    created_at: '2026-07-08T00:00:00Z',
    updated_at: '2026-07-08T00:00:00Z',
    ...over,
  };
}

/** Тело последнего запроса метода method к /v1/registries (JSON-парс). */
function lastBody(fetchMock: ReturnType<typeof vi.fn>, method: string): Record<string, unknown> | undefined {
  const calls = fetchMock.mock.calls.filter(
    ([u, init]) => String(u).includes('/v1/registries') && ((init as RequestInit | undefined)?.method ?? 'GET') === method,
  );
  const last = calls[calls.length - 1];
  return last ? (JSON.parse(String((last[1] as RequestInit).body)) as Record<string, unknown>) : undefined;
}

/** URL последнего запроса метода method к /v1/registries. */
function lastUrl(fetchMock: ReturnType<typeof vi.fn>, method: string): string | undefined {
  const calls = fetchMock.mock.calls.filter(
    ([u, init]) => String(u).includes('/v1/registries') && ((init as RequestInit | undefined)?.method ?? 'GET') === method,
  );
  const last = calls[calls.length - 1];
  return last ? String(last[0]) : undefined;
}

/**
 * fetch-мок для Access целиком: /v1/apikeys — пустой список (эти тесты не про
 * ключи); /v1/registries — мутируемый список. POST апсертит по host (gar
 * нормализуется мастером: username → `_json_key`), PATCH частично правит по id
 * (host неизменяем), DELETE вычищает по id. `opts.postStatus/postBody` — сценарий
 * 400 (кривой host/JSON): вместо апсерта возвращается готовый error-body.
 */
function registriesMock(initial: RegistryInfo[], opts: { postStatus?: number; postBody?: unknown } = {}) {
  let regs = [...initial];
  return vi.fn((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? 'GET';
    if (u.includes('/v1/apikeys')) {
      return Promise.resolve(new Response(JSON.stringify(noKeys), { status: 200, headers: JSON_HDR }));
    }
    if (u.includes('/v1/registries')) {
      if (method === 'POST') {
        if (opts.postStatus !== undefined && opts.postStatus >= 400) {
          return Promise.resolve(new Response(JSON.stringify(opts.postBody), { status: opts.postStatus, headers: JSON_HDR }));
        }
        const body = JSON.parse(String(init?.body)) as {
          host: string;
          type: RegistryInfo['type'];
          username?: string;
          token: string;
          note?: string;
        };
        const now = '2026-07-08T12:00:00Z';
        const existing = regs.find((r) => r.host === body.host);
        const updated: RegistryInfo = {
          id: existing?.id ?? `r-${String(regs.length + 1)}`,
          host: body.host,
          type: body.type,
          username: body.type === 'gar' ? '_json_key' : (body.username ?? ''),
          note: body.note ?? '',
          created_at: existing?.created_at ?? now,
          updated_at: now,
        };
        regs = existing !== undefined ? regs.map((r) => (r.id === existing.id ? updated : r)) : [...regs, updated];
        return Promise.resolve(new Response(JSON.stringify({ registry: updated }), { status: 201, headers: JSON_HDR }));
      }
      if (method === 'PATCH') {
        const id = u.split('/v1/registries/')[1];
        const body = JSON.parse(String(init?.body)) as {
          type?: RegistryInfo['type'];
          username?: string;
          token?: string;
          note?: string;
        };
        const now = '2026-07-08T13:00:00Z';
        const existing = regs.find((r) => r.id === id);
        if (existing === undefined) {
          return Promise.resolve(new Response(JSON.stringify({ error: 'not_found' }), { status: 404, headers: JSON_HDR }));
        }
        const type = body.type ?? existing.type;
        const updated: RegistryInfo = {
          ...existing,
          type,
          username: type === 'gar' ? '_json_key' : (body.username ?? existing.username),
          note: body.note ?? existing.note,
          updated_at: now,
        };
        regs = regs.map((r) => (r.id === id ? updated : r));
        return Promise.resolve(new Response(JSON.stringify({ registry: updated }), { status: 200, headers: JSON_HDR }));
      }
      if (method === 'DELETE') {
        const id = u.split('/v1/registries/')[1];
        regs = regs.filter((r) => r.id !== id);
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(new Response(JSON.stringify({ registries: regs }), { status: 200, headers: JSON_HDR }));
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Реестры — список', () => {
  it('пусто → пустое состояние', async () => {
    vi.stubGlobal('fetch', registriesMock([]));
    renderEn();
    expect(await screen.findByText('No registries yet.')).toBeTruthy();
  });

  it('рендерит host/username/note/тип/updated + маску токена (токен никогда не читается)', async () => {
    vi.stubGlobal('fetch', registriesMock([reg()]));
    renderEn();
    expect(await screen.findByText('ghcr.io')).toBeTruthy();
    expect(screen.getByText('bot')).toBeTruthy();
    expect(screen.getByText('main')).toBeTruthy();
    // Маска токена — вместо секрета (RegistryInfo структурно его не несёт).
    expect(screen.getByText('••••')).toBeTruthy();
    // Колонка «Тип» + подпись типа в строке (label дропдауна, не сырой код).
    expect(screen.getByText('Type')).toBeTruthy();
    expect(screen.getByText('ghcr')).toBeTruthy(); // exact — не путается с 'ghcr.io'
  });
});

describe('Реестры — тип и динамическая форма', () => {
  it('дропдаун переключает поля: gar → textarea + нет username; generic/ghcr → username есть', async () => {
    vi.stubGlobal('fetch', registriesMock([]));
    renderEn();
    await screen.findByText('No registries yet.');

    fireEvent.click(screen.getByRole('button', { name: 'Add registry' }));
    const dialog = await screen.findByRole('dialog');

    // ghcr (дефолт): есть username, token — однострочный input.
    expect(within(dialog).getByLabelText('Username')).toBeTruthy();
    expect((within(dialog).getByLabelText('Token') as HTMLElement).tagName).toBe('INPUT');

    // gar: username исчезает, token становится textarea.
    fireEvent.change(within(dialog).getByRole('combobox'), { target: { value: 'gar' } });
    expect(within(dialog).queryByLabelText('Username')).toBeNull();
    expect((within(dialog).getByLabelText('Token') as HTMLElement).tagName).toBe('TEXTAREA');

    // generic: username снова появляется, token — input.
    fireEvent.change(within(dialog).getByRole('combobox'), { target: { value: 'generic' } });
    expect(within(dialog).getByLabelText('Username')).toBeTruthy();
    expect((within(dialog).getByLabelText('Token') as HTMLElement).tagName).toBe('INPUT');
  });

  it('подсказка под тип меняется с дропдауном', async () => {
    vi.stubGlobal('fetch', registriesMock([]));
    renderEn();
    await screen.findByText('No registries yet.');

    fireEvent.click(screen.getByRole('button', { name: 'Add registry' }));
    const dialog = await screen.findByRole('dialog');

    // ghcr: подсказка про PAT read:packages.
    expect(within(dialog).getByText(/read:packages/)).toBeTruthy();
    expect(within(dialog).queryByText(/Artifact Registry Reader/)).toBeNull();

    // gar: подсказка про SA JSON key / Artifact Registry Reader.
    fireEvent.change(within(dialog).getByRole('combobox'), { target: { value: 'gar' } });
    expect(within(dialog).getByText(/Artifact Registry Reader/)).toBeTruthy();
    expect(within(dialog).queryByText(/read:packages/)).toBeNull();
  });
});

describe('Реестры — добавление (upsert по типу)', () => {
  it('ghcr → POST с type=ghcr и username', async () => {
    const fetchMock = registriesMock([]);
    vi.stubGlobal('fetch', fetchMock);
    renderEn();
    await screen.findByText('No registries yet.');

    fireEvent.click(screen.getByRole('button', { name: 'Add registry' }));
    const dialog = await screen.findByRole('dialog');
    // host для ghcr предзаполнен 'ghcr.io'.
    expect((within(dialog).getByLabelText('Host') as HTMLInputElement).value).toBe('ghcr.io');
    fireEvent.change(within(dialog).getByLabelText('Username'), { target: { value: 'bot' } });
    fireEvent.change(within(dialog).getByLabelText('Token'), { target: { value: 'tok_123' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(lastBody(fetchMock, 'POST')).toBeDefined();
    });
    const body = lastBody(fetchMock, 'POST');
    expect(body).toMatchObject({ host: 'ghcr.io', type: 'ghcr', username: 'bot', token: 'tok_123' });
    // Новая строка появилась.
    expect(await screen.findByText('ghcr.io')).toBeTruthy();
  });

  it('gar → POST с type=gar и БЕЗ username (мастер форсит _json_key)', async () => {
    const fetchMock = registriesMock([]);
    vi.stubGlobal('fetch', fetchMock);
    renderEn();
    await screen.findByText('No registries yet.');

    fireEvent.click(screen.getByRole('button', { name: 'Add registry' }));
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByRole('combobox'), { target: { value: 'gar' } });
    fireEvent.change(within(dialog).getByLabelText('Host'), { target: { value: 'us-docker.pkg.dev' } });
    fireEvent.change(within(dialog).getByLabelText('Token'), {
      target: { value: '{"type":"service_account","private_key":"x"}' },
    });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(lastBody(fetchMock, 'POST')).toBeDefined();
    });
    const body = lastBody(fetchMock, 'POST');
    expect(body?.type).toBe('gar');
    expect(body?.host).toBe('us-docker.pkg.dev');
    expect(String(body?.token)).toContain('service_account');
    // username не отправляется (сервер форсит _json_key).
    expect(body).not.toHaveProperty('username');
  });

  it('400 (кривой GAR JSON) — «проверьте поля», detail мастера не показан (tracker #1005)', async () => {
    vi.stubGlobal(
      'fetch',
      registriesMock([], {
        postStatus: 400,
        postBody: { error: 'bad_request', detail: 'GAR credential must be a service-account JSON key' },
      }),
    );
    renderEn();
    await screen.findByText('No registries yet.');

    fireEvent.click(screen.getByRole('button', { name: 'Add registry' }));
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByRole('combobox'), { target: { value: 'gar' } });
    fireEvent.change(within(dialog).getByLabelText('Host'), { target: { value: 'us-docker.pkg.dev' } });
    fireEvent.change(within(dialog).getByLabelText('Token'), { target: { value: 'not-json' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));

    expect(await screen.findByText(/rejected these values/)).toBeTruthy();
    expect(document.body.textContent).not.toContain('service-account JSON key');
    // Диалог остался открытым (ошибка рядом с формой, не потеряна).
    expect(within(dialog).getByRole('button', { name: 'Save' })).toBeTruthy();
  });
});

describe('Реестры — редактирование (PATCH)', () => {
  it('карандаш открывает форму предзаполненной: host read-only + type/username/note из строки', async () => {
    vi.stubGlobal('fetch', registriesMock([reg()]));
    renderEn();
    await screen.findByText('ghcr.io');

    fireEvent.click(screen.getByRole('button', { name: 'Edit' }));
    const dialog = await screen.findByRole('dialog');

    const hostInput = within(dialog).getByLabelText('Host') as HTMLInputElement;
    expect(hostInput.value).toBe('ghcr.io');
    expect(hostInput.readOnly).toBe(true); // host неизменяем
    expect((within(dialog).getByRole('combobox') as HTMLSelectElement).value).toBe('ghcr');
    expect((within(dialog).getByLabelText('Username') as HTMLInputElement).value).toBe('bot');
    expect((within(dialog).getByLabelText('Note') as HTMLInputElement).value).toBe('main');
  });

  it('пустой token → PATCH БЕЗ token в теле (секрет не трогаем)', async () => {
    const fetchMock = registriesMock([reg()]);
    vi.stubGlobal('fetch', fetchMock);
    renderEn();
    await screen.findByText('ghcr.io');

    fireEvent.click(screen.getByRole('button', { name: 'Edit' }));
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText('Username'), { target: { value: 'bot2' } });
    // token оставляем пустым.
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(lastBody(fetchMock, 'PATCH')).toBeDefined();
    });
    const body = lastBody(fetchMock, 'PATCH');
    expect(body).not.toHaveProperty('token'); // keep-secret
    expect(body).toMatchObject({ type: 'ghcr', username: 'bot2', note: 'main' });
    expect(lastUrl(fetchMock, 'PATCH')).toContain('/v1/registries/r1');
    // username обновился в списке.
    expect(await screen.findByText('bot2')).toBeTruthy();
  });

  it('заполненный token → PATCH С token (ротация)', async () => {
    const fetchMock = registriesMock([reg()]);
    vi.stubGlobal('fetch', fetchMock);
    renderEn();
    await screen.findByText('ghcr.io');

    fireEvent.click(screen.getByRole('button', { name: 'Edit' }));
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText('Token'), { target: { value: 'rotated_tok' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(lastBody(fetchMock, 'PATCH')).toBeDefined();
    });
    const body = lastBody(fetchMock, 'PATCH');
    expect(body?.token).toBe('rotated_tok'); // ротация
  });
});

describe('Реестры — удаление', () => {
  it('confirm → DELETE → строка пропадает', async () => {
    vi.stubGlobal('fetch', registriesMock([reg()]));
    renderEn();
    await screen.findByText('ghcr.io');

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    const alert = await screen.findByRole('alertdialog');
    fireEvent.click(within(alert).getByRole('button', { name: 'Delete' }));

    await waitFor(() => {
      expect(screen.queryByText('ghcr.io')).toBeNull();
    });
    expect(await screen.findByText('No registries yet.')).toBeTruthy();
  });
});
