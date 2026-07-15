import { render, screen, fireEvent, waitFor, act, within } from '@testing-library/react';
import { describe, it, expect, vi, afterEach } from 'vitest';
import { I18nProvider } from '../lib/i18n';
import { ToastProvider } from '../components/Toast';
import { Backups } from '../screens/Backups';

// Radix (AlertDialog) дёргает эти API при фокусе — в jsdom их нет.
HTMLElement.prototype.scrollIntoView = () => {};
if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false;

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
      // Фиделити реального store (пункт (16) леджер-триажа): s3_secret_key —
      // write-only, НИКОГДА не эхается в ответ; он лишь флипает has_s3_secret.
      const body = JSON.parse(String(init?.body));
      const { s3_secret_key, ...rest } = body;
      state.settings = { ...state.settings, ...rest };
      if (s3_secret_key !== undefined) state.settings.has_s3_secret = s3_secret_key !== '';
      return json({ settings: state.settings });
    }
    if (url.includes('/v1/backups/runs')) return json({ runs: state.runs });
    if (url.includes('/v1/backups/run')) return json({ started: true }, 202);
    return json({ error: 'not_found' }, 404);
  });
}

const renderEn = () => render(<I18nProvider initialLang="en"><Backups /></I18nProvider>);
// С провайдером тостов — для тестов, где нужен видимый success-тост (иначе
// useToast падает в no-op фолбэк и тост в DOM не появляется).
const renderEnToast = () =>
  render(
    <I18nProvider initialLang="en">
      <ToastProvider>
        <Backups />
      </ToastProvider>
    </I18nProvider>,
  );

const lastBody = (m: ReturnType<typeof vi.fn>, method: string) => {
  const call = [...m.mock.calls].reverse().find(([, init]) => (init as RequestInit)?.method === method);
  return call ? JSON.parse(String((call[1] as RequestInit).body)) : undefined;
};

