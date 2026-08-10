// Project-контекст (мультипроект W1): чистый резолв выбора, персист,
// сам селектор в Shell и связка «смена проекта → окружения перезапрашиваются
// с ?project= нового проекта».

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { Environment, ProjectInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { EnvProvider, useEnv } from '../lib/env';
import {
  PROJECT_STORAGE_KEY,
  ProjectProvider,
  eventProjectOf,
  keepForProject,
  resolveProject,
  storedProject,
  useProject,
} from '../lib/project';
import { ProjectSelector } from '../components/Shell';
import { Fleet } from '../screens/Fleet';

const proj = (slug: string): ProjectInfo => ({
  id: `id-${slug}`,
  slug,
  match_size: 2,
  created_at: '2026-07-01T00:00:00Z',
});
const game = proj('game');
const arena = proj('arena');

const devOf = (project: string): Environment => ({
  project,
  name: 'dev',
  production: false,
  auto_deploy: true,
  retention_keep: 20,
  created_at: '2026-07-01T00:00:00Z',
});

const jsonRes = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

// --- чистые хелперы ---

describe('project — resolveProject', () => {
  it('сохранённый слаг валиден → он', () => {
    expect(resolveProject('arena', [game, arena])).toBe('arena');
  });
  it('проекта из localStorage больше нет → ПЕРВЫЙ (старейший), а не null', () => {
    // «Все проекты» не существует как режим: без выбора панели нечего показывать.
    expect(resolveProject('ghost', [game, arena])).toBe('game');
  });
  it('выбора не было → первый', () => {
    expect(resolveProject(null, [game, arena])).toBe('game');
  });
  it('проектов нет вовсе → null (свежая установка)', () => {
    expect(resolveProject('game', [])).toBeNull();
    expect(resolveProject(null, [])).toBeNull();
  });
});

describe('project — storedProject', () => {
  it('пусто/стёрто → null, иначе слаг', () => {
    expect(storedProject()).toBeNull();
    localStorage.setItem(PROJECT_STORAGE_KEY, '');
    expect(storedProject()).toBeNull();
    localStorage.setItem(PROJECT_STORAGE_KEY, 'arena');
    expect(storedProject()).toBe('arena');
  });
});

describe('project — eventProjectOf / keepForProject (лента событий)', () => {
  const ev = (id: number, payload: Record<string, unknown>) => ({
    id,
    ts: '2026-08-01T00:00:00Z',
    kind: 'version_registered',
    payload,
  });

  it('проект события берётся из payload.project; иначе undefined', () => {
    expect(eventProjectOf(ev(1, { project: 'game' }))).toBe('game');
    expect(eventProjectOf(ev(2, {}))).toBeUndefined();
    // Не строка — тоже «не знаем», а не приведение к строке.
    expect(eventProjectOf(ev(3, { project: 42 }))).toBeUndefined();
  });

  it('фильтр НЕ скрывающий: уходят только события ЧУЖОГО проекта', () => {
    const events = [ev(1, { project: 'game' }), ev(2, { project: 'arena' }), ev(3, {})];
    const kept = keepForProject(events, 'game');
    // Событие без атрибуции ОСТАЁТСЯ: у проекта нет режима «Все», и строгий
    // фильтр спрятал бы его навсегда — это была бы потеря данных.
    expect(kept.map((e) => e.id)).toEqual([1, 3]);
  });

  it('проект не выбран → лента не режется', () => {
    const events = [ev(1, { project: 'game' }), ev(2, { project: 'arena' })];
    expect(keepForProject(events, null)).toHaveLength(2);
  });
});

// --- провайдер ---

function ProjectProbe() {
  const { selected, projects, loading } = useProject();
  return (
    <div>
      <span data-testid="selected">{selected ?? 'NONE'}</span>
      <span data-testid="count">{projects.length}</span>
      <span data-testid="loading">{loading ? 'yes' : 'no'}</span>
      <ProjectSelector />
    </div>
  );
}

function renderProjects(body: unknown, status = 200) {
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(jsonRes(body, status))));
  return render(
    <I18nProvider initialLang="en">
      <ProjectProvider>
        <ProjectProbe />
      </ProjectProvider>
    </I18nProvider>,
  );
}

describe('ProjectProvider', () => {
  it('дефолт — первый проект списка', async () => {
    renderProjects({ projects: [game, arena] });
    await waitFor(() => {
      expect(screen.getByTestId('selected').textContent).toBe('game');
    });
  });

  it('сохранённый выбор с прошлой сессии выигрывает у дефолта', async () => {
    localStorage.setItem(PROJECT_STORAGE_KEY, 'arena');
    renderProjects({ projects: [game, arena] });
    await waitFor(() => {
      expect(screen.getByTestId('selected').textContent).toBe('arena');
    });
  });

  it('сохранённый проект исчез из списка → откат на первый, но ПЕРСИСТ цел', async () => {
    localStorage.setItem(PROJECT_STORAGE_KEY, 'ghost');
    renderProjects({ projects: [game, arena] });
    await waitFor(() => {
      expect(screen.getByTestId('selected').textContent).toBe('game');
    });
    // Проект мог пропасть временно (мастер лежит) — выбор должен вернуться.
    expect(localStorage.getItem(PROJECT_STORAGE_KEY)).toBe('ghost');
  });

  it('проектов нет → выбора нет, и это не ошибка', async () => {
    renderProjects({ projects: [] });
    await waitFor(() => {
      expect(screen.getByTestId('loading').textContent).toBe('no');
    });
    expect(screen.getByTestId('selected').textContent).toBe('NONE');
    expect(screen.getByText('No projects yet')).toBeTruthy();
  });

  it('список не приехал → чип-предупреждение с повтором по клику', async () => {
    let call = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(() => {
        call += 1;
        return Promise.resolve(call === 1 ? jsonRes({ error: 'internal' }, 500) : jsonRes({ projects: [game] }));
      }),
    );
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <ProjectProbe />
        </ProjectProvider>
      </I18nProvider>,
    );
    const chip = await screen.findByRole('button', { name: /Projects unavailable/ });
    expect(screen.getByTestId('selected').textContent).toBe('NONE');

    fireEvent.click(chip);
    await waitFor(() => {
      expect(screen.getByTestId('selected').textContent).toBe('game');
    });
  });
});

