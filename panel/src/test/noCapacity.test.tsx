// Пины карточки #1071: «у проекта ноль нод — дедиков не будет» система обязана
// СКАЗАТЬ, а не оставлять оператора перед пустым экраном.
//
// Замер, с которого заведена карточка (дев-стенд, 12.08.2026): владелец завёл
// проект `khl-legends`, зарегистрировал версию, авто-деплой отработал — дедиков
// ноль, ни ошибки, ни предупреждения. Причина в том, что нода принадлежит РОВНО
// одному проекту (`nodes.project_id`, #1064/#1065), и планировщик берёт
// кандидатов только среди нод своего проекта. Алерт эту дыру не закрывает, и
// формулировку карточки «алерты тут не помогают» стоит уточнить (сверено с
// `infra/roles/birdman_monitoring_dev/templates/rules.yml.j2`): `NodeDown` висит
// на heartbeat СУЩЕСТВУЮЩЕЙ ноды, `BufferEmptyAllocFail` — на попытках
// аллокации, которых при нуле нод никто не делает, а `BufferEmptyReady*` здесь
// как раз ЗАГОРИТСЯ (мастер эмитит явный нулевой ready по флиту с
// `buffer_ready > 0`, #960) — только скажет «регион без ready-серверов» в
// алерт-канал, то есть назовёт симптом и не тому, кто стоит перед экраном.
//
// Пиним три поверхности панели:
//   1) Деплои — баннер над карточками «active {semver} · 0 дедиков» / «Живых
//      дедиков нет», то есть ровно там, где рождается вопрос;
//   2) Деплои — тост в момент действия, когда мастер вернул warning
//      no_live_nodes (раньше здесь был зелёный «успех»);
//   3) Обзор — карточка тачек при пустом флоте перестаёт печатать «all active».
//
// Негативы обязательны: с живой нодой сигнала быть не должно, и при НЕприехавшем
// списке нод — тоже (иначе сбой запроса рисовал бы «нод нет» полному флоту).

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ReactElement } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import type { Environment, NodeInfo, SessionInfo, VersionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import type { Lang } from '../lib/i18n';
import { ThemeProvider } from '../lib/theme';
import { SessionContext } from '../lib/session';
import { EnvContext } from '../lib/env';
import { ToastProvider } from '../components/Toast';
import { Deploys } from '../screens/Deploys';
import { Overview } from '../screens/Overview';

// Спарклайн (uPlot) падает на null-canvas в jsdom и к сигналу отношения не имеет.
vi.mock('../components/Sparkline', async () => {
  const actual = await vi.importActual<typeof import('../components/Sparkline')>('../components/Sparkline');
  return { ...actual, Sparkline: () => null };
});

// Radix Dialog дёргает эти API при фокусе — в jsdom их нет.
HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

const dev: Environment = {
  project: 'khl-legends', name: 'dev', production: false,
  auto_deploy: true, retention_keep: 20, created_at: '2026-08-01T00:00:00Z',
};

const mkVer = (over: Partial<VersionInfo>): VersionInfo => ({
  id: over.id ?? 'v1',
  project: 'khl-legends',
  semver: over.semver ?? '1.0.0',
  image_ref: 'ghcr.io/example/khl:1.0.0',
  env: over.env ?? 'dev',
  state: over.state ?? 'active',
  created_at: '2026-08-12T00:00:00Z',
  deprecated_at: over.deprecated_at,
  promoted_from: over.promoted_from,
});

const mkNode = (id: string, state: NodeInfo['state']): NodeInfo => ({
  id,
  project: 'khl-legends',
  region: 'eu',
  env: 'dev',
  hostname: `${id}.eu`,
  public_ip: '203.0.113.20',
  capacity_slots: 8,
  agent_version: '0.1.0',
  state,
  created_at: '2026-08-12T00:00:00Z',
});

const sess = (...scopes: SessionInfo['scopes']): SessionInfo => ({ scopes, name: 'k' });

const baseEnv = {
  environments: [dev],
  project: 'khl-legends',
  selected: null as string | null,
  setSelected: () => {},
  loading: false,
  reload: () => {},
};

interface FetchOpts {
  versions: VersionInfo[];
  nodes?: NodeInfo[];
  /** true → GET /v1/nodes отдаёт 500 (список так и не приезжает). */
  nodesFail?: boolean;
  /** Тело deploy-ответа мастера ({deploy: …}). */
  deploy?: Record<string, unknown>;
}

function stubFetch(o: FetchOpts) {
  const json = (body: unknown, status = 200) =>
    Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      const u = String(url);
      if (u.includes('/v1/deploy')) return json({ deploy: o.deploy ?? { state: 'prepulling', pending_nodes: 1 } });
      if (u.includes('/v1/nodes')) {
        return o.nodesFail === true ? json({ error: 'internal' }, 500) : json({ nodes: o.nodes ?? [] });
      }
      if (u.includes('/v1/versions')) return json({ versions: o.versions });
      if (u.includes('/v1/servers')) return json({ servers: [] });
      if (u.includes('/v1/matches')) return json({ matches: [] });
      if (u.includes('/v1/events')) return json({ events: [] });
      return json({});
    }),
  );
}

