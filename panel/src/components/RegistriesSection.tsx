// «Админка» (П2): раздел «Реестры» — приватные container-registry credentials
// (docs/superpowers/specs/2026-07-09-registries-design.md §4). CRUD-вью в
// духе Access.tsx (KeysTable/CreateKeyDialog): таблица (host/username/note/
// updated + токен ВСЕГДА маской — RegistryInfo структурно не несёт токен, его
// неоткуда прочитать) и одна форма добавления, которая же — путь замены
// токена (тот же host в POST — backend апсертит username/token/note целиком).

import { useMemo, useState } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import * as Dialog from '@radix-ui/react-dialog';
import { api, ApiError } from '../lib/api';
import type { RegistryInfo } from '../lib/api';
import { useAsync } from '../lib/useAsync';
import { useT, useFormat } from '../lib/i18n';
import { useToast } from './Toast';
import { DataTable } from './DataTable';
import { ConfirmButton } from './ConfirmDialog';
import { Card, CardHeader, ErrorNote, LoadingRow } from './ui';

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
            <UpsertRegistryDialog onSaved={regs.reload} />
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
        cell: ({ row }) => <DeleteRegistryAction registry={row.original} onDone={reload} />,
      },
    ],
    [t, fmt, reload],
  );
  return <DataTable columns={columns} data={registries} rowId={(r) => r.id} empty={t('access.registries.empty')} />;
}

function DeleteRegistryAction({ registry, onDone }: { registry: RegistryInfo; onDone: () => void }) {
  const { t } = useT();
  const toast = useToast();
  return (
    <div className="flex justify-end">
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
    </div>
  );
}

// --- добавление/замена токена: одна форма, submit = upsert по host ---

function UpsertRegistryDialog({ onSaved }: { onSaved: () => void }) {
  const { t } = useT();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [host, setHost] = useState('');
  const [username, setUsername] = useState('');
  const [token, setToken] = useState('');
  const [note, setNote] = useState('');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reset = () => {
    setHost('');
    setUsername('');
    setToken('');
    setNote('');
    setPending(false);
    setError(null);
  };

  const valid = host.trim() !== '' && username.trim() !== '' && token.trim() !== '';

  const submit = () => {
    if (!valid || pending) return;
    setPending(true);
    setError(null);
    api
      .upsertRegistry({ host: host.trim(), username: username.trim(), token: token.trim(), note: note.trim() })
      .then((reg) => {
        setPending(false);
        setOpen(false);
        reset();
        toast.success(t('access.registries.toast.saved', { host: reg.host }));
        onSaved();
      })
      .catch((e: unknown) => {
        setPending(false);
        setError(e instanceof ApiError ? (e.detail ?? e.code) : t('access.registries.create.err'));
      });
  };

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return;
        setOpen(o);
        if (!o) reset(); // закрыли — форма (и введённый токен) уходит из памяти
      }}
    >
      <Dialog.Trigger asChild>
        <button type="button" className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90">
          {t('access.registries.add')}
        </button>
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content className="fixed top-1/2 left-1/2 z-50 w-[min(32rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl">
          <Dialog.Title className="text-base font-semibold">{t('access.registries.create.title')}</Dialog.Title>
          <Dialog.Description className="mt-1 text-sm text-muted">{t('access.registries.create.desc')}</Dialog.Description>
          <div className="mt-4 flex flex-col gap-3">
            <label className="flex flex-col gap-1 text-sm font-medium">
              {t('access.registries.create.host')}
              <input
                autoFocus
                value={host}
                onChange={(e) => {
                  setHost(e.target.value);
                }}
                placeholder={t('access.registries.create.hostPlaceholder')}
                className="rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal placeholder:text-muted"
              />
            </label>
            <label className="flex flex-col gap-1 text-sm font-medium">
              {t('access.registries.create.username')}
              <input
                value={username}
                onChange={(e) => {
                  setUsername(e.target.value);
                }}
                placeholder={t('access.registries.create.usernamePlaceholder')}
                className="rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal placeholder:text-muted"
              />
            </label>
            <label className="flex flex-col gap-1 text-sm font-medium">
              {t('access.registries.create.token')}
              <input
                type="password"
                value={token}
                onChange={(e) => {
                  setToken(e.target.value);
                }}
                placeholder={t('access.registries.create.tokenPlaceholder')}
                className="rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal placeholder:text-muted"
              />
            </label>
            <label className="flex flex-col gap-1 text-sm font-medium">
              {t('access.registries.create.note')}
              <input
                value={note}
                onChange={(e) => {
                  setNote(e.target.value);
                }}
                placeholder={t('access.registries.create.notePlaceholder')}
                className="rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal placeholder:text-muted"
              />
            </label>
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
