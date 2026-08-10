// Project-контекст (мультипроект W1): верхний переключатель панели. Одна
// админка обслуживает несколько проектов, и ВСЁ, что показывают экраны, живёт
// внутри выбранного — окружения, флот, версии, матчи, статистика.
//
// Отличие от env-контекста, из-за которого это отдельный провайдер, а не ещё
// одно поле в нём: у env есть режим «All» (null = без фильтра), а у проекта его
// НЕТ (решение владельца). Выбор всегда конкретный, поэтому `?project=` известен
// на каждом запросе — и прежняя деградация «master отвечает 400 several projects
// exist» больше недостижима: панель никогда не спрашивает окружения без проекта.
//
// null у `selected` означает не «все проекты», а «проекта ещё нет»: список
// грузится, не приехал, либо в базе нет ни одного проекта (свежая установка —
// проект заводится неявно первой нодой или версией, ensureProject).

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { api } from './api';
import type { ApiEvent, ProjectInfo } from './api';
import { useData } from './live';

/** Ключ localStorage для выбранного проекта (слаг). */
export const PROJECT_STORAGE_KEY = 'birdman.project';

export interface ProjectContextValue {
  /** Проекты платформы, старейший первым (порядок API). */
  projects: ProjectInfo[];
  /** Выбранный слаг; null — проектов нет / список ещё не приехал. */
  selected: string | null;
  setSelected: (slug: string) => void;
  loading: boolean;
  /** Ошибка GET /v1/projects: селектор заменяется предупреждением (Shell). */
  error?: Error;
  reload: () => void;
}

// Экспортируется для юнит-тестов (инъекция управляемого контекста, как
// EnvContext/SessionContext). В приложении — только через <ProjectProvider>.
export const ProjectContext = createContext<ProjectContextValue | null>(null);

// Фолбэк без провайдера — для изолированных юнит-тестов экранов: проектов нет,
// выбора нет. Экраны в этом режиме ведут себя как до мультипроекта.
const FALLBACK_PROJECT: ProjectContextValue = {
  projects: [],
  selected: null,
  setSelected: () => {},
  loading: false,
  reload: () => {},
};

/** Прочитать сохранённый слаг (или null). Приватный режим/мусор → null. */
export function storedProject(): string | null {
  try {
    const v = localStorage.getItem(PROJECT_STORAGE_KEY);
    return v === null || v === '' ? null : v;
  } catch {
    return null;
  }
}

/**
 * Эффективный выбор: сохранённый слаг, если такой проект ещё существует; иначе
 * ПЕРВЫЙ проект списка (он старейший — стабильный дефолт), а на пустом списке
 * null. Откат на первый, а не на null — потому что «All» у проекта не бывает:
 * панель без выбранного проекта показывать нечего.
 */
export function resolveProject(stored: string | null, projects: ProjectInfo[]): string | null {
  if (projects.length === 0) return null;
  if (stored !== null && projects.some((p) => p.slug === stored)) return stored;
  return projects[0].slug;
}

export function ProjectProvider({ children }: { children: ReactNode }) {
  const [projects, setProjects] = useState<ProjectInfo[]>([]);
  // ЖЕЛАЕМЫЙ (персистентный) выбор пользователя; эффективный `selected` ниже —
  // производный: он существует только пока такой проект есть в списке.
  const [desired, setDesired] = useState<string | null>(() => storedProject());
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error | undefined>(undefined);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api
      .listProjects()
      .then((list) => {
        if (cancelled) return;
        setProjects(list);
        setError(undefined);
        setLoading(false);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setProjects([]);
        setError(e instanceof Error ? e : new Error(String(e)));
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [reloadKey]);

  const setSelected = useCallback((slug: string) => {
    try {
      localStorage.setItem(PROJECT_STORAGE_KEY, slug);
    } catch {
      /* приватный режим — выбор не переживёт перезагрузку */
    }
    setDesired(slug);
  }, []);

  const reload = useCallback(() => {
    setReloadKey((k) => k + 1);
  }, []);

  // Персист (localStorage) при откате НЕ трогаем: проект мог исчезнуть из
  // списка временно (мастер лежит), и сохранённый выбор должен вернуться.
  const selected = useMemo(() => resolveProject(desired, projects), [desired, projects]);

  const value = useMemo(
    () => ({ projects, selected, setSelected, loading, error, reload }),
    [projects, selected, setSelected, loading, error, reload],
  );
  return <ProjectContext.Provider value={value}>{children}</ProjectContext.Provider>;
}

export function useProject(): ProjectContextValue {
  return useContext(ProjectContext) ?? FALLBACK_PROJECT;
}

/**
 * `useData` для проектных списков (мультипроект W2): отдаёт фетчеру выбранный
 * слаг и САМ добавляет проект в зависимости — смена проекта перезапрашивает
 * данные, и забыть об этом на очередном экране нельзя по построению. Пока
 * проекта нет (грузится / их вообще нет) — пустой список, а не запрос без
 * фильтра: панель не показывает чужие данные, даже мельком.
 */
export function useProjectList<T>(fetcher: (project: string) => Promise<T[]>, deps: unknown[]) {
  const ctx = useContext(ProjectContext);
  // Различаем ДВА «проекта нет», и разница принципиальная:
  //  • провайдера нет вовсе (изолированные юнит-тесты экранов) → проектного
  //    измерения не существует, сужать нечем: фетчим БЕЗ фильтра, ровно как
  //    до мультипроекта. Пустая строка отсеивается в qs(), так что параметр
  //    в запрос не попадает. Тот же приём, что у FALLBACK_ENV.
  //  • провайдер есть и говорит null (список грузится / проектов нет) →
  //    пустой список: чужие данные не показываем даже мельком.
  const project = ctx === null ? '' : ctx.selected;
  return useData<T[]>(
    () => (project === null ? Promise.resolve([]) : fetcher(project)),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- deps задаёт экран, проект добавляем мы
    [...deps, project],
  );
}

/**
 * Проект события ленты — для клиентского сужения (у таблицы `events` нет
 * project_id, а из payload проект несут только часть видов: version/deploy/
 * fleet/environment; у node/server/match-событий его нет вовсе).
 */
export function eventProjectOf(e: ApiEvent): string | undefined {
  const v = e.payload.project;
  return typeof v === 'string' ? v : undefined;
}

/**
 * Фильтр ленты по проекту — НЕ скрывающий: событие уходит, только если оно
 * ЯВНО принадлежит другому проекту. Событие без атрибуции остаётся видимым.
 *
 * Так, а не «оставить только совпадающие», потому что у проекта нет режима
 * «Все» (в отличие от env): строгий фильтр спрятал бы неатрибутированные
 * события НАВСЕГДА и ни из какого режима их было бы не достать — потеря
 * данных ради косметики. Настоящее решение — project_id в самой таблице
 * событий; до него честнее показать лишнее, чем утаить нужное.
 */
export function keepForProject(events: ApiEvent[], project: string | null): ApiEvent[] {
  if (project === null) return events;
  return events.filter((e) => {
    const p = eventProjectOf(e);
    return p === undefined || p === project;
  });
}
