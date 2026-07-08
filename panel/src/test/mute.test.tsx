import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ReactElement } from 'react';
import { ApiError } from '../lib/api';
import type { ActiveAlert, AlertMute, SessionInfo } from '../lib/api';
import {
  MUTE_PRESETS,
  countCriticalAlerts,
  muteErrorMessage,
  normalizeMuteRegion,
  presetExpiry,
} from '../lib/alerts';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import { ThemeProvider } from '../lib/theme';
import { Alerts } from '../screens/Alerts';
import { Shell } from '../components/Shell';

// Radix (Dialog/AlertDialog) в jsdom дёргает эти API при фокусе.
HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

const sess = (...scopes: SessionInfo['scopes']): SessionInfo => ({ scopes, name: 'k' });

function withSession(session: SessionInfo, ui: ReactElement): ReactElement {
  return (
    <SessionContext.Provider value={{ session, login: async () => {}, logout: async () => {}, invalidate: () => {} }}>
      <I18nProvider initialLang="en">{ui}</I18nProvider>
    </SessionContext.Provider>
  );
}

const activeCrit = (over: Partial<ActiveAlert> = {}): ActiveAlert => ({
  name: 'NodeDown',
  severity: 'critical',
  region: 'dev',
  node: 'n1',
  state: 'firing',
  active_at: '2026-07-08T09:40:55Z',
  value: '',
  description: 'node silent',
  muted: false,
  ...over,
});

