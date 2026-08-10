import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { ApiError } from '../lib/api';
import type { ProjectInfo } from '../lib/api';
import { alertDescription, alertProjectOf, alertScopeOf, alertsUnavailable, isPlatformAlert } from '../lib/alerts';
import { toneOfAlertActive, toneOfSeverity } from '../components/Badge';
import { I18nProvider } from '../lib/i18n';
import { ProjectProvider } from '../lib/project';
import { ProjectSelector } from '../components/Shell';
import { Alerts } from '../screens/Alerts';

describe('toneOfSeverity', () => {
  it('critical → dead, warning → warn, прочее → neutral', () => {
    expect(toneOfSeverity('critical')).toBe('dead');
    expect(toneOfSeverity('warning')).toBe('warn');
    expect(toneOfSeverity('info')).toBe('neutral');
    expect(toneOfSeverity('')).toBe('neutral');
  });
});

describe('toneOfAlertActive', () => {
  it('горит → dead, погас → neutral', () => {
    expect(toneOfAlertActive(true)).toBe('dead');
    expect(toneOfAlertActive(false)).toBe('neutral');
  });
});

describe('alertsUnavailable — мягкая деградация', () => {
  it('503 alerts_unconfigured → unconfigured', () => {
    expect(alertsUnavailable(new ApiError(503, 'alerts_unconfigured'))).toBe('unconfigured');
  });
  it('502/504/upstream → upstream', () => {
    expect(alertsUnavailable(new ApiError(502, 'upstream'))).toBe('upstream');
    expect(alertsUnavailable(new ApiError(504, 'bad'))).toBe('upstream');
  });
  it('прочие ошибки → null (обычный ErrorNote)', () => {
    expect(alertsUnavailable(new ApiError(500, 'internal'))).toBeNull();
    expect(alertsUnavailable(new Error('x'))).toBeNull();
  });
});

// --- рендер экрана: мокаем fetch по URL ---

function routeFetch(byFrag: (url: string) => { status: number; body: unknown }) {
  return vi.fn((url: string) => {
    const { status, body } = byFrag(url);
    return Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
  });
}

const rule = {
  name: 'NodeDown',
  group: 'birdman-availability',
  severity: 'critical',
  expr: 'birdman_node_heartbeat_age_seconds > 30',
  for: '0s',
  state: 'inactive',
  description: 'Нода молчит > порога; сработал карантин.',
};

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe('Alerts — пустые состояния', () => {
  it('нет активных / есть правила / нет истории', async () => {
    vi.stubGlobal(
      'fetch',
      routeFetch((url) => {
        if (url.includes('/alerts/active')) return { status: 200, body: { alerts: [] } };
        if (url.includes('/alerts/rules')) return { status: 200, body: { rules: [rule] } };
        return { status: 200, body: { alerts: [] } }; // history
      }),
    );
    render(
      <I18nProvider initialLang="en">
        <Alerts />
      </I18nProvider>,
    );
    expect(await screen.findByText('No active alerts — all clear.')).toBeTruthy();
    expect(await screen.findByText('NodeDown')).toBeTruthy();
    // severity-бейдж переведён (сырой код critical остаётся в title)
    expect(await screen.findByText('Critical')).toBeTruthy();
    expect(await screen.findByText('No alerts have fired yet.')).toBeTruthy();
  });
});

describe('Alerts — vmalert не настроен', () => {
  it('503 alerts_unconfigured → мягкая подпись, не падение', async () => {
    vi.stubGlobal(
      'fetch',
      routeFetch((url) => {
        if (url.includes('/alerts/history')) return { status: 200, body: { alerts: [] } };
        return { status: 503, body: { error: 'alerts_unconfigured', detail: 'vmalert_url is not set' } };
      }),
    );
    render(
      <I18nProvider initialLang="en">
        <Alerts />
      </I18nProvider>,
    );
    const hits = await screen.findAllByText('Alerts are not configured on this master (vmalert_url is empty).');
    expect(hits.length).toBeGreaterThan(0);
  });
});

// --- двуязычные описания алертов (EN канон + опциональный RU, фоллбэк на EN) ---

