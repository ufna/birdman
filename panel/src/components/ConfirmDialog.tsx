// Подтверждение деструктивных/операционных действий (Deploy/Rollback/Drain).
// Radix AlertDialog: фокус-трап, Esc, клик по фону — как ожидается от модалки;
// действие асинхронное, с состоянием «выполняем…» и ошибкой прямо в диалоге.

import { useState } from 'react';
import type { ReactNode } from 'react';
import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { ApiError } from '../lib/api';
import { useT } from '../lib/i18n';
import type { I18nContextValue } from '../lib/i18n';

export type ActionTone = 'accent' | 'dead';

interface ConfirmButtonProps {
  label: ReactNode;
  title: string;
  description: ReactNode;
  confirmLabel: string;
  tone?: ActionTone;
  disabled?: boolean;
  onConfirm: () => Promise<void>;
  /** Доп. классы триггера (по умолчанию — компактная обводка). */
  triggerClass?: string;
  /** title-атрибут триггер-кнопки (тултип-причина дизейбла). */
  triggerTitle?: string;
  /** Локализованное сообщение для конкретной ошибки (напр. 409 last_admin_key);
   *  вернул undefined — падаем на дефолтный errMessage. */
  errorOverride?: (e: unknown) => string | undefined;
}

const actionBg: Record<ActionTone, string> = {
  accent: 'bg-accent text-white hover:opacity-90',
  dead: 'bg-dead text-white hover:opacity-90',
};

export function ConfirmButton({
  label,
  title,
  description,
  confirmLabel,
  tone = 'accent',
  disabled = false,
  onConfirm,
  triggerClass,
  triggerTitle,
  errorOverride,
}: ConfirmButtonProps) {
  const { t } = useT();
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const run = () => {
    setPending(true);
    setError(null);
    onConfirm()
      .then(() => {
        setPending(false);
        setOpen(false);
      })
      .catch((e: unknown) => {
        setPending(false);
        setError(errorOverride?.(e) ?? errMessage(e, t));
      });
  };

  return (
    <AlertDialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return; // не закрывать на время запроса
        setOpen(o);
        if (!o) setError(null);
      }}
    >
      <AlertDialog.Trigger asChild>
        <button
          type="button"
          disabled={disabled}
          title={triggerTitle}
          className={
            triggerClass ??
            'rounded-lg border border-line px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:text-ink disabled:opacity-40'
          }
        >
          {label}
        </button>
      </AlertDialog.Trigger>
      <AlertDialog.Portal>
        <AlertDialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px] data-[state=open]:animate-in" />
        <AlertDialog.Content
          className="fixed top-1/2 left-1/2 z-50 w-[min(30rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl"
          onEscapeKeyDown={(e) => {
            if (pending) e.preventDefault();
          }}
        >
          <AlertDialog.Title className="text-base font-semibold">{title}</AlertDialog.Title>
          <AlertDialog.Description asChild>
            <div className="mt-2 text-sm text-muted">{description}</div>
          </AlertDialog.Description>
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
              disabled={pending}
              onClick={run}
              className={`rounded-lg px-3 py-1.5 text-sm font-medium transition-opacity disabled:opacity-60 ${actionBg[tone]}`}
            >
              {pending ? t('common.running') : confirmLabel}
            </button>
          </div>
        </AlertDialog.Content>
      </AlertDialog.Portal>
    </AlertDialog.Root>
  );
}

function errMessage(e: unknown, t: I18nContextValue['t']): string {
  if (e instanceof ApiError) {
    if (e.status === 403) return t('confirm.err.forbidden');
    if (e.status === 409) return e.detail ?? t('confirm.err.conflict');
    return e.detail !== undefined ? `${e.code}: ${e.detail}` : e.code;
  }
  return e instanceof Error ? e.message : t('confirm.err.generic');
}
