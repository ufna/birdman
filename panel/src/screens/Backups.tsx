// «Админка» (admin-only): вкладка «Бекапы» (Backups v1, docs/superpowers/specs/
// 2026-07-13-backups-admin-v1-design.md). Дампы Postgres силами master:
// статус последнего прогона, форма настроек (расписание + локальный ретеншн +
// S3-оффсайт с write-only секретом), история прогонов и ручной run-now.
// Данные — useAsync; страховочный 30-с поллинг открытой вкладки крутит ТОЛЬКО
// прогоны (SSE есть только у падений) — settings не поллятся, чтобы фоновая
// перезагрузка не затирала несохранённые правки (ревью Task 5). Форма живёт
// от baseline (последняя подтверждённая сервером версия: GET при загрузке,
// ответ PATCH после save). PATCH несёт ТОЛЬКО изменённые поля (диф против
// baseline); s3_secret_key шлём лишь при ротации (пустое поле = keep).

import { useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import { api, ApiError } from '../lib/api';
import type { BackupSettings, BackupSettingsPatch, BackupRun } from '../lib/api';
import { useAsync } from '../lib/useAsync';
import { useT, useFormat } from '../lib/i18n';
import type { MessageKey } from '../lib/i18n';
import { formatBytes } from '../lib/metrics';
import { useToast } from '../components/Toast';
import { DataTable } from '../components/DataTable';
import { StateBadge } from '../components/Badge';
import type { Tone } from '../components/Badge';
import { Card, CardHeader, ErrorNote, LoadingRow } from '../components/ui';

const inputClass = 'rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal placeholder:text-muted';

/** Тон результата прогона: ok → good, error → dead, running → warn. */
function resultTone(result: BackupRun['result']): Tone {
  switch (result) {
    case 'ok':
      return 'good';
    case 'error':
      return 'dead';
    default:
      return 'warn';
  }
}

export function Backups() {
  const { t } = useT();
  const fmt = useFormat();
  const toast = useToast();
  const settings = useAsync(() => api.getBackupSettings(), []);
  const runs = useAsync(() => api.listBackupRuns(50), []);

  // Страховочный поллинг открытой вкладки: ТОЛЬКО прогоны раз в 30 с (SSE-
  // событие есть лишь у падений — успехи иначе не приедут). settings намеренно
  // НЕ поллим: их меняет только эта форма, а фоновое перечитывание затирало бы
  // несохранённые правки и набранный секрет (ревью Task 5, Important);
  // baseline после save обновляется ответом PATCH.
  useEffect(() => {
    const id = setInterval(() => {
      runs.reload();
    }, 30_000);
    return () => {
      clearInterval(id);
    };
  }, [runs.reload]);

  // Отложенный ре-фетч истории после run-now: id в ref, чистим на unmount.
  const runReloadTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (runReloadTimer.current !== null) clearTimeout(runReloadTimer.current);
    },
    [],
  );

  // ── форма: локальный стейт + baseline — последняя подтверждённая сервером
  // версия настроек (GET при первой загрузке, ответ PATCH после save).
  // Гидрация ТОЛЬКО пока baseline не установлен: фоновые перезагрузки данных
  // форму не перезаписывают.
  const [baseline, setBaseline] = useState<BackupSettings | null>(null);
  const [form, setForm] = useState<BackupSettings | null>(null);
  const [secret, setSecret] = useState(''); // write-only поле, всегда отдельно
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (settings.data && baseline === null) {
      setBaseline(settings.data);
      setForm(settings.data);
      setSecret('');
    }
  }, [settings.data, baseline]);

  const dirty = useMemo(() => {
    if (!form || !baseline) return false;
    if (secret !== '') return true;
    return JSON.stringify(form) !== JSON.stringify(baseline);
  }, [form, baseline, secret]);

  // Guard чисел: Number('')===0 и NaN не должны уезжать в PATCH — Save просто
  // disabled (серверная валидация остаётся второй линией).
  const formValid =
    form !== null &&
    [form.interval_hours, form.retention_local, form.retention_s3].every((n) => Number.isFinite(n) && n >= 1);

  const lastOk = runs.data?.find((r) => r.result === 'ok');
  const lastS3 = runs.data?.find((r) => r.result === 'ok' && r.s3_uploaded);
  const lastRun = runs.data?.[0];

  const update = (patch: Partial<BackupSettings>) => {
    setForm((f) => (f ? { ...f, ...patch } : f));
  };

  function save() {
    if (!form || !baseline || !formValid) return;
    const cur = baseline;
    // диф против подтверждённого сервером — PATCH несёт только изменённые поля
    const body: BackupSettingsPatch = {};
    if (form.enabled !== cur.enabled) body.enabled = form.enabled;
    if (form.interval_hours !== cur.interval_hours) body.interval_hours = form.interval_hours;
    if (form.retention_local !== cur.retention_local) body.retention_local = form.retention_local;
    if (form.s3_enabled !== cur.s3_enabled) body.s3_enabled = form.s3_enabled;
    if (form.s3_endpoint !== cur.s3_endpoint) body.s3_endpoint = form.s3_endpoint.trim();
    if (form.s3_region !== cur.s3_region) body.s3_region = form.s3_region.trim();
    if (form.s3_bucket !== cur.s3_bucket) body.s3_bucket = form.s3_bucket.trim();
    if (form.s3_prefix !== cur.s3_prefix) body.s3_prefix = form.s3_prefix.trim();
    if (form.s3_access_key !== cur.s3_access_key) body.s3_access_key = form.s3_access_key.trim();
    if (form.retention_s3 !== cur.retention_s3) body.retention_s3 = form.retention_s3;
    if (secret.trim() !== '') body.s3_secret_key = secret.trim(); // ротация только когда введён
    api
      .patchBackupSettings(body)
      .then((saved) => {
        // Ответ PATCH — новая подтверждённая версия: baseline и форма сразу из
        // него, без отката к старым значениям и без лишнего GET (ревью Task 5).
        setError(null);
        setBaseline(saved);
        setForm(saved);
        setSecret('');
        toast.success(t('backups.toast.saved'));
      })
      .catch((e: ApiError) => {
        setError(e.detail ?? e.code);
      });
  }

  function runNow() {
    api
      .runBackupNow()
      .then(() => {
        toast.success(t('backups.toast.runStarted'));
        // Прогон асинхронный — статус/история подтянутся поллингом; лёгкий
        // ре-фетч чуть раньше, чтобы 'running' проявилось быстрее.
        if (runReloadTimer.current !== null) clearTimeout(runReloadTimer.current);
        runReloadTimer.current = setTimeout(() => {
          runs.reload();
        }, 2000);
      })
      .catch((e: ApiError) => {
        if (e.status === 409) toast.error(t('backups.toast.runBusy'));
        else toast.error(e.detail ?? e.code);
      });
  }

  function testS3() {
    api
      .testBackupS3()
      .then(() => {
        toast.success(t('backups.toast.s3ok'));
      })
      .catch((e: ApiError) => {
        toast.error(e.detail ?? e.code);
      });
  }

  // Размер — деталью «Последнего бекапа» (а не отдельным значением): в истории
  // тот же размер уже показан колонкой, дублировать одиночным числом ни к чему.
  const sizeDetail =
    lastOk && lastOk.size_bytes != null
      ? `${t('backups.status.size')}: ${formatBytes(lastOk.size_bytes)}`
      : undefined;

  const nextScheduled = () => {
    const s = baseline;
    if (!s || !s.enabled) return t('backups.status.nextDisabled');
    const base = lastOk?.started_at ?? new Date().toISOString();
    return fmt.stamp(new Date(new Date(base).getTime() + s.interval_hours * 3600e3).toISOString());
  };

  // «Проверить соединение» тестирует СОХРАНЁННУЮ конфигурацию — недоступна при
  // грязной форме (сначала сохрани) и когда секрет ещё не задан. baseline —
  // и есть последняя сохранённая версия (после rotate сразу актуален).
  const canTest = baseline?.has_s3_secret === true && !dirty;

  return (
    <div className="flex flex-col gap-4">
      <header>
        <h1 className="text-lg font-semibold">{t('backups.title')}</h1>
        <p className="mt-0.5 text-sm text-muted">{t('backups.subtitle')}</p>
      </header>

      {/* Статус + ручной прогон */}
      <Card>
        <CardHeader
          title={t('backups.status.title')}
          aside={
            <button
              type="button"
              onClick={runNow}
              disabled={lastRun?.result === 'running'}
              className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
            >
              {t('backups.runNow')}
            </button>
          }
        />
        {runs.error !== undefined && runs.data === undefined ? (
          <div className="p-4">
            <ErrorNote error={runs.error} retry={runs.reload} />
          </div>
        ) : runs.data === undefined ? (
          <LoadingRow />
        ) : (
          <>
            <div className="grid gap-4 p-4 sm:grid-cols-3">
              <Stat
                label={t('backups.status.last')}
                value={lastOk ? fmt.stamp(lastOk.started_at) : t('backups.status.lastNever')}
                detail={sizeDetail}
              />
              <Stat label={t('backups.status.s3Last')} value={lastS3 ? fmt.stamp(lastS3.started_at) : '—'} />
              <Stat label={t('backups.status.next')} value={nextScheduled()} />
            </div>
            {lastRun?.result === 'error' && (
              <div role="alert" className="border-t border-line bg-dead-bg px-4 py-2.5 text-sm text-dead">
                <span className="font-medium">{t('backups.status.lastError')}</span>
                {lastRun.error !== '' && <span className="font-mono"> — {lastRun.error}</span>}
              </div>
            )}
          </>
        )}
      </Card>

      {/* Настройки */}
      <Card>
        <CardHeader title={t('backups.form.title')} />
        {settings.error !== undefined && settings.data === undefined ? (
          <div className="p-4">
            <ErrorNote error={settings.error} retry={settings.reload} />
          </div>
        ) : form === null ? (
          <LoadingRow />
        ) : (
          <div className="flex flex-col gap-4 p-4">
            <label className="flex items-center gap-2 text-sm font-medium">
              <input type="checkbox" checked={form.enabled} onChange={(e) => update({ enabled: e.target.checked })} />
              {t('backups.form.enabled')}
            </label>
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="flex flex-col gap-1">
                <label htmlFor="bk-interval" className="text-sm font-medium">
                  {t('backups.form.interval')}
                </label>
                <input
                  id="bk-interval"
                  type="number"
                  min={1}
                  value={form.interval_hours}
                  onChange={(e) => update({ interval_hours: Number(e.target.value) })}
                  className={inputClass}
                />
              </div>
              <div className="flex flex-col gap-1">
                <label htmlFor="bk-retention-local" className="text-sm font-medium">
                  {t('backups.form.retentionLocal')}
                </label>
                <input
                  id="bk-retention-local"
                  type="number"
                  min={1}
                  value={form.retention_local}
                  onChange={(e) => update({ retention_local: Number(e.target.value) })}
                  className={inputClass}
                />
              </div>
            </div>
            <p className="text-xs text-muted">{t('backups.form.retentionHint')}</p>

            {/* S3-оффсайт */}
            <div className="mt-1 flex flex-col gap-3 border-t border-line pt-4">
              <label className="flex items-center gap-2 text-sm font-medium">
                <input type="checkbox" checked={form.s3_enabled} onChange={(e) => update({ s3_enabled: e.target.checked })} />
                {t('backups.form.s3.enabled')}
              </label>
              <div className="flex flex-col gap-1">
                <label htmlFor="bk-s3-endpoint" className="text-sm font-medium">
                  {t('backups.form.s3.endpoint')}
                </label>
                <input
                  id="bk-s3-endpoint"
                  value={form.s3_endpoint}
                  onChange={(e) => update({ s3_endpoint: e.target.value })}
                  className={inputClass}
                />
                <p className="text-xs text-muted">{t('backups.form.s3.endpointHint')}</p>
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                <div className="flex flex-col gap-1">
                  <label htmlFor="bk-s3-region" className="text-sm font-medium">
                    {t('backups.form.s3.region')}
                  </label>
                  <input id="bk-s3-region" value={form.s3_region} onChange={(e) => update({ s3_region: e.target.value })} className={inputClass} />
                </div>
                <div className="flex flex-col gap-1">
                  <label htmlFor="bk-s3-bucket" className="text-sm font-medium">
                    {t('backups.form.s3.bucket')}
                  </label>
                  <input id="bk-s3-bucket" value={form.s3_bucket} onChange={(e) => update({ s3_bucket: e.target.value })} className={inputClass} />
                </div>
                <div className="flex flex-col gap-1">
                  <label htmlFor="bk-s3-prefix" className="text-sm font-medium">
                    {t('backups.form.s3.prefix')}
                  </label>
                  <input id="bk-s3-prefix" value={form.s3_prefix} onChange={(e) => update({ s3_prefix: e.target.value })} className={inputClass} />
                </div>
                <div className="flex flex-col gap-1">
                  <label htmlFor="bk-s3-access" className="text-sm font-medium">
                    {t('backups.form.s3.accessKey')}
                  </label>
                  <input id="bk-s3-access" value={form.s3_access_key} onChange={(e) => update({ s3_access_key: e.target.value })} className={inputClass} />
                </div>
                <div className="flex flex-col gap-1">
                  <label htmlFor="bk-s3-secret" className="text-sm font-medium">
                    {t('backups.form.s3.secretKey')}
                  </label>
                  <input
                    id="bk-s3-secret"
                    type="password"
                    value={secret}
                    onChange={(e) => setSecret(e.target.value)}
                    placeholder={form.has_s3_secret ? t('backups.form.s3.secretKeyKeep') : ''}
                    className={inputClass}
                  />
                </div>
                <div className="flex flex-col gap-1">
                  <label htmlFor="bk-s3-retention" className="text-sm font-medium">
                    {t('backups.form.s3.retention')}
                  </label>
                  <input
                    id="bk-s3-retention"
                    type="number"
                    min={1}
                    value={form.retention_s3}
                    onChange={(e) => update({ retention_s3: Number(e.target.value) })}
                    className={inputClass}
                  />
                </div>
              </div>
            </div>

            {error !== null && (
              <p role="alert" className="rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
                {error}
              </p>
            )}

            <div className="flex flex-wrap justify-end gap-2">
              <button
                type="button"
                onClick={testS3}
                disabled={!canTest}
                title={t('backups.form.s3.testHint')}
                className="rounded-lg border border-line px-3 py-1.5 text-sm font-medium text-muted transition-colors hover:text-ink disabled:opacity-50"
              >
                {t('backups.form.s3.test')}
              </button>
              <button
                type="button"
                onClick={save}
                disabled={!dirty || !formValid}
                className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
              >
                {t('backups.form.save')}
              </button>
            </div>
          </div>
        )}
      </Card>

      {/* История */}
      <Card>
        <CardHeader
          title={t('backups.history.title')}
          aside={runs.data !== undefined ? <span className="tabular font-mono text-xs text-muted">{runs.data.length}</span> : undefined}
        />
        {runs.error !== undefined && runs.data === undefined ? (
          <div className="p-4">
            <ErrorNote error={runs.error} retry={runs.reload} />
          </div>
        ) : runs.data === undefined ? (
          <LoadingRow />
        ) : (
          <HistoryTable runs={runs.data} />
        )}
      </Card>
    </div>
  );
}

