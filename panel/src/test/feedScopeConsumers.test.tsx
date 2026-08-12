// Вспомогательные потребители /v1/events сужены по проекту (tracker #1024).
//
// #987 сузил ЛЕНТЫ (экран «События» + лента Обзора), но три места, которые
// лентой не являются, остались с голым запросом:
//   Fleet.tsx        — lookup node_id → причина карантина;
//   Deploys.tsx      — сидирование прогресса деплоя по version_id;
//   ServerDrawer.tsx — таймлайн дедика, фильтр клиентом по server_id.
//
// Утечки ПОКАЗА тут не было и нет: все три матчат по ключу, а сами
// ноды/версии/дедики уже сужены проектом, так что чужая запись просто не
// сматчится. Чинилось ОКНО: лимит (500/1000) делился между проектами, и на
// установке с несколькими живыми проектами события соседа выталкивали свои за
// край — тогда Флот переставал показывать причину карантина СВОЕЙ ноды, у
// Деплоев не сидировался прогресс СВОЕЙ версии, а таймлайн СВОЕГО дедика
// приезжал обрезанным. Дефект латентный и растёт с числом проектов.
//
// Поэтому проверяется ЗАПРОС, а не видимый набор: утверждение про набор
// пережило бы откат сужения (чужое и так не матчится), а про запрос — нет.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { useEffect } from 'react';
import { render, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import type { Environment, ProjectInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { EnvProvider } from '../lib/env';
import { LiveContext } from '../lib/live';
import { ProjectProvider } from '../lib/project';
import { DrawerProvider, useServerDrawer } from '../lib/drawer';
import { Fleet } from '../screens/Fleet';
import { Deploys } from '../screens/Deploys';

const proj = (slug: string): ProjectInfo => ({ id: `id-${slug}`, slug, match_size: 2, created_at: '2026-07-01T00:00:00Z' });
const game = proj('game');
const arena = proj('arena');
const dev: Environment = {
  project: 'game', name: 'dev', production: false, auto_deploy: true, retention_keep: 20,
  created_at: '2026-07-01T00:00:00Z',
};

const jsonRes = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

/** Запросы к ленте БЕЗ `?project=` — их не должно быть ни одного. */
const bareFeedRequests = (urls: string[]) =>
  urls.filter((u) => u.startsWith('/v1/events') && !u.includes('project='));

/**
 * `/v1/projects` отвечает НЕ мгновенно: окно «список ещё в пути» — ровно то, в
 * котором голый запрос и уходил, а на мгновенном ответе его не увидеть.
 */
function stubApi(urls: string[], projectsDelayMs = 0) {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      const u = String(url);
      urls.push(u);
      if (u.startsWith('/v1/projects')) {
        const res = jsonRes({ projects: [game, arena] });
        return projectsDelayMs === 0
          ? Promise.resolve(res)
          : new Promise((r) => setTimeout(() => r(res), projectsDelayMs));
      }
      if (u.startsWith('/v1/environments')) return Promise.resolve(jsonRes({ environments: [dev] }));
      if (u.startsWith('/v1/nodes')) return Promise.resolve(jsonRes({ nodes: [] }));
      if (u.startsWith('/v1/servers')) return Promise.resolve(jsonRes({ servers: [] }));
      if (u.startsWith('/v1/versions')) return Promise.resolve(jsonRes({ versions: [] }));
      if (u.startsWith('/v1/events')) return Promise.resolve(jsonRes({ events: [] }));
      return Promise.resolve(jsonRes({}));
    }),
  );
}

const LIVE = { status: 'live' as const, subscribe: () => () => {} };

function Wrap({ children }: { children: ReactNode }) {
  return (
    <I18nProvider initialLang="en">
      <LiveContext.Provider value={LIVE}>
        <ProjectProvider>
          <EnvProvider>
            <DrawerProvider>{children}</DrawerProvider>
          </EnvProvider>
        </ProjectProvider>
      </LiveContext.Provider>
    </I18nProvider>
  );
}

