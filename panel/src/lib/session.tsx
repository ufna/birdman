// Сессия панели: cookie-логин по API-ключу (POST /v1/session), скоупы — из
// ключа. При старте пробуем существующую cookie (GET /v1/session).

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { api, ApiError } from './api';
import type { SessionInfo } from './api';
import type { I18nContextValue } from './i18n';

interface SessionContextValue {
  /** null — не залогинен; undefined — ещё проверяем cookie. */
  session: SessionInfo | null | undefined;
  login: (apiKey: string) => Promise<void>;
  logout: () => Promise<void>;
  /** Сброс без запроса — когда API ответил 401 (сессия истекла). */
  invalidate: () => void;
}

const SessionContext = createContext<SessionContextValue | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<SessionInfo | null | undefined>(undefined);

  useEffect(() => {
    let cancelled = false;
    api
      .getSession()
      .then((s) => {
        if (!cancelled) setSession(s);
      })
      .catch(() => {
        if (!cancelled) setSession(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const login = useCallback(async (apiKey: string) => {
    const s = await api.createSession(apiKey.trim());
    setSession(s);
  }, []);

  const logout = useCallback(async () => {
    try {
      await api.deleteSession();
    } catch {
      /* logout идемпотентен: локально выходим в любом случае */
    }
    setSession(null);
  }, []);

  const invalidate = useCallback(() => {
    setSession(null);
  }, []);

  const value = useMemo(
    () => ({ session, login, logout, invalidate }),
    [session, login, logout, invalidate],
  );
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionContextValue {
  const ctx = useContext(SessionContext);
  if (ctx === null) throw new Error('useSession must be used within a SessionProvider');
  return ctx;
}

/** true, если у сессии есть доступ на чтение (readonly или admin). */
export function canRead(s: SessionInfo): boolean {
  return s.scopes.includes('readonly') || s.scopes.includes('admin');
}

/**
 * true, если сессия может деплоить/откатывать (скоуп deploy или admin —
 * admin включает остальные скоупы, master §6). Гейтит кнопки Deploy/Rollback.
 */
export function canDeploy(s: SessionInfo): boolean {
  return s.scopes.includes('deploy') || s.scopes.includes('admin');
}

/** true, если сессия admin — гейтит операции с тачкой (Drain/Undrain). */
export function canAdmin(s: SessionInfo): boolean {
  return s.scopes.includes('admin');
}

/** Человеческое сообщение об ошибке логина (локализуется через переданный t). */
export function loginErrorMessage(e: unknown, t: I18nContextValue['t']): string {
  if (e instanceof ApiError) {
    if (e.status === 401) return t('login.err.badKey');
    return e.detail !== undefined ? `${e.code}: ${e.detail}` : e.code;
  }
  return t('login.err.unreachable');
}
