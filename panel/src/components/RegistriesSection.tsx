// «Админка» (П2): раздел «Реестры» — приватные container-registry credentials,
// типизированные v2 (docs/superpowers/specs/2026-07-10-registries-v2-design.md
// §4). CRUD-вью в духе Access.tsx: таблица (host/тип/username/note/updated +
// токен ВСЕГДА маской — RegistryInfo структурно не несёт токен, его неоткуда
// прочитать) и одна динамическая форма. Дропдаун «Тип» (ghcr/GAR/generic)
// переключает поля и подсказку: gar — без username (мастер форсит `_json_key`)
// и токен-textarea под SA-JSON-ключ; ghcr/generic — username + однострочный
// password-токен. Add шлёт POST (upsert по host), карандаш-edit — PATCH по id
// (host read-only; пустой токен → секрет цел, иначе ротация).

import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import * as Dialog from '@radix-ui/react-dialog';
import { api, ApiError } from '../lib/api';
import type { RegistryInfo, RegistryInput, RegistryPatch, RegistryType } from '../lib/api';
import { useAsync } from '../lib/useAsync';
import { useT, useFormat } from '../lib/i18n';
import type { MessageKey } from '../lib/i18n';
import { useToast } from './Toast';
import { DataTable } from './DataTable';
import { ConfirmButton } from './ConfirmDialog';
import { Card, CardHeader, ErrorNote, LoadingRow } from './ui';

const REGISTRY_TYPES: RegistryType[] = ['ghcr', 'gar', 'generic'];

export function RegistriesSection() {
  const { t } = useT();
  const regs = useAsync(() => api.listRegistries(), []);

  return (
    <Card>
      <CardHeader
        title={t('access.registries')}
        aside={
          <div className="flex items-center gap-3">
            {regs.data !== undefined && (
              <span className="tabular font-mono text-xs text-muted">{regs.data.length}</span>
            )}
            <RegistryFormDialog
              mode="add"
              onSaved={regs.reload}
              trigger={
                <button type="button" className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90">
                  {t('access.registries.add')}
                </button>
              }
            />
          </div>
        }
      />
      <p className="border-b border-line px-4 py-2.5 text-xs text-muted">{t('access.registries.hint')}</p>
      {regs.error !== undefined && regs.data === undefined ? (
        <div className="p-4">
          <ErrorNote error={regs.error} retry={regs.reload} />
        </div>
      ) : regs.data === undefined ? (
        <LoadingRow />
      ) : (
        <RegistriesTable registries={regs.data} reload={regs.reload} />
      )}
    </Card>
  );
}

/** Подпись типа реестра (label дропдауна, не сырой код). */
function RegistryTypeLabel({ type }: { type: RegistryType }) {
  const { t } = useT();
  return (
    <span className="inline-flex items-center rounded border border-line px-1.5 py-0.5 text-[11px] font-medium text-muted">
      {t(`access.registries.type.${type}` as MessageKey)}
    </span>
  );
}

