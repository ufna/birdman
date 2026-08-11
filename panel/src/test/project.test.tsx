// Project-контекст (мультипроект W1): чистый резолв выбора, персист,
// сам селектор в Shell и связка «смена проекта → окружения перезапрашиваются
// с ?project= нового проекта».

import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { Environment, ProjectInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { EnvProvider, useEnv } from '../lib/env';
import { LiveContext } from '../lib/live';
import type { StreamEvent } from '../lib/sse';
import { EventsFeed } from '../components/EventsFeed';
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
import { Events } from '../screens/Events';
import { Stats } from '../screens/Stats';
import { DrawerProvider } from '../lib/drawer';

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

// --- фикстуры и стабы для лент событий (экран Событий + лента Обзора) ---

interface EventFixture {
  id: number;
  ts: string;
  kind: string;
  project?: string;
  payload: Record<string, unknown>;
}

const gameEvent: EventFixture = {
  id: 1, ts: '2026-08-01T10:00:00Z', kind: 'version_registered', project: 'game', payload: { semver: '1.0.0' },
};
const arenaEvent: EventFixture = {
  id: 2, ts: '2026-08-01T11:00:00Z', kind: 'version_registered', project: 'arena', payload: { semver: '9.9.9' },
};
// Без поля project — платформенное: принадлежит установке, а не проекту.
const platformEvent: EventFixture = {
  id: 3, ts: '2026-08-01T12:00:00Z', kind: 'backup_failed', payload: { note: 'nightly' },
};

/**
 * Стаб мастера для лент. `/v1/events` ведёт себя КАК СЕРВЕР: с `?project=`
 * отдаёт события этого проекта плюс платформенные (сужение не скрывающее,
 * master store/events.go), а БЕЗ параметра — вообще всё, события всех проектов.
 * Второе и делает голый запрос опасным: клиентского фильтра списка больше нет.
 */
function stubFeedApi(urls: string[], events: EventFixture[]) {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      const u = String(url);
      urls.push(u);
      if (u.startsWith('/v1/projects')) return Promise.resolve(jsonRes({ projects: [game, arena] }));
      if (u.startsWith('/v1/environments')) return Promise.resolve(jsonRes({ environments: [] }));
      if (u.startsWith('/v1/nodes')) return Promise.resolve(jsonRes({ nodes: [] }));
      if (u.startsWith('/v1/events')) {
        const want = new URL(u, 'http://x').searchParams.get('project');
        const list =
          want === null ? events : events.filter((e) => e.project === undefined || e.project === want);
        return Promise.resolve(jsonRes({ events: list }));
      }
      return Promise.resolve(jsonRes({}));
    }),
  );
}

/** Запросы к ленте, ушедшие БЕЗ `?project=`; их не должно быть ни одного. */
const bareFeedRequests = (urls: string[]) =>
  urls.filter((u) => u.startsWith('/v1/events') && !u.includes('project='));

/** Строка ленты (`<li>`), содержащая заданный текст. */
function rowOf(text: RegExp): HTMLElement {
  const li = screen.getByText(text).closest('li');
  if (li === null) throw new Error(`строка ленты с ${String(text)} не найдена`);
  return li;
}

/** Управляемый LiveContext: тест сам решает, когда стрим прислал событие. */
function makeLive() {
  const listeners = new Set<(e: StreamEvent) => void>();
  const value = {
    status: 'live' as const,
    subscribe: (fn: (e: StreamEvent) => void) => {
      listeners.add(fn);
      return () => {
        listeners.delete(fn);
      };
    },
  };
  const emit = (e: EventFixture) => {
    listeners.forEach((fn) => {
      fn({ id: e.id, kind: e.kind, event: e });
    });
  };
  return { emit, value };
}

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

