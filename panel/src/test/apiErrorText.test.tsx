// Проза мастера не доезжает до DOM панели (tracker #1005).
//
// До этой карточки восемнадцать мест писали `e.detail ?? e.code` прямо в
// состояние: привязанный ключ читал в русском интерфейсе `key is bound to
// game/dev`, а на экране логина и в диалогах — `${code}: ${detail}`. Правило
// «панель 100% EN+RU» (docs/specs/panel.md §1 п.6) держится теперь ОДНОЙ
// функцией `lib/apiError.ts`, и держат её эти три группы:
//
//   1. свойство самой функции — ни на одном входе detail не вытекает;
//   2. головная поверхность карточки — PromoteDialog у привязанного ключа;
//   3. форма — 400 переводится, а не пересказывает мастера.

import { describe, expect, it, vi, afterEach } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { ApiError } from '../lib/api';
import type { Environment, SessionInfo, VersionInfo } from '../lib/api';
import { apiErrorMessage } from '../lib/apiError';
import { I18nProvider } from '../lib/i18n';
import { en } from '../lib/locales/en';
import { ru } from '../lib/locales/ru';
import { SessionContext } from '../lib/session';
import { EnvContext } from '../lib/env';
import { Deploys } from '../screens/Deploys';

HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

afterEach(() => {
  vi.unstubAllGlobals();
});

// --- 1. свойство функции: detail не вытекает ни на одном входе ---

/** Маркер, которого нет ни в одном каталоге: увидели его в ответе — значит
 *  тело мастера пролезло. Ловит и `detail` целиком, и `${code}: ${detail}`. */
const LEAK = 'ZZ-detail-must-not-leak-ZZ';

// t() поверх канонического каталога: тест про ФУНКЦИЮ, рендер ей не нужен.
const t = (key: keyof typeof en, params?: Record<string, string | number>): string => {
  const raw = (en as Record<string, string>)[key];
  return params === undefined
    ? raw
    : raw.replace(/\{(\w+)\}/g, (m, p: string) => (p in params ? String(params[p]) : m));
};

describe('apiErrorMessage: тело ответа мастера не доезжает до UI', () => {
  // Статусы, которыми мастер отвечает панели (auth.go, server.go, ops.go), плюс
  // соседние — на случай смены гейта.
  const STATUSES = [400, 401, 403, 404, 405, 409, 422, 429, 500, 502, 503, 504];
  const OPTS = [
    {},
    { refusal: 'ключ привязан к game/dev' },
    { forbidden: 'confirm.err.forbidden' as const },
    { generic: 'confirm.err.generic' as const },
    { byStatus: { 400: 'ui.err.badRequest' as const, 409: 'confirm.err.conflict' as const } },
    {
      refusal: 'ключ привязан к game/dev',
      forbidden: 'confirm.err.forbidden' as const,
      generic: 'confirm.err.generic' as const,
      byStatus: { 400: 'ui.err.badRequest' as const, 409: 'confirm.err.conflict' as const },
    },
  ];

  it('ни один (статус × набор текстов) не отдаёт detail наружу', () => {
    let checked = 0;
    for (const status of STATUSES) {
      for (const opts of OPTS) {
        const out = apiErrorMessage(new ApiError(status, 'forbidden', LEAK), t, opts);
        expect(out).not.toContain(LEAK);
        expect(out.length).toBeGreaterThan(0);
        checked += 1;
      }
    }
    expect(checked).toBe(STATUSES.length * OPTS.length);
  });

  it('detail отсутствует (undefined) — ответ всё равно осмысленный, не "undefined"', () => {
    for (const status of STATUSES) {
      const out = apiErrorMessage(new ApiError(status, 'some_code', undefined), t);
      expect(out).not.toContain('undefined');
    }
  });

  it('не-ApiError не показывает своё .message (сеть, парсер, брошенная строка)', () => {
    expect(apiErrorMessage(new Error(LEAK), t)).not.toContain(LEAK);
    expect(apiErrorMessage(new TypeError(LEAK), t)).not.toContain(LEAK);
    expect(apiErrorMessage(LEAK, t)).not.toContain(LEAK);
    expect(apiErrorMessage(undefined, t)).toBe(en['ui.err.offline']);
  });

  it('403: привязка СПЕЦИФИЧНЕЕ и своего 403-текста поверхности, и byStatus', () => {
    const refusal = 'ключ привязан к game/dev';
    // Ровно тот порядок веток, отсутствие которого — дефект #1010: поверхность
    // со своим 403 не должна отключать честный диагноз привязанному ключу.
    const out = apiErrorMessage(new ApiError(403, 'forbidden', LEAK), t, {
      refusal,
      forbidden: 'confirm.err.forbidden',
      byStatus: { 403: 'confirm.err.forbidden' },
    });
    expect(out).toBe(refusal);
  });

  it('403 без привязки → текст ДЕЙСТВИЯ, если поверхность его передала', () => {
    expect(apiErrorMessage(new ApiError(403, 'forbidden', LEAK), t, { forbidden: 'confirm.err.forbidden' })).toBe(
      en['confirm.err.forbidden'],
    );
    // Дефолт — текст ЧТЕНИЯ (про readonly), как было до #1005 у ErrorNote.
    expect(apiErrorMessage(new ApiError(403, 'forbidden', LEAK), t)).toBe(en['ui.err.forbidden']);
  });

  it('неизвестный статус → общий текст, и машинный КОД в нём сохранён', () => {
    const out = apiErrorMessage(new ApiError(500, 'internal', LEAK), t);
    expect(out).toContain('internal');
    expect(out).not.toContain(LEAK);
  });

  it('все ключи механизма есть в ОБОИХ каталогах (правило 100% EN+RU)', () => {
    for (const key of ['ui.err.expired', 'ui.err.offline', 'ui.err.code', 'ui.err.badRequest'] as const) {
      expect(en[key]).toBeTruthy();
      expect(ru[key]).toBeTruthy();
      expect(ru[key]).not.toBe(en[key]); // перевод, а не копия английского
    }
  });
});