function RegistriesTable({ registries, reload }: { registries: RegistryInfo[]; reload: () => void }) {
  const { t } = useT();
  const fmt = useFormat();
  const columns = useMemo<ColumnDef<RegistryInfo, unknown>[]>(
    () => [
      {
        id: 'host',
        header: t('access.registries.col.host'),
        cell: ({ row }) => <span className="font-mono text-sm">{row.original.host}</span>,
      },
      {
        id: 'type',
        header: t('access.registries.col.type'),
        cell: ({ row }) => <RegistryTypeLabel type={row.original.type} />,
      },
      {
        id: 'username',
        header: t('access.registries.col.username'),
        cell: ({ row }) => <span className="text-sm">{row.original.username}</span>,
      },
      {
        id: 'token',
        header: t('access.registries.col.token'),
        // Всегда маска — RegistryInfo структурно не несёт токен (write-only API).
        cell: () => <span className="font-mono text-sm text-muted">{t('access.registries.tokenMasked')}</span>,
      },
      {
        id: 'note',
        header: t('access.registries.col.note'),
        cell: ({ row }) => <span className="text-sm text-muted">{row.original.note}</span>,
      },
      {
        id: 'updated',
        header: t('access.registries.col.updated'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{fmt.stamp(row.original.updated_at)}</span>,
      },
      {
        id: 'actions',
        header: '',
        cell: ({ row }) => (
          <div className="flex justify-end gap-2">
            <RegistryFormDialog
              mode="edit"
              registry={row.original}
              onSaved={reload}
              trigger={
                <button
                  type="button"
                  aria-label={t('access.registries.edit')}
                  className="rounded-lg border border-line px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:text-ink"
                >
                  <PencilIcon />
                </button>
              }
            />
            <DeleteRegistryAction registry={row.original} onDone={reload} />
          </div>
        ),
      },
    ],
    [t, fmt, reload],
  );
  return <DataTable columns={columns} data={registries} rowId={(r) => r.id} empty={t('access.registries.empty')} />;
}

function PencilIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" aria-hidden className="size-3.5">
      <path
        d="M4 20h4L18.5 9.5a2.12 2.12 0 0 0-3-3L5 17v3Z"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function DeleteRegistryAction({ registry, onDone }: { registry: RegistryInfo; onDone: () => void }) {
  const { t } = useT();
  const toast = useToast();
  return (
    <ConfirmButton
      label={t('access.registries.delete')}
      tone="dead"
      title={t('access.registries.delete.title', { host: registry.host })}
      description={t('access.registries.delete.desc')}
      confirmLabel={t('access.registries.delete')}
      onConfirm={async () => {
        await api.deleteRegistry(registry.id);
        toast.success(t('access.registries.toast.deleted', { host: registry.host }));
        onDone();
      }}
    />
  );
}

// --- динамическая форма add/edit: дропдаун типа → поля + подсказка ---

const inputClass = 'rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal placeholder:text-muted';

function RegistryFormDialog({
  mode,
  registry,
  trigger,
  onSaved,
}: {
  mode: 'add' | 'edit';
  registry?: RegistryInfo;
  trigger: ReactNode;
  onSaved: () => void;
}) {
  const { t } = useT();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [type, setType] = useState<RegistryType>(registry?.type ?? 'ghcr');
  const [host, setHost] = useState(registry?.host ?? 'ghcr.io'); // add-дефолт: ghcr → ghcr.io
  const [username, setUsername] = useState(registry?.username ?? '');
  const [token, setToken] = useState('');
  const [note, setNote] = useState(registry?.note ?? '');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const isEdit = mode === 'edit';
  const needsUsername = type !== 'gar'; // gar: мастер форсит `_json_key`, поля нет

  // Полный сброс — в т.ч. чистит введённый токен из памяти. При открытии
  // (пере)выводит поля из актуального registry (edit) / дефолтов (add).
  const reset = () => {
    setType(registry?.type ?? 'ghcr');
    setHost(registry?.host ?? 'ghcr.io');
    setUsername(registry?.username ?? '');
    setToken('');
    setNote(registry?.note ?? '');
    setPending(false);
    setError(null);
  };

  // Смена типа адаптирует префилл host (только add — в edit host неизменяем).
  const onTypeChange = (next: RegistryType) => {
    setType(next);
    if (!isEdit) {
      setHost((h) => {
        if (next === 'ghcr' && h.trim() === '') return 'ghcr.io';
        if (next !== 'ghcr' && h === 'ghcr.io') return '';
        return h;
      });
    }
  };

  const hostOk = host.trim() !== '';
  const usernameOk = !needsUsername || username.trim() !== '';
  const tokenOk = isEdit || token.trim() !== ''; // edit: пустой токен = keep-secret
  const valid = hostOk && usernameOk && tokenOk;

  const fail = (e: unknown) => {
    setPending(false);
    setError(e instanceof ApiError ? (e.detail ?? e.code) : t('access.registries.create.err'));
  };

  const submit = () => {
    if (!valid || pending) return;
    setPending(true);
    setError(null);

    if (isEdit && registry !== undefined) {
      const body: RegistryPatch = { type, note: note.trim() };
      if (needsUsername) body.username = username.trim();
      if (token.trim() !== '') body.token = token.trim(); // включаем ТОЛЬКО если задан → ротация
      api
        .patchRegistry(registry.id, body)
        .then((reg) => {
          setPending(false);
          setOpen(false);
          reset();
          toast.success(t('access.registries.toast.updated', { host: reg.host }));
          onSaved();
        })
        .catch(fail);
      return;
    }

    const body: RegistryInput = { host: host.trim(), type, token: token.trim(), note: note.trim() };
    if (needsUsername) body.username = username.trim(); // gar → username не шлём
    api
      .upsertRegistry(body)
      .then((reg) => {
        setPending(false);
        setOpen(false);
        reset();
        toast.success(t('access.registries.toast.saved', { host: reg.host }));
        onSaved();
      })
      .catch(fail);
  };

  const hostPlaceholder = type === 'gar' ? t('access.registries.create.hostGarPlaceholder') : t('access.registries.create.hostPlaceholder');
  const tokenPlaceholder = isEdit
    ? t('access.registries.keepSecret')
    : type === 'gar'
      ? t('access.registries.create.tokenGarPlaceholder')
      : t('access.registries.create.tokenPlaceholder');

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return;
        setOpen(o);
        reset(); // открыли — префилл из registry/дефолтов; закрыли — токен уходит из памяти
      }}
    >
      <Dialog.Trigger asChild>{trigger}</Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content className="fixed top-1/2 left-1/2 z-50 w-[min(32rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl">
          <Dialog.Title className="text-base font-semibold">
            {isEdit ? t('access.registries.edit.title', { host: registry?.host ?? '' }) : t('access.registries.create.title')}
          </Dialog.Title>
          <Dialog.Description className="mt-1 text-sm text-muted">
            {isEdit ? t('access.registries.edit.desc') : t('access.registries.create.desc')}
          </Dialog.Description>
          <div className="mt-4 flex flex-col gap-3">
            <label className="flex flex-col gap-1 text-sm font-medium">
              {t('access.registries.col.type')}
              <select
                value={type}
                onChange={(e) => {
                  onTypeChange(e.target.value as RegistryType);
                }}
                className={inputClass}
              >
                {REGISTRY_TYPES.map((rt) => (
                  <option key={rt} value={rt}>
                    {t(`access.registries.type.${rt}` as MessageKey)}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex flex-col gap-1 text-sm font-medium">
              {t('access.registries.create.host')}
              <input
                autoFocus={!isEdit}
                value={host}
                readOnly={isEdit}
                onChange={(e) => {
                  setHost(e.target.value);
                }}
                placeholder={hostPlaceholder}
                className={`${inputClass} ${isEdit ? 'text-muted' : ''}`}
              />
            </label>
            {needsUsername && (
              <label className="flex flex-col gap-1 text-sm font-medium">
                {t('access.registries.create.username')}
                <input
                  value={username}
                  onChange={(e) => {
                    setUsername(e.target.value);
                  }}
                  placeholder={t('access.registries.create.usernamePlaceholder')}
                  className={inputClass}
                />
              </label>
            )}
            <label className="flex flex-col gap-1 text-sm font-medium">
              {t('access.registries.create.token')}
              {type === 'gar' ? (
                <textarea
                  value={token}
                  onChange={(e) => {
                    setToken(e.target.value);
                  }}
                  rows={5}
                  placeholder={tokenPlaceholder}
                  className={`${inputClass} font-mono`}
                />
              ) : (
                <input
                  type="password"
                  value={token}
                  onChange={(e) => {
                    setToken(e.target.value);
                  }}
                  placeholder={tokenPlaceholder}
                  className={inputClass}
                />
              )}
            </label>
            <label className="flex flex-col gap-1 text-sm font-medium">
              {t('access.registries.create.note')}
              <input
                value={note}
                onChange={(e) => {
                  setNote(e.target.value);
                }}
                placeholder={t('access.registries.create.notePlaceholder')}
                className={inputClass}
              />
            </label>
            <p className="rounded-lg bg-paper px-3 py-2 text-xs text-muted">{t(`access.registries.help.${type}` as MessageKey)}</p>
            {error !== null && (
              <p role="alert" className="rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
                {error}
              </p>
            )}
          </div>
          <div className="mt-5 flex justify-end gap-2">
            <Dialog.Close asChild>
              <button type="button" className="rounded-lg border border-line px-3 py-1.5 text-sm text-muted hover:text-ink">
                {t('common.cancel')}
              </button>
            </Dialog.Close>
            <button
              type="button"
              disabled={!valid || pending}
              onClick={submit}
              className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
            >
              {pending ? t('common.running') : t('access.registries.create.submit')}
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
