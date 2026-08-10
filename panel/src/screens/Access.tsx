// Админка (П2, admin-only; nav-ключ nav.access — маршрут/гейт исторический
// /access не переименовывали, T6 меняет только видимые EN/RU-значения):
// секция «API-ключи» (GET/POST/DELETE /v1/apikeys) — таблица (name, scopes,
// created, active/revoked), создание диалогом с чекбоксами скоупов (секрет
// показывается РОВНО один раз, после закрытия вычищается из памяти), отзыв
// (confirm + 409 last_admin_key) и purge отозванных (hard-delete строки,
// ?purge=true — см. RevokeAction/PurgeAction ниже); секция «Реестры»
// (RegistriesSection) — приватные registry-креды, см. её файл. Экран скрыт
// из навигации/роутинга для не-admin (эндпоинты admin-scoped).

/* eslint-disable react-refresh/only-export-components -- SCOPE_OPTIONS/BINDABLE_SCOPES/canBindScopes — таблица скоупов и её предикаты рядом с экраном, покрыты юнит-тестами напрямую. Правило про гранулярность Fast Refresh в dev-сервере, не про корректность; разносить файл по модулям ради него дороже, чем оно стоит. Политика — в eslint.config.js. */

import { useMemo, useState } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import * as Dialog from '@radix-ui/react-dialog';
import { api, ApiError } from '../lib/api';
import type { ApiKey, CreatedApiKey, KeyBinding, Scope } from '../lib/api';
import { useAsync } from '../lib/useAsync';
import { useEnv } from '../lib/env';
import { useT, useFormat } from '../lib/i18n';
import type { I18nContextValue } from '../lib/i18n';
import { useToast } from '../components/Toast';
import { DataTable } from '../components/DataTable';
import { StateBadge, toneOfKeyStatus } from '../components/Badge';
import { ConfirmButton } from '../components/ConfirmDialog';
import { RegistriesSection } from '../components/RegistriesSection';
import { EnvironmentsSection } from '../components/EnvironmentsSection';
import { EnvTag } from '../components/EnvTag';
import { Card, CardHeader, ErrorNote, LoadingRow } from '../components/ui';

/** Скоупы, которые можно выдать ключу (совпадает с validScopes в master). */
export const SCOPE_OPTIONS: Scope[] = ['admin', 'deploy', 'matchmaking', 'allocate', 'readonly'];

/** Скоупы, для которых привязка (project, env) осмысленна и энфорсится
 *  (environments v1 §5): deploy/matchmaking/allocate. admin — несовместим. */
export const BINDABLE_SCOPES: Scope[] = ['deploy', 'matchmaking', 'allocate'];

/** Можно ли привязать ключ с таким набором скоупов: есть bindable-скоуп и НЕ admin. */
export function canBindScopes(scopes: Set<Scope>): boolean {
  return !scopes.has('admin') && BINDABLE_SCOPES.some((s) => scopes.has(s));
}

/** errorOverride для отзыва: 409 last_admin_key → человекочитаемо; иначе — дефолт. */
export function lastAdminKeyMessage(t: I18nContextValue['t']): (e: unknown) => string | undefined {
  return (e: unknown) => (e instanceof ApiError && e.code === 'last_admin_key' ? t('access.err.lastAdmin') : undefined);
}

export function Access() {
  const { t } = useT();
  const keys = useAsync(() => api.listApiKeys(), []);

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-lg font-semibold">{t('nav.access')}</h1>
        <CreateKeyDialog onCreated={keys.reload} />
      </div>
      <Card>
        <CardHeader
          title={t('access.keys')}
          aside={keys.data !== undefined ? <span className="tabular font-mono text-xs text-muted">{keys.data.length}</span> : undefined}
        />
        {keys.error !== undefined && keys.data === undefined ? (
          <div className="p-4">
            <ErrorNote error={keys.error} retry={keys.reload} />
          </div>
        ) : keys.data === undefined ? (
          <LoadingRow />
        ) : (
          <KeysTable keys={keys.data} reload={keys.reload} />
        )}
      </Card>
      <EnvironmentsSection />
      <RegistriesSection />
    </div>
  );
}

