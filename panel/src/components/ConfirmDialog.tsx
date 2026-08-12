// Подтверждение деструктивных/операционных действий (Deploy/Rollback/Drain).
// Radix AlertDialog: фокус-трап, Esc, клик по фону — как ожидается от модалки;
// действие асинхронное, с состоянием «выполняем…» и ошибкой прямо в диалоге.

import { useState } from 'react';
import type { ReactNode } from 'react';
import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { apiErrorMessage } from '../lib/apiError';
import { useT } from '../lib/i18n';
import type { I18nContextValue } from '../lib/i18n';
import { useBindingRefusal } from '../lib/session';

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
  // 403 в диалоге действия — тот же класс лжи, что чинит #1000 на чтениях:
  // «Недостаточно прав для этого действия» привязанному ключу называет скоуп,
  // хотя отказ пришёл по привязке (`requireBinding` — deploy/rollback/promote,
  // master §6). Привязанный ключ с deploy достигает этих кнопок штатно.
  const bound = useBindingRefusal();
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
        // errorOverride ПРОКИДЫВАЕТСЯ внутрь, а не зовётся здесь (tracker #1010):
        // порядок «привязка важнее уточнения поверхности» живёт в apiErrorMessage,
        // в единственном месте, и его нельзя обойти, забыв про него на вызове.
        setError(errMessage(e, t, bound, errorOverride));
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

/** `bound` — текст отказа по привязке (useBindingRefusal) или undefined у
 *  непривязанного ключа; во втором случае остаётся прежний
 *  `confirm.err.forbidden` про права — текст ДЕЙСТВИЯ, а не чтения.
 *
 *  С #1005 тело — общий `apiErrorMessage`. Две ветки, которые тут были своими,
 *  сырую прозу мастера и уносили: 409 отдавал `e.detail` как есть, а хвост —
 *  `${code}: ${detail}`. Теперь 409 переведён своим ключом через `byStatus`,
 *  а прочие коды показывают только код. */
function errMessage(
  e: unknown,
  t: I18nContextValue['t'],
  bound: string | undefined,
  override?: (e: unknown) => string | undefined,
): string {
  return apiErrorMessage(e, t, {
    refusal: bound,
    forbidden: 'confirm.err.forbidden',
    generic: 'confirm.err.generic',
    byStatus: { 409: 'confirm.err.conflict' },
    override,
  });
}