// --- 2 и 3: живые поверхности ---

const dev: Environment = { project: 'game', name: 'dev', production: false, auto_deploy: true, retention_keep: 20, created_at: '2026-07-01T00:00:00Z' };
const prod: Environment = { project: 'game', name: 'prod', production: true, auto_deploy: false, retention_keep: 0, created_at: '2026-07-01T00:00:00Z' };

const version: VersionInfo = {
  id: 'v-act',
  project: 'game',
  semver: '1.0.0',
  image_ref: 'ghcr.io/x/game:1.0.0',
  env: 'dev',
  state: 'active',
  created_at: '2026-07-10T00:00:00Z',
};

/** Привязанный ключ с deploy: до кнопки Promote доходит штатно (Deploys.tsx:40
 *  гейтит по canDeploy), а мастер отказывает ВСЕГДА — deploy.go:164
 *  requireBinding на целевом env, а целевой тут prod. */
const BOUND_DEPLOY: SessionInfo = { scopes: ['deploy'], name: 'ci-bound', binding: { project: 'game', env: 'dev' } };
const UNBOUND_DEPLOY: SessionInfo = { scopes: ['deploy'], name: 'ci-global' };

/** Тело отказа — форма, которую отдаёт master привязанному ключу
 *  (`auth.go` fail-closed). Хвост после двоеточия несёт МАРКЕР: сама фраза
 *  «key is bound to game/dev» пересекается с английским текстом каталога
 *  («your key is bound to game/dev…»), поэтому по ней утечку не отличить. */
const BOUND_DETAIL = 'key is bound to game/dev: promote target env is not narrowable';
const BOUND_MARKER = 'not narrowable';

function promoteFetch(promoteStatus: number, promoteBody: unknown) {
  return vi.fn((url: string, init?: RequestInit) => {
    const u = String(url);
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
    if (u.includes('/v1/promote')) return json(promoteBody, promoteStatus);
    if (u.includes('/v1/versions')) return json({ versions: [version] });
    if (u.includes('/v1/servers')) return json({ servers: [] });
    if (u.includes('/v1/nodes')) return json({ nodes: [] });
    if (u.includes('/v1/events')) return json({ events: [] });
    void init;
    return json({});
  });
}