function renderDeploys(lang: Lang = 'en') {
  return render(
    <SessionContext.Provider value={{ session: sess('admin'), login: async () => {}, logout: async () => {}, invalidate: () => {} }}>
      <I18nProvider initialLang={lang}>
        <ToastProvider>
          <EnvContext.Provider value={baseEnv}>
            <Deploys navigate={() => {}} />
          </EnvContext.Provider>
        </ToastProvider>
      </I18nProvider>
    </SessionContext.Provider>,
  );
}

const renderOverview = (ui: ReactElement) =>
  render(
    <ThemeProvider>
      <I18nProvider initialLang="en">
        <Tooltip.Provider>{ui}</Tooltip.Provider>
      </I18nProvider>
    </ThemeProvider>,
  );

const EN_BANNER = 'No nodes in this project — no dedic will start';
const RU_BANNER = 'В проекте нет ни одной тачки — дедики не запустятся';

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Деплои — баннер «у проекта нет тачек»', () => {
  it('активная версия + ноль нод → баннер называет причину и упоминает версию с проектом', async () => {
    stubFetch({ versions: [mkVer({ state: 'active', semver: '1.0.0' })], nodes: [] });
    renderDeploys();
    expect(await screen.findByText(EN_BANNER)).toBeTruthy();
    // Тело баннера объясняет механизм, а не просто констатирует пустоту.
    const body = screen.getByText(/has no node of its own/);
    expect(body.textContent).toContain('1.0.0');
    expect(body.textContent).toContain('khl-legends');
    // И даёт куда идти.
    expect(screen.getByRole('button', { name: 'Go to Fleet' })).toBeTruthy();
  });

  it('баннер переведён (RU) — строка идёт через каталог, а не захардкожена', async () => {
    stubFetch({ versions: [mkVer({ state: 'active' })], nodes: [] });
    renderDeploys('ru');
    expect(await screen.findByText(RU_BANNER)).toBeTruthy();
    expect(screen.queryByText(EN_BANNER)).toBeNull();
  });

  it('есть живая нода → баннера нет', async () => {
    stubFetch({ versions: [mkVer({ state: 'active' })], nodes: [mkNode('n1', 'active')] });
    renderDeploys();
    await screen.findAllByText('1.0.0');
    expect(screen.queryByText(EN_BANNER)).toBeNull();
  });

  it('единственная нода выведена из флота (dead) → ёмкости нет, баннер есть', async () => {
    stubFetch({ versions: [mkVer({ state: 'active' })], nodes: [mkNode('n1', 'dead')] });
    renderDeploys();
    expect(await screen.findByText(EN_BANNER)).toBeTruthy();
  });

  it('список нод не приехал (500) → баннера НЕТ: сбой запроса не равен «нод нет»', async () => {
    stubFetch({ versions: [mkVer({ state: 'active' })], nodesFail: true });
    renderDeploys();
    await screen.findAllByText('1.0.0'); // экран отрисован, версии на месте
    expect(screen.queryByText(EN_BANNER)).toBeNull();
  });

  it('активной версии нет → баннера нет (вопрос оператора ещё не про ёмкость)', async () => {
    stubFetch({ versions: [mkVer({ state: 'registered' })], nodes: [] });
    renderDeploys();
    await screen.findAllByText('1.0.0');
    expect(screen.queryByText(EN_BANNER)).toBeNull();
  });
});

describe('Деплои — тост в момент действия (warning мастера)', () => {
  it('deploy вернул warning no_live_nodes → тост говорит, что дедиков не будет, а не «успех»', async () => {
    stubFetch({
      versions: [mkVer({ id: 'v-dep', state: 'deprecated', semver: '0.9.0' }), mkVer({ state: 'active' })],
      nodes: [mkNode('n1', 'active')], // баннера нет — проверяем именно тост
      deploy: { state: 'active', pending_nodes: 0, warning: 'no_live_nodes' },
    });
    renderDeploys();
    fireEvent.click((await screen.findAllByRole('button', { name: 'Deploy' }))[0]);
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Deploy' }));
    await waitFor(() => {
      expect(
        screen.getByText('0.9.0 is active, but the project has no nodes — no dedic will start'),
      ).toBeTruthy();
    });
    expect(screen.queryByText('Deploy of 0.9.0 started')).toBeNull();
  });

  it('без warning — прежний тост об успехе (контроль: сигнал не сработал на пустом месте)', async () => {
    stubFetch({
      versions: [mkVer({ id: 'v-dep', state: 'deprecated', semver: '0.9.0' }), mkVer({ state: 'active' })],
      nodes: [mkNode('n1', 'active')],
      deploy: { state: 'prepulling', pending_nodes: 1 },
    });
    renderDeploys();
    fireEvent.click((await screen.findAllByRole('button', { name: 'Deploy' }))[0]);
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Deploy' }));
    await waitFor(() => {
      expect(screen.getByText('Deploy of 0.9.0 started')).toBeTruthy();
    });
  });
});

describe('Обзор — карточка тачек при пустом флоте', () => {
  it('нод ноль → «no nodes in this project», а не «all active»', async () => {
    stubFetch({ versions: [], nodes: [] });
    renderOverview(<Overview />);
    expect(await screen.findByText('no nodes in this project')).toBeTruthy();
    expect(screen.queryByText('all active')).toBeNull();
  });

  it('живая нода → прежнее «all active» (контроль)', async () => {
    stubFetch({ versions: [], nodes: [mkNode('n1', 'active')] });
    renderOverview(<Overview />);
    expect(await screen.findByText('all active')).toBeTruthy();
    expect(screen.queryByText('no nodes in this project')).toBeNull();
  });
});