describe('alertDescription — выбор описания по локали', () => {
  it('RU-локаль + непустой description_ru → RU', () => {
    expect(alertDescription({ description: 'node is unreachable', description_ru: 'нода недоступна' }, 'ru')).toBe(
      'нода недоступна',
    );
  });
  it('RU-локаль без description_ru (или пустой) → фоллбэк на EN', () => {
    expect(alertDescription({ description: 'node is unreachable' }, 'ru')).toBe('node is unreachable');
    expect(alertDescription({ description: 'node is unreachable', description_ru: '' }, 'ru')).toBe('node is unreachable');
  });
  it('EN-локаль → всегда EN description (даже если есть description_ru)', () => {
    expect(alertDescription({ description: 'node is unreachable', description_ru: 'нода недоступна' }, 'en')).toBe(
      'node is unreachable',
    );
  });
});

const activeBilingual = {
  name: 'NodeDown',
  severity: 'critical',
  region: 'eu',
  node: 'n1',
  state: 'firing',
  active_at: '2026-07-08T09:00:00Z',
  value: '42',
  description: 'node is unreachable',
  description_ru: 'нода недоступна',
  muted: false,
};

function renderAlertsWith(lang: 'en' | 'ru', active: unknown[]) {
  vi.stubGlobal(
    'fetch',
    routeFetch((url) => {
      if (url.includes('/alerts/active')) return { status: 200, body: { alerts: active } };
      if (url.includes('/alerts/rules')) return { status: 200, body: { rules: [] } };
      return { status: 200, body: { alerts: [] } }; // history / mutes
    }),
  );
  render(
    <I18nProvider initialLang={lang}>
      <Alerts />
    </I18nProvider>,
  );
}

describe('Alerts — описание активного алерта по локали', () => {
  it('RU-локаль показывает description_ru, когда он есть', async () => {
    renderAlertsWith('ru', [activeBilingual]);
    expect(await screen.findByText('нода недоступна')).toBeTruthy();
    expect(screen.queryByText('node is unreachable')).toBeNull();
  });
  it('RU-локаль фоллбэчится на description, когда description_ru нет', async () => {
    const { description_ru, ...noRu } = activeBilingual;
    void description_ru;
    renderAlertsWith('ru', [noRu]);
    expect(await screen.findByText('node is unreachable')).toBeTruthy();
  });
  it('EN-локаль показывает description', async () => {
    renderAlertsWith('en', [activeBilingual]);
    expect(await screen.findByText('node is unreachable')).toBeTruthy();
    expect(screen.queryByText('нода недоступна')).toBeNull();
  });
});

// --- проектное измерение алертов (мультипроект, #956) ---

describe('alertScopeOf / isPlatformAlert / alertProjectOf', () => {
  it('scope от мастера главнее вывода из project', () => {
    expect(alertScopeOf({ project: '', scope: 'platform' })).toBe('platform');
    expect(alertScopeOf({ project: 'game', scope: 'project' })).toBe('project');
  });
  it('scope нет, но project непустой → проектный (страховка для чужого прокси)', () => {
    expect(alertScopeOf({ project: 'game' })).toBe('project');
  });
  it('мастер БЕЗ additive-полей → область неизвестна, а НЕ «платформенный»', () => {
    // Иначе на старом мастере подпись навесилась бы на все алерты подряд,
    // включая проектные, — честная с виду и ничего не значащая.
    expect(alertScopeOf({})).toBeUndefined();
    expect(isPlatformAlert({})).toBe(false);
    expect(alertScopeOf({ project: '' })).toBeUndefined();
  });
  it('проект для показа: пусто/нет поля → undefined', () => {
    expect(alertProjectOf({ project: 'game' })).toBe('game');
    expect(alertProjectOf({ project: '' })).toBeUndefined();
    expect(alertProjectOf({})).toBeUndefined();
  });
});

const projectInfo = (slug: string): ProjectInfo => ({
  id: `id-${slug}`,
  slug,
  match_size: 2,
  created_at: '2026-07-01T00:00:00Z',
});
const game = projectInfo('game');
const arena = projectInfo('arena');

/** Активный алерт в форме мастера ПОСЛЕ #955: project + выведенный scope. */
const activeOf = (name: string, project: string) => ({
  name,
  severity: 'critical',
  region: 'eu',
  node: 'n1',
  state: 'firing',
  active_at: '2026-08-01T09:00:00Z',
  value: '1',
  description: `${name} fired`,
  project,
  scope: project === '' ? 'platform' : 'project',
  muted: false,
});

