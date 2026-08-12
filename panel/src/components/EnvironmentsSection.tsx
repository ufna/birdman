// «Админка» (environments v1 §8): секция «Окружения» — CRUD окружений проекта
// (docs/superpowers/specs/2026-07-13-environments-v1-design.md §2). Вью в духе
// RegistriesSection.tsx: таблица (name/production/auto_deploy/retention/created)
// и одна форма add/edit. Guardrail production×auto_deploy — и клиентски (чекбокс
// auto_deploy глохнет при production), и от API (400, показываем detail).
//
// Удаление сносит окружение ВМЕСТЕ с содержимым (см. DeleteEnvironmentAction):
// диалог показывает состав из GET .../usage, требует подтверждения вводом имени
// и блокируется, пока в окружении есть ноды (их сначала переводят на «Флоте»).
//
// Источник данных — общий useEnv() (тот же список, что кормит чипы Shell): после
// create/patch/delete зовём reload — обновляются и чипы, и эта таблица.

import { useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import * as AlertDialog from '@radix-ui/react-alert-dialog';
import * as Dialog from '@radix-ui/react-dialog';
import { api, ApiError } from '../lib/api';
import { apiErrorMessage } from '../lib/apiError';
import type { Environment, EnvironmentUsage } from '../lib/api';
import { useEnv } from '../lib/env';
import { useT, useFormat } from '../lib/i18n';
import { useToast } from './Toast';
import { DataTable } from './DataTable';
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

/** Строка состава окружения в диалоге удаления (метка + число). */
function UsageRow({ id, label, value }: { id: string; label: string; value: number }) {
  return (
    <li data-testid={`env-usage-${id}`} className="flex items-baseline justify-between gap-3">
      <span className="text-muted">{label}</span>
      <span className="tabular font-mono font-medium">{value}</span>
    </li>
  );
}

/**
 * Удаление окружения ВМЕСТЕ с содержимым (запрос владельца; снимает прежний тупик
 * «использованный env неудаляем»). Диалог:
 *   1. при открытии зовёт GET .../usage и показывает состав («что снесём»);
 *   2. ноды в окружении — блокирующее условие: ноду нельзя молча осиротить,
 *      удаление недоступно, пока их не переведут на экране «Флот» (Move to env…);
 *   3. подтверждение вводом ИМЕНИ (имя рядом + кнопка копирования): кнопка
 *      «Удалить» жива, только когда введённое совпадает ТОЧНО (без trim —
 *      лишний пробел не считается совпадением, как и на сервере).
 * Пустому окружению шаг подтверждения тоже показываем (единообразие): сервер
 * примет confirm и без необходимости (204).
 */
function DeleteEnvironmentAction({
  project,
  environment,
  onDone,
}: {
  project: string;
  environment: Environment;
  onDone: () => void;
}) {
  const { t, tp } = useT();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [usage, setUsage] = useState<EnvironmentUsage | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [typed, setTyped] = useState('');
  const [copied, setCopied] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Состав грузим на каждое открытие (данные могли устареть с прошлого раза).
  useEffect(() => {
    if (!open) return;
    let stale = false;
    setUsage(null);
    setLoadError(null);
    api.environmentUsage(project, environment.name).then(
      (u) => {
        if (!stale) setUsage(u);
      },
      (e: unknown) => {
        if (!stale) setLoadError(apiErrorMessage(e, t, { forbidden: 'confirm.err.forbidden', generic: 'access.environments.delete.usageErr', byStatus: { 400: 'ui.err.badRequest' } }));
      },
    );
    return () => {
      stale = true;
    };
  }, [open, project, environment.name, t]);

  const blocked = usage !== null && usage.nodes > 0;
  const nameMatches = typed === environment.name; // ТОЧНОЕ совпадение, без trim
  const canDelete = usage !== null && !blocked && nameMatches && !pending;

  const copyName = () => {
    void navigator.clipboard?.writeText(environment.name).then(
      () => {
        setCopied(true);
      },
      () => {
        /* clipboard недоступен — имя всё равно видно, можно набрать руками */
      },
    );
  };

  const submit = () => {
    if (!canDelete) return;
    setPending(true);
    setError(null);
    api
      .deleteEnvironment(project, environment.name, typed)
      .then((deleted) => {
        setPending(false);
        setOpen(false);
        toast.success(
          deleted === undefined
            ? t('access.environments.toast.deleted', { name: environment.name })
            : t('access.environments.toast.deletedCascade', {
                name: deleted.name,
                versions: deleted.versions,
                fleets: deleted.fleets,
                matches: deleted.matches,
                servers: deleted.servers,
                keys: deleted.api_keys_revoked,
              }),
        );
        onDone();
      })
      .catch((e: unknown) => {
        setPending(false);
        // 409 — ноды появились (или уехали) между usage и удалением: показываем
        // detail сервера прямо в диалоге и обновляем состав.
        setError(apiErrorMessage(e, t, { forbidden: 'confirm.err.forbidden', generic: 'access.environments.delete.err', byStatus: { 400: 'ui.err.badRequest' } }));
        if (e instanceof ApiError && e.status === 409) {
          api.environmentUsage(project, environment.name).then(
            (u) => {
              setUsage(u);
            },
            () => {
              /* состав не перечитался — на решение это не влияет, ошибка уже видна */
            },
          );
        }
      });
  };

  return (
    <AlertDialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return;
        setOpen(o);
        setTyped('');
        setCopied(false);
        setError(null);
      }}
    >
      <AlertDialog.Trigger asChild>
        <button
          type="button"
          className="rounded-lg border border-line px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:text-ink"
        >
          {t('access.environments.delete')}
        </button>
      </AlertDialog.Trigger>
      <AlertDialog.Portal>
        <AlertDialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <AlertDialog.Content
          className="fixed top-1/2 left-1/2 z-50 w-[min(30rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl"
          onEscapeKeyDown={(e) => {
            if (pending) e.preventDefault();
          }}
        >
          <AlertDialog.Title className="text-base font-semibold">
            {t('access.environments.delete.title', { name: environment.name })}
          </AlertDialog.Title>
          <AlertDialog.Description asChild>
            <div className="mt-2 text-sm text-muted">{t('access.environments.delete.desc')}</div>
          </AlertDialog.Description>

          {loadError !== null && (
            <p role="alert" className="mt-3 rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
              {loadError}
            </p>
          )}
          {usage === null && loadError === null && <LoadingRow />}

          {usage !== null && (
            <>
              <p className="mt-4 text-xs font-medium">{t('access.environments.delete.usage')}</p>
              <ul className="mt-1.5 grid grid-cols-2 gap-x-6 gap-y-1 rounded-lg border border-line bg-paper px-3 py-2 text-xs">
                <UsageRow id="versions" label={t('access.environments.delete.usage.versions')} value={usage.versions} />
                <UsageRow id="fleets" label={t('access.environments.delete.usage.fleets')} value={usage.fleets} />
                <UsageRow id="matches" label={t('access.environments.delete.usage.matches')} value={usage.matches} />
                <UsageRow id="servers" label={t('access.environments.delete.usage.servers')} value={usage.servers} />
              </ul>
              <p className="mt-1.5 text-xs text-muted">{tp('access.environments.delete.usage.keys', usage.api_keys)}</p>

              {blocked ? (
                // Ноды не каскадятся никогда: живой агент не должен остаться с
                // node_id в несуществующем окружении.
                <p role="alert" className="mt-3 rounded-lg bg-warn-bg px-3 py-2 text-xs text-warn">
                  {tp('access.environments.delete.nodesBlock', usage.nodes)}
                </p>
              ) : (
                <div className="mt-4 flex flex-col gap-1.5">
                  <label htmlFor="env-delete-confirm" className="text-xs font-medium">
                    {t('access.environments.delete.confirmLabel')}
                  </label>
                  <div className="flex items-center gap-2">
                    <code className="rounded border border-line bg-paper px-2 py-1 font-mono text-xs">
                      {environment.name}
                    </code>
                    <button
                      type="button"
                      onClick={copyName}
                      aria-label={t('access.environments.delete.copy')}
                      className="rounded-md border border-line px-2 py-1 text-[11px] font-medium text-muted transition-colors hover:text-ink"
                    >
                      {copied ? t('access.environments.delete.copied') : t('access.environments.delete.copy')}
                    </button>
                  </div>
                  <input
                    id="env-delete-confirm"
                    autoFocus
                    autoComplete="off"
                    value={typed}
                    onChange={(e) => {
                      setTyped(e.target.value);
                    }}
                    placeholder={environment.name}
                    className={`${inputClass} font-mono`}
                  />
                </div>
              )}
            </>
          )}

          {error !== null && (
            <p role="alert" className="mt-3 rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
              {error}
            </p>
          )}

          <div className="mt-5 flex justify-end gap-2">
            <AlertDialog.Cancel asChild>
              <button
                type="button"
                disabled={pending}
                className="rounded-lg border border-line px-3 py-1.5 text-sm text-muted transition-colors hover:text-ink disabled:opacity-40"
              >
                {t('common.cancel')}
              </button>
            </AlertDialog.Cancel>
            <button
              type="button"
              disabled={!canDelete}
              onClick={submit}
              className="rounded-lg bg-dead px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
            >
              {pending ? t('common.running') : t('access.environments.delete')}
            </button>
          </div>
        </AlertDialog.Content>
      </AlertDialog.Portal>
    </AlertDialog.Root>
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
    setError(apiErrorMessage(e, t, { forbidden: 'confirm.err.forbidden', generic: 'access.environments.create.err', byStatus: { 400: 'ui.err.badRequest' } }));
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
