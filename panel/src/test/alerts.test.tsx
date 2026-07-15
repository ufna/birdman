import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { ApiError } from '../lib/api';
import { alertDescription, alertsUnavailable } from '../lib/alerts';
import { toneOfAlertActive, toneOfSeverity } from '../components/Badge';
import { I18nProvider } from '../lib/i18n';
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