/** Запись истории в той же форме. */
const eventOf = (name: string, project: string) => ({
  name,
  severity: 'warning',
  region: 'eu',
  node: 'n2',
  startsAt: '2026-08-01T08:00:00Z',
  endsAt: '2026-08-01T08:30:00Z',
  description: `${name} history`,
  active: false,
  received_at: '2026-08-01T08:30:01Z',
  project,
  scope: project === '' ? 'platform' : 'project',
  muted: false,
});

const jsonRes = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });

/**
 * Мок API алертов, который ЧЕСТНО повторяет серверный контракт #955: ?project=
 * фильтрует НЕ СКРЫВАЮЩЕ — запись уходит, только если её project явно чужой.
 * Так тест проверяет панель против настоящего поведения мастера, а не против
 * удобной выдумки. Правила отдаются всегда: они не сужаются вовсе.
 */
function stubAlertsApi(data: { active: unknown[]; history: unknown[]; rules?: unknown[] }, urls: string[]) {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      const u = String(url);
      urls.push(u);
      if (u.startsWith('/v1/projects')) return Promise.resolve(jsonRes({ projects: [game, arena] }));
      const want = new URL(u, 'http://x').searchParams.get('project') ?? '';
      const keep = (a: unknown) => {
        const p = (a as { project?: string }).project;
        return want === '' || p === undefined || p === '' || p === want;
      };
      if (u.startsWith('/v1/alerts/active')) return Promise.resolve(jsonRes({ alerts: data.active.filter(keep) }));
      if (u.startsWith('/v1/alerts/history')) return Promise.resolve(jsonRes({ alerts: data.history.filter(keep) }));
      if (u.startsWith('/v1/alerts/rules')) return Promise.resolve(jsonRes({ rules: data.rules ?? [] }));
      return Promise.resolve(jsonRes({ mutes: [] }));
    }),
  );
}

function renderUnderProject(lang: 'en' | 'ru' = 'en') {
  render(
    <I18nProvider initialLang={lang}>
      <ProjectProvider>
        <div>
          <ProjectSelector />
          <Alerts />
        </div>
      </ProjectProvider>
    </I18nProvider>,
  );
}

/** Чип области внутри строки/ячейки этого алерта (а не «где-то на экране»). */
function chipNear(name: string): HTMLElement | null {
  const row = screen.getByText(name).closest('li, td');
  return row === null ? null : within(row as HTMLElement).queryByText('platform');
}

