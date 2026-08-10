// Env-контекст (environments v1 §8): глобальный переключатель окружения панели.
// Чипы data-driven из GET /v1/environments (non-production сначала, затем
// production, затем «All»); выбор персистится в localStorage, «All» по
// умолчанию. Фильтрует Overview/Fleet/Deploys/Matches/Stats/Cost (+Events
// клиентски) — экраны читают selected через useEnv() и сужают данные сами.
//
// Окружения живут ВНУТРИ проекта, поэтому этот провайдер вложен в
// ProjectProvider (мультипроект W1) и запрашивает список ВСЕГДА с ?project=
// выбранного проекта, перезапрашивая его при смене. Прежнее sole-project
// допущение снято: `project` больше не выводится из самих окружений, а
// приходит из ProjectContext — иначе админка Environments и привязка ключей
// в Access не знали бы, к какому проекту относится их правка.

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { api } from './api';
import type { ApiEvent, Environment } from './api';
import { useProject } from './project';

/** Ключ localStorage для выбранного окружения (имя env; отсутствует = «All»). */
export const ENV_STORAGE_KEY = 'birdman.env';

/** «All» — отсутствие фильтра по окружению (null-сентинел). */
export type EnvSelection = string | null;

interface EnvContextValue {
  /** Окружения проекта (non-production сначала); пусто, пока грузится/недоступно. */
  environments: Environment[];
  /** Слаг ВЫБРАННОГО проекта (из ProjectContext); null, пока проекта нет. */
  project: string | null;
  /**
   * ЭФФЕКТИВНЫЙ выбор: имя окружения или null («All»). Пока окружений нет
   * (грузятся / список недоступен / их вообще нет) — всегда null: фильтр не
   * смеет молча резать экраны по списку, которого мы не видим (follow-up p2).
   * Персистентный выбор при этом цел и вернётся, как только список приедет.
   */
  selected: EnvSelection;
  setSelected: (env: EnvSelection) => void;
  loading: boolean;
  /** Ошибка GET /v1/environments: чипы заменяются предупреждением (Shell). */
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
  const { selected: project, loading: projectLoading } = useProject();
  const [environments, setEnvironments] = useState<Environment[]>([]);
  // ЖЕЛАЕМЫЙ (персистентный) выбор пользователя. Эффективный `selected` ниже —
  // производный: без списка окружений он всегда «All».
  const [desired, setDesired] = useState<EnvSelection>(() => storedEnv());
  const [envLoading, setEnvLoading] = useState(true);
  const [error, setError] = useState<Error | undefined>(undefined);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    // Проекта нет (грузится / их вообще нет) — спрашивать окружения не у кого.
    // Это НЕ ошибка: чипов просто нет, а «загружаемся» скажет projectLoading.
    if (project === null) {
      setEnvironments([]);
      setError(undefined);
      setEnvLoading(false);
      return;
    }
    let cancelled = false;
    setEnvLoading(true);
    api
      .listEnvironments(project)
      .then((envs) => {
        if (cancelled) return;
        const ordered = orderedEnvs(envs);
        setEnvironments(ordered);
        // Стёртый/несуществующий сохранённый env — откат на «All». Сюда же
        // попадает смена проекта: у нового проекта свои окружения, и env,
        // которого в нём нет, молча не должен резать экраны.
        setDesired((cur) => resolveSelection(cur, ordered));
        setError(undefined);
        setEnvLoading(false);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        // Список недоступен → окружений мы не знаем: чипы уходят, а фильтр
        // деградирует в «All» (см. derived `selected`) — данные не режем молча.
        setEnvironments([]);
        setError(e instanceof Error ? e : new Error(String(e)));
        setEnvLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [reloadKey, project]);

  const setSelected = useCallback((env: EnvSelection) => {
    try {
      if (env === null) localStorage.removeItem(ENV_STORAGE_KEY);
      else localStorage.setItem(ENV_STORAGE_KEY, env);
    } catch {
      /* приватный режим — выбор не переживёт перезагрузку */
    }
    setDesired(env);
  }, []);

  const reload = useCallback(() => {
    setReloadKey((k) => k + 1);
  }, []);

  // Нет списка (грузится / недоступен / пуст) → «All». Есть — сохранённый выбор,
  // если такое окружение ещё существует. Персист (localStorage) при деградации
  // НЕ трогаем: список вернётся — вернётся и выбор.
  const selected = useMemo(
    () => (environments.length === 0 ? null : resolveSelection(desired, environments)),
    [environments, desired],
  );
  // «Грузимся» — это и загрузка проектов тоже: пока их нет, окружения не
  // запрашиваются вовсе, и без этого слагаемого панель отрапортовала бы
  // готовность, ничего ещё не показав.
  const loading = projectLoading || envLoading;

  const value = useMemo(
    () => ({ environments, project, selected, setSelected, loading, error, reload }),
    [environments, project, selected, setSelected, loading, error, reload],
  );
  return <EnvContext.Provider value={value}>{children}</EnvContext.Provider>;
}

export function useEnv(): EnvContextValue {
  return useContext(EnvContext) ?? FALLBACK_ENV;
}
