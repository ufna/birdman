// Task 6 («Админка»: раздел «Реестры»): CRUD-вью поверх GET/POST/DELETE
// /v1/registries (docs/superpowers/specs/2026-07-09-registries-design.md §4).
// Рендерим весь экран Access (не изолированный RegistriesSection) — секция
// живёт там же, что и «API-ключи», и это ближе к реальной композиции.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { RegistryInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { Access } from '../screens/Access';

// Radix (Dialog/AlertDialog) дёргает эти API при фокусе — в jsdom их нет.
HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

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
    username: 'bot',
    note: 'main',
    created_at: '2026-07-08T00:00:00Z',
    updated_at: '2026-07-08T00:00:00Z',
    ...over,
  };
}

/**
 * fetch-мок для Access целиком: /v1/apikeys — фиксированный пустой список
 * (эти тесты не про ключи); /v1/registries — мутируемый список, POST
 * эмулирует апсерт по host (как реальный backend: тот же host заменяет
 * username/token/note), DELETE вычищает по id. `opts.postStatus/postBody` —
 * для сценария 400 (docker.io): вместо апсерта отвечает готовым error-body.
 */
function registriesMock(initial: RegistryInfo[], opts: { postStatus?: number; postBody?: unknown } = {}) {
  let regs = [...initial];
  return vi.fn((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? 'GET';
    if (u.includes('/v1/apikeys')) {
      return Promise.resolve(
        new Response(JSON.stringify(noKeys), { status: 200, headers: { 'Content-Type': 'application/json' } }),
      );
    }
    if (u.includes('/v1/registries')) {
      if (method === 'POST') {
        if (opts.postStatus !== undefined && opts.postStatus >= 400) {
          return Promise.resolve(
            new Response(JSON.stringify(opts.postBody), { status: opts.postStatus, headers: { 'Content-Type': 'application/json' } }),
          );
        }
        const body = JSON.parse(String(init?.body)) as { host: string; username: string; token: string; note?: string };
        const now = '2026-07-08T12:00:00Z';
        const existing = regs.find((r) => r.host === body.host);
        const updated: RegistryInfo = {
          id: existing?.id ?? `r-${String(regs.length + 1)}`,
          host: body.host,
          username: body.username,
          note: body.note ?? '',
          created_at: existing?.created_at ?? now,
          updated_at: now,
        };
        regs = existing !== undefined ? regs.map((r) => (r.id === existing.id ? updated : r)) : [...regs, updated];
        return Promise.resolve(
          new Response(JSON.stringify({ registry: updated }), { status: 201, headers: { 'Content-Type': 'application/json' } }),
        );
      }
      if (method === 'DELETE') {
        const id = u.split('/v1/registries/')[1];
        regs = regs.filter((r) => r.id !== id);
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(
        new Response(JSON.stringify({ registries: regs }), { status: 200, headers: { 'Content-Type': 'application/json' } }),
      );
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

  it('рендерит host/username/note/updated + маску токена (токен никогда не читается)', async () => {
    vi.stubGlobal('fetch', registriesMock([reg()]));
    renderEn();
    expect(await screen.findByText('ghcr.io')).toBeTruthy();
    expect(screen.getByText('bot')).toBeTruthy();
    expect(screen.getByText('main')).toBeTruthy();
    expect(screen.getByText('••••')).toBeTruthy();
  });
});

describe('Реестры — добавление (upsert)', () => {
  it('новый host появляется в списке', async () => {
    vi.stubGlobal('fetch', registriesMock([]));
    renderEn();
    await screen.findByText('No registries yet.');

    fireEvent.click(screen.getByRole('button', { name: 'Add registry' }));
    fireEvent.change(await screen.findByPlaceholderText('e.g. ghcr.io'), { target: { value: 'ghcr.io' } });
    fireEvent.change(screen.getByPlaceholderText('registry username'), { target: { value: 'bot' } });
    fireEvent.change(screen.getByPlaceholderText('personal access token'), { target: { value: 'tok_123' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(await screen.findByText('ghcr.io')).toBeTruthy();
    expect(screen.getByText('bot')).toBeTruthy();
  });

  it('повторный POST того же host заменяет username (не добавляет вторую строку)', async () => {
    vi.stubGlobal('fetch', registriesMock([reg({ username: 'bot' })]));
    renderEn();
    await screen.findByText('bot');

    fireEvent.click(screen.getByRole('button', { name: 'Add registry' }));
    fireEvent.change(await screen.findByPlaceholderText('e.g. ghcr.io'), { target: { value: 'ghcr.io' } });
    fireEvent.change(screen.getByPlaceholderText('registry username'), { target: { value: 'bot2' } });
    fireEvent.change(screen.getByPlaceholderText('personal access token'), { target: { value: 'newtok' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(await screen.findByText('bot2')).toBeTruthy();
    expect(screen.queryByText('bot')).toBeNull();
    // Один host — одна строка: если бы апсерт добавил вторую, getByText ниже
    // кинул бы "Found multiple elements".
    expect(screen.getByText('ghcr.io')).toBeTruthy();
  });

  it('400 docker.io — detail из ответа API показан в форме', async () => {
    vi.stubGlobal(
      'fetch',
      registriesMock([], {
        postStatus: 400,
        postBody: {
          error: 'bad_request',
          detail:
            'docker.io is not supported in v1 (host-match cannot follow its registry-1.docker.io resolution) — got "docker.io"',
        },
      }),
    );
    renderEn();
    await screen.findByText('No registries yet.');

    fireEvent.click(screen.getByRole('button', { name: 'Add registry' }));
    fireEvent.change(await screen.findByPlaceholderText('e.g. ghcr.io'), { target: { value: 'docker.io' } });
    fireEvent.change(screen.getByPlaceholderText('registry username'), { target: { value: 'bot' } });
    fireEvent.change(screen.getByPlaceholderText('personal access token'), { target: { value: 'tok' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(await screen.findByText(/docker\.io is not supported in v1/)).toBeTruthy();
    // Диалог остался открытым (ошибка показана рядом с формой, не потеряна).
    expect(screen.getByRole('button', { name: 'Save' })).toBeTruthy();
  });
});

describe('Реестры — удаление', () => {
  it('confirm → DELETE → строка пропадает', async () => {
    vi.stubGlobal('fetch', registriesMock([reg()]));
    renderEn();
    await screen.findByText('ghcr.io');

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    const dialog = await screen.findByRole('alertdialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete' }));

    await waitFor(() => {
      expect(screen.queryByText('ghcr.io')).toBeNull();
    });
    expect(await screen.findByText('No registries yet.')).toBeTruthy();
  });
});
