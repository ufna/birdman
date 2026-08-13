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
import type { Environment, ProjectInfo, SessionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { EnvContext } from '../lib/env';
import { ProjectContext } from '../lib/project';
import { SessionContext } from '../lib/session';
import { ThemeProvider } from '../lib/theme';
import { DrawerProvider } from '../lib/drawer';
import { Routed } from '../App';
import {
  EnvChips,
  ScopeIndicator,
  ScopePicker,
  effectiveSectionOf,
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

// Раздел — свойство пары (путь, сессия), поэтому сессия здесь такой же вход
// теста, как путь: admin видит все разделы, readonly — не admin-only.
const adminSession: SessionInfo = { name: 'k', scopes: ['admin'] };
const readonlySession: SessionInfo = { name: 'ro', scopes: ['readonly'] };

const sessionValue = (session: SessionInfo) => ({
  session,
  login: async () => {},
  logout: async () => {},
  invalidate: () => {},
});

function withScope(node: ReactNode, env: string | null = 'dev', session: SessionInfo = adminSession) {
  return render(
    <I18nProvider initialLang="en">
      <SessionContext.Provider value={sessionValue(session)}>
        <ProjectContext.Provider value={projectValue}>
          <EnvContext.Provider value={envValue(env)}>{node}</EnvContext.Provider>
        </ProjectContext.Provider>
      </SessionContext.Provider>
    </I18nProvider>,
  );
}

// --- ось за осью: какой путь что потребляет ---

/**
 * Полная классификация путей навигации ГЛАЗАМИ ADMIN — то есть сессии, которой
 * доступны все разделы. Держим её ЗДЕСЬ списком, а не выводим из тех же
 * множеств, что и код: иначе тест повторял бы константу и падал бы только
 * вместе с ней. Новый экран сюда не попадёт сам — тест ниже сверяет ключи с
 * реальным составом навигации и заставит классифицировать осознанно.
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

/**
 * Та же классификация ГЛАЗАМИ READONLY-СЕССИИ. Отличается ровно двумя
 * строками, и это не поправка к таблице выше, а следствие того, ЧТО РЕАЛЬНО
 * ПОКАЗАНО: `/backups` и `/access` — admin-only, у не-admin роутер рендерит
 * там Обзор, а Обзор сужается обеими осями. Пишем её списком целиком, как и
 * первую: выведи её из первой «по правилу» — и тест повторит проверяемую
 * логику вместо того, чтобы её проверять.
 */
const READONLY_CLASSIFICATION: Record<string, { project: boolean; env: boolean }> = {
  '/': { project: true, env: true },
  '/fleet': { project: true, env: true },
  '/matches': { project: true, env: true },
  '/deploys': { project: true, env: true },
  '/events': { project: true, env: true },
  '/stats': { project: true, env: true },
  '/cost': { project: true, env: true },
  '/alerts': { project: true, env: false },
  '/logs': { project: true, env: false },
  // Оба admin-only пути показывают не свой экран, а Обзор — значит и оси у них
  // Обзора, обе живые (tracker #1111).
  '/backups': { project: true, env: true },
  '/access': { project: true, env: true },
};

describe('скоуп панели: классификация путей по осям', () => {
  it('таблица истинности pathUsesProject / pathUsesEnv (admin)', () => {
    for (const [path, want] of Object.entries(CLASSIFICATION)) {
      expect({ path, ...want }).toEqual({
        path,
        project: pathUsesProject(path, adminSession),
        env: pathUsesEnv(path, adminSession),
      });
    }
  });

  it('таблица истинности для readonly: admin-only пути отвечают как Обзор', () => {
    for (const [path, want] of Object.entries(READONLY_CLASSIFICATION)) {
      expect({ path, ...want }).toEqual({
        path,
        project: pathUsesProject(path, readonlySession),
        env: pathUsesEnv(path, readonlySession),
      });
    }
  });

  it('обе таблицы покрывают одни и те же пути — новый экран не проскочит в одной из них', () => {
    expect(new Set(Object.keys(READONLY_CLASSIFICATION))).toEqual(new Set(Object.keys(CLASSIFICATION)));
  });

  it('классифицирован КАЖДЫЙ путь навигации — новый экран не проскочит молча', () => {
    const nav = navItemsFor(adminSession).map((it) => it.path);
    expect(new Set(Object.keys(CLASSIFICATION))).toEqual(new Set(nav));
  });

  it('инвариант: слепой к проекту слеп и к окружению (окружения принадлежат проекту)', () => {
    for (const session of [adminSession, readonlySession]) {
      for (const path of Object.keys(CLASSIFICATION)) {
        if (!pathUsesProject(path, session)) expect(pathUsesEnv(path, session)).toBe(false);
      }
    }
  });

  it('pathUsesScope = «есть хоть одна живая ось»: пусто только на Бекапах и только у admin', () => {
    const scopeless = (session: SessionInfo) =>
      Object.keys(CLASSIFICATION).filter((p) => !pathUsesScope(p, session));
    expect(scopeless(adminSession)).toEqual(['/backups']);
    // У readonly Бекапов на экране нет вовсе — пустого от скоупа экрана тоже.
    expect(scopeless(readonlySession)).toEqual([]);
  });
});

// --- одно правило сопоставления пути на всю панель (tracker #1109) ---

/**
 * Классификация выше отвечает про КОРНИ разделов. Про произвольный путь её
 * спрашивают через `sectionOf` — ту же функцию, которой роутер выбирает экран,
 * а нав подсвечивает пункт. Пока правил было два (роутер префиксно, множества
 * скоупа точным `has`), на под-пути `/logs/x` экран был Логов, а классификация
 * — «неизвестный путь», и спрятанные чипы окружения возвращались вхолостую.
 */
describe('раздел пути: роутер, нав и скоуп сопоставляют путь одним правилом', () => {
  // Разбор ПУТИ спрашиваем под admin-сессией: ей доступны все разделы, поэтому
  // права здесь ничего не переписывают и видно ровно поведение по пути.
  const sectionOf = (path: string) => effectiveSectionOf(path, adminSession);

  it('корень раздела — сам себе раздел', () => {
    for (const path of Object.keys(CLASSIFICATION)) expect(sectionOf(path)).toBe(path);
  });

  it('под-путь принадлежит своему разделу', () => {
    expect(sectionOf('/logs/x')).toBe('/logs');
    expect(sectionOf('/logs/x/y')).toBe('/logs');
    expect(sectionOf('/logs/')).toBe('/logs');
    expect(sectionOf('/backups/2026-08-01')).toBe('/backups');
    expect(sectionOf('/fleet/node-1')).toBe('/fleet');
  });

  it('режем по ГРАНИЦЕ СЕГМЕНТА: посторонний путь — Обзор, а не соседний раздел', () => {
    // Голый startsWith отдавал бы эти пути Логам/Бекапам/Админке.
    expect(sectionOf('/logsomething')).toBe('/');
    expect(sectionOf('/backupsx')).toBe('/');
    expect(sectionOf('/accesskey')).toBe('/');
    expect(sectionOf('/nope')).toBe('/');
  });

  it('ни один корень раздела не префикс другого — порядок перебора ничего не решает', () => {
    const roots = Object.keys(CLASSIFICATION).filter((p) => p !== '/');
    for (const a of roots) {
      for (const b of roots) {
        if (a !== b) expect({ a, b, nested: b.startsWith(`${a}/`) }).toEqual({ a, b, nested: false });
      }
    }
  });

  it('предикаты скоупа спрашивают РАЗДЕЛ, а не сырой путь', () => {
    expect(pathUsesEnv('/logs/x', adminSession)).toBe(false);
    expect(pathUsesEnv('/alerts/rule-1', adminSession)).toBe(false);
    expect(pathUsesEnv('/access/keys/1', adminSession)).toBe(false);
    expect(pathUsesProject('/backups/2026-08-01', adminSession)).toBe(false);
    expect(pathUsesScope('/backups/2026-08-01', adminSession)).toBe(false);
    // А посторонний путь — это Обзор, где живы обе оси.
    expect(pathUsesEnv('/logsomething', adminSession)).toBe(true);
    expect(pathUsesProject('/backupsx', adminSession)).toBe(true);
  });
});

// --- второе измерение раздела: СЕССИЯ (tracker #1111) ---

/**
 * Экран выбирается парой (путь, права), а классификация скоупа спрашивала
 * только путь. У не-admin на `/backups` роутер рендерит Обзор — экран, который
 * сужается ОБЕИМИ осями, — а предикаты продолжали отвечать про Бекапы, где
 * мертвы обе, и селектор пропадал целиком: оператор не видел, в каком он
 * проекте, и не мог переключиться, не уйдя с экрана.
 *
 * Гейт и здесь один на всю панель — `adminOnly` в NAV_ITEMS: раздела нет в
 * наве сессии ⇒ его нет и на экране. Поэтому проверяем не список из двух
 * путей, зашитый в тест, а СВЯЗЬ этого списка с составом нава.
 */
describe('раздел зависит и от СЕССИИ: admin-only разделы у не-admin деградируют в Обзор', () => {
  it('по сессии гейтятся ровно те разделы, что вырезаны из нава, — и их два', () => {
    const paths = Object.keys(CLASSIFICATION);
    const gated = paths.filter((p) => effectiveSectionOf(p, readonlySession) !== effectiveSectionOf(p, adminSession));
    const navRo = navItemsFor(readonlySession).map((it) => it.path);
    expect(new Set(gated)).toEqual(new Set(['/backups', '/access']));
    // Источник гейта один: «нет в наве» и «переписан роутером» — одно и то же.
    expect(new Set(paths.filter((p) => !navRo.includes(p)))).toEqual(new Set(gated));
  });

  it('у не-admin admin-only раздел — Обзор, и на под-пути тоже', () => {
    expect(effectiveSectionOf('/backups', readonlySession)).toBe('/');
    expect(effectiveSectionOf('/access', readonlySession)).toBe('/');
    expect(effectiveSectionOf('/backups/2026-08-01', readonlySession)).toBe('/');
    expect(effectiveSectionOf('/access/keys/1', readonlySession)).toBe('/');
  });

  it('у admin те же пути остаются своими разделами', () => {
    expect(effectiveSectionOf('/backups', adminSession)).toBe('/backups');
    expect(effectiveSectionOf('/backups/2026-08-01', adminSession)).toBe('/backups');
    expect(effectiveSectionOf('/access/keys/1', adminSession)).toBe('/access');
  });

  it('разделы без adminOnly правами не трогаются вовсе', () => {
    for (const p of ['/', '/fleet', '/logs', '/logs/x', '/alerts', '/logsomething', '/nope']) {
      expect({ p, section: effectiveSectionOf(p, readonlySession) }).toEqual({
        p,
        section: effectiveSectionOf(p, adminSession),
      });
    }
  });

  it('инвариант: эффективный раздел ВСЕГДА есть в наве своей сессии', () => {
    const paths = [...Object.keys(CLASSIFICATION), '/logs/x', '/backups/2026-08-01', '/access/keys/1', '/nope'];
    for (const session of [adminSession, readonlySession, null]) {
      const nav = navItemsFor(session).map((it) => it.path);
      for (const p of paths) {
        expect({ p, inNav: nav.includes(effectiveSectionOf(p, session)) }).toEqual({ p, inNav: true });
      }
    }
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

  it.each(['/logs/x', '/access/keys/1', '/alerts/rule-1'])(
    'на ПОД-ПУТИ %s чипы окружения остаются спрятанными',
    (path) => {
      withScope(<ScopePicker path={path} />);
      expect(projectGroup()).toBeTruthy();
      expect(envGroup()).toBeNull();
    },
  );

  it('на под-пути Бекапов блока по-прежнему нет вовсе', () => {
    withScope(<ScopePicker path="/backups/2026-08-01" />);
    expect(projectGroup()).toBeNull();
    expect(envGroup()).toBeNull();
  });

  it.each(['/backups', '/access'])(
    'у не-admin на admin-only %s показан Обзор — значит рисуются ОБЕ оси',
    (path) => {
      withScope(<ScopePicker path={path} />, 'dev', readonlySession);
      expect(projectGroup()).toBeTruthy();
      expect(envGroup()).toBeTruthy();
    },
  );

  it('мобильный индикатор у не-admin на Бекапах называет обе половины, а не молчит', () => {
    const { container } = withScope(<ScopeIndicator path="/backups" />, 'dev', readonlySession);
    expect(container.textContent).toContain('game');
    expect(container.textContent).toContain('dev');
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
  window.history.pushState(null, '', '/');
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

// --- пин на ДВА места сразу: экран и скоуп приходят из одного правила ---

/**
 * Проверки выше по отдельности спрашивают ЛИБО роутер, ЛИБО классификацию — а
 * дефект #1109 был именно в РАСХОЖДЕНИИ двух половин, и такой дефект виден
 * только когда обе половины отвечают про один и тот же путь в одном рендере.
 * Поэтому монтируем НАСТОЯЩИЙ роутер вместе с оболочкой и спрашиваем разом:
 * какой экран показан, какой пункт нава подсвечен и какие половины скоупа
 * нарисованы. Откат правки предикатов красит кейс `/logs/x` (чипы вернутся),
 * откат правки роутера — кейс `/logsomething` (посторонний путь уедет в Логи).
 *
 * С tracker #1111 сессия — такой же вход, как путь: расхождение «экран ≠
 * классификация» бывает и по правам, и увидеть его можно только тут же, в
 * одном рендере настоящего роутера.
 */
function renderRouted(path: string, session: SessionInfo = adminSession) {
  window.history.pushState(null, '', path);
  stubFetch();
  return render(
    <ThemeProvider>
      <I18nProvider initialLang="en">
        <SessionContext.Provider value={sessionValue(session)}>
          <ProjectContext.Provider value={projectValue}>
            <EnvContext.Provider value={envValue('dev')}>
              <DrawerProvider>
                <Routed />
              </DrawerProvider>
            </EnvContext.Provider>
          </ProjectContext.Provider>
        </SessionContext.Provider>
      </I18nProvider>
    </ThemeProvider>,
  );
}

/** href пункта навигации с aria-current="page" — «где панель считает, что мы». */
const currentNavHref = () => screen.getByRole('link', { current: 'page' }).getAttribute('href');

const LOGS_MARKER = 'Enter a search above and run it to see matching log lines.';
const OVERVIEW_MARKER = 'Live matches';
const BACKUPS_MARKER = 'Postgres dumps by the master: schedule, local retention and S3 offsite.';
const ACCESS_MARKER = 'API keys';

describe('роутер и скоуп сопоставляют путь одинаково', () => {
  it('под-путь /logs/x: экран Логов, подсвечены Логи — и НИ ОДНОГО чипа окружения', async () => {
    renderRouted('/logs/x');
    await screen.findByText(LOGS_MARKER);
    expect(currentNavHref()).toBe('/logs');
    // Ровно то, ради чего #1106 их прятал: на под-пути они не возвращаются.
    expect(envGroup()).toBeNull();
    expect(projectGroup()).toBeTruthy();
  });

  it('корень /logs даёт ТО ЖЕ, что его под-путь (иначе правило снова двойное)', async () => {
    renderRouted('/logs');
    await screen.findByText(LOGS_MARKER);
    expect(currentNavHref()).toBe('/logs');
    expect(envGroup()).toBeNull();
    expect(projectGroup()).toBeTruthy();
  });

  it('посторонний /logsomething — Обзор с обеими осями, а не Логи без чипов', async () => {
    renderRouted('/logsomething');
    await screen.findByText(OVERVIEW_MARKER);
    expect(screen.queryByText(LOGS_MARKER)).toBeNull();
    expect(currentNavHref()).toBe('/');
    expect(envGroup()).toBeTruthy();
    expect(projectGroup()).toBeTruthy();
  });
});

/**
 * То же расхождение, но по ПРАВАМ (tracker #1111). Не-admin попадает на
 * admin-only URL руками или чужой закладкой, открытой readonly-ключом; экран
 * ему подменяют на Обзор, а скоуп до этой правки продолжал отвечать про
 * подменённый раздел — и на `/backups` прятал селектор ЦЕЛИКОМ, хотя Обзор
 * сужается обеими осями. Кейсы admin рядом — чтобы правка «показать оси
 * не-admin» не оказалась правкой «показать оси всем».
 */
describe('роутер и скоуп сходятся и по ПРАВАМ сессии', () => {
  it.each([
    ['/backups', BACKUPS_MARKER],
    ['/access', ACCESS_MARKER],
  ])('не-admin на admin-only %s: экран Обзора — и ОБЕ оси в селекторе', async (path, hidden) => {
    renderRouted(path, readonlySession);
    await screen.findByText(OVERVIEW_MARKER);
    expect(screen.queryByText(hidden)).toBeNull();
    // Подсветка нава — про тот же экран: подсвечен Обзор, а не пустота.
    expect(currentNavHref()).toBe('/');
    expect(projectGroup()).toBeTruthy();
    expect(envGroup()).toBeTruthy();
  });

  it('admin на /backups: экран Бекапов, и блока скоупа по-прежнему нет вовсе', async () => {
    renderRouted('/backups', adminSession);
    await screen.findByText(BACKUPS_MARKER);
    expect(currentNavHref()).toBe('/backups');
    expect(projectGroup()).toBeNull();
    expect(envGroup()).toBeNull();
  });

  it('admin на /access: экран Админки, проект жив, чипы окружения спрятаны', async () => {
    renderRouted('/access', adminSession);
    await screen.findByText(ACCESS_MARKER);
    expect(currentNavHref()).toBe('/access');
    expect(projectGroup()).toBeTruthy();
    expect(envGroup()).toBeNull();
  });
});
