// Env-контекст (environments v1 §8): чистые хелперы (порядок чипов, резолв
// выбора, фильтр, env события) + рендер переключателя (EnvChips) + интеграция
// «переключатель фильтрует» на экране Флот (клиентский фильтр нод по env).

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { ReactElement, ReactNode } from 'react';
import type { ApiEvent, Environment, NodeInfo, SessionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import { ProjectContext } from '../lib/project';
import type { ProjectContextValue } from '../lib/project';
import {
  EnvContext,
  EnvProvider,
  envMatches,
  eventEnvOf,
  keepForEnv,
  orderedEnvs,
  resolveSelection,
  storedEnv,
  useEnv,
} from '../lib/env';
import { EnvChips } from '../components/Shell';
import { EventsFeed } from '../components/EventsFeed';
import { Fleet } from '../screens/Fleet';

const dev: Environment = { project: 'game', name: 'dev', production: false, auto_deploy: true, retention_keep: 20, created_at: '2026-07-01T00:00:00Z' };
const staging: Environment = { project: 'game', name: 'staging', production: false, auto_deploy: false, retention_keep: 5, created_at: '2026-07-01T00:00:00Z' };
const prod: Environment = { project: 'game', name: 'prod', production: true, auto_deploy: false, retention_keep: 0, created_at: '2026-07-01T00:00:00Z' };

const ev = (kind: string, payload: Record<string, unknown>): ApiEvent => ({ id: 1, ts: '2026-07-14T00:00:00Z', kind, payload });

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

// --- чистые хелперы ---

describe('env — orderedEnvs (non-production сначала, затем production)', () => {
  it('non-production идут первыми, внутри группы — по имени', () => {
    expect(orderedEnvs([prod, dev, staging]).map((e) => e.name)).toEqual(['dev', 'staging', 'prod']);
  });
});

describe('env — resolveSelection', () => {
  it('сохранённое имя валидно → оно; невалидно/стёрто → «All» (null)', () => {
    expect(resolveSelection('dev', [dev, prod])).toBe('dev');
    expect(resolveSelection('ghost', [dev, prod])).toBeNull(); // env исчез → откат на All
    expect(resolveSelection(null, [dev, prod])).toBeNull();
  });
});

describe('env — envMatches / keepForEnv', () => {
  it('«All» (null) пропускает всё; конкретный env — точное совпадение', () => {
    expect(envMatches('dev', null)).toBe(true);
    expect(envMatches('dev', 'dev')).toBe(true);
    expect(envMatches('prod', 'dev')).toBe(false);
    expect(envMatches(undefined, 'dev')).toBe(false);
  });
  it('keepForEnv фильтрует по извлекателю env; null — без фильтра', () => {
    const items = [{ env: 'dev' }, { env: 'prod' }, { env: 'dev' }];
    expect(keepForEnv(items, 'dev', (i) => i.env)).toHaveLength(2);
    expect(keepForEnv(items, null, (i) => i.env)).toHaveLength(3);
  });
});

describe('env — eventEnvOf (M13)', () => {
  it('version_promoted берёт env из payload.to_env', () => {
    expect(eventEnvOf(ev('version_promoted', { from_env: 'dev', to_env: 'prod' }))).toBe('prod');
  });
  it('прочие события — из payload.env', () => {
    expect(eventEnvOf(ev('version_registered', { env: 'dev' }))).toBe('dev');
    expect(eventEnvOf(ev('version_retired', { env: 'staging' }))).toBe('staging');
  });
  it('нет env в payload → undefined (видно только в «All»)', () => {
    expect(eventEnvOf(ev('node_created', {}))).toBeUndefined();
    expect(eventEnvOf(ev('node_env_changed', { from: 'dev', to: 'prod' }))).toBeUndefined();
  });
});

describe('env — storedEnv', () => {
  it('пусто/мусор → null; валидное имя → оно', () => {
    expect(storedEnv()).toBeNull();
    localStorage.setItem('birdman.env', 'prod');
    expect(storedEnv()).toBe('prod');
    localStorage.setItem('birdman.env', '');
    expect(storedEnv()).toBeNull();
  });
});

// --- EnvChips: порядок, aria, клик ---

function withEnv(value: Parameters<typeof EnvContext.Provider>[0]['value'], ui: ReactNode): ReactElement {
  return (
    <I18nProvider initialLang="en">
      <EnvContext.Provider value={value}>{ui}</EnvContext.Provider>
    </I18nProvider>
  );
}

const baseEnv = {
  environments: [dev, staging, prod],
  project: 'game',
  selected: null as string | null,
  setSelected: () => {},
  loading: false,
  reload: () => {},
};

describe('EnvChips', () => {
  it('порядок чипов: non-production, затем production, «All» в конце', () => {
    render(withEnv(baseEnv, <EnvChips />));
    const chips = screen.getAllByRole('button').map((b) => b.textContent);
    expect(chips).toEqual(['dev', 'staging', 'prod', 'All']);
  });

  it('выбранный env помечен aria-pressed; клик по другому чипу вызывает setSelected', () => {
    const setSelected = vi.fn();
    render(withEnv({ ...baseEnv, selected: 'dev', setSelected }, <EnvChips />));
    expect(screen.getByRole('button', { name: 'dev' }).getAttribute('aria-pressed')).toBe('true');
    fireEvent.click(screen.getByRole('button', { name: 'prod' }));
    expect(setSelected).toHaveBeenCalledWith('prod');
    fireEvent.click(screen.getByRole('button', { name: 'All' }));
    expect(setSelected).toHaveBeenCalledWith(null); // «All» = null
  });

  it('нет окружений → ничего не рендерит (фильтр по env и так no-op)', () => {
    const { container } = render(withEnv({ ...baseEnv, environments: [] }, <EnvChips />));
    expect(container.querySelector('[role="group"]')).toBeNull();
  });
});

// --- деградация: список окружений недоступен (follow-up p2) ---

/** Пробник эффективного выбора из useEnv (внутри EnvProvider) + сами чипы. */
function EnvProbe() {
  const { selected, environments } = useEnv();
  return (
    <div>
      <span data-testid="selected">{selected ?? 'ALL'}</span>
      <span data-testid="count">{environments.length}</span>
      <EnvChips />
    </div>
  );
}

const jsonRes = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

/** Проект инжектим фиксированным: тесты ниже про ОКРУЖЕНИЯ, а не про выбор
 *  проекта (он — предмет project.test.tsx). EnvProvider без выбранного проекта
 *  окружения не запрашивает вовсе (мультипроект W1). */
const PROJECT_GAME: ProjectContextValue = {
  projects: [{ id: 'p1', slug: 'game', match_size: 2, created_at: '2026-07-01T00:00:00Z' }],
  selected: 'game',
  setSelected: () => {},
  loading: false,
  reload: () => {},
};

function renderProvider() {
  return render(
    <I18nProvider initialLang="en">
      <ProjectContext.Provider value={PROJECT_GAME}>
        <EnvProvider>
          <EnvProbe />
        </EnvProvider>
      </ProjectContext.Provider>
    </I18nProvider>,
  );
}

describe('EnvProvider — деградация при недоступном GET /v1/environments', () => {
  it('список не приехал → сохранённый фильтр НЕ применяется (эффективный выбор «All»)', async () => {
    localStorage.setItem('birdman.env', 'prod'); // выбор с прошлой сессии
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(jsonRes({ error: 'internal', detail: 'db down' }, 500))));
    renderProvider();
    // Молча резать данные несуществующим (для нас) окружением нельзя.
    await waitFor(() => {
      expect(screen.getByTestId('selected').textContent).toBe('ALL');
    });
    // Персист цел: список вернётся — вернётся и выбор (проверка ниже).
    expect(localStorage.getItem('birdman.env')).toBe('prod');
  });

  it('вместо чипов — чип-предупреждение с тултипом «фильтр отключён»', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(jsonRes({ error: 'internal', detail: 'db down' }, 500))));
    renderProvider();
    const chip = await screen.findByRole('button', { name: /Environments unavailable/ });
    expect(chip.getAttribute('title')).toMatch(/env filter is off/i);
    expect(screen.queryByRole('button', { name: 'All' })).toBeNull(); // обычных чипов нет
  });

  it('повтор по клику: список приехал → чипы вернулись и сохранённый выбор восстановлен', async () => {
    localStorage.setItem('birdman.env', 'prod');
    let call = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(() => {
        call += 1;
        return Promise.resolve(call === 1 ? jsonRes({ error: 'internal' }, 500) : jsonRes({ environments: [dev, prod] }));
      }),
    );
    renderProvider();
    const chip = await screen.findByRole('button', { name: /Environments unavailable/ });
    expect(screen.getByTestId('selected').textContent).toBe('ALL');

    fireEvent.click(chip); // retry
    await waitFor(() => {
      expect(screen.getByTestId('count').textContent).toBe('2');
    });
    expect(screen.getByTestId('selected').textContent).toBe('prod'); // выбор пережил деградацию
    expect(screen.getByRole('button', { name: 'All' })).toBeTruthy(); // обычные чипы вернулись
  });
});

