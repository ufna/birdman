// Скоуп (проект + окружение) переехал из контента в сайдбар (слайс A), а с
// tracker #1106 спрашивается ПО КАЖДОЙ ОСИ ОТДЕЛЬНО. Проверяем контракт, а не
// вёрстку: половина скоупа рендерится ровно там, где влияет; где не влияет —
// не рендерится вовсе (иначе селектор врал бы, что на что-то влияет); а
// мобильный индикатор называет ровно те половины, что показаны в меню.
//
// Главное здесь — НЕ таблица истинности (её легко подогнать под константу), а
// проверка самой КЛАССИФИКАЦИИ: экран, объявленный слепым к выбору окружения,
// не должен читать `selected` из EnvContext вовсе. Читает — значит классификация
// врёт, и тест обязан покраснеть, кто бы её ни сломал: автор, спрятавший чипы
// на env-зависимом экране, или автор, сделавший env-зависимым экран, где чипы
// уже спрятаны.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { ReactNode } from 'react';
import type { Environment, ProjectInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { EnvContext } from '../lib/env';
import { ProjectContext } from '../lib/project';
import { DrawerProvider } from '../lib/drawer';
import {
  EnvChips,
  ScopeIndicator,
  ScopePicker,
  navItemsFor,
  pathUsesEnv,
  pathUsesProject,
  pathUsesScope,
} from '../components/Shell';
import { Access } from '../screens/Access';
import { Alerts } from '../screens/Alerts';
import { Fleet } from '../screens/Fleet';
import { Logs } from '../screens/Logs';

const dev: Environment = {
  project: 'game',
  name: 'dev',
  production: false,
  auto_deploy: true,
  retention_keep: 20,
  created_at: '2026-07-01T00:00:00Z',
};

const projects: ProjectInfo[] = [
  { id: 'p1', slug: 'game', match_size: 2, created_at: '2026-07-01T00:00:00Z' },
  { id: 'p2', slug: 'other', match_size: 4, created_at: '2026-07-01T00:00:00Z' },
];

const envValue = (selected: string | null) => ({
  environments: [dev],
  project: 'game',
  selected,
  setSelected: () => {},
  loading: false,
  reload: () => {},
});

const projectValue = {
  projects,
  selected: 'game',
  setSelected: () => {},
  loading: false,
  reload: () => {},
};

function withScope(node: ReactNode, env: string | null = 'dev') {
  return render(
    <I18nProvider initialLang="en">
      <ProjectContext.Provider value={projectValue}>
        <EnvContext.Provider value={envValue(env)}>{node}</EnvContext.Provider>
      </ProjectContext.Provider>
    </I18nProvider>,
  );
}

// --- ось за осью: какой путь что потребляет ---

/**
 * Полная классификация путей навигации. Держим её ЗДЕСЬ списком, а не выводим
 * из тех же множеств, что и код: иначе тест повторял бы константу и падал бы
 * только вместе с ней. Новый экран сюда не попадёт сам — тест ниже сверяет
 * ключи с реальным составом навигации и заставит классифицировать осознанно.
 */
const CLASSIFICATION: Record<string, { project: boolean; env: boolean }> = {
  // Продуктовые экраны: сужаются обеими осями.
  '/': { project: true, env: true },
  '/fleet': { project: true, env: true },
  '/matches': { project: true, env: true },
  '/deploys': { project: true, env: true },
  '/events': { project: true, env: true },
  '/stats': { project: true, env: true },
  '/cost': { project: true, env: true },
  // Алерты живут в осях проект×регион; окружения не читают вовсе (mute — тоже).
  '/alerts': { project: true, env: false },
  // Логи: поиск проект не несёт, но дровер дедика по клику резолвит его В ПРОЕКТЕ.
  '/logs': { project: true, env: false },
  // Бекапы: у мастера они одни на установку, ручки не принимают ?project=.
  '/backups': { project: false, env: false },
  // Админка: проект кормит привязку ключа, отметку текущего в «Проектах» и всю
  // секцию «Окружения». Выбранное ОКРУЖЕНИЕ не кормит ничего.
  '/access': { project: true, env: false },
};

describe('скоуп панели: классификация путей по осям', () => {
  it('таблица истинности pathUsesProject / pathUsesEnv', () => {
    for (const [path, want] of Object.entries(CLASSIFICATION)) {
      expect({ path, ...want }).toEqual({ path, project: pathUsesProject(path), env: pathUsesEnv(path) });
    }
  });

  it('классифицирован КАЖДЫЙ путь навигации — новый экран не проскочит молча', () => {
    const nav = navItemsFor({ name: 'k', scopes: ['admin'] }).map((it) => it.path);
    expect(new Set(Object.keys(CLASSIFICATION))).toEqual(new Set(nav));
  });

  it('инвариант: слепой к проекту слеп и к окружению (окружения принадлежат проекту)', () => {
    for (const path of Object.keys(CLASSIFICATION)) {
      if (!pathUsesProject(path)) expect(pathUsesEnv(path)).toBe(false);
    }
  });

  it('pathUsesScope = «есть хоть одна живая ось»: пусто только на Бекапах', () => {
    const scopeless = Object.keys(CLASSIFICATION).filter((p) => !pathUsesScope(p));
    expect(scopeless).toEqual(['/backups']);
  });
});

// --- что реально рендерится ---

const projectGroup = () => screen.queryByRole('group', { name: /project/i });
const envGroup = () => screen.queryByRole('group', { name: /environment/i });

describe('скоуп панели: место и видимость', () => {
  it('на экране с обеими осями рендерятся оба переключателя', () => {
    withScope(<ScopePicker path="/fleet" />);
    expect(projectGroup()).toBeTruthy();
    expect(envGroup()).toBeTruthy();
  });

  it('на Бекапах блока нет вовсе', () => {
    withScope(<ScopePicker path="/backups" />);
    expect(projectGroup()).toBeNull();
    expect(envGroup()).toBeNull();
  });

  it.each(['/logs', '/access', '/alerts'])('на %s остаётся только селектор проекта', (path) => {
    withScope(<ScopePicker path={path} />);
    expect(projectGroup()).toBeTruthy();
    expect(envGroup()).toBeNull();
  });

  it('мобильный индикатор показывает проект и окружение', () => {
    const { container } = withScope(<ScopeIndicator path="/fleet" />);
    expect(container.textContent).toContain('game');
    expect(container.textContent).toContain('dev');
  });

  it('мобильный индикатор при «всех окружениях» показывает All, а не пустоту', () => {
    const { container } = withScope(<ScopeIndicator path="/fleet" />, null);
    expect(container.textContent).toContain('All');
  });

  it('мобильный индикатор без живой оси env называет только проект', () => {
    const { container } = withScope(<ScopeIndicator path="/logs" />);
    expect(container.textContent).toBe('game');
  });

  it('мобильного индикатора нет на экране без скоупа', () => {
    const { container } = withScope(<ScopeIndicator path="/backups" />);
    expect(container.textContent).toBe('');
  });
});

// --- фальсифицируемая проверка самой классификации ---

/** EnvContext, записывающий, какие поля с него читали за рендер. */
function recordingEnv(reads: Set<string>, selected: string | null = 'dev') {
  return new Proxy(envValue(selected), {
    get(target, prop, receiver) {
      if (typeof prop === 'string') reads.add(prop);
      return Reflect.get(target, prop, receiver) as unknown;
    },
  });
}

/** Ответы на всё, что просят экраны: пустые списки под любым ключом-обёрткой. */
function stubFetch() {
  const body = {
    projects,
    environments: [dev],
    alerts: [],
    rules: [],
    mutes: [],
    events: [],
    apikeys: [],
    registries: [],
    nodes: [],
    servers: [],
    versions: [],
    matches: [],
  };
  vi.stubGlobal('fetch', vi.fn(() =>
    Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })),
  ));
}