/** Метрика статуса: подпись + значение + опциональная деталь. */
function Stat({ label, value, detail }: { label: string; value: ReactNode; detail?: ReactNode }) {
  return (
    <div>
      <div className="text-xs font-medium tracking-wide text-muted uppercase">{label}</div>
      <div className="tabular mt-1 text-lg font-semibold">{value}</div>
      {detail !== undefined && <div className="mt-0.5 text-xs text-muted">{detail}</div>}
    </div>
  );
}

function HistoryTable({ runs }: { runs: BackupRun[] }) {
  const { t } = useT();
  const fmt = useFormat();
  const columns = useMemo<ColumnDef<BackupRun, unknown>[]>(
    () => [
      {
        id: 'when',
        header: t('backups.history.col.when'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{fmt.stamp(row.original.started_at)}</span>,
      },
      {
        id: 'kind',
        header: t('backups.history.col.kind'),
        cell: ({ row }) => <span className="text-sm">{t(`backups.history.kind.${row.original.kind}` as MessageKey)}</span>,
      },
      {
        id: 'result',
        header: t('backups.history.col.result'),
        cell: ({ row }) => (
          <StateBadge state={t(`backups.history.result.${row.original.result}` as MessageKey)} tone={resultTone(row.original.result)} />
        ),
      },
      {
        id: 'size',
        header: t('backups.history.col.size'),
        cell: ({ row }) => (
          <span className="tabular font-mono text-xs text-muted">{row.original.size_bytes != null ? formatBytes(row.original.size_bytes) : '—'}</span>
        ),
      },
      {
        id: 's3',
        header: t('backups.history.col.s3'),
        cell: ({ row }) => <span className="text-sm">{row.original.s3_uploaded ? '✓' : '—'}</span>,
      },
      {
        id: 'error',
        header: t('backups.history.col.error'),
        cell: ({ row }) =>
          row.original.error !== '' ? <span className="font-mono text-xs text-dead">{row.original.error}</span> : <span className="text-muted">—</span>,
      },
    ],
    [t, fmt],
  );
  return <DataTable columns={columns} data={runs} rowId={(r) => String(r.id)} empty={t('backups.history.empty')} />;
}