// --- интеграция: переключатель фильтрует экран Флот ---

const sess = (...scopes: SessionInfo['scopes']): SessionInfo => ({ scopes, name: 'k' });

function mkNode(id: string, env: string): NodeInfo {
  return {
    id,
    project: 'game',
    region: 'eu',
    env,
    hostname: `${id}.host`,
    public_ip: '10.0.0.1',
    capacity_slots: 8,
    agent_version: '0.1.0',
    state: 'active',
    last_heartbeat_at: new Date().toISOString(),
    created_at: '2026-07-11T00:00:00Z',
  };
}

function fleetFetch(nodes: NodeInfo[]) {
  return vi.fn((url: string) => {
    const u = String(url);
    const body = u.includes('/v1/nodes')
      ? { nodes }
      : u.includes('/v1/servers')
        ? { servers: [] }
        : u.includes('/v1/versions')
          ? { versions: [] }
          : u.includes('/v1/events')
            ? { events: [] }
            : {};
    return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }));
  });
}

function renderFleet(selected: string | null) {
  return render(
    <SessionContext.Provider value={{ session: sess('admin'), login: async () => {}, logout: async () => {}, invalidate: () => {} }}>
      {withEnv({ ...baseEnv, selected }, <Fleet />)}
    </SessionContext.Provider>,
  );
}

