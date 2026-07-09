// Сворачиваемая карточка-инструкция «Как залить билд» на экране Деплоев:
// образ в ghcr → deploy-ключ → регистрация версии и деплой, с готовыми
// curl-командами (copy-кнопки, origin/project уже подставлены) и
// внутренней ссылкой на экран Доступ для админа (не полный reload — тот же
// паттерн, что у навигации в Shell.tsx). Строки команд — из
// lib/deployHowto.ts; тут только раскладка и подписи через t().

import { useId, useState } from 'react';
import type { ReactNode } from 'react';
import { deployCurl, registerVersionCurl } from '../lib/deployHowto';
import type { HowtoCtx } from '../lib/deployHowto';
import { canAdmin, useSession } from '../lib/session';
import { useT } from '../lib/i18n';
import { Card, CardHeader } from './ui';

const EXAMPLE_SEMVER = '1.2.3';
const STUB_WORKFLOW_PATH = '.github/workflows/stub-server.yml';

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
  const { session } = useSession();
  const isAdmin = session != null && canAdmin(session);
  const [expanded, setExpanded] = useState(defaultExpanded);
  const contentId = useId();

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
          </Step>

          <Step title={t('deploys.howto.step3.title')}>
            <p className="text-muted">{t('deploys.howto.step3.registerLabel')}</p>
            <CodeBlock
              code={registerVersionCurl(ctx, EXAMPLE_SEMVER, 'prod')}
              testId="howto-register-cmd"
              copyLabel={t('deploys.howto.copyRegister')}
              copiedLabel={t('deploys.howto.copied')}
            />
            <p className="text-muted">{t('deploys.howto.step3.deployLabel')}</p>
            <CodeBlock
              code={deployCurl(ctx)}
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