function KeysTable({ keys, reload }: { keys: ApiKey[]; reload: () => void }) {
  const { t } = useT();
  const fmt = useFormat();
  const columns = useMemo<ColumnDef<ApiKey, unknown>[]>(
    () => [
      {
        id: 'name',
        header: t('access.col.name'),
        cell: ({ row }) => (
          <div className="min-w-32">
            <div className="text-sm font-medium">{row.original.name}</div>
            <div className="font-mono text-[11px] text-muted">{row.original.id}</div>
          </div>
        ),
      },
      {
        id: 'scopes',
        header: t('access.col.scopes'),
        cell: ({ row }) => <ScopeChips scopes={row.original.scopes} />,
      },
      {
        id: 'binding',
        header: t('access.col.binding'),
        cell: ({ row }) =>
          row.original.project !== undefined && row.original.env !== undefined ? (
            <EnvTag env={`${row.original.project} / ${row.original.env}`} />
          ) : (
            <span className="text-xs text-muted">{t('access.binding.global')}</span>
          ),
      },
      {
        id: 'created',
        header: t('access.col.created'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{fmt.stamp(row.original.created_at)}</span>,
      },
      {
        id: 'status',
        header: t('access.col.status'),
        cell: ({ row }) => {
          const revoked = row.original.revoked_at !== null;
          return <StateBadge state={revoked ? 'revoked' : 'active'} tone={toneOfKeyStatus(revoked)} domain="keystatus" />;
        },
      },
      {
        id: 'actions',
        header: '',
        cell: ({ row }) =>
          row.original.revoked_at === null ? (
            <RevokeAction apiKey={row.original} onDone={reload} />
          ) : (
            <PurgeAction apiKey={row.original} onDone={reload} />
          ),
      },
    ],
    [t, fmt, reload],
  );
  return <DataTable columns={columns} data={keys} rowId={(k) => k.id} empty={t('access.empty')} />;
}

function ScopeChips({ scopes }: { scopes: Scope[] }) {
  const { t, has } = useT();
  return (
    <div className="flex flex-wrap gap-1">
      {scopes.map((s) => (
        <span key={s} title={s} className="rounded border border-line px-1.5 py-0.5 text-[10px] tracking-wide text-muted uppercase">
          {has(`scope.${s}`) ? t(`scope.${s}` as Parameters<typeof t>[0]) : s}
        </span>
      ))}
    </div>
  );
}

function RevokeAction({ apiKey, onDone }: { apiKey: ApiKey; onDone: () => void }) {
  const { t } = useT();
  const toast = useToast();
  return (
    <div className="flex justify-end">
      <ConfirmButton
        label={t('access.revoke')}
        tone="dead"
        title={t('access.revoke.title', { name: apiKey.name })}
        description={t('access.revoke.desc')}
        confirmLabel={t('access.revoke')}
        errorOverride={lastAdminKeyMessage(t)}
        onConfirm={async () => {
          await api.revokeApiKey(apiKey.id);
          toast.success(t('access.toast.revoked', { name: apiKey.name }));
          onDone();
        }}
      />
    </div>
  );
}

/** Purge (hard-delete) — только для ЯВНО revoked-строк (Task 6, registries v1
 *  design §6): кнопка сама по себе гейтит недостижимый на active-ключах 409
 *  not_revoked, но ConfirmButton всё равно переживёт его дефолтным errMessage,
 *  если store когда-нибудь вернёт его по другой причине. */
function PurgeAction({ apiKey, onDone }: { apiKey: ApiKey; onDone: () => void }) {
  const { t } = useT();
  const toast = useToast();
  return (
    <div className="flex justify-end">
      <ConfirmButton
        label={t('access.purge')}
        tone="dead"
        title={t('access.purge.title', { name: apiKey.name })}
        description={t('access.purge.desc')}
        confirmLabel={t('access.purge')}
        onConfirm={async () => {
          await api.purgeApiKey(apiKey.id);
          toast.success(t('access.toast.purged', { name: apiKey.name }));
          onDone();
        }}
      />
    </div>
  );
}

// --- создание ключа: форма → секрет (один раз) ---

function CreateKeyDialog({ onCreated }: { onCreated: () => void }) {
  const { t } = useT();
  const toast = useToast();
  const { environments, project } = useEnv();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState('');
  const [scopes, setScopes] = useState<Set<Scope>>(new Set());
  const [bindEnv, setBindEnv] = useState(''); // '' = Global (без привязки)
  const [created, setCreated] = useState<CreatedApiKey | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Привязка осмысленна только для deploy/matchmaking/allocate без admin и
  // когда есть окружения и известен проект (environments v1 §5).
  const bindable = canBindScopes(scopes) && environments.length > 0 && project !== null;

  // Полный сброс — в т.ч. чистит секрет из памяти после закрытия.
  const reset = () => {
    setName('');
    setScopes(new Set());
    setBindEnv('');
    setCreated(null);
    setPending(false);
    setError(null);
  };

  const submit = () => {
    if (name.trim() === '' || scopes.size === 0 || pending) return;
    setPending(true);
    setError(null);
    const binding: KeyBinding | undefined =
      bindable && bindEnv !== '' && project !== null ? { project, env: bindEnv } : undefined;
    api
      .createApiKey(name.trim(), [...scopes], binding)
      .then((res) => {
        setCreated(res); // переключает диалог в режим показа секрета
        setPending(false);
        toast.success(t('access.toast.created', { name: res.key.name }));
        onCreated();
      })
      .catch((e: unknown) => {
        setPending(false);
        setError(e instanceof ApiError ? (e.detail ?? e.code) : t('access.create.err'));
      });
  };

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return;
        setOpen(o);
        if (!o) reset(); // закрыли — секрет уходит из памяти
      }}
    >
      <Dialog.Trigger asChild>
        <button type="button" className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90">
          {t('access.newKey')}
        </button>
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content className="fixed top-1/2 left-1/2 z-50 w-[min(32rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl">
          {created === null ? (
            <>
              <Dialog.Title className="text-base font-semibold">{t('access.create.title')}</Dialog.Title>
              <Dialog.Description className="mt-1 text-sm text-muted">{t('access.create.desc')}</Dialog.Description>
              <div className="mt-4 flex flex-col gap-3">
                <label className="flex flex-col gap-1 text-sm font-medium">
                  {t('access.create.name')}
                  <input
                    autoFocus
                    value={name}
                    onChange={(e) => {
                      setName(e.target.value);
                    }}
                    placeholder={t('access.create.namePlaceholder')}
                    className="rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal placeholder:text-muted"
                  />
                </label>
                <fieldset className="flex flex-col gap-2">
                  <legend className="mb-1 text-sm font-medium">{t('access.create.scopes')}</legend>
                  <div className="grid grid-cols-2 gap-2">
                    {SCOPE_OPTIONS.map((s) => (
                      <label key={s} className="flex items-center gap-2 rounded-lg border border-line px-2.5 py-1.5 text-sm">
                        <input
                          type="checkbox"
                          checked={scopes.has(s)}
                          onChange={(e) => {
                            setScopes((prev) => {
                              const next = new Set(prev);
                              if (e.target.checked) next.add(s);
                              else next.delete(s);
                              return next;
                            });
                          }}
                        />
                        {t(`scope.${s}` as Parameters<typeof t>[0])}
                      </label>
                    ))}
                  </div>
                </fieldset>
                {environments.length > 0 && (
                  <label className="flex flex-col gap-1 text-sm font-medium">
                    {t('access.create.binding')}
                    <select
                      value={bindEnv}
                      disabled={!bindable}
                      onChange={(e) => {
                        setBindEnv(e.target.value);
                      }}
                      className="rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal disabled:opacity-50"
                    >
                      <option value="">{t('access.create.bindingGlobal')}</option>
                      {environments.map((env) => (
                        <option key={env.name} value={env.name}>
                          {project !== null ? `${project} / ${env.name}` : env.name}
                        </option>
                      ))}
                    </select>
                    <span className="text-xs font-normal text-muted">{t('access.create.bindingHint')}</span>
                  </label>
                )}
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
                  disabled={name.trim() === '' || scopes.size === 0 || pending}
                  onClick={submit}
                  className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
                >
                  {pending ? t('common.running') : t('access.create.submit')}
                </button>
              </div>
            </>
          ) : (
            <SecretReveal created={created} onDone={() => { setOpen(false); reset(); }} />
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/** Показ секрета РОВНО один раз: копирование + предупреждение «больше не покажем». */
export function SecretReveal({ created, onDone }: { created: CreatedApiKey; onDone: () => void }) {
  const { t } = useT();
  const [copied, setCopied] = useState(false);
  const copy = () => {
    void navigator.clipboard?.writeText(created.secret).then(
      () => {
        setCopied(true);
      },
      () => {
        /* clipboard недоступен — секрет всё равно виден для ручного копирования */
      },
    );
  };
  return (
    <div>
      <Dialog.Title className="text-base font-semibold">{t('access.secret.title', { name: created.key.name })}</Dialog.Title>
      <Dialog.Description className="mt-1 text-sm text-dead">{t('access.secret.warning')}</Dialog.Description>
      <div className="mt-4 flex items-center gap-2 rounded-lg border border-line bg-paper px-3 py-2">
        <code className="flex-1 overflow-x-auto font-mono text-xs break-all whitespace-nowrap">{created.secret}</code>
        <button
          type="button"
          onClick={copy}
          className="shrink-0 rounded-md border border-line px-2 py-1 text-xs font-medium text-muted hover:text-ink"
        >
          {copied ? t('access.secret.copied') : t('access.secret.copy')}
        </button>
      </div>
      <div className="mt-5 flex justify-end">
        <button type="button" onClick={onDone} className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white hover:opacity-90">
          {t('access.secret.done')}
        </button>
      </div>
    </div>
  );
}