describe('переключатель фильтрует — Флот по env', () => {
  const nodes = [mkNode('dev-1', 'dev'), mkNode('prod-1', 'prod')];

  it('selected=dev → видна только dev-нода, prod скрыта', async () => {
    vi.stubGlobal('fetch', fleetFetch(nodes));
    renderFleet('dev');
    expect(await screen.findByText('dev-1.host')).toBeTruthy();
    expect(screen.queryByText('prod-1.host')).toBeNull();
  });

  it('selected=null («All») → видны обе ноды', async () => {
    vi.stubGlobal('fetch', fleetFetch(nodes));
    renderFleet(null);
    expect(await screen.findByText('dev-1.host')).toBeTruthy();
    await waitFor(() => {
      expect(screen.getByText('prod-1.host')).toBeTruthy();
    });
  });
});

// --- интеграция: лента событий Overview фильтруется по env (I1 ревью, M13) ---

describe('EventsFeed фильтруется по env (I1)', () => {
  // Микс: dev-событие (payload.env), prod-событие через version_promoted
  // (env := to_env), событие БЕЗ env (видно только в «All»).
  const feed: ApiEvent[] = [
    { id: 1, ts: '2026-07-14T00:00:01Z', kind: 'version_registered', payload: { env: 'dev', semver: '1.0.0' } },
    { id: 2, ts: '2026-07-14T00:00:02Z', kind: 'version_promoted', payload: { from_env: 'dev', to_env: 'prod' } },
    { id: 3, ts: '2026-07-14T00:00:03Z', kind: 'node_created', payload: {} },
  ];

  function feedFetch() {
    return vi.fn((url: string) => {
      const body = String(url).includes('/v1/events') ? { events: feed } : {};
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    });
  }

  it('selected=dev → только dev-событие; prod и без-env скрыты', async () => {
    vi.stubGlobal('fetch', feedFetch());
    render(withEnv({ ...baseEnv, selected: 'dev' }, <EventsFeed />));
    expect(await screen.findByText('Version registered')).toBeTruthy();
    expect(screen.queryByText('Version promoted')).toBeNull(); // to_env=prod
    expect(screen.queryByText('Node registered')).toBeNull(); // без env — только в «All»
  });

  it('selected=null («All») → видны все три', async () => {
    vi.stubGlobal('fetch', feedFetch());
    render(withEnv({ ...baseEnv, selected: null }, <EventsFeed />));
    expect(await screen.findByText('Version registered')).toBeTruthy();
    expect(screen.getByText('Version promoted')).toBeTruthy();
    expect(screen.getByText('Node registered')).toBeTruthy();
  });

  it('под фильтром пусто, но события есть → «No events for these filters.»', async () => {
    vi.stubGlobal('fetch', feedFetch());
    render(withEnv({ ...baseEnv, selected: 'staging' }, <EventsFeed />));
    expect(await screen.findByText('No events for these filters.')).toBeTruthy();
  });
});
