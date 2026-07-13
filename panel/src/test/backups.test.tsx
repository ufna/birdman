import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, it, expect, vi, afterEach } from 'vitest';
import { I18nProvider } from '../lib/i18n';
import { Backups } from '../screens/Backups';

HTMLElement.prototype.scrollIntoView = () => {};

const settings = (over: Record<string, unknown> = {}) => ({
  enabled: true, interval_hours: 6, retention_local: 14,
  s3_enabled: false, s3_endpoint: '', s3_region: '', s3_bucket: '',
  s3_prefix: '', s3_access_key: '', has_s3_secret: false, retention_s3: 30,
  updated_at: '2026-07-13T10:00:00Z', ...over,
});
const run = (over: Record<string, unknown> = {}) => ({
  id: 1, started_at: '2026-07-13T06:00:00Z', finished_at: '2026-07-13T06:00:01Z',
  kind: 'scheduled', result: 'ok', size_bytes: 41984, s3_uploaded: false, error: '', ...over,
});

function backupsMock(state = { settings: settings(), runs: [run()] }) {
  return vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? 'GET';
    const json = (v: unknown, status = 200) =>
      Promise.resolve(new Response(JSON.stringify(v), { status, headers: { 'Content-Type': 'application/json' } }));
    if (url.includes('/v1/backups/settings') && method === 'GET') return json({ settings: state.settings });
    if (url.includes('/v1/backups/settings') && method === 'PATCH') {
      state.settings = { ...state.settings, ...JSON.parse(String(init?.body)) };
      return json({ settings: state.settings });
    }
    if (url.includes('/v1/backups/runs')) return json({ runs: state.runs });
    if (url.includes('/v1/backups/run')) return json({ started: true }, 202);
    return json({ error: 'not_found' }, 404);
  });
}

const renderEn = () => render(<I18nProvider initialLang="en"><Backups /></I18nProvider>);

const lastBody = (m: ReturnType<typeof vi.fn>, method: string) => {
  const call = [...m.mock.calls].reverse().find(([, init]) => (init as RequestInit)?.method === method);
  return call ? JSON.parse(String((call[1] as RequestInit).body)) : undefined;
};

afterEach(() => vi.unstubAllGlobals());

describe('Backups screen', () => {
  it('renders status and history', async () => {
    vi.stubGlobal('fetch', backupsMock());
    renderEn();
    await screen.findByText('Last backup');
    expect(screen.getByText('41.0 KiB')).toBeTruthy();
    expect(screen.getByText('History')).toBeTruthy();
  });

  it('save sends PATCH without s3_secret_key when the field is empty (keep)', async () => {
    const fetchMock = backupsMock({ settings: settings({ s3_enabled: true, s3_endpoint: 'https://s3.example.com', s3_bucket: 'b', s3_access_key: 'ak', has_s3_secret: true }), runs: [] });
    vi.stubGlobal('fetch', fetchMock);
    renderEn();
    await screen.findByLabelText('Interval (hours)');
    fireEvent.change(screen.getByLabelText('Interval (hours)'), { target: { value: '12' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(lastBody(fetchMock, 'PATCH')).toBeDefined());
    const body = lastBody(fetchMock, 'PATCH');
    expect(body.interval_hours).toBe(12);
    expect(body).not.toHaveProperty('s3_secret_key');
  });

  it('save sends s3_secret_key when filled (rotate)', async () => {
    const fetchMock = backupsMock({ settings: settings({ s3_enabled: true, s3_endpoint: 'https://s3.example.com', s3_bucket: 'b', s3_access_key: 'ak', has_s3_secret: true }), runs: [] });
    vi.stubGlobal('fetch', fetchMock);
    renderEn();
    await screen.findByLabelText('Secret key');
    fireEvent.change(screen.getByLabelText('Secret key'), { target: { value: 'new-secret' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(lastBody(fetchMock, 'PATCH')).toBeDefined());
    expect(lastBody(fetchMock, 'PATCH').s3_secret_key).toBe('new-secret');
  });

  it('Run now POSTs /v1/backups/run', async () => {
    const fetchMock = backupsMock();
    vi.stubGlobal('fetch', fetchMock);
    renderEn();
    await screen.findByText('Last backup');
    fireEvent.click(screen.getByRole('button', { name: 'Run now' }));
    await waitFor(() => {
      const posted = fetchMock.mock.calls.some(([u, i]) =>
        String(u).endsWith('/v1/backups/run') && (i as RequestInit)?.method === 'POST');
      expect(posted).toBe(true);
    });
  });
});