describe('Флот: окно событий сужено проектом (tracker #1024)', () => {
  it('запрос уходит с ?project=, голого нет ни одного', async () => {
    const urls: string[] = [];
    stubApi(urls);
    render(
      <Wrap>
        <Fleet />
      </Wrap>,
    );
    await waitFor(() => {
      expect(urls.some((u) => u.startsWith('/v1/events') && u.includes('project=game'))).toBe(true);
    });
    expect(bareFeedRequests(urls)).toEqual([]);
  });

  it('пока /v1/projects в пути — окно НЕ запрашивается вовсе (то самое окно)', async () => {
    const urls: string[] = [];
    stubApi(urls, 40);
    render(
      <Wrap>
        <Fleet />
      </Wrap>,
    );
    // До ответа списка проектов чей это срез — неизвестно; голый запрос привёз
    // бы окно всех проектов сразу.
    expect(bareFeedRequests(urls)).toEqual([]);
    await waitFor(() => {
      expect(urls.some((u) => u.startsWith('/v1/events') && u.includes('project=game'))).toBe(true);
    });
    expect(bareFeedRequests(urls)).toEqual([]);
  });
});

describe('Деплои: сидирование прогресса сужено проектом (tracker #1024)', () => {
  it('запрос уходит с ?project=, голого нет ни одного', async () => {
    const urls: string[] = [];
    stubApi(urls);
    render(
      <Wrap>
        <Deploys navigate={() => {}} />
      </Wrap>,
    );
    await waitFor(() => {
      expect(urls.some((u) => u.startsWith('/v1/events') && u.includes('project=game'))).toBe(true);
    });
    expect(bareFeedRequests(urls)).toEqual([]);
  });

  it('пока /v1/projects в пути — окно НЕ запрашивается', async () => {
    const urls: string[] = [];
    stubApi(urls, 40);
    render(
      <Wrap>
        <Deploys navigate={() => {}} />
      </Wrap>,
    );
    expect(bareFeedRequests(urls)).toEqual([]);
    await waitFor(() => {
      expect(urls.some((u) => u.startsWith('/v1/events') && u.includes('project=game'))).toBe(true);
    });
    expect(bareFeedRequests(urls)).toEqual([]);
  });
});

describe('Дровер дедика: таймлайн сужен проектом (tracker #1024)', () => {
  it('таймлайн запрашивает окно с ?project=, голого нет ни одного', async () => {
    const urls: string[] = [];
    stubApi(urls);

    // Сам дровер рендерит DrawerProvider — отдельно монтировать не надо.
    render(
      <Wrap>
        <OpenDrawer id="srv-1" />
      </Wrap>,
    );
    await waitFor(() => {
      expect(urls.some((u) => u.startsWith('/v1/events') && u.includes('project=game'))).toBe(true);
    });
    expect(bareFeedRequests(urls)).toEqual([]);
  });
});

/** Открывает дровер сразу после монтирования — дешевле, чем собирать путь
 *  Флот → строка таблицы → клик, а проверяем мы запрос таймлайна. */
function OpenDrawer({ id }: { id: string }) {
  const { open } = useServerDrawer();
  useEffect(() => {
    open(id);
  }, [open, id]);
  return null;
}

// Санити: без него «ноль голых запросов» проходит и на пустом наборе.
describe('санити: проверка не вырождена', () => {
  it('bareFeedRequests действительно ловит голый запрос', () => {
    expect(bareFeedRequests(['/v1/events?limit=500'])).toEqual(['/v1/events?limit=500']);
    expect(bareFeedRequests(['/v1/events?limit=500&project=game'])).toEqual([]);
  });

  it('в каждом кейсе выше запрос к ленте вообще БЫЛ (иначе ноль голых — пустышка)', async () => {
    const urls: string[] = [];
    stubApi(urls);
    render(
      <Wrap>
        <Fleet />
      </Wrap>,
    );
    await waitFor(() => {
      expect(urls.filter((u) => u.startsWith('/v1/events')).length).toBeGreaterThan(0);
    });
  });
});