afterEach(() => {
  vi.useRealTimers(); // no-op в тестах без fake timers; страховка при падении внутри
  vi.unstubAllGlobals();
});

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
    // PATCH несёт ТОЛЬКО изменённые поля — нетронутые в форме не отправляются.
    expect(body).not.toHaveProperty('enabled');
    expect(body).not.toHaveProperty('retention_local');
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
    // enabled=true → прямой POST без подтверждающего диалога.
    expect(screen.queryByRole('alertdialog')).toBeNull();
  });

  // Important из ревью Task 5: 30-с поллинг НЕ должен затирать несохранённые
  // правки формы. Поллятся только прогоны (runs); settings перечитываются лишь
  // при первой загрузке — форма живёт от baseline и ответа PATCH.
  it('30s polling refetches runs only and keeps unsaved form edits', async () => {
    // Fake timers ДО render: интервал компонента должен встать на фейковый clock.
    vi.useFakeTimers({ toFake: ['setTimeout', 'setInterval', 'clearTimeout', 'clearInterval'] });
    const fetchMock = backupsMock();
    vi.stubGlobal('fetch', fetchMock);
    renderEn();
    // Первичная загрузка: цепочка fetch→text→setState — микротаски (не таймеры);
    // осаждаем вручную вместо findBy/waitFor (те завязаны на real timers).
    await act(async () => {
      for (let i = 0; i < 20; i++) await Promise.resolve();
    });
    fireEvent.change(screen.getByLabelText('Interval (hours)'), { target: { value: '12' } });

    const count = (part: string) => fetchMock.mock.calls.filter(([u]) => String(u).includes(part)).length;
    const settingsBefore = count('/v1/backups/settings');
    const runsBefore = count('/v1/backups/runs');

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });

    expect(count('/v1/backups/runs')).toBeGreaterThan(runsBefore); // поллинг живой
    expect(count('/v1/backups/settings')).toBe(settingsBefore); // settings НЕ поллятся
    // Несохранённая правка цела — фоновое обновление не перегидрировало форму.
    expect((screen.getByLabelText('Interval (hours)') as HTMLInputElement).value).toBe('12');
  });

  // Гейт run-now: грязная форма блокирует ручной прогон (он идёт по СОХРАНЁННЫМ
  // настройкам) с title-подсказкой; после Save baseline обновлён → кнопка снова
  // активна и title исчезает.
  it('Run now is gated while the form is dirty (title hint), re-enables after save', async () => {
    const fetchMock = backupsMock(); // enabled=true, последний прогон ok
    vi.stubGlobal('fetch', fetchMock);
    renderEn();
    await screen.findByLabelText('Interval (hours)');

    fireEvent.change(screen.getByLabelText('Interval (hours)'), { target: { value: '12' } });
    const gated = screen.getByRole('button', { name: 'Run now' });
    expect((gated as HTMLButtonElement).disabled).toBe(true);
    expect(gated.getAttribute('title')).toBe('Save or revert your changes first — a manual run uses the saved settings');

    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(lastBody(fetchMock, 'PATCH')).toBeDefined());
    await waitFor(() => {
      expect((screen.getByRole('button', { name: 'Run now' }) as HTMLButtonElement).disabled).toBe(false);
    });
    expect(screen.getByRole('button', { name: 'Run now' }).getAttribute('title')).toBeNull();
  });

  // Гейт run-now: идущий прогон блокирует кнопку с причиной (reuse runBusy).
  it('Run now is gated with a reason while a run is in progress', async () => {
    const fetchMock = backupsMock({ settings: settings(), runs: [run({ result: 'running', finished_at: null, size_bytes: null })] });
    vi.stubGlobal('fetch', fetchMock);
    renderEn();
    await screen.findByText('Last backup');
    const gated = screen.getByRole('button', { name: 'Run now' });
    expect((gated as HTMLButtonElement).disabled).toBe(true);
    expect(gated.getAttribute('title')).toBe('A backup run is already in progress');
  });

  // Плановые бекапы выключены → run-now идёт через подтверждающий диалог: клик по
  // триггеру не постит сразу; Cancel не постит; confirm постит и даёт success-тост.
  it('Run now confirms via dialog when scheduled backups are disabled', async () => {
    const fetchMock = backupsMock({ settings: settings({ enabled: false }), runs: [run()] });
    vi.stubGlobal('fetch', fetchMock);
    renderEnToast();
    await screen.findByText('Last backup');

    const posted = () =>
      fetchMock.mock.calls.some(([u, i]) => String(u).endsWith('/v1/backups/run') && (i as RequestInit)?.method === 'POST');

    fireEvent.click(screen.getByRole('button', { name: 'Run now' })); // триггер
    const dialog = await screen.findByRole('alertdialog');
    expect(within(dialog).getByText('Backups are disabled')).toBeTruthy();
    expect(posted()).toBe(false);

    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull());
    expect(posted()).toBe(false);

    fireEvent.click(screen.getByRole('button', { name: 'Run now' })); // триггер снова
    const dialog2 = await screen.findByRole('alertdialog');
    fireEvent.click(within(dialog2).getByRole('button', { name: 'Run now' })); // подтверждение
    await waitFor(() => expect(posted()).toBe(true));
    await screen.findByText('Backup started');
  });

  // Фиделити (16): ответ PATCH флипает has_s3_secret по факту ротации, но самого
  // секрета НИКОГДА не эхает — placeholder Secret key переходит в keep-режим, а
  // последующий Save другого поля не тащит s3_secret_key.
  it('rotating the S3 secret flips has_s3_secret via the PATCH response; secret is never echoed', async () => {
    const fetchMock = backupsMock({
      settings: settings({ s3_enabled: true, s3_endpoint: 'https://s3.example.com', s3_bucket: 'b', s3_access_key: 'ak', has_s3_secret: false }),
      runs: [],
    });
    vi.stubGlobal('fetch', fetchMock);
    renderEn();

    const secretInput = await screen.findByLabelText('Secret key');
    expect((secretInput as HTMLInputElement).placeholder).toBe(''); // has_s3_secret=false → пусто

    fireEvent.change(secretInput, { target: { value: 'sekret' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(lastBody(fetchMock, 'PATCH')).toBeDefined());

    await waitFor(() => {
      expect((screen.getByLabelText('Secret key') as HTMLInputElement).placeholder).toBe('Leave empty to keep the current secret');
    });

    fireEvent.change(screen.getByLabelText('Interval (hours)'), { target: { value: '9' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(lastBody(fetchMock, 'PATCH')?.interval_hours).toBe(9));
    expect(lastBody(fetchMock, 'PATCH')).not.toHaveProperty('s3_secret_key');
  });
});