function alertsFetch(data: { active?: ActiveAlert[]; rules?: unknown[]; history?: unknown[]; mutes?: AlertMute[] }) {
  const { active = [], rules = [], history = [], mutes = [] } = data;
  return vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    let status = 200;
    let body: unknown = {};
    if (url.includes('/alerts/active')) body = { alerts: active };
    else if (url.includes('/alerts/rules')) body = { rules };
    else if (url.includes('/alerts/history')) body = { alerts: history };
    else if (url.includes('/alerts/mutes')) {
      if (method === 'POST') {
        status = 201;
        body = { mute: { id: 'new', alertname: 'NodeDown', region: 'dev', note: '', created_at: '', expires_at: null, created_by: 'k' } };
      } else if (method === 'DELETE') {
        return Promise.resolve(new Response(null, { status: 204 }));
      } else body = { mutes };
    }
    return Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('mute вью-модель (чистые функции)', () => {
  it('presetExpiry: относительные окна + forever', () => {
    const now = Date.parse('2026-07-08T00:00:00.000Z');
    expect(presetExpiry('1h', now)).toBe('2026-07-08T01:00:00.000Z');
    expect(presetExpiry('8h', now)).toBe('2026-07-08T08:00:00.000Z');
    expect(presetExpiry('24h', now)).toBe('2026-07-09T00:00:00.000Z');
    expect(presetExpiry('forever', now)).toBeUndefined();
    expect(MUTE_PRESETS).toEqual(['1h', '8h', '24h', 'forever']);
  });
  it('countCriticalAlerts: только не-muted critical', () => {
    expect(countCriticalAlerts(undefined)).toBe(0);
    expect(countCriticalAlerts([])).toBe(0);
    expect(countCriticalAlerts([activeCrit(), activeCrit({ muted: true }), activeCrit({ severity: 'warning' })])).toBe(1);
  });
  it('normalizeMuteRegion: пусто/пробелы → undefined', () => {
    expect(normalizeMuteRegion(undefined)).toBeUndefined();
    expect(normalizeMuteRegion('')).toBeUndefined();
    expect(normalizeMuteRegion('  ')).toBeUndefined();
    expect(normalizeMuteRegion('dev')).toBe('dev');
  });
  it('muteErrorMessage: статусы → локализованные ключи', () => {
    const t = ((k: string) => k) as Parameters<typeof muteErrorMessage>[1];
    expect(muteErrorMessage(new ApiError(400, 'bad_request'), t)).toBe('alerts.mute.err.bad');
    expect(muteErrorMessage(new ApiError(404, 'not_found'), t)).toBe('alerts.mute.err.gone');
    expect(muteErrorMessage(new ApiError(409, 'conflict'), t)).toBe('alerts.mute.err.conflict');
    expect(muteErrorMessage(new ApiError(403, 'forbidden'), t)).toBe('confirm.err.forbidden');
    expect(muteErrorMessage(new Error('x'), t)).toBe('confirm.err.generic');
  });
});

describe('Alerts — scope-гейт кнопок mute и приглушение muted', () => {
  it('admin видит «Mute» на активном алерте', async () => {
    vi.stubGlobal('fetch', alertsFetch({ active: [activeCrit()] }));
    render(withSession(sess('admin'), <Alerts />));
    await screen.findByText('NodeDown');
    expect(screen.getAllByRole('button', { name: 'Mute' }).length).toBeGreaterThan(0);
  });
  it('readonly НЕ видит «Mute»', async () => {
    vi.stubGlobal('fetch', alertsFetch({ active: [activeCrit()] }));
    render(withSession(sess('readonly'), <Alerts />));
    await screen.findByText('NodeDown');
    expect(screen.queryByRole('button', { name: 'Mute' })).toBeNull();
  });
  it('muted-алерт: бейдж «muted» и без кнопки «Mute»', async () => {
    vi.stubGlobal('fetch', alertsFetch({ active: [activeCrit({ muted: true })] }));
    render(withSession(sess('admin'), <Alerts />));
    await screen.findByText('NodeDown');
    expect(screen.getByText('muted')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Mute' })).toBeNull();
  });
});

describe('Alerts — секция «Заглушённые»', () => {
  const mute: AlertMute = {
    id: 'm1',
    alertname: 'CrashLoop',
    region: null,
    note: 'flapping node',
    created_at: '2026-07-08T09:00:00Z',
    expires_at: null,
    created_by: 'ops',
  };
  it('список mute + «all regions» + заметка; admin видит «Unmute»', async () => {
    vi.stubGlobal('fetch', alertsFetch({ mutes: [mute] }));
    render(withSession(sess('admin'), <Alerts />));
    expect(await screen.findByText('CrashLoop')).toBeTruthy();
    expect(screen.getByText('all regions')).toBeTruthy();
    expect(screen.getByText('flapping node')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Unmute' })).toBeTruthy();
  });
  it('readonly видит список, но не «Unmute»', async () => {
    vi.stubGlobal('fetch', alertsFetch({ mutes: [mute] }));
    render(withSession(sess('readonly'), <Alerts />));
    expect(await screen.findByText('CrashLoop')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Unmute' })).toBeNull();
  });
});

describe('Alerts — постановка mute (диалог → POST) и возврат фокуса', () => {
  it('admin: Mute → диалог → подтверждение шлёт POST /v1/alerts/mutes', async () => {
    const fetchMock = alertsFetch({ active: [activeCrit()] });
    vi.stubGlobal('fetch', fetchMock);
    render(withSession(sess('admin'), <Alerts />));
    await screen.findByText('NodeDown');
    fireEvent.click(screen.getAllByRole('button', { name: 'Mute' })[0]);
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Mute NodeDown')).toBeTruthy();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Mute' }));
    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith('/v1/alerts/mutes', expect.objectContaining({ method: 'POST' }));
    });
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });
  });

  it('фокус возвращается на триггер при закрытии диалога (Cancel)', async () => {
    vi.stubGlobal('fetch', alertsFetch({ active: [activeCrit()] }));
    render(withSession(sess('admin'), <Alerts />));
    await screen.findByText('NodeDown');
    const trigger = screen.getAllByRole('button', { name: 'Mute' })[0];
    fireEvent.click(trigger);
    const dialog = await screen.findByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });
    await waitFor(() => {
      expect(document.activeElement).toBe(trigger);
    });
  });
});

describe('Shell — бейдж активных critical-алертов', () => {
  function renderShell(active: ActiveAlert[]) {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        const body = url.includes('/alerts/active') ? { alerts: active } : {};
        return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }));
      }),
    );
    return render(
      <ThemeProvider>
        <SessionContext.Provider value={{ session: sess('readonly'), login: async () => {}, logout: async () => {}, invalidate: () => {} }}>
          <I18nProvider initialLang="en">
            <Shell path="/" navigate={() => {}}>
              <div />
            </Shell>
          </I18nProvider>
        </SessionContext.Provider>
      </ThemeProvider>,
    );
  }
  it('показывает число не-muted critical', async () => {
    renderShell([activeCrit(), activeCrit({ muted: true }), activeCrit({ severity: 'warning' })]);
    expect(await screen.findByLabelText('1 active critical alerts')).toBeTruthy();
  });
  it('нет critical → нет бейджа', async () => {
    renderShell([activeCrit({ severity: 'warning' })]);
    await screen.findAllByText('Alerts');
    expect(screen.queryByLabelText(/active critical alerts/)).toBeNull();
  });
});
