import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { SessionInfo } from '../lib/api';
import { ApiError } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { navItemsFor } from '../components/Shell';
import { Access, lastAdminKeyMessage } from '../screens/Access';

// Radix (Dialog/AlertDialog) дёргает эти API при фокусе — в jsdom их нет.
HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

const sess = (...scopes: SessionInfo['scopes']): SessionInfo => ({ scopes, name: 'k' });
const renderEn = (ui: React.ReactElement) => render(<I18nProvider initialLang="en">{ui}</I18nProvider>);

describe('scope-гейт экрана Доступ (navItemsFor)', () => {
  it('admin видит /access; readonly и не-сессия — нет', () => {
    expect(navItemsFor(sess('admin')).map((i) => i.path)).toContain('/access');
    expect(navItemsFor(sess('readonly')).map((i) => i.path)).not.toContain('/access');
    expect(navItemsFor(null).map((i) => i.path)).not.toContain('/access');
  });
  it('прочие П2-экраны видны всем (readonly тоже)', () => {
    const paths = navItemsFor(sess('readonly')).map((i) => i.path);
    expect(paths).toEqual(expect.arrayContaining(['/stats', '/cost', '/alerts']));
  });
});

describe('lastAdminKeyMessage — 409 last_admin_key', () => {
  const t = ((k: string) => k) as Parameters<typeof lastAdminKeyMessage>[0];
  it('409 last_admin_key → человекочитаемый ключ, прочее → undefined', () => {
    const map = lastAdminKeyMessage(t);
    expect(map(new ApiError(409, 'last_admin_key', 'self-lockout'))).toBe('access.err.lastAdmin');
    expect(map(new ApiError(404, 'not_found'))).toBeUndefined();
    expect(map(new Error('x'))).toBeUndefined();
  });
});

// --- рендер: fetch по методу+URL ---

const adminKey = {
  id: '00000000-0000-4000-8000-0000000000ad',
  name: 'bootstrap-admin',
  scopes: ['admin'],
  created_at: '2026-07-08T00:02:13Z',
  revoked_at: null,
};
const created = {
  key: { id: 'new-id', name: 'ci-deploy', scopes: ['deploy'], created_at: '2026-07-08T10:00:00Z', revoked_at: null },
  secret: 'bmk_new.secretvalue123',
};

function apiMock(onDelete?: () => { status: number; body: unknown }) {
  return vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    let res: { status: number; body: unknown } = { status: 200, body: { apikeys: [adminKey] } };
    // Access теперь всегда рендерит и секцию «Реестры» (Task 6) — она делает
    // свой GET /v1/registries на маунте; эти тесты про ключи, так что отдаём
    // пустой список (иначе секция зависла бы в LoadingRow навсегда).
    if (url.includes('/v1/registries')) {
      res = { status: 200, body: { registries: [] } };
    } else if (url.includes('/v1/apikeys')) {
      if (method === 'POST') res = { status: 201, body: created };
      else if (method === 'DELETE') res = onDelete ? onDelete() : { status: 200, body: { key: adminKey } };
    }
    return Promise.resolve(new Response(JSON.stringify(res.body), { status: res.status, headers: { 'Content-Type': 'application/json' } }));
  });
}

const revokedKey = {
  id: 'rk-1',
  name: 'old-ci',
  scopes: ['deploy'],
  created_at: '2026-07-01T00:00:00Z',
  revoked_at: '2026-07-05T00:00:00Z',
};

/**
 * fetch-мок для purge-тестов: держит мутируемый список ключей (как
 * registriesMock в registries.test.tsx), чтобы reload после purge реально
 * отражал удаление строки. /v1/registries — пустой список (не о нём тест).
 */
