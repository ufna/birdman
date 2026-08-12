// Сессия панели: cookie-логин по API-ключу (POST /v1/session), скоупы — из
// ключа. При старте пробуем существующую cookie (GET /v1/session).

/* eslint-disable react-refresh/only-export-components -- контекст, провайдер, useSession и чистые предикаты прав canRead/canDeploy/canAdmin. Правило про гранулярность Fast Refresh в dev-сервере, не про корректность; разносить файл по модулям ради него дороже, чем оно стоит. Политика — в eslint.config.js. */

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { api, ApiError } from './api';
import type { KeyBinding, Scope, SessionInfo } from './api';
import { apiErrorMessage } from './apiError';
import { useT } from './i18n';
import type { I18nContextValue } from './i18n';

interface SessionContextValue {
  /** null — не залогинен; undefined — ещё проверяем cookie. */
  session: SessionInfo | null | undefined;
  login: (apiKey: string) => Promise<void>;
  logout: () => Promise<void>;
  /** Сброс без запроса — когда API ответил 401 (сессия истекла). */
  invalidate: () => void;
}

// Экспортируется для юнит-тестов (инъекция управляемой сессии, как LiveContext).
// В приложении используется только через <SessionProvider>/useSession.
export const SessionContext = createContext<SessionContextValue | null>(null);

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

// Фолбэк без провайдера: «не залогинен» (session=null), операции — no-op.
// Реальное приложение всегда обёрнуто в <SessionProvider>; фолбэк нужен
// изолированным юнит-тестам экранов, которые гейтят кнопки по скоупу (как
// FALLBACK_LIVE в live.tsx): без сессии admin-действия просто скрыты.
const FALLBACK_SESSION: SessionContextValue = {
  session: null,
  login: async () => {},
  logout: async () => {},
  invalidate: () => {},
};

export function useSession(): SessionContextValue {
  return useContext(SessionContext) ?? FALLBACK_SESSION;
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

/**
 * Привязка ключа текущей сессии или undefined, если ключ глобальный (а также
 * пока сессия не загружена / её нет). Master отдаёт поле только привязанному
 * ключу — master.md §6 «Сессия сообщает привязку ключа».
 */
export function useKeyBinding(): KeyBinding | undefined {
  return useSession().session?.binding;
}

/**
 * Текст отказа 403 ПО ПРИВЯЗКЕ — или undefined, если ключ сессии не привязан
 * (tracker #1000).
 *
 * До #1000 панель знала о сессии только скоупы и потому объясняла ЛЮБОЙ 403
 * нехваткой скоупа: привязанному readonly-ключу она говорила «нужен ключ со
 * скоупом readonly или admin», хотя readonly у него есть, — локализованный, но
 * ложный диагноз. Теперь причина берётся из сессии.
 *
 * Возвращает undefined, а НЕ готовый запасной текст, нарочно: у каждой
 * поверхности свой правильный запасной вариант. У чтений это `ui.err.forbidden`
 * («нужен readonly или admin»), у диалога действия — `confirm.err.forbidden`
 * («недостаточно прав для этого действия»), и подставить туда первый значило бы
 * заменить одну ложь другой: действию нужен deploy/admin, а не readonly.
 * Поэтому вызывающий пишет `useBindingRefusal() ?? t(<свой ключ>)`.
 *
 * Формулировка намеренно говорит про КЛЮЧ, а не про «логи/метрики привязанным
 * недоступны»: серверное сужение (#994) вернёт привязанному оператору его
 * собственные данные, и текст обязан пережить это, не превратившись во вторую
 * ложь.
 *
 * `need` — скоуп, которого ТРЕБУЕТ эта поверхность (tracker #1022). Без него
 * хук объяснял привязкой ЛЮБОЙ 403 у привязанного ключа, хотя master отдаёт
 * 403 из ДВУХ независимых мест, и второе про скоупы:
 *
 *   1. `requireScope` (`master/internal/httpapi/auth.go:120-138`) —
 *      `403 forbidden "scope <name> required"`;
 *   2. `narrowScope` fail-closed — `403 forbidden "key is bound to …"`.
 *
 * Различить их панель МОЖЕТ, и не по прозе `detail` (её в UI не носят, #996), а
 * по ПОРЯДКУ гейтов: `requireScope` — обёртка НАД хендлером, то есть работает
 * раньше `narrowScope`, и пускает при наличии либо запрошенного скоупа, либо
 * `admin` — ровно то, что считают canRead/canDeploy/canAdmin рядом. Значит нет
 * нужного скоупа ⇒ 403 пришёл от первого гейта и привязка ни при чём; скоуп
 * есть ⇒ первый гейт пройден и остаётся только второй. Ключу, который И
 * привязан, И без readonly, панель до этого говорила «ключ привязан к
 * game/dev», и оператор шёл менять привязку вместо того, чтобы выдать себе
 * readonly.
 *
 * Дефолт `readonly` — для ЧТЕНИЙ (их большинство); поверхность ДЕЙСТВИЯ обязана
 * попросить свой (`deploy`, `admin`), иначе снова назовёт не ту причину.
 */
export function useBindingRefusal(need: Scope = 'readonly'): string | undefined {
  const { t } = useT();
  const { session } = useSession();
  if (session == null) return undefined;
  const binding = session.binding;
  if (binding === undefined) return undefined;
  // Скоупа нет — честная причина отказа СКОУП, а не привязка (см. выше).
  if (!session.scopes.includes(need) && !session.scopes.includes('admin')) return undefined;
  return t('ui.err.boundKey', { project: binding.project, env: binding.env });
}

/**
 * Человеческое сообщение об ошибке логина (локализуется через переданный t).
 *
 * 401 тут значит НЕ «сессия истекла», а «ключ не подошёл» — сессии ещё нет,
 * поэтому общий текст `ui.err.expired` был бы ложью, и ветка остаётся своей.
 * Всё остальное с #1005 идёт общим путём: раньше хвост печатал
 * `${code}: ${detail}` — сырую прозу мастера прямо на экране логина.
 */
export function loginErrorMessage(e: unknown, t: I18nContextValue['t']): string {
  if (e instanceof ApiError && e.status === 401) return t('login.err.badKey');
  return apiErrorMessage(e, t, { generic: 'login.err.unreachable' });
}
