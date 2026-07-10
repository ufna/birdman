// Task 1 («Как залить билд»): чистые билдеры curl (lib/deployHowto.ts),
// карточка-инструкция (components/DeployHowto.tsx) и её интеграция в
// экран Деплоев (пустое состояние — развёрнута).

import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, within } from '@testing-library/react';
import type { ReactElement } from 'react';
import type { SessionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import {
  GENERIC_IMAGE_EXAMPLE,
  buildHowtoCtx,
  deployCurl,
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

afterEach(() => {
  vi.unstubAllGlobals();
});

// --- lib/deployHowto: билдеры curl (чистые функции, точные строки) ---

describe('lib/deployHowto — registerVersionCurl', () => {
  it('auth-заголовок, URL с origin, JSON-поля project/semver/image_ref/channel', () => {
    const cmd = registerVersionCurl(ctx, '1.2.3', 'prod');
    expect(cmd).toContain('curl -H "Authorization: Bearer $BIRDMAN_DEPLOY_KEY"');
    expect(cmd).toContain('http://127.0.0.1:8100/v1/versions');
    expect(cmd).toContain('"project": "ourgame"');
    expect(cmd).toContain('"semver": "1.2.3"');
    expect(cmd).toContain('"image_ref": "ghcr.io/x/y:1.0.0"');
    expect(cmd).toContain('"channel": "prod"');
  });

  it('channel staging тоже подставляется как есть', () => {
    expect(registerVersionCurl(ctx, '1.2.3', 'staging')).toContain('"channel": "staging"');
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
    expect(screen.getByText(/staging/)).toBeTruthy(); // channel staging упомянут
    expect(screen.getByRole('button', { name: 'Hide steps' }).getAttribute('aria-expanded')).toBe('true');
  });

  it('defaultExpanded — сразу развёрнута без клика', () => {
    render(withSession(sess('readonly'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));
    expect(screen.getByText('1. Build & push the server image')).toBeTruthy();
  });
});

describe('DeployHowto — ссылка на Админку по скоупу', () => {
  it('admin видит внутреннюю ссылку /access и переходит через navigate (не полный reload)', () => {
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

describe('DeployHowto — copy-кнопки пишут точную команду в clipboard', () => {
  it('копирует curl регистрации версии', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));

    const block = screen.getByTestId('howto-register-cmd');
    fireEvent.click(within(block).getByRole('button', { name: 'Copy register command' }));
    expect(writeText).toHaveBeenCalledWith(registerVersionCurl(ctx, '1.2.3', 'prod'));
    expect(await within(block).findByRole('button', { name: 'Copied' })).toBeTruthy();
  });

  it('копирует curl деплоя', () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    render(withSession(sess('admin'), <DeployHowto ctx={ctx} navigate={() => {}} defaultExpanded />));

    const block = screen.getByTestId('howto-deploy-cmd');
    fireEvent.click(within(block).getByRole('button', { name: 'Copy deploy command' }));
    expect(writeText).toHaveBeenCalledWith(deployCurl(ctx));
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
