// Env-контекст (environments v1 §8): глобальный переключатель окружения панели.
// Чипы data-driven из GET /v1/environments (non-production сначала, затем
// production, затем «All»); выбор персистится в localStorage, «All» по
// умолчанию. Фильтрует Overview/Fleet/Deploys/Matches/Stats/Cost (+Events
// клиентски) — экраны читают selected через useEnv() и сужают данные сами.
//
// Панель живёт sole-project допущением (как везде в v0): проект берём из самих
// окружений (все одного проекта) — он нужен админке Environments для create/
// patch/delete. Баннер «выберите проект» при >1 проекте — вне скоупа v1.

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { api } from './api';
import type { ApiEvent, Environment } from './api';

/** Ключ localStorage для выбранного окружения (имя env; отсутствует = «All»). */
export const ENV_STORAGE_KEY = 'birdman.env';

/** «All» — отсутствие фильтра по окружению (null-сентинел). */
export type EnvSelection = string | null;

interface EnvContextValue {
  /** Окружения проекта (non-production сначала); пусто, пока грузится/недоступно. */
  environments: Environment[];
  /** Слаг единственного проекта (sole-project) — из окружений; null, если неизвестен. */
  project: string | null;
  /** Выбранное окружение или null («All»). */
  selected: EnvSelection;
  setSelected: (env: EnvSelection) => void;
  loading: boolean;
  error?: Error;
  reload: () => void;
}

// Экспортируется для юнит-тестов (инъекция управляемого контекста, как
// SessionContext/LiveContext). В приложении — только через <EnvProvider>.
export const EnvContext = createContext<EnvContextValue | null>(null);

// Фолбэк без провайдера: окружений нет, «All». Реальное приложение всегда
// обёрнуто в <EnvProvider>; фолбэк нужен изолированным юнит-тестам экранов —
// без окружений фильтр по env — no-op (показываем всё, как в v0).
const FALLBACK_ENV: EnvContextValue = {
  environments: [],
  project: null,
  selected: null,
  setSelected: () => {},
  loading: false,
  reload: () => {},
};

/** Прочитать сохранённый выбор (или null = «All»). Приватный режим/мусор → null. */
export function storedEnv(): EnvSelection {
  try {
    const v = localStorage.getItem(ENV_STORAGE_KEY);
    return v === null || v === '' ? null : v;
  } catch {
    return null;
  }
}

/**
 * Порядок чипов: non-production сначала, затем production, внутри группы —
 * стабильно по имени (environments v1 §8). API уже отдаёт в этом порядке
 * (order by production, name), но сортируем защитно — порядок не зависит от бэка.
 */
export function orderedEnvs(environments: Environment[]): Environment[] {
  return [...environments].sort(
    (a, b) => Number(a.production) - Number(b.production) || a.name.localeCompare(b.name),
  );
}

/** Валидный выбор: сохранённое имя, если оно ещё существует в списке; иначе «All». */
export function resolveSelection(stored: EnvSelection, environments: Environment[]): EnvSelection {
  if (stored === null) return null;
  return environments.some((e) => e.name === stored) ? stored : null;
}

/** Проходит ли сущность фильтр по env: «All» (null) пропускает всё. */
export function envMatches(entityEnv: string | undefined, selected: EnvSelection): boolean {
  return selected === null || entityEnv === selected;
}

/** Отфильтровать по выбранному env через извлекатель env сущности (клиентский фильтр). */
export function keepForEnv<T>(items: T[], selected: EnvSelection, envOf: (item: T) => string | undefined): T[] {
  if (selected === null) return items;
  return items.filter((it) => envOf(it) === selected);
}

/**
 * Окружение события ленты для клиентского фильтра (environments v1 §7, M13):
 * version_promoted несёт env в payload.to_env (env := целевой), прочие — в
 * payload.env. События БЕЗ env (undefined) видны только в режиме «All».
 */
export function eventEnvOf(e: ApiEvent): string | undefined {
  const key = e.kind === 'version_promoted' ? 'to_env' : 'env';
  const v = e.payload[key];
  return typeof v === 'string' ? v : undefined;
}

export function EnvProvider({ children }: { children: ReactNode }) {
  const [environments, setEnvironments] = useState<Environment[]>([]);
  const [selected, setSelectedState] = useState<EnvSelection>(() => storedEnv());
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error | undefined>(undefined);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api
      .listEnvironments()
      .then((envs) => {
        if (cancelled) return;
        const ordered = orderedEnvs(envs);
        setEnvironments(ordered);
        // Стёртый/несуществующий сохранённый env — откат на «All».
        setSelectedState((cur) => resolveSelection(cur, ordered));
        setError(undefined);
        setLoading(false);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setError(e instanceof Error ? e : new Error(String(e)));
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [reloadKey]);

  const setSelected = useCallback((env: EnvSelection) => {
    try {
      if (env === null) localStorage.removeItem(ENV_STORAGE_KEY);
      else localStorage.setItem(ENV_STORAGE_KEY, env);
    } catch {
      /* приватный режим — выбор не переживёт перезагрузку */
    }
    setSelectedState(env);
  }, []);

  const reload = useCallback(() => {
    setReloadKey((k) => k + 1);
  }, []);

  const project = environments[0]?.project ?? null;

  const value = useMemo(
    () => ({ environments, project, selected, setSelected, loading, error, reload }),
    [environments, project, selected, setSelected, loading, error, reload],
  );
  return <EnvContext.Provider value={value}>{children}</EnvContext.Provider>;
}

export function useEnv(): EnvContextValue {
  return useContext(EnvContext) ?? FALLBACK_ENV;
}