function renderDeploys(session: SessionInfo, lang: 'en' | 'ru' = 'en') {
  const envValue = {
    environments: [dev, prod],
    project: 'game',
    selected: null as string | null,
    setSelected: () => {},
    loading: false,
    reload: () => {},
  };
  return render(
    <SessionContext.Provider value={{ session, login: async () => {}, logout: async () => {}, invalidate: () => {} }}>
      <I18nProvider initialLang={lang}>
        <EnvContext.Provider value={envValue}>
          <Deploys navigate={() => {}} />
        </EnvContext.Provider>
      </I18nProvider>
    </SessionContext.Provider>,
  );
}

/** Открыть Promote-диалог и подтвердить. Подписи триггера и кнопки диалога —
 *  РАЗНЫЕ ключи каталога и в RU расходятся («Промоут» / «Промоутнуть»). */
async function promote(lang: 'en' | 'ru' = 'en') {
  const dict = lang === 'en' ? en : ru;
  fireEvent.click(await screen.findByRole('button', { name: dict['deploys.promote'] }));
  const dialog = screen.getByRole('dialog');
  fireEvent.click(within(dialog).getByRole('button', { name: dict['deploys.promote.confirm'] }));
}

describe('PromoteDialog: 403 привязанного ключа — головная поверхность #1005', () => {
  const refuse = () => promoteFetch(403, { error: 'forbidden', detail: BOUND_DETAIL });

  it('RU: диалог называет привязку своими словами, английской строки мастера нет', async () => {
    vi.stubGlobal('fetch', refuse());
    renderDeploys(BOUND_DEPLOY, 'ru');
    await promote('ru');

    await waitFor(() => {
      expect(screen.getByText(/ключ привязан к game\/dev/)).toBeTruthy();
    });
    // Именно этот текст и был дефектом карточки.
    expect(document.body.textContent).not.toContain(BOUND_MARKER);
  });

  it('EN: тот же смысл по-английски', async () => {
    vi.stubGlobal('fetch', refuse());
    renderDeploys(BOUND_DEPLOY, 'en');
    await promote();

    await waitFor(() => {
      expect(screen.getByText(/your key is bound to game\/dev/)).toBeTruthy();
    });
    expect(document.body.textContent).not.toContain(BOUND_MARKER);
  });

  it('НЕпривязанный ключ на 403 → текст ДЕЙСТВИЯ (не «нужен readonly»)', async () => {
    vi.stubGlobal('fetch', promoteFetch(403, { error: 'forbidden', detail: 'scope deploy required' }));
    renderDeploys(UNBOUND_DEPLOY, 'en');
    await promote();

    await waitFor(() => {
      expect(screen.getByText(en['confirm.err.forbidden'])).toBeTruthy();
    });
    // Ложный диагноз про чтение здесь был бы такой же ошибкой, как #1000.
    expect(document.body.textContent).not.toContain(en['ui.err.forbidden']);
    expect(document.body.textContent).not.toContain('scope deploy required');
  });

  it('400 у формы промоута → «проверьте поля», а не пересказ мастера', async () => {
    vi.stubGlobal('fetch', promoteFetch(400, { error: 'bad_request', detail: 'to_env is required' }));
    renderDeploys(UNBOUND_DEPLOY, 'en');
    await promote();

    await waitFor(() => {
      expect(screen.getByText(en['ui.err.badRequest'])).toBeTruthy();
    });
    expect(document.body.textContent).not.toContain('to_env is required');
  });

  it('прочий код (500) → общий текст с машинным кодом, тела мастера нет', async () => {
    vi.stubGlobal('fetch', promoteFetch(500, { error: 'internal', detail: 'pq: deadlock detected' }));
    renderDeploys(UNBOUND_DEPLOY, 'en');
    await promote();

    await waitFor(() => {
      expect(screen.getByText(/error internal/)).toBeTruthy();
    });
    expect(document.body.textContent).not.toContain('deadlock detected');
  });
});
