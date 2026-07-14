// Сворачиваемая карточка-инструкция «Как залить билд» на экране Деплоев:
// образ в ghcr → deploy-ключ → регистрация версии и деплой, с готовыми
// curl-командами (copy-кнопки, origin/project уже подставлены) и
// внутренней ссылкой на экран Админка для админа (не полный reload — тот же
// паттерн, что у навигации в Shell.tsx). Строки команд — из
// lib/deployHowto.ts; тут только раскладка и подписи через t().
//
// Task 7 (registries v1 §5) добавляет: (а) в шаге 1 — подсказку про
// приватный реестр (ссылка на Админку → Реестры для админа, текст для
// остальных); (б) в шаге 2 (admin-only) — пикер существующих deploy/admin
// ключей (выбор вставляет `# key: <name>` в обе команды) и inline-создание
// нового deploy-ключа (секрет подставляется в команды, только в state
// карточки — мирроит SecretReveal из screens/Access.tsx; «скрыть» возвращает
// плейсхолдер, но комментарий с именем остаётся).

import { useId, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { api, ApiError } from '../lib/api';
import type { ApiKey } from '../lib/api';
import {
  defaultDeployKeyName,
  deployCurl,
  deployKeyOptions,
  registerVersionCurl,
} from '../lib/deployHowto';
import type { HowtoCtx, HowtoKey } from '../lib/deployHowto';
import { canAdmin, useSession } from '../lib/session';
import { useEnv } from '../lib/env';
import { useAsync } from '../lib/useAsync';
import { useT } from '../lib/i18n';
import { useToast } from './Toast';
import { Card, CardHeader } from './ui';

const EXAMPLE_SEMVER = '1.2.3';
const STUB_WORKFLOW_PATH = '.github/workflows/stub-server.yml';
/** Пример env для curl, когда конкретный env не выбран и окружений ещё нет. */
const EXAMPLE_ENV_FALLBACK = 'dev';

export function DeployHowto({
  ctx,
  navigate,
  defaultExpanded = false,
}: {
  ctx: HowtoCtx;
  /** Внутренняя навигация без полного reload (см. lib/usePath.ts). */
  navigate: (path: string) => void;
  defaultExpanded?: boolean;
}) {
  const { t } = useT();
  const toast = useToast();
  const { session } = useSession();
  const isAdmin = session != null && canAdmin(session);
  const { selected, environments } = useEnv();
  // env для примера curl: выбранный env → первый non-production env → 'dev'.
  // Так команда всегда рабочая (env обязателен в POST /v1/versions).
  const exampleEnv = selected ?? environments.find((e) => !e.production)?.name ?? environments[0]?.name ?? EXAMPLE_ENV_FALLBACK;
  const [expanded, setExpanded] = useState(defaultExpanded);
  const contentId = useId();

  // --- Task 7 §5: пикер существующих ключей + inline-создание нового ---
  // Фетчим /v1/apikeys только для admin И только когда карточка развёрнута
  // (пикер иначе не виден) — не тратим admin-scoped запрос на схлопнутую
  // карточку. pickedKeyId — id из <select>; newKeyName/newKeySecret — ключ,
  // созданный ПРЯМО СЕЙЧАС (secret известен только до «скрыть»). Выбор в
  // селекте и создание взаимно исключают друг друга (см. handlePick/handleCreate).
  const keysState = useAsync(
    () => (isAdmin && expanded ? api.listApiKeys() : Promise.resolve<ApiKey[]>([])),
    [isAdmin, expanded],
  );
  const keyOptions = useMemo(() => deployKeyOptions(keysState.data ?? []), [keysState.data]);
  const [pickedKeyId, setPickedKeyId] = useState('');
  const [newKeyName, setNewKeyName] = useState<string | null>(null);
  const [newKeySecret, setNewKeySecret] = useState<string | null>(null);
  const [createName, setCreateName] = useState(() => defaultDeployKeyName());
  const [creating, setCreating] = useState(false);
  const [createErr, setCreateErr] = useState<string | null>(null);

  const pickedKey = keyOptions.find((k) => k.id === pickedKeyId);
  const effectiveKey: HowtoKey | undefined =
    newKeyName !== null
      ? { name: newKeyName, secret: newKeySecret ?? undefined }
      : pickedKey !== undefined
        ? { name: pickedKey.name }
        : undefined;

  const handlePick = (id: string) => {
    setPickedKeyId(id);
    setNewKeyName(null);
    setNewKeySecret(null);
  };

  const handleCreate = () => {
    if (creating) return;
    const name = createName.trim();
    if (name === '') return;
    setCreating(true);
    setCreateErr(null);
    api
      .createApiKey(name, ['deploy'])
      .then((res) => {
        setCreating(false);
        setPickedKeyId('');
        setNewKeyName(res.key.name);
        setNewKeySecret(res.secret);
        toast.success(t('deploys.howto.key.toastCreated', { name: res.key.name }));
        keysState.reload();
      })
      .catch((e: unknown) => {
        setCreating(false);
        setCreateErr(e instanceof ApiError ? (e.detail ?? e.code) : t('deploys.howto.key.createErr'));
      });
  };

  const handleHideSecret = () => {
    setNewKeySecret(null);
  };

  return (
    <Card>
      <CardHeader
        title={t('deploys.howto.title')}
        aside={
          <button
            type="button"
            aria-expanded={expanded}
            aria-controls={contentId}
            onClick={() => {
              setExpanded((e) => !e);
            }}
            className="rounded-md border border-line px-2 py-1 text-xs font-medium text-muted hover:text-ink"
          >
            {expanded ? t('deploys.howto.collapse') : t('deploys.howto.expand')}
          </button>
        }
      />
      {expanded && (
        <div id={contentId} className="flex flex-col gap-5 p-4 text-sm">
          <Step title={t('deploys.howto.step1.title')}>
            <p className="text-muted">{t('deploys.howto.step1.desc')}</p>
            <p className="font-mono text-xs break-all">{ctx.exampleImage}</p>
            <p className="text-xs text-muted">
              {t('deploys.howto.step1.stubNote')} <code className="font-mono">{STUB_WORKFLOW_PATH}</code>
            </p>
            <p className="text-xs text-muted">{t('deploys.howto.step1.pullNote')}</p>
            {isAdmin ? (
              <p>
                <a
                  href="/access"
                  onClick={(e) => {
                    e.preventDefault();
                    navigate('/access');
                  }}
                  className="font-medium text-accent-ink hover:underline"
                >
                  {t('deploys.howto.step1.registryLink')}
                </a>
              </p>
            ) : (
              <p className="text-muted">{t('deploys.howto.step1.registryHint')}</p>
            )}
          </Step>

          <Step title={t('deploys.howto.step2.title')}>
            <p className="text-muted">{t('deploys.howto.step2.desc')}</p>
            {isAdmin ? (
              <p>
                <a
                  href="/access"
                  onClick={(e) => {
                    e.preventDefault();
                    navigate('/access');
                  }}
                  className="font-medium text-accent-ink hover:underline"
                >
                  {t('deploys.howto.step2.adminLink')}
                </a>
              </p>
            ) : (
              <p className="text-muted">{t('deploys.howto.step2.readonlyHint')}</p>
            )}
            <p className="text-xs text-muted">{t('deploys.howto.step2.secretNote')}</p>
            {isAdmin && (
              <DeployKeySection
                keyOptions={keyOptions}
                pickedKeyId={pickedKeyId}
                onPick={handlePick}
                createName={createName}
                onCreateNameChange={setCreateName}
                onCreate={handleCreate}
                creating={creating}
                createErr={createErr}
                newKeySecret={newKeySecret}
                onHideSecret={handleHideSecret}
              />
            )}
          </Step>

          <Step title={t('deploys.howto.step3.title')}>
            <p className="text-muted">{t('deploys.howto.step3.registerLabel')}</p>
            <CodeBlock
              code={registerVersionCurl(ctx, EXAMPLE_SEMVER, exampleEnv, effectiveKey)}
              testId="howto-register-cmd"
              copyLabel={t('deploys.howto.copyRegister')}
              copiedLabel={t('deploys.howto.copied')}
            />
            <p className="text-muted">{t('deploys.howto.step3.deployLabel')}</p>
            <CodeBlock
              code={deployCurl(ctx, effectiveKey)}
              testId="howto-deploy-cmd"
              copyLabel={t('deploys.howto.copyDeploy')}
              copiedLabel={t('deploys.howto.copied')}
            />
            <p className="text-xs text-muted">{t('deploys.howto.step3.localNote')}</p>
            <p className="text-xs text-muted">{t('deploys.howto.step3.tableNote')}</p>
          </Step>
        </div>
      )}
    </Card>
  );
}

/**
 * Шаг 2, admin-only (Task 7 §5): select существующих deploy/admin ключей
 * (выбор → имя вставляется комментарием в обе curl-команды родителя) +
 * inline-создание нового (имя-инпут с дефолтом deploy-YYYYMMDD → кнопка
 * создаёт ключ, свежий секрет тут же подставляется в команды). Пока
 * newKeySecret не null — вместо формы создания показан warning + «скрыть
 * секрет» (секрет уходит из commands, комментарий с именем остаётся).
 */
function DeployKeySection({
  keyOptions,
  pickedKeyId,
  onPick,
  createName,
  onCreateNameChange,
  onCreate,
  creating,
  createErr,
  newKeySecret,
  onHideSecret,
}: {
  keyOptions: ApiKey[];
  pickedKeyId: string;
  onPick: (id: string) => void;
  createName: string;
  onCreateNameChange: (v: string) => void;
  onCreate: () => void;
  creating: boolean;
  createErr: string | null;
  newKeySecret: string | null;
  onHideSecret: () => void;
}) {
  const { t } = useT();
  return (
    <div className="flex flex-col gap-2 rounded-lg border border-line bg-paper p-3">
      <label className="flex flex-col gap-1 text-xs font-medium text-muted">
        {t('deploys.howto.key.pickLabel')}
        <select
          value={pickedKeyId}
          onChange={(e) => {
            onPick(e.target.value);
          }}
          className="rounded-lg border border-line bg-card px-2 py-1.5 text-sm font-normal text-ink"
        >
          <option value="">{t('deploys.howto.key.pickPlaceholder')}</option>
          {keyOptions.map((k) => (
            <option key={k.id} value={k.id}>
              {k.name}
            </option>
          ))}
        </select>
      </label>
      {newKeySecret !== null ? (
        <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg bg-dead-bg px-3 py-2">
          <p className="text-xs text-dead">{t('deploys.howto.key.secretWarning')}</p>
          <button
            type="button"
            onClick={onHideSecret}
            className="shrink-0 rounded-md border border-line px-2 py-1 text-xs font-medium text-muted hover:text-ink"
          >
            {t('deploys.howto.key.hideSecret')}
          </button>
        </div>
      ) : (
        <div className="flex flex-wrap items-end gap-2">
          <label className="flex flex-col gap-1 text-xs font-medium text-muted">
            {t('deploys.howto.key.nameLabel')}
            <input
              value={createName}
              onChange={(e) => {
                onCreateNameChange(e.target.value);
              }}
              className="rounded-lg border border-line bg-card px-2 py-1.5 text-sm font-normal text-ink"
            />
          </label>
          <button
            type="button"
            disabled={creating || createName.trim() === ''}
            onClick={onCreate}
            className="rounded-lg border border-line px-2.5 py-1.5 text-xs font-medium text-muted hover:text-ink disabled:opacity-40"
          >
            {creating ? t('common.running') : t('deploys.howto.key.createButton')}
          </button>
        </div>
      )}
      {createErr !== null && (
        <p role="alert" className="text-xs text-dead">
          {createErr}
        </p>
      )}
    </div>
  );
}

function Step({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5">
      <h3 className="text-sm font-semibold">{title}</h3>
      {children}
    </div>
  );
}

/** Код-блок с copy-кнопкой — идиома из screens/Access.tsx (SecretReveal). */
function CodeBlock({
  code,
  testId,
  copyLabel,
  copiedLabel,
}: {
  code: string;
  testId: string;
  copyLabel: string;
  copiedLabel: string;
}) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    void navigator.clipboard?.writeText(code).then(
      () => {
        setCopied(true);
      },
      () => {
        /* clipboard недоступен — команда всё равно видна для ручного копирования */
      },
    );
  };
  return (
    <div data-testid={testId} className="relative rounded-lg border border-line bg-paper">
      <pre className="overflow-x-auto p-3 pr-24 font-mono text-xs whitespace-pre">{code}</pre>
      <button
        type="button"
        onClick={copy}
        className="absolute top-2 right-2 shrink-0 rounded-md border border-line bg-card px-2 py-1 text-xs font-medium text-muted hover:text-ink"
      >
        {copied ? copiedLabel : copyLabel}
      </button>
    </div>
  );
}
