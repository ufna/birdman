// Task 1 («Как залить билд»): чистые билдеры curl (lib/deployHowto.ts),
// карточка-инструкция (components/DeployHowto.tsx) и её интеграция в
// экран Деплоев (пустое состояние — развёрнута).
// Task 7 (registries v1 §5) добавляет: подстановку key в билдеры, шаг-1
// подсказку про реестры и интерактивный пикер/inline-создание deploy-ключа
// на шаге 2 (admin-only) — см. описания ниже.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ReactElement } from 'react';
import type { ApiKey, SessionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import {
  GENERIC_IMAGE_EXAMPLE,
  buildHowtoCtx,
  defaultDeployKeyName,
  deployCurl,
  deployKeyOptions,
  registerVersionCurl,
} from '../lib/deployHowto';
import type { HowtoCtx } from '../lib/deployHowto';
import { DeployHowto } from '../components/DeployHowto';
import { Deploys } from '../screens/Deploys';

const sess = (...scopes: SessionInfo['scopes']): SessionInfo => ({ scopes, name: 'k' });

function withSession(session: SessionInfo | null, ui: ReactElement): ReactElement {
  return (
    <SessionContext.Provider value={{ session, login: async () => {}, logout: async () => {}, invalidate: () => {} }}>
      <I18nProvider initialLang="en">{ui}</I18nProvider>
    </SessionContext.Provider>
  );
}

const ctx: HowtoCtx = { origin: 'http://127.0.0.1:8100', project: 'ourgame', exampleImage: 'ghcr.io/x/y:1.0.0' };

/**
 * fetch-мок для /v1/apikeys, нужный с Task 7: DeployHowto теперь делает
 * GET /v1/apikeys для admin-сессии (пикер ключей), так что любой рендер
 * карточки с admin-сессией должен либо ждать этот запрос (проверяя пикер),
 * либо хотя бы не падать на нём — отсюда стаб и в тестах Task 1, которые
 * пикер не проверяют. POST — создание deploy-ключа (кнопка «создать»): без
 * override эхо-ит name/scopes из тела запроса (как реальный backend — секрет
 * фиксирован тестовым значением); `create` override — для сценария ошибки.
 */
function apikeysFetch(list: ApiKey[], create?: { status: number; body: unknown }) {
  return vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    if (String(url).includes('/v1/apikeys')) {
      if (method === 'POST') {
        if (create !== undefined) {
          return Promise.resolve(new Response(JSON.stringify(create.body), { status: create.status, headers: { 'Content-Type': 'application/json' } }));
        }
        const reqBody = JSON.parse(String(init?.body)) as { name: string; scopes: string[] };
        const body = {
          key: { id: 'new', name: reqBody.name, scopes: reqBody.scopes, created_at: '2026-07-10T00:00:00Z', revoked_at: null },
          secret: 'bmk_freshsecret',
        };
        return Promise.resolve(new Response(JSON.stringify(body), { status: 201, headers: { 'Content-Type': 'application/json' } }));
      }
      return Promise.resolve(new Response(JSON.stringify({ apikeys: list }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

// --- lib/deployHowto: билдеры curl (чистые функции, точные строки) ---

describe('lib/deployHowto — registerVersionCurl', () => {
  it('auth-заголовок, URL с origin, JSON-поля project/semver/image_ref/env', () => {
    const cmd = registerVersionCurl(ctx, '1.2.3', 'dev');
    expect(cmd).toContain('curl -H "Authorization: Bearer $BIRDMAN_DEPLOY_KEY"');
    expect(cmd).toContain('http://127.0.0.1:8100/v1/versions');
    expect(cmd).toContain('"project": "ourgame"');
    expect(cmd).toContain('"semver": "1.2.3"');
    expect(cmd).toContain('"image_ref": "ghcr.io/x/y:1.0.0"');
    expect(cmd).toContain('"env": "dev"'); // env заменил channel (environments v1)
    expect(cmd).not.toContain('channel');
  });

  it('любой env подставляется как есть (dev/prod/staging)', () => {
    expect(registerVersionCurl(ctx, '1.2.3', 'staging')).toContain('"env": "staging"');
    expect(registerVersionCurl(ctx, '1.2.3', 'prod')).toContain('"env": "prod"');
  });
});

describe('lib/deployHowto — deployCurl', () => {
  it('auth-заголовок, URL с origin, version_id-плейсхолдер', () => {
    const cmd = deployCurl(ctx);
    expect(cmd).toContain('curl -H "Authorization: Bearer $BIRDMAN_DEPLOY_KEY"');
    expect(cmd).toContain('http://127.0.0.1:8100/v1/deploy');
    expect(cmd).toContain('"version_id"');
  });
});

// --- Task 7: билдеры с key-вариантами (none / name-only / name+secret) ---

describe('lib/deployHowto — registerVersionCurl с key (Task 7 §5)', () => {
  it('без key — поведение не меняется (нет comment, плейсхолдер в Bearer)', () => {
    const cmd = registerVersionCurl(ctx, '1.2.3', 'prod');
    expect(cmd).not.toContain('# key:');
    expect(cmd).toContain('Bearer $BIRDMAN_DEPLOY_KEY');
  });

  it('только name (выбран существующий ключ) — comment над командой, Bearer остаётся плейсхолдером', () => {
    const cmd = registerVersionCurl(ctx, '1.2.3', 'prod', { name: 'ci-deploy' });
    expect(cmd).toContain('# key: ci-deploy');
    expect(cmd).toContain('Bearer $BIRDMAN_DEPLOY_KEY');
    expect(cmd.indexOf('# key: ci-deploy')).toBeLessThan(cmd.indexOf('curl'));
  });

  it('name+secret (ключ создан только что) — comment И секрет в Bearer, плейсхолдера больше нет', () => {
    const cmd = registerVersionCurl(ctx, '1.2.3', 'prod', { name: 'deploy-20260710', secret: 'bmk_freshsecret' });
    expect(cmd).toContain('# key: deploy-20260710');
    expect(cmd).toContain('Bearer bmk_freshsecret');
    expect(cmd).not.toContain('$BIRDMAN_DEPLOY_KEY');
  });
});

describe('lib/deployHowto — deployCurl с key (Task 7 §5)', () => {
  it('без key — не меняется', () => {
    expect(deployCurl(ctx)).not.toContain('# key:');
  });
  it('только name — comment, плейсхолдер остаётся', () => {
    const cmd = deployCurl(ctx, { name: 'ci-deploy' });
    expect(cmd).toContain('# key: ci-deploy');
    expect(cmd).toContain('Bearer $BIRDMAN_DEPLOY_KEY');
  });
  it('name+secret — comment и секрет в Bearer', () => {
    const cmd = deployCurl(ctx, { name: 'deploy-20260710', secret: 'bmk_freshsecret' });
    expect(cmd).toContain('# key: deploy-20260710');
    expect(cmd).toContain('Bearer bmk_freshsecret');
  });
});

describe('lib/deployHowto — deployKeyOptions (Task 7 §5)', () => {
  const mk = (over: Partial<ApiKey>): ApiKey => ({
    id: over.id ?? 'x',
    name: over.name ?? 'n',
    scopes: over.scopes ?? ['deploy'],
    created_at: '2026-01-01T00:00:00Z',
    revoked_at: over.revoked_at ?? null,
  });

  it('оставляет только активные ключи со скоупом deploy или admin', () => {
    const keys = [
      mk({ id: '1', name: 'a', scopes: ['deploy'] }),
      mk({ id: '2', name: 'b', scopes: ['admin'] }),
      mk({ id: '3', name: 'c', scopes: ['readonly'] }), // без deploy/admin
      mk({ id: '4', name: 'd', scopes: ['deploy'], revoked_at: '2026-01-02T00:00:00Z' }), // revoked
      mk({ id: '5', name: 'e', scopes: ['matchmaking', 'allocate'] }), // ни deploy, ни admin
    ];
    expect(deployKeyOptions(keys).map((k) => k.id)).toEqual(['1', '2']);
  });

  it('пустой список ключей → пустой список опций', () => {
    expect(deployKeyOptions([])).toEqual([]);
  });
});

describe('lib/deployHowto — defaultDeployKeyName (Task 7 §5)', () => {
  it('форматирует deploy-YYYYMMDD в UTC (не зависит от локальной TZ)', () => {
    expect(defaultDeployKeyName(new Date('2026-07-10T23:30:00Z'))).toBe('deploy-20260710');
    expect(defaultDeployKeyName(new Date('2026-01-05T00:00:00Z'))).toBe('deploy-20260105');
  });
});

describe('lib/deployHowto — GENERIC_IMAGE_EXAMPLE', () => {
  it('заглушка без хардкода реальной org/repo', () => {
    expect(GENERIC_IMAGE_EXAMPLE).toBe('ghcr.io/<org>/<game>-server:1.2.3');
  });
});

describe('lib/deployHowto — buildHowtoCtx', () => {
  it('ровно один проект → реальный slug + image_ref активной версии', () => {
    const c = buildHowtoCtx('http://h', [
      {
        project: 'ourgame',
        versions: [
          { state: 'registered', image_ref: 'ghcr.io/a/b:0.1.0' },
          { state: 'active', image_ref: 'ghcr.io/a/b:0.2.0' },
        ],
      },
    ]);
    expect(c).toEqual({ origin: 'http://h', project: 'ourgame', exampleImage: 'ghcr.io/a/b:0.2.0' });
  });

  it('несколько проектов → плейсхолдер <project> и generic-образ', () => {
    const c = buildHowtoCtx('http://h', [
      { project: 'a', versions: [] },
      { project: 'b', versions: [] },
    ]);
    expect(c.project).toBe('<project>');
    expect(c.exampleImage).toBe(GENERIC_IMAGE_EXAMPLE);
  });

  it('ноль проектов → тот же плейсхолдер', () => {
    const c = buildHowtoCtx('http://h', []);
    expect(c.project).toBe('<project>');
    expect(c.exampleImage).toBe(GENERIC_IMAGE_EXAMPLE);
  });

  it('единственный проект без активной версии → image_ref первой версии', () => {
    const c = buildHowtoCtx('http://h', [
      { project: 'ourgame', versions: [{ state: 'registered', image_ref: 'ghcr.io/a/b:0.1.0' }] },
    ]);
    expect(c.exampleImage).toBe('ghcr.io/a/b:0.1.0');
  });

  it('единственный проект вообще без версий → generic-образ', () => {
    const c = buildHowtoCtx('http://h', [{ project: 'ourgame', versions: [] }]);
    expect(c.exampleImage).toBe(GENERIC_IMAGE_EXAMPLE);
  });
});

// --- components/DeployHowto: рендер, сворачивание, скоуп, copy ---

describe('DeployHowto — сворачивание по умолчанию', () => {
  it('свёрнута: шагов не видно; клик по тогглу — шаги видны, aria-expanded переключается', () => {
    render(withSession(sess('readonly'), <DeployHowto ctx={ctx} navigate={() => {}} />));
    expect(screen.queryByText('1. Build & push the server image')).toBeNull();
    const toggle = screen.getByRole('button', { name: 'Show steps' });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');

    fireEvent.click(toggle);
    expect(screen.getByText('1. Build & push the server image')).toBeTruthy();
    expect(screen.getByText(/non-production/)).toBeTruthy(); // register-label теперь про env, не channel
    expect(screen.getByRole('button', { name: 'Hide steps' }).getAttribute('aria-expanded')).toBe('true');
  });

  it('defaultExpanded — сразу развёрнута без клика', () => {
    render(withSession(sess('readonly'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));
    expect(screen.getByText('1. Build & push the server image')).toBeTruthy();
  });
});

describe('DeployHowto — ссылка на Админку по скоупу', () => {
  it('admin видит внутреннюю ссылку /access и переходит через navigate (не полный reload)', () => {
    vi.stubGlobal('fetch', apikeysFetch([]));
    const navigate = vi.fn();
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={navigate} defaultExpanded />));
    const link = screen.getByRole('link', { name: 'Create one on the Admin screen' });
    expect(link.getAttribute('href')).toBe('/access');
    fireEvent.click(link);
    expect(navigate).toHaveBeenCalledWith('/access');
  });

  it('readonly видит текст без ссылки', () => {
    render(withSession(sess('readonly'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));
    expect(screen.queryByRole('link', { name: 'Create one on the Admin screen' })).toBeNull();
    expect(screen.getByText('An admin creates one on the Admin screen.')).toBeTruthy();
  });

  it('нет сессии — тоже текст без ссылки (не считается админом)', () => {
    render(withSession(null, <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));
    expect(screen.queryByRole('link', { name: 'Create one on the Admin screen' })).toBeNull();
  });
});

// --- Task 7 §5: шаг 1 — подсказка про приватный реестр ---

describe('DeployHowto — шаг 1: подсказка про приватный реестр (Task 7 §5)', () => {
  it('admin видит внутреннюю ссылку на Реестры и переходит через navigate', () => {
    vi.stubGlobal('fetch', apikeysFetch([]));
    const navigate = vi.fn();
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={navigate} defaultExpanded />));
    const link = screen.getByRole('link', { name: 'Private image? Connect it: Admin → Registries' });
    expect(link.getAttribute('href')).toBe('/access');
    fireEvent.click(link);
    expect(navigate).toHaveBeenCalledWith('/access');
  });

  it('readonly видит текст без ссылки', () => {
    render(withSession(sess('readonly'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));
    expect(screen.queryByRole('link', { name: /Registries/ })).toBeNull();
    expect(screen.getByText('Private image? An admin connects it: Admin → Registries.')).toBeTruthy();
  });
});

describe('DeployHowto — copy-кнопки пишут точную команду в clipboard', () => {
  it('копирует curl регистрации версии', async () => {
    vi.stubGlobal('fetch', apikeysFetch([]));
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));

    const block = screen.getByTestId('howto-register-cmd');
    fireEvent.click(within(block).getByRole('button', { name: 'Copy register command' }));
    // Без EnvProvider useEnv=fallback → exampleEnv по умолчанию 'dev'.
    expect(writeText).toHaveBeenCalledWith(registerVersionCurl(ctx, '1.2.3', 'dev'));
    expect(await within(block).findByRole('button', { name: 'Copied' })).toBeTruthy();
  });

  it('копирует curl деплоя', () => {
    vi.stubGlobal('fetch', apikeysFetch([]));
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));

    const block = screen.getByTestId('howto-deploy-cmd');
    fireEvent.click(within(block).getByRole('button', { name: 'Copy deploy command' }));
    expect(writeText).toHaveBeenCalledWith(deployCurl(ctx));
  });
});

// --- Task 7 §5: шаг 2 — пикер существующих deploy-ключей ---

describe('DeployHowto — пикер ключей (Task 7 §5)', () => {
  const activeDeploy: ApiKey = { id: '1', name: 'ci-deploy', scopes: ['deploy'], created_at: '2026-01-01T00:00:00Z', revoked_at: null };
  const revoked: ApiKey = { id: '2', name: 'old', scopes: ['deploy'], created_at: '2026-01-01T00:00:00Z', revoked_at: '2026-02-01T00:00:00Z' };
  const readonlyScoped: ApiKey = { id: '3', name: 'readonly-key', scopes: ['readonly'], created_at: '2026-01-01T00:00:00Z', revoked_at: null };

  it('лист только активные ключи со скоупом deploy/admin — revoked и readonly-only исключены', async () => {
    vi.stubGlobal('fetch', apikeysFetch([activeDeploy, revoked, readonlyScoped]));
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));

    const select = await screen.findByRole('combobox', { name: 'Use an existing key' });
    const options = within(select).getAllByRole('option').map((o) => o.textContent);
    expect(options).toEqual(['— none —', 'ci-deploy']);
  });

  it('выбор существующего ключа вставляет "# key: <name>" в ОБЕ curl-команды, секрет остаётся плейсхолдером', async () => {
    vi.stubGlobal('fetch', apikeysFetch([activeDeploy]));
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));

    const select = await screen.findByRole('combobox', { name: 'Use an existing key' });
    fireEvent.change(select, { target: { value: '1' } });

    const registerBlock = screen.getByTestId('howto-register-cmd');
    const deployBlock = screen.getByTestId('howto-deploy-cmd');
    expect(registerBlock.textContent).toContain('# key: ci-deploy');
    expect(registerBlock.textContent).toContain('$BIRDMAN_DEPLOY_KEY');
    expect(deployBlock.textContent).toContain('# key: ci-deploy');
    expect(deployBlock.textContent).toContain('$BIRDMAN_DEPLOY_KEY');
  });

  it('не-админ не видит пикер (текущее поведение шага 2 сохранено)', () => {
    render(withSession(sess('readonly'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));
    expect(screen.queryByRole('combobox', { name: 'Use an existing key' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Create a deploy key & fill in' })).toBeNull();
  });
});

describe('DeployHowto — inline-создание deploy-ключа (Task 7 §5)', () => {
  it('создание подставляет свежий секрет в обе команды, показывает warning; «скрыть» возвращает плейсхолдер, но комментарий остаётся', async () => {
    vi.stubGlobal('fetch', apikeysFetch([]));
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));

    const nameInput = await screen.findByLabelText('Key name');
    fireEvent.change(nameInput, { target: { value: 'deploy-20260710' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create a deploy key & fill in' }));

    const registerBlock = screen.getByTestId('howto-register-cmd');
    const deployBlock = screen.getByTestId('howto-deploy-cmd');
    await waitFor(() => {
      expect(registerBlock.textContent).toContain('bmk_freshsecret');
    });
    expect(registerBlock.textContent).toContain('# key: deploy-20260710');
    expect(deployBlock.textContent).toContain('bmk_freshsecret');
    expect(screen.getByText("Copy the secret now — it won't be shown again.")).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Hide secret' }));
    expect(registerBlock.textContent).not.toContain('bmk_freshsecret');
    expect(registerBlock.textContent).toContain('$BIRDMAN_DEPLOY_KEY');
    expect(registerBlock.textContent).toContain('# key: deploy-20260710'); // имя остаётся — секрет лишь скрыт
  });

  it('copy-кнопка копирует команду с уже подставленным секретом (то, что видно)', async () => {
    vi.stubGlobal('fetch', apikeysFetch([]));
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));

    // Имя фиксируем явно — дефолт инпута (deploy-YYYYMMDD от реального now())
    // недетерминирован по дате прогона теста.
    fireEvent.change(await screen.findByLabelText('Key name'), { target: { value: 'deploy-fixed' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create a deploy key & fill in' }));
    const block = await screen.findByTestId('howto-register-cmd');
    await waitFor(() => {
      expect(block.textContent).toContain('bmk_freshsecret');
    });

    fireEvent.click(within(block).getByRole('button', { name: 'Copy register command' }));
    expect(writeText).toHaveBeenCalledWith(registerVersionCurl(ctx, '1.2.3', 'dev', { name: 'deploy-fixed', secret: 'bmk_freshsecret' }));
  });

  it('ошибка создания (400) показана рядом с формой, секрет не подставляется', async () => {
    vi.stubGlobal(
      'fetch',
      apikeysFetch([], { status: 400, body: { error: 'bad_request', detail: 'name is required' } }),
    );
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));

    fireEvent.click(screen.getByRole('button', { name: 'Create a deploy key & fill in' }));
    expect(await screen.findByText(/name is required/)).toBeTruthy();
    expect(screen.getByTestId('howto-register-cmd').textContent).toContain('$BIRDMAN_DEPLOY_KEY');
  });
});

// --- screens/Deploys: интеграция — пустое состояние разворачивает карточку ---

describe('Deploys — пустое состояние показывает развёрнутую карточку-инструкцию', () => {
  it('нет версий вообще → интро-текст сохранён, карточка развёрнута без клика', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        const u = String(url);
        const body = u.includes('/v1/versions')
          ? { versions: [] }
          : u.includes('/v1/servers')
            ? { servers: [] }
            : u.includes('/v1/nodes')
              ? { nodes: [] }
              : u.includes('/v1/events')
                ? { events: [] }
                : {};
        return Promise.resolve(
          new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }),
        );
      }),
    );

    render(withSession(sess('readonly'), <Deploys navigate={() => {}} />));

    expect(await screen.findByText('No versions yet. Register a build:')).toBeTruthy();
    expect(screen.getByText('1. Build & push the server image')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Hide steps' })).toBeTruthy();
  });
});
