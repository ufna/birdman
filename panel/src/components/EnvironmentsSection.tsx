// «Админка» (environments v1 §8): секция «Окружения» — CRUD окружений проекта
// (docs/superpowers/specs/2026-07-13-environments-v1-design.md §2). Вью в духе
// RegistriesSection.tsx: таблица (name/production/auto_deploy/retention/created)
// и одна форма add/edit. Guardrail production×auto_deploy — и клиентски (чекбокс
// auto_deploy глохнет при production), и от API (400, показываем detail).
// Удаление разрешено только у никогда не использованного env; иначе 409 —
// честная подсказка «используется, удалить нельзя» (I10, история версий хранится).
//
// Источник данных — общий useEnv() (тот же список, что кормит чипы Shell): после
// create/patch/delete зовём reload — обновляются и чипы, и эта таблица.

import { useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import * as Dialog from '@radix-ui/react-dialog';
import { api, ApiError } from '../lib/api';
import type { Environment } from '../lib/api';
import { useEnv } from '../lib/env';
import { useT, useFormat } from '../lib/i18n';
import { useToast } from './Toast';
import { DataTable } from './DataTable';
import { ConfirmButton } from './ConfirmDialog';
import { Card, CardHeader, ErrorNote, LoadingRow } from './ui';

export function EnvironmentsSection() {
  const { t } = useT();
  const { environments, project, loading, error, reload } = useEnv();

  return (
    <Card>
      <CardHeader
        title={t('access.environments')}
        aside={
          <div className="flex items-center gap-3">
            <span className="tabular font-mono text-xs text-muted">{environments.length}</span>
            {project !== null && (
              <EnvironmentFormDialog
                mode="add"
                project={project}
                onSaved={reload}
                trigger={
                  <button type="button" className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90">
                    {t('access.environments.add')}
                  </button>
                }
              />
            )}
          </div>
        }
      />
      <p className="border-b border-line px-4 py-2.5 text-xs text-muted">{t('access.environments.hint')}</p>
      {error !== undefined && environments.length === 0 ? (
        <div className="p-4">
          <ErrorNote error={error} retry={reload} />
        </div>
      ) : loading && environments.length === 0 ? (
        <LoadingRow />
      ) : (
        <EnvironmentsTable environments={environments} project={project} reload={reload} />
      )}
    </Card>
  );
}

/** Бейдж флага (on/off) — цвет-нейтрально, подпись несёт смысл. */
function FlagBadge({ on }: { on: boolean }) {
  const { t } = useT();
  return (
    <span
      className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[11px] font-medium ${
        on ? 'border-good/40 text-good' : 'border-line text-muted'
      }`}
    >
      <span aria-hidden className={`size-1.5 rounded-full ${on ? 'bg-good' : 'bg-line'}`} />
      {on ? t('access.environments.flag.on') : t('access.environments.flag.off')}
    </span>
  );
}

function EnvironmentsTable({
  environments,
  project,
  reload,
}: {
  environments: Environment[];
  project: string | null;
  reload: () => void;
}) {
  const { t } = useT();
  const fmt = useFormat();
  const columns = useMemo<ColumnDef<Environment, unknown>[]>(
    () => [
      {
        id: 'name',
        header: t('access.environments.col.name'),
        cell: ({ row }) => <span className="font-mono text-sm font-medium">{row.original.name}</span>,
      },
      {
        id: 'production',
        header: t('access.environments.col.production'),
        cell: ({ row }) => <FlagBadge on={row.original.production} />,
      },
      {
        id: 'autoDeploy',
        header: t('access.environments.col.autoDeploy'),
        cell: ({ row }) => <FlagBadge on={row.original.auto_deploy} />,
      },
      {
        id: 'retention',
        header: t('access.environments.col.retention'),
        cell: ({ row }) => (
          <span className="tabular font-mono text-xs">
            {row.original.retention_keep === 0 ? t('access.environments.retentionUnlimited') : row.original.retention_keep}
          </span>
        ),
      },
      {
        id: 'created',
        header: t('access.environments.col.created'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{fmt.stamp(row.original.created_at)}</span>,
      },
      {
        id: 'actions',
        header: '',
        cell: ({ row }) => (
          <div className="flex justify-end gap-2">
            {project !== null && (
              <EnvironmentFormDialog
                mode="edit"
                project={project}
                environment={row.original}
                onSaved={reload}
                trigger={
                  <button
                    type="button"
                    aria-label={t('access.environments.edit')}
                    className="rounded-lg border border-line px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:text-ink"
                  >
                    <PencilIcon />
                  </button>
                }
              />
            )}
            {project !== null && <DeleteEnvironmentAction project={project} environment={row.original} onDone={reload} />}
          </div>
        ),
      },
    ],
    [t, fmt, project, reload],
  );
  return <DataTable columns={columns} data={environments} rowId={(e) => e.name} empty={t('access.environments.empty')} />;
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

function DeleteEnvironmentAction({
  project,
  environment,
  onDone,
}: {
  project: string;
  environment: Environment;
  onDone: () => void;
}) {
  const { t } = useT();
  const toast = useToast();
  return (
    <ConfirmButton
      label={t('access.environments.delete')}
      tone="dead"
      title={t('access.environments.delete.title', { name: environment.name })}
      description={t('access.environments.delete.desc')}
      confirmLabel={t('access.environments.delete')}
      // Использованный env неудаляем (409) — честная подсказка вместо сырого кода.
      errorOverride={(e) =>
        e instanceof ApiError && e.status === 409 ? t('access.environments.delete.usedHint') : undefined
      }
      onConfirm={async () => {
        await api.deleteEnvironment(project, environment.name);
        toast.success(t('access.environments.toast.deleted', { name: environment.name }));
        onDone();
      }}
    />
  );
}

const inputClass = 'rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal placeholder:text-muted';

function EnvironmentFormDialog({
  mode,
  project,
  environment,
  trigger,
  onSaved,
}: {
  mode: 'add' | 'edit';
  project: string;
  environment?: Environment;
  trigger: ReactNode;
  onSaved: () => void;
}) {
  const { t } = useT();
  const toast = useToast();
  const isEdit = mode === 'edit';
  const [open, setOpen] = useState(false);
  const [name, setName] = useState(environment?.name ?? '');
  const [production, setProduction] = useState(environment?.production ?? false);
  const [autoDeploy, setAutoDeploy] = useState(environment?.auto_deploy ?? false);
  const [retention, setRetention] = useState(String(environment?.retention_keep ?? 0));
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reset = () => {
    setName(environment?.name ?? '');
    setProduction(environment?.production ?? false);
    setAutoDeploy(environment?.auto_deploy ?? false);
    setRetention(String(environment?.retention_keep ?? 0));
    setPending(false);
    setError(null);
  };

  // Guardrail (клиентский; API дублирует 400): production ⇒ !auto_deploy.
  useEffect(() => {
    if (production) setAutoDeploy(false);
  }, [production]);

  const retentionNum = Number(retention);
  const retentionOk = Number.isInteger(retentionNum) && retentionNum >= 0;
  const nameOk = isEdit || /^[a-z0-9][a-z0-9-]*$/.test(name.trim());
  const valid = nameOk && retentionOk;

  const fail = (e: unknown) => {
    setPending(false);
    setError(e instanceof ApiError ? (e.detail ?? e.code) : t('access.environments.create.err'));
  };

  const submit = () => {
    if (!valid || pending) return;
    setPending(true);
    setError(null);
    if (isEdit && environment !== undefined) {
      api
        .patchEnvironment(project, environment.name, {
          production,
          auto_deploy: autoDeploy,
          retention_keep: retentionNum,
        })
        .then(() => {
          setPending(false);
          setOpen(false);
          toast.success(t('access.environments.toast.updated', { name: environment.name }));
          onSaved();
        })
        .catch(fail);
      return;
    }
    api
      .createEnvironment({
        project,
        name: name.trim(),
        production,
        auto_deploy: autoDeploy,
        retention_keep: retentionNum,
      })
      .then((env) => {
        setPending(false);
        setOpen(false);
        reset();
        toast.success(t('access.environments.toast.created', { name: env.name }));
        onSaved();
      })
      .catch(fail);
  };

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return;
        setOpen(o);
        reset();
      }}
    >
      <Dialog.Trigger asChild>{trigger}</Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content className="fixed top-1/2 left-1/2 z-50 w-[min(28rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl">
          <Dialog.Title className="text-base font-semibold">
            {isEdit ? t('access.environments.edit.title', { name: environment?.name ?? '' }) : t('access.environments.create.title')}
          </Dialog.Title>
          <Dialog.Description className="mt-1 text-sm text-muted">
            {isEdit ? t('access.environments.edit.desc') : t('access.environments.create.desc')}
          </Dialog.Description>
          <div className="mt-4 flex flex-col gap-3">
            {!isEdit && (
              <label className="flex flex-col gap-1 text-sm font-medium">
                {t('access.environments.create.name')}
                <input
                  autoFocus
                  value={name}
                  onChange={(e) => {
                    setName(e.target.value);
                  }}
                  placeholder={t('access.environments.create.namePlaceholder')}
                  className={`${inputClass} font-mono`}
                />
              </label>
            )}
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={production}
                onChange={(e) => {
                  setProduction(e.target.checked);
                }}
              />
              {t('access.environments.create.production')}
            </label>
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={autoDeploy}
                disabled={production}
                onChange={(e) => {
                  setAutoDeploy(e.target.checked);
                }}
              />
              {t('access.environments.create.autoDeploy')}
            </label>
            {production && <p className="text-xs text-muted">{t('access.environments.guardrail')}</p>}
            <label className="flex flex-col gap-1 text-sm font-medium">
              {t('access.environments.create.retention')}
              <input
                type="number"
                min={0}
                value={retention}
                onChange={(e) => {
                  setRetention(e.target.value);
                }}
                className={`${inputClass} w-28`}
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
              {pending ? t('common.running') : t('access.environments.create.submit')}
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
