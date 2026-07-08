// Логин: API-ключ → POST /v1/session → HttpOnly cookie. Ключ нигде не
// сохраняется на клиенте.

import { useState } from 'react';
import type { FormEvent } from 'react';
import { loginErrorMessage, useSession } from '../lib/session';
import { Brand, Card } from '../components/ui';

export function Login() {
  const { login } = useSession();
  const [key, setKey] = useState('');
  const [reveal, setReveal] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = key.trim(); // paste artifacts (trailing \n/space) → spurious 401
    if (trimmed === '' || pending) return;
    setPending(true);
    setError(null);
    login(trimmed).catch((err: unknown) => {
      setError(loginErrorMessage(err));
      setPending(false);
    });
  };

  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <div className="w-full max-w-sm">
        <div className="mb-5 flex justify-center">
          <Brand />
        </div>
        <Card className="p-5">
          <form onSubmit={submit} className="flex flex-col gap-3">
            <label htmlFor="api-key" className="text-sm font-medium">
              API-ключ
            </label>
            <div className="relative">
              <input
                id="api-key"
                type={reveal ? 'text' : 'password'}
                autoFocus
                autoComplete="off"
                spellCheck={false}
                value={key}
                onChange={(e) => {
                  setKey(e.target.value);
                }}
                placeholder="bmk_…"
                className="w-full rounded-lg border border-line bg-paper px-3 py-2 pr-10 font-mono text-sm placeholder:text-muted"
              />
              <button
                type="button"
                onClick={() => {
                  setReveal((v) => !v);
                }}
                aria-label={reveal ? 'Скрыть ключ' : 'Показать ключ'}
                aria-pressed={reveal}
                title={reveal ? 'Скрыть ключ' : 'Показать ключ'}
                className="absolute inset-y-0 right-0 flex w-10 items-center justify-center text-muted hover:text-ink"
              >
                {reveal ? <EyeOff /> : <Eye />}
              </button>
            </div>
            {error !== null && (
              <p role="alert" className="rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
                {error}
              </p>
            )}
            <button
              type="submit"
              disabled={pending || key.trim() === ''}
              className="rounded-lg bg-accent px-3 py-2 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
            >
              {pending ? 'Проверяем…' : 'Войти'}
            </button>
          </form>
        </Card>
        <p className="mt-4 text-center text-xs text-muted">
          Нужен ключ со скоупом <span className="font-mono">readonly</span> или{' '}
          <span className="font-mono">admin</span>. Первый admin-ключ master печатает в лог при
          старте с пустой таблицей ключей.
        </p>
      </div>
    </div>
  );
}

// Инлайновые иконки (без внешних зависимостей — CSP-safe).
function Eye() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z" />
      <circle cx="12" cy="12" r="3" />
    </svg>
  );
}

function EyeOff() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M9.9 4.24A9.1 9.1 0 0 1 12 4c6.5 0 10 7 10 7a13.2 13.2 0 0 1-1.67 2.4M6.6 6.6C3.9 8.24 2 12 2 12s3.5 7 10 7a9.1 9.1 0 0 0 3.4-.66" />
      <path d="M9.9 9.9a3 3 0 0 0 4.2 4.2" />
      <path d="m2 2 20 20" />
    </svg>
  );
}
