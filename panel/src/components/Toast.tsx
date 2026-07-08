// Переиспользуемые тосты для результатов действий (ключ создан/отозван, mute
// поставлен/снят, деплой запущен, ошибка). Доступные: успех/инфо — role=status
// (вежливо), ошибка — role=alert (ассертивно); авто-скрытие, стопка, ручное
// закрытие. Провайдер вешается высоко (App), хук — с no-op фолбэком, чтобы
// изолированные юнит-тесты экранов монтировались без обёртки (как useLive/i18n).

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { useT } from '../lib/i18n';

export type ToastKind = 'success' | 'error' | 'info';

export interface Toast {
  id: number;
  kind: ToastKind;
  message: string;
}

export interface ToastApi {
  notify: (kind: ToastKind, message: string) => void;
  success: (message: string) => void;
  error: (message: string) => void;
  info: (message: string) => void;
  dismiss: (id: number) => void;
}

// Ошибки держим дольше (успеть прочитать), успех/инфо — короче.
const DURATION: Record<ToastKind, number> = { success: 4500, info: 4500, error: 7000 };

const ToastContext = createContext<ToastApi | null>(null);

// Фолбэк без провайдера: тосты — no-op (только для изолированных тестов).
const FALLBACK: ToastApi = {
  notify: () => {},
  success: () => {},
  error: () => {},
  info: () => {},
  dismiss: () => {},
};

export function useToast(): ToastApi {
  return useContext(ToastContext) ?? FALLBACK;
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const seq = useRef(0);
  const timers = useRef(new Map<number, ReturnType<typeof setTimeout>>());

  const dismiss = useCallback((id: number) => {
    setToasts((list) => list.filter((x) => x.id !== id));
    const timer = timers.current.get(id);
    if (timer !== undefined) {
      clearTimeout(timer);
      timers.current.delete(id);
    }
  }, []);

  const notify = useCallback(
    (kind: ToastKind, message: string) => {
      const id = ++seq.current;
      setToasts((list) => [...list, { id, kind, message }]);
      const timer = setTimeout(() => {
        dismiss(id);
      }, DURATION[kind]);
      timers.current.set(id, timer);
    },
    [dismiss],
  );

  // Чистим все таймеры при размонтировании провайдера.
  useEffect(() => {
    const map = timers.current;
    return () => {
      map.forEach((t) => {
        clearTimeout(t);
      });
      map.clear();
    };
  }, []);

  const api = useMemo<ToastApi>(
    () => ({
      notify,
      success: (m) => {
        notify('success', m);
      },
      error: (m) => {
        notify('error', m);
      },
      info: (m) => {
        notify('info', m);
      },
      dismiss,
    }),
    [notify, dismiss],
  );

  return (
    <ToastContext.Provider value={api}>
      {children}
      <ToastViewport toasts={toasts} dismiss={dismiss} />
    </ToastContext.Provider>
  );
}

const toneClass: Record<ToastKind, string> = {
  success: 'border-good/40 text-good',
  error: 'border-dead/40 text-dead',
  info: 'border-line text-accent-ink',
};

function ToastViewport({ toasts, dismiss }: { toasts: Toast[]; dismiss: (id: number) => void }) {
  const { t } = useT();
  if (toasts.length === 0) return null;
  return (
    <div className="pointer-events-none fixed inset-x-4 bottom-4 z-[60] flex flex-col gap-2 sm:inset-x-auto sm:right-4 sm:bottom-4 sm:w-80">
      {toasts.map((toast) => (
        <div
          key={toast.id}
          role={toast.kind === 'error' ? 'alert' : 'status'}
          className={`toast-item pointer-events-auto flex items-start gap-2.5 rounded-xl border bg-card px-3.5 py-2.5 text-sm shadow-lg ${toneClass[toast.kind]}`}
        >
          <ToastGlyph kind={toast.kind} />
          <span className="min-w-0 flex-1 pt-px break-words text-ink">{toast.message}</span>
          <button
            type="button"
            onClick={() => {
              dismiss(toast.id);
            }}
            aria-label={t('common.close')}
            className="-mr-1 shrink-0 rounded-md p-0.5 text-muted transition-colors hover:text-ink"
          >
            <svg viewBox="0 0 16 16" className="size-3.5" aria-hidden>
              <path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
            </svg>
          </button>
        </div>
      ))}
    </div>
  );
}

function ToastGlyph({ kind }: { kind: ToastKind }) {
  const path =
    kind === 'success' ? (
      <path d="M3.5 8.5l3 3 6-7" />
    ) : kind === 'error' ? (
      <path d="M8 4.5v4.5M8 11.5h.01" />
    ) : (
      <path d="M8 7.5v4M8 4.5h.01" />
    );
  return (
    <svg
      viewBox="0 0 16 16"
      className="mt-px size-4 shrink-0"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      {kind !== 'success' && <circle cx="8" cy="8" r="6.25" />}
      {path}
    </svg>
  );
}