// --- селектор ---

describe('ProjectSelector', () => {
  it('несколько проектов → select; выбор переключается и персистится', async () => {
    renderProjects({ projects: [game, arena] });
    const select = await screen.findByRole('combobox', { name: 'Project' });
    expect((select as HTMLSelectElement).value).toBe('game');

    fireEvent.change(select, { target: { value: 'arena' } });
    await waitFor(() => {
      expect(screen.getByTestId('selected').textContent).toBe('arena');
    });
    expect(localStorage.getItem(PROJECT_STORAGE_KEY)).toBe('arena');
  });

  it('ровно один проект → подпись без выпадающего списка (выбирать не из чего)', async () => {
    renderProjects({ projects: [game] });
    await waitFor(() => {
      expect(screen.getByTestId('selected').textContent).toBe('game');
    });
    expect(screen.queryByRole('combobox')).toBeNull();
    // Ищем внутри самого селектора: слаг «game» есть и в пробнике выбора.
    expect(within(screen.getByRole('group', { name: 'Project' })).getByText('game')).toBeTruthy();
  });
});

// --- связка project → env ---

/** Пробник: какие окружения видит EnvProvider под выбранным проектом. */
function EnvUnderProject() {
  const { environments } = useEnv();
  const { selected } = useProject();
  return (
    <div>
      <span data-testid="project">{selected ?? 'NONE'}</span>
      <span data-testid="envs">{environments.map((e) => `${e.project}/${e.name}`).join(',')}</span>
      <ProjectSelector />
    </div>
  );
}

describe('EnvProvider под ProjectProvider', () => {
  it('окружения запрашиваются с ?project= выбранного и ПЕРЕзапрашиваются при смене', async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        const u = String(url);
        urls.push(u);
        if (u.startsWith('/v1/projects')) return Promise.resolve(jsonRes({ projects: [game, arena] }));
        // Отдаём окружение ТОГО проекта, который спросили, — так видно, что
        // параметр реально доехал, а не «список просто перезапросили».
        const asked = new URL(u, 'http://x').searchParams.get('project') ?? '';
        return Promise.resolve(jsonRes({ environments: [devOf(asked)] }));
      }),
    );
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <EnvProvider>
            <EnvUnderProject />
          </EnvProvider>
        </ProjectProvider>
      </I18nProvider>,
    );

    await waitFor(() => {
      expect(screen.getByTestId('envs').textContent).toBe('game/dev');
    });
    expect(urls).toContain('/v1/environments?project=game');
    // Именно этого master и не мог отдать раньше: без ?project= при нескольких
    // проектах он отвечал 400 «several projects exist».
    expect(urls.some((u) => u === '/v1/environments')).toBe(false);

    fireEvent.change(await screen.findByRole('combobox', { name: 'Project' }), { target: { value: 'arena' } });
    await waitFor(() => {
      expect(screen.getByTestId('envs').textContent).toBe('arena/dev');
    });
    expect(urls).toContain('/v1/environments?project=arena');
  });

  it('экран Флот запрашивает ноды с ?project= и перезапрашивает при смене', async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        const u = String(url);
        urls.push(u);
        if (u.startsWith('/v1/projects')) return Promise.resolve(jsonRes({ projects: [game, arena] }));
        if (u.startsWith('/v1/environments')) return Promise.resolve(jsonRes({ environments: [] }));
        if (u.startsWith('/v1/nodes')) return Promise.resolve(jsonRes({ nodes: [] }));
        if (u.startsWith('/v1/servers')) return Promise.resolve(jsonRes({ servers: [] }));
        if (u.startsWith('/v1/versions')) return Promise.resolve(jsonRes({ versions: [] }));
        return Promise.resolve(jsonRes({ events: [] }));
      }),
    );
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <div>
            <ProjectSelector />
            <Fleet />
          </div>
        </ProjectProvider>
      </I18nProvider>,
    );

    await waitFor(() => {
      expect(urls.some((u) => u === '/v1/nodes?project=game')).toBe(true);
    });
    // Сужение серверное: голого /v1/nodes (все проекты сразу) быть не должно.
    expect(urls.some((u) => u === '/v1/nodes')).toBe(false);

    fireEvent.change(await screen.findByRole('combobox', { name: 'Project' }), { target: { value: 'arena' } });
    await waitFor(() => {
      expect(urls.some((u) => u === '/v1/nodes?project=arena')).toBe(true);
    });
  });

  it('проекта нет → окружения не запрашиваются вовсе', async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        urls.push(String(url));
        return Promise.resolve(jsonRes({ projects: [] }));
      }),
    );
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <EnvProvider>
            <EnvUnderProject />
          </EnvProvider>
        </ProjectProvider>
      </I18nProvider>,
    );
    await waitFor(() => {
      expect(screen.getByTestId('project').textContent).toBe('NONE');
    });
    expect(urls.some((u) => u.startsWith('/v1/environments'))).toBe(false);
  });
});