function apiMockWithList(
  initial: { id: string; name: string; scopes: string[]; created_at: string; revoked_at: string | null }[],
  onPurge?: () => { status: number; body: unknown },
) {
  let list = [...initial];
  return vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    if (url.includes('/v1/registries')) {
      return Promise.resolve(new Response(JSON.stringify({ registries: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }
    if (url.includes('/v1/apikeys')) {
      if (method === 'DELETE') {
        if (url.includes('purge=true')) {
          if (onPurge) {
            const res = onPurge();
            return Promise.resolve(
              new Response(res.body != null ? JSON.stringify(res.body) : null, {
                status: res.status,
                headers: res.body != null ? { 'Content-Type': 'application/json' } : {},
              }),
            );
          }
          const id = url.split('/v1/apikeys/')[1]?.split('?')[0];
          list = list.filter((k) => k.id !== id);
          return Promise.resolve(new Response(null, { status: 204 }));
        }
        return Promise.resolve(new Response(JSON.stringify({ key: list[0] }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
      }
      return Promise.resolve(new Response(JSON.stringify({ apikeys: list }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Access — список ключей', () => {
  it('рендерит имя, скоуп-чип и статус', async () => {
    vi.stubGlobal('fetch', apiMock());
    renderEn(<Access />);
    expect(await screen.findByText('bootstrap-admin')).toBeTruthy();
    // Скоуп-чип "Admin" совпадает текстом с заголовком экрана (nav.access
    // теперь тоже "Admin", Task 6 rename) — скоуп внутри таблицы ключей.
    const table = screen.getByRole('table');
    expect(within(table).getByText('Admin')).toBeTruthy(); // scope-чип
    expect(screen.getByText('Active')).toBeTruthy(); // статус-бейдж
  });
});

describe('Access — создание ключа: секрет показывается один раз', () => {
  it('форма → секрет с копированием → закрытие вычищает секрет', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    vi.stubGlobal('fetch', apiMock());

    renderEn(<Access />);
    await screen.findByText('bootstrap-admin');

    fireEvent.click(screen.getByRole('button', { name: 'New key' }));
    fireEvent.change(await screen.findByPlaceholderText('e.g. ci-deploy'), { target: { value: 'ci-deploy' } });
    fireEvent.click(screen.getByRole('checkbox', { name: 'Deploy' }));
    fireEvent.click(screen.getByRole('button', { name: 'Create key' }));

    // Секрет виден ровно здесь + предупреждение «больше не покажем».
    expect(await screen.findByText('bmk_new.secretvalue123')).toBeTruthy();
    expect(screen.getByText("Copy the secret now — it won't be shown again.")).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Copy' }));
    await waitFor(() => {
      expect(writeText).toHaveBeenCalledWith('bmk_new.secretvalue123');
    });

    // Закрыли — секрет ушёл из памяти/DOM.
    fireEvent.click(screen.getByRole('button', { name: 'Done' }));
    await waitFor(() => {
      expect(screen.queryByText('bmk_new.secretvalue123')).toBeNull();
    });
  });
});

describe('Access — отзыв: 409 last_admin_key человекочитаемо', () => {
  it('DELETE 409 → показывает локализованную ошибку, ключ не пропадает', async () => {
    vi.stubGlobal(
      'fetch',
      apiMock(() => ({ status: 409, body: { error: 'last_admin_key', detail: 'refusing to revoke the last active admin key' } })),
    );
    renderEn(<Access />);
    await screen.findByText('bootstrap-admin');

    fireEvent.click(screen.getByRole('button', { name: 'Revoke' })); // триггер строки
    const dialog = await screen.findByRole('alertdialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Revoke' })); // подтверждение

    expect(await screen.findByText("Can't revoke the last active admin key (self-lockout).")).toBeTruthy();
  });
});

describe('Access — purge отозванных ключей (Task 6, DELETE ?purge=true)', () => {
  it('purge-кнопка есть ТОЛЬКО у revoked-строки; активная сохраняет Revoke', async () => {
    vi.stubGlobal('fetch', apiMockWithList([adminKey, revokedKey]));
    renderEn(<Access />);
    await screen.findByText('bootstrap-admin');
    await screen.findByText('old-ci');

    expect(screen.getAllByRole('button', { name: 'Revoke' }).length).toBe(1);
    expect(screen.getAllByRole('button', { name: 'Delete forever' }).length).toBe(1);
  });

  it('confirm → DELETE .../{id}?purge=true → строка пропадает после reload', async () => {
    const fetchMock = apiMockWithList([revokedKey]);
    vi.stubGlobal('fetch', fetchMock);
    renderEn(<Access />);
    await screen.findByText('old-ci');

    fireEvent.click(screen.getByRole('button', { name: 'Delete forever' })); // триггер строки
    const dialog = await screen.findByRole('alertdialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete forever' })); // подтверждение

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining(`/v1/apikeys/${revokedKey.id}?purge=true`),
        expect.objectContaining({ method: 'DELETE' }),
      );
    });
    await waitFor(() => {
      expect(screen.queryByText('old-ci')).toBeNull();
    });
  });

  it('409 not_revoked (гипотетический прямой вызов на активном) — сообщение в диалоге, ключ остаётся', async () => {
    // Purge-кнопка в UI недостижима у active-строк (гейт кнопкой), но store
    // всё равно возвращает 409 — конфирм должен пережить это, не потеряв ключ.
    vi.stubGlobal(
      'fetch',
      apiMockWithList([revokedKey], () => ({ status: 409, body: { error: 'not_revoked', detail: 'key is still active; revoke it before purging' } })),
    );
    renderEn(<Access />);
    await screen.findByText('old-ci');

    fireEvent.click(screen.getByRole('button', { name: 'Delete forever' }));
    const dialog = await screen.findByRole('alertdialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete forever' }));

    expect(await screen.findByText(/State conflict/)).toBeTruthy();
    expect(document.body.textContent).not.toContain('revoke it before purging');
    expect(screen.getByText('old-ci')).toBeTruthy();
  });
});
