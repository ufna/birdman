// Секция «Проекты» в Доступе (слайс B). Проверяем три границы, а не вёрстку:
// дубль слага доносит до пользователя 409 мастера, живые ноды блокируют
// удаление (кнопка остаётся выключенной), а непустой проект требует ввода слага
// — то есть случайным кликом его не снести.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ProjectInfo, ProjectUsage } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { ProjectContext } from '../lib/project';
import { ToastProvider } from '../components/Toast';
import { ProjectsSection } from '../components/ProjectsSection';

HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

const projects: ProjectInfo[] = [
  { id: 'p1', slug: 'game', match_size: 2, created_at: '2026-07-01T00:00:00Z' },
];

const emptyUsage: ProjectUsage = {
  environments: 2,
  versions: 0,
  fleets: 0,
  nodes: 0,
  servers: 0,
  matches: 0,
  api_keys: 0,
};

function renderSection() {
  return render(
    <I18nProvider initialLang="en">
      <ToastProvider>
        <ProjectContext.Provider
          value={{ projects, selected: 'game', setSelected: () => {}, loading: false, reload: () => {} }}
        >
          <ProjectsSection />
        </ProjectContext.Provider>
      </ToastProvider>
    </I18nProvider>,
  );
}

/** fetch-мок: usage настраиваемый, POST /v1/projects — заданный ответ. */
function stubFetch(usage: ProjectUsage, create: { status: number; body: unknown } = { status: 201, body: {} }) {
  return vi.fn((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? 'GET';
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
    if (u.endsWith('/usage')) return json({ usage });
    if (method === 'POST' && u.includes('/v1/projects')) return json(create.body, create.status);
    return json({});
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Админка → Проекты', () => {
  it('проект виден в списке с размером матча', () => {
    vi.stubGlobal('fetch', stubFetch(emptyUsage));
    renderSection();
    expect(screen.getByText('game')).toBeTruthy();
    expect(screen.getByText(/match size 2/)).toBeTruthy();
  });

  it('занятый слаг: 409 мастера показывается в форме', async () => {
    vi.stubGlobal(
      'fetch',
      stubFetch(emptyUsage, { status: 409, body: { error: 'conflict', detail: 'project "game" already exists' } }),
    );
    renderSection();

    fireEvent.click(screen.getByRole('button', { name: 'New project' }));
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByPlaceholderText('my-game'), { target: { value: 'game' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));

    expect((await screen.findByRole('alert')).textContent).toContain('already exists');
  });

  it('живые ноды блокируют удаление, ввод слага не предлагается', async () => {
    vi.stubGlobal('fetch', stubFetch({ ...emptyUsage, nodes: 2, versions: 3 }));
    renderSection();

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    const dialog = await screen.findByRole('alertdialog');
    await waitFor(() => {
      expect(within(dialog).getByRole('alert').textContent).toContain('live node');
    });
    expect(within(dialog).queryByPlaceholderText('game')).toBeNull();
    expect(within(dialog).getByRole('button', { name: 'Delete' }).hasAttribute('disabled')).toBe(true);
  });

  it('непустой проект: кнопка оживает только после точного ввода слага', async () => {
    vi.stubGlobal('fetch', stubFetch({ ...emptyUsage, versions: 3, api_keys: 1 }));
    renderSection();

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    const dialog = await screen.findByRole('alertdialog');
    const input = await within(dialog).findByPlaceholderText('game');
    const confirm = within(dialog).getByRole('button', { name: 'Delete' });

    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.change(input, { target: { value: 'gam' } });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.change(input, { target: { value: 'game' } });
    expect(confirm.hasAttribute('disabled')).toBe(false);
  });
});