describe('Alerts — сужение по проекту (не скрывающее)', () => {
  it('платформенный алерт виден при выбранном проекте, подписан и НЕ исчезает при смене', async () => {
    // Главный тест карточки: молча спрятать «мастер лежит» — худший исход.
    const urls: string[] = [];
    stubAlertsApi(
      {
        active: [activeOf('MasterDown', ''), activeOf('BufferEmptyReadyProd', 'game'), activeOf('AllocationFailures', 'arena')],
        history: [],
      },
      urls,
    );
    renderUnderProject();

    // Проект game: свой алерт виден, чужой (arena) — нет, платформенный виден.
    await waitFor(() => {
      expect(screen.getByText('BufferEmptyReadyProd')).toBeTruthy();
    });
    expect(urls.some((u) => u === '/v1/alerts/active?project=game')).toBe(true);
    expect(screen.getByText('MasterDown')).toBeTruthy();
    expect(screen.queryByText('AllocationFailures')).toBeNull();
    // Подпись стоит ИМЕННО на платформенном и ИМЕННО на нём одном.
    expect(chipNear('MasterDown')).toBeTruthy();
    expect(chipNear('BufferEmptyReadyProd')).toBeNull();
    // Проект проектного алерта показан рядом с регионом и нодой.
    expect(screen.getByText('game · eu · n1')).toBeTruthy();

    fireEvent.change(await screen.findByRole('combobox', { name: 'Project' }), { target: { value: 'arena' } });

    // Смена проекта ПЕРЕСЧИТЫВАЕТ данные (грабли #948): новый ?project= ушёл,
    // алерт соседнего проекта приехал, свой прежний — ушёл…
    await waitFor(() => {
      expect(screen.getByText('AllocationFailures')).toBeTruthy();
    });
    expect(urls.some((u) => u === '/v1/alerts/active?project=arena')).toBe(true);
    expect(screen.queryByText('BufferEmptyReadyProd')).toBeNull();
    // …а платформенный остался на экране и остался подписанным.
    expect(screen.getByText('MasterDown')).toBeTruthy();
    expect(chipNear('MasterDown')).toBeTruthy();
  });

  it('история сужается тем же ?project=, платформенная запись остаётся', async () => {
    const urls: string[] = [];
    stubAlertsApi(
      {
        active: [],
        history: [eventOf('CertExpirySoon', ''), eventOf('BufferEmptyAllocFail', 'game'), eventOf('TickDegraded', 'arena')],
      },
      urls,
    );
    renderUnderProject();

    await waitFor(() => {
      expect(screen.getByText('BufferEmptyAllocFail')).toBeTruthy();
    });
    expect(urls.some((u) => u === '/v1/alerts/history?limit=50&project=game')).toBe(true);
    expect(screen.getByText('CertExpirySoon')).toBeTruthy();
    expect(chipNear('CertExpirySoon')).toBeTruthy();
    expect(screen.queryByText('TickDegraded')).toBeNull();
    // Колонка «где»: проект проектной записи рядом с регионом и нодой.
    expect(screen.getByText('game · eu · n2')).toBeTruthy();
    expect(screen.getByText('eu · n2')).toBeTruthy(); // платформенная — без проекта
  });

  it('каталог правил НЕ сужается — и говорит об этом', async () => {
    const urls: string[] = [];
    stubAlertsApi({ active: [], history: [], rules: [rule] }, urls);
    renderUnderProject();

    await waitFor(() => {
      expect(screen.getByText('NodeDown')).toBeTruthy();
    });
    fireEvent.change(await screen.findByRole('combobox', { name: 'Project' }), { target: { value: 'arena' } });
    await waitFor(() => {
      expect(urls.some((u) => u === '/v1/alerts/active?project=arena')).toBe(true);
    });

    const ruleUrls = urls.filter((u) => u.startsWith('/v1/alerts/rules'));
    expect(ruleUrls.length).toBeGreaterThan(0);
    // Ни одного запроса правил с фильтром: у правила проекта нет в принципе.
    expect(ruleUrls.every((u) => u === '/v1/alerts/rules')).toBe(true);
    expect(screen.getByText(/The rule catalogue is platform-wide/)).toBeTruthy();
    // И подпись, что сужение не скрывающее, — у сужаемой секции.
    expect(screen.getByText(/Platform alerts \(master, nodes, disks/)).toBeTruthy();
  });

  it('master БЕЗ additive-полей: видно всё, подписи не выдумываются', async () => {
    // Поля project/scope опциональны — панель обязана пережить мастер, который
    // ещё не перевыкачен: сужать нечем, значит видно всё и подписей нет.
    const urls: string[] = [];
    const bare = (name: string) => {
      const { project, scope, ...rest } = activeOf(name, 'game');
      void project;
      void scope;
      return rest;
    };
    stubAlertsApi({ active: [bare('MasterDown'), bare('BufferEmptyReadyProd')], history: [], rules: [] }, urls);
    renderUnderProject();

    await waitFor(() => {
      expect(screen.getByText('MasterDown')).toBeTruthy();
    });
    expect(screen.getByText('BufferEmptyReadyProd')).toBeTruthy();
    expect(screen.queryByText('platform')).toBeNull();
    // Проекта в мета-строке тоже нет — только регион и нода.
    expect(screen.getAllByText('eu · n1').length).toBe(2);
  });

  it('RU-локаль: подпись платформенного алерта переведена', async () => {
    stubAlertsApi({ active: [activeOf('MasterDown', '')], history: [], rules: [] }, []);
    renderUnderProject('ru');
    await waitFor(() => {
      expect(screen.getByText('MasterDown')).toBeTruthy();
    });
    expect(screen.getByText('платформенный')).toBeTruthy();
    expect(screen.queryByText('platform')).toBeNull();
  });
});
