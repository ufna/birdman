// Логин: API-ключ → POST /v1/session → HttpOnly cookie. Ключ нигде не
// сохраняется на клиенте.

import { useState } from 'react';
import type { FormEvent } from 'react';
import { loginErrorMessage, useSession } from '../lib/session';
import { Brand, Card } from '../components/ui';

export function Login() {
  const { login } = useSession();
  const [key, setKey] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (key.trim() === '' || pending) return;
    setPending(true);
    setError(null);
    login(key).catch((err: unknown) => {
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
            <input
              id="api-key"
              type="password"
              autoFocus
              autoComplete="off"
              spellCheck={false}
              value={key}
              onChange={(e) => {
                setKey(e.target.value);
              }}
              placeholder="bmk_…"
              className="rounded-lg border border-line bg-paper px-3 py-2 font-mono text-sm placeholder:text-muted"
            />
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