// Эпик #968 закрыт: проект приезжает ПОЛЕМ события (колонка events.project_id),
// а не угадывается по payload. Список сервер сужает сам; эти функции остались
// для событий из живого стрима — он один на сессию и о выбранном проекте не
// знает (LiveProvider по устройству дерева стоит выше ProjectProvider).
describe('project — eventProjectOf / keepForProject (события из живого стрима)', () => {
  const ev = (id: number, project?: string) => ({
    id,
    ts: '2026-08-01T00:00:00Z',
    kind: 'version_registered',
    project,
    payload: {},
  });

  it('проект берётся из поля события; пусто — платформенное', () => {
    expect(eventProjectOf(ev(1, 'game'))).toBe('game');
    expect(eventProjectOf(ev(2))).toBeUndefined();
    // Пустая строка = платформенное событие, а не проект с пустым именем.
    expect(eventProjectOf(ev(3, ''))).toBeUndefined();
  });

  it('фильтр НЕ скрывающий: уходят только события ЧУЖОГО проекта', () => {
    const events = [ev(1, 'game'), ev(2, 'arena'), ev(3)];
    const kept = keepForProject(events, 'game');
    // Платформенное событие ОСТАЁТСЯ: спрятать его значило бы утверждать, что
    // при выбранном проекте на платформе ничего не происходит.
    expect(kept.map((e) => e.id)).toEqual([1, 3]);
  });

  it('проект не выбран → лента не режется', () => {
    // null — проекта ещё нет; '' — провайдера нет вовсе (изолированный
    // юнит-тест экрана). Обе трактовки значат «проектного измерения нет».
    expect(keepForProject([ev(1, 'game'), ev(2, 'arena')], null)).toHaveLength(2);
    expect(keepForProject([ev(1, 'game'), ev(2, 'arena')], '')).toHaveLength(2);
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

  // --- лента событий: сужение СЕРВЕРНОЕ (эпик #968) ---
  //
  // Клиентского проектного фильтра у списка больше нет, поэтому единственная
  // защита от чужих событий — сам ЗАПРОС. Значит и проверять надо запрос, как в
  // кейсе про Флот выше: утверждения про видимый набор переживают откат
  // серверного сужения, пока в браузере лежит вторая копия правила (ревью #987).

  it('экран Событий: голого /v1/events не бывает, смена проекта перезапрашивает суженную ленту', async () => {
    const urls: string[] = [];
    stubFeedApi(urls, [gameEvent, arenaEvent, platformEvent]);
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <DrawerProvider>
            <div>
              <ProjectSelector />
              <Events />
            </div>
          </DrawerProvider>
        </ProjectProvider>
      </I18nProvider>,
    );

    await waitFor(() => {
      expect(screen.getByText(/1\.0\.0/)).toBeTruthy();
    });
    // Сужение серверное: голой ленты (события всех проектов сразу) быть не
    // должно НИ ОДНОГО раза — в том числе в окне, пока грузится /v1/projects.
    expect(bareFeedRequests(urls)).toEqual([]);
    expect(urls).toContain('/v1/events?limit=500&project=game');
    expect(screen.queryByText(/9\.9\.9/)).toBeNull();

    // Сменить ОКНО ленты, затем проект. Порядок не случаен: раньше первый шаг
    // маскировал дефект (эффект перезапускался по limit, и запрос ВПЕРВЫЕ уходил
    // суженным), а второй его показывал — рефетча по смене проекта не было, и
    // своих событий нового проекта на экране не появлялось вовсе (ревью #987).
    fireEvent.change(screen.getByRole('combobox', { name: 'Feed window size' }), { target: { value: '200' } });
    await waitFor(() => {
      expect(urls).toContain('/v1/events?limit=200&project=game');
    });

    fireEvent.change(await screen.findByRole('combobox', { name: 'Project' }), { target: { value: 'arena' } });
    await waitFor(() => {
      expect(urls).toContain('/v1/events?limit=200&project=arena');
    });
    await waitFor(() => {
      expect(screen.getByText(/9\.9\.9/)).toBeTruthy();
    });
    expect(screen.queryByText(/1\.0\.0/)).toBeNull();
    expect(bareFeedRequests(urls)).toEqual([]);
  });

  // «Проект неизвестен» и «проектов нет» — РАЗНЫЕ состояния, и лента обязана
  // вести себя по-разному. Спутать их — значит либо показать чужое (первое как
  // второе), либо спрятать платформенные события на свежей установке (второе
  // как первое). Второе и произошло в первой попытке доработки #987.

  it('список проектов ещё грузится → лента не запрашивается (чей это срез — неизвестно)', async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        const u = String(url);
        urls.push(u);
        // /v1/projects не отвечает никогда — провайдер остаётся в loading.
        if (u.startsWith('/v1/projects')) return new Promise<Response>(() => undefined);
        return Promise.resolve(jsonRes({}));
      }),
    );
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <DrawerProvider>
            <Events />
          </DrawerProvider>
        </ProjectProvider>
      </I18nProvider>,
    );
    await waitFor(() => {
      expect(urls.some((u) => u.startsWith('/v1/projects'))).toBe(true);
    });
    expect(urls.some((u) => u.startsWith('/v1/events'))).toBe(false);
  });

  it('/v1/projects упал → лента тоже не запрашивается (сужать нечем, но чужое возможно)', async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        urls.push(String(url));
        return Promise.resolve(jsonRes({ error: 'boom' }, 500));
      }),
    );
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <DrawerProvider>
            <Events />
          </DrawerProvider>
        </ProjectProvider>
      </I18nProvider>,
    );
    await waitFor(() => {
      expect(urls.some((u) => u.startsWith('/v1/projects'))).toBe(true);
    });
    // Список не приехал — проекты могут существовать, значит голый запрос
    // показал бы чужое. Молчим (в шапке при этом висит чип-предупреждение).
    expect(urls.some((u) => u.startsWith('/v1/events'))).toBe(false);
  });

  it('проектов нет вовсе (свежая установка) → лента ЗАПРАШИВАЕТСЯ и платформенные видны', async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        const u = String(url);
        urls.push(u);
        if (u.startsWith('/v1/projects')) return Promise.resolve(jsonRes({ projects: [] }));
        if (u.startsWith('/v1/events')) return Promise.resolve(jsonRes({ events: [platformEvent] }));
        return Promise.resolve(jsonRes({}));
      }),
    );
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <DrawerProvider>
            <Events />
          </DrawerProvider>
        </ProjectProvider>
      </I18nProvider>,
    );
    // Проект заводится только первым касанием (master ensureProject), и ровно
    // в этом окне случаются платформенные события: создание ключа, бекап,
    // серты. Сужать здесь нечем И НЕЧЕГО — чужих проектов не существует,
    // поэтому голый запрос безопасен, а молчание спрятало бы единственное,
    // что на установке вообще есть (приёмка «платформенные видны»).
    await screen.findByText(/nightly/);
    expect(urls).toContain('/v1/events?limit=500');
    expect(within(rowOf(/nightly/)).getByText('platform')).toBeTruthy();
  });

  it('платформенное событие видно при выбранном проекте и ПОДПИСАНО (EN+RU)', async () => {
    const urls: string[] = [];
    stubFeedApi(urls, [gameEvent, arenaEvent, platformEvent]);
    const en = render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <DrawerProvider>
            <Events />
          </DrawerProvider>
        </ProjectProvider>
      </I18nProvider>,
    );
    await screen.findByText(/nightly/);
    // Платформенное событие остаётся видимым при выбранном проекте (сужение не
    // скрывающее) — и подписано, иначе оператор прочтёт его как своё.
    expect(within(rowOf(/nightly/)).getByText('platform')).toBeTruthy();
    // Проектное событие подписи НЕ получает — иначе она ничего не значит.
    expect(within(rowOf(/1\.0\.0/)).queryByText('platform')).toBeNull();
    en.unmount();

    render(
      <I18nProvider initialLang="ru">
        <ProjectProvider>
          <DrawerProvider>
            <Events />
          </DrawerProvider>
        </ProjectProvider>
      </I18nProvider>,
    );
    await screen.findByText(/nightly/);
    expect(within(rowOf(/nightly/)).getByText('платформенное')).toBeTruthy();
  });

  it('экран Событий: чужое событие из ЖИВОГО стрима в ленту не попадает, платформенное попадает', async () => {
    const urls: string[] = [];
    stubFeedApi(urls, [gameEvent]);
    const live = makeLive();
    render(
      <I18nProvider initialLang="en">
        <LiveContext.Provider value={live.value}>
          <ProjectProvider>
            <DrawerProvider>
              <Events />
            </DrawerProvider>
          </ProjectProvider>
        </LiveContext.Provider>
      </I18nProvider>,
    );
    await waitFor(() => {
      expect(screen.getByText(/1\.0\.0/)).toBeTruthy();
    });
    act(() => {
      live.emit(arenaEvent);
      live.emit(platformEvent);
    });
    // Стрим — единственный источник, который сервер панели не сужает: #999 сузил
    // его по ПРИВЯЗКЕ КЛЮЧА, а сессия панели ходит непривязанным admin-ключом.
    // Поэтому чужое отсекается на входе в список, платформенное остаётся.
    expect(screen.queryByText(/9\.9\.9/)).toBeNull();
    expect(screen.getByText(/nightly/)).toBeTruthy();
  });

  it('лента Обзора запрашивает события с ?project= и голой ленты не спрашивает', async () => {
    const urls: string[] = [];
    stubFeedApi(urls, [gameEvent, arenaEvent, platformEvent]);
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <div>
            <ProjectSelector />
            <EventsFeed />
          </div>
        </ProjectProvider>
      </I18nProvider>,
    );
    await waitFor(() => {
      expect(urls).toContain('/v1/events?limit=40&project=game');
    });
    // Раньше первый запрос уходил, пока грузился /v1/projects, — чужое событие
    // приезжало в браузер с полным payload, и прятал его только клиентский
    // фильтр. «Не приезжают вовсе» значит, что такого запроса нет.
    expect(bareFeedRequests(urls)).toEqual([]);
    expect(screen.queryByText(/9\.9\.9/)).toBeNull();

    fireEvent.change(await screen.findByRole('combobox', { name: 'Project' }), { target: { value: 'arena' } });
    await waitFor(() => {
      expect(urls).toContain('/v1/events?limit=40&project=arena');
    });
    expect(bareFeedRequests(urls)).toEqual([]);
  });

  // Лента Обзора — вторая лента, и правила у неё те же. Держим их под теми же
  // утверждениями: без этого её половину диффа можно откатить незамеченной.

  it('лента Обзора: чужое событие из ЖИВОГО стрима не попадает в окно, платформенное попадает', async () => {
    const urls: string[] = [];
    stubFeedApi(urls, [gameEvent]);
    const live = makeLive();
    render(
      <I18nProvider initialLang="en">
        <LiveContext.Provider value={live.value}>
          <ProjectProvider>
            <EventsFeed />
          </ProjectProvider>
        </LiveContext.Provider>
      </I18nProvider>,
    );
    await waitFor(() => {
      expect(screen.getByText(/1\.0\.0/)).toBeTruthy();
    });
    act(() => {
      live.emit(arenaEvent);
      live.emit(platformEvent);
    });
    // Отсев именно НА ВХОДЕ, а не в показе: иначе чужие события занимают места
    // в окне FEED_CAP и вытесняют свои ещё до того, как их кто-то увидит.
    expect(screen.queryByText(/9\.9\.9/)).toBeNull();
    expect(screen.getByText(/nightly/)).toBeTruthy();
  });

  it('лента Обзора: платформенное событие подписано', async () => {
    const urls: string[] = [];
    stubFeedApi(urls, [gameEvent, platformEvent]);
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <EventsFeed />
        </ProjectProvider>
      </I18nProvider>,
    );
    await screen.findByText(/nightly/);
    expect(within(rowOf(/nightly/)).getByText('platform')).toBeTruthy();
    expect(within(rowOf(/1\.0\.0/)).queryByText('platform')).toBeNull();
  });

  // Статистика — единственный экран, который НЕЛЬЗЯ сузить в браузере:
  // иммутабельная часть окна приходит уже агрегированной из роллапов
  // (мультипроект W3 дал им колонку project). Значит проверять надо именно
  // параметр запроса, а не отфильтрованный результат.
  it('экран Статистики запрашивает агрегаты с ?project= и переспрашивает при смене', async () => {
    const urls: string[] = [];
    const emptyOverview = {
      days: 7,
      timezone: 'UTC',
      generated_at: '2026-08-01T00:00:00Z',
      matches_per_day: { unit: 'matches/day', keys: [], points: [] },
      players_per_day: { unit: 'players/day', keys: [], points: [] },
      peak_ccu_per_day: { unit: 'players', points: [] },
      peak_ccu: 0,
      avg_match_duration_seconds: null,
      avg_match_duration_per_day: { unit: 'seconds', points: [] },
      version_distribution: [],
      time_to_match: { p50_seconds: null, p95_seconds: null, samples: 0, source: 'matches', note: '' },
    };
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        const u = String(url);
        urls.push(u);
        if (u.startsWith('/v1/projects')) return Promise.resolve(jsonRes({ projects: [game, arena] }));
        if (u.startsWith('/v1/environments')) return Promise.resolve(jsonRes({ environments: [] }));
        if (u.startsWith('/v1/stats/overview')) return Promise.resolve(jsonRes(emptyOverview));
        return Promise.resolve(jsonRes({}));
      }),
    );
    render(
      <I18nProvider initialLang="en">
        <ProjectProvider>
          <div>
            <ProjectSelector />
            <Stats />
          </div>
        </ProjectProvider>
      </I18nProvider>,
    );

    await waitFor(() => {
      expect(urls.some((u) => u.startsWith('/v1/stats/overview') && u.includes('project=game'))).toBe(true);
    });
    fireEvent.change(await screen.findByRole('combobox', { name: 'Project' }), { target: { value: 'arena' } });
    await waitFor(() => {
      expect(urls.some((u) => u.startsWith('/v1/stats/overview') && u.includes('project=arena'))).toBe(true);
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