/**
 * Монтирует узел с записывающим EnvContext и возвращает прочитанные поля.
 * `marker` — текст, который экран обязан показать: без него утверждение «не
 * читал selected» означало бы лишь «не успел отрендериться», и тест краснел бы
 * не на том. Данные приезжают асинхронно, поэтому маркера ждём.
 */
async function envReadsOf(node: ReactNode, marker: string): Promise<Set<string>> {
  const reads = new Set<string>();
  stubFetch();
  render(
    <I18nProvider initialLang="en">
      <ProjectContext.Provider value={projectValue}>
        <EnvContext.Provider value={recordingEnv(reads)}>
          <DrawerProvider>{node}</DrawerProvider>
        </EnvContext.Provider>
      </ProjectContext.Provider>
    </I18nProvider>,
  );
  await screen.findByText(marker);
  return reads;
}

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe('классификация не врёт: env-слепые экраны не читают выбранное окружение', () => {
  // Санити ПЕРВЫМ: без него «никто не прочитал selected» проходит и на
  // сломанном рекордере, и вся секция ниже — пустышка.
  it('санити: рекордер ловит чтение — EnvChips читают selected', async () => {
    const reads = await envReadsOf(<EnvChips />, 'All');
    expect(reads.has('selected')).toBe(true);
  });

  it('санити: env-ЗАВИСИМЫЙ экран (Флот) selected читает', async () => {
    const reads = await envReadsOf(<Fleet />, 'Fleet');
    expect(reads.has('selected')).toBe(true);
  });

  it('Логи не читают выбранное окружение', async () => {
    const reads = await envReadsOf(<Logs />, 'Logs');
    expect(reads.has('selected')).toBe(false);
  });

  it('Алерты не читают выбранное окружение', async () => {
    const reads = await envReadsOf(<Alerts />, 'Firing history');
    expect(reads.has('selected')).toBe(false);
  });

  it('Админка не читает выбранное окружение (но читает список окружений и проект)', async () => {
    const reads = await envReadsOf(<Access />, 'API keys');
    expect(reads.has('selected')).toBe(false);
    // Именно поэтому прячем ЧИПЫ, а не селектор проекта: список окружений и
    // проект Админке нужны — их берут привязка ключа и секция «Окружения».
    expect(reads.has('environments')).toBe(true);
    expect(reads.has('project')).toBe(true);
  });
});
