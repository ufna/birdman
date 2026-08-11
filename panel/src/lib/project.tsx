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

/* eslint-disable react-refresh/only-export-components -- контекст, провайдер, useProject/useProjectList и чистые resolveProject/keepForProject — одна единица смысла. Правило про гранулярность Fast Refresh в dev-сервере, не про корректность; разносить файл по модулям ради него дороже, чем оно стоит. Политика — в eslint.config.js. */

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { api } from './api';
import type { ApiEvent, ProjectInfo } from './api';
import { useData } from './live';
import { useAsync } from './useAsync';

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
 * Слаг для сужения запроса. Различаем ДВА «проекта нет», и разница
 * принципиальная:
 *  • провайдера нет вовсе (изолированные юнит-тесты экранов) → проектного
 *    измерения не существует, сужать нечем: фетчим БЕЗ фильтра ('' отсеивается
 *    в qs(), параметр в запрос не попадает), ровно как до мультипроекта. Тот же
 *    приём, что у FALLBACK_ENV.
 *  • провайдер есть и говорит null (список грузится / проектов нет) → null:
 *    вызывающий не запрашивает ничего, чужие данные не показываем даже мельком.
 */
export function useProjectSlug(): string | null {
  const ctx = useContext(ProjectContext);
  return ctx === null ? '' : ctx.selected;
}

/**
 * Как ленте событий поступить с запросом `/v1/events`. Состояния ТРИ, и путать
 * их нельзя — обе половины приёмки эпика #968 живут именно здесь:
 *
 *  • `narrow` — проект выбран: просим `?project=`, сервер сужает сам.
 *  • `wait`   — проект НЕИЗВЕСТЕН: список проектов ещё грузится или не приехал
 *    (ошибка `/v1/projects`). Голый запрос отдал бы события ВСЕХ проектов, а
 *    панель не показывает чужое даже мельком — не спрашиваем ничего.
 *  • `all`    — сужать НЕЧЕМ, и чужого не существует: провайдера нет вовсе
 *    (изолированный юнит-тест экрана) либо проектов в базе нет ни одного —
 *    свежая установка, где проект заводится только первым касанием
 *    (`ensureProject`). Голый запрос здесь безопасен и ОБЯЗАТЕЛЕН: ровно в этом
 *    окне случаются платформенные события (создание ключа, бекап, серты), и
 *    приёмка «платформенные видны» относится в первую очередь к нему.
 *
 * Разница между `wait` и `all` — это разница между «не знаю, чьи это события»
 * и «других проектов нет»: в первом случае молчание правильно, во втором оно
 * прячет единственное, что вообще есть.
 */
export type FeedScope = { kind: 'narrow'; project: string } | { kind: 'all' } | { kind: 'wait' };

/** Ссылочно стабильный `FeedScope` — можно класть прямо в deps эффекта. */
export function useFeedScope(): FeedScope {
  const ctx = useContext(ProjectContext);
  const noProvider = ctx === null;
  const selected = ctx?.selected ?? null;
  const loading = ctx?.loading ?? false;
  const listFailed = ctx?.error !== undefined;
  return useMemo(() => {
    if (noProvider) return { kind: 'all' };
    if (selected !== null) return { kind: 'narrow', project: selected };
    // Список ещё в пути или не приехал — чей это будет срез, неизвестно.
    if (loading || listFailed) return { kind: 'wait' };
    return { kind: 'all' };
  }, [noProvider, selected, loading, listFailed]);
}

/** Слаг для фильтра событий живого стрима: `null` — резать нечем. */
export function scopeProject(scope: FeedScope): string | null {
  return scope.kind === 'narrow' ? scope.project : null;
}

/**
 * `useData` для проектных списков (мультипроект W2): отдаёт фетчеру выбранный
 * слаг и САМ добавляет проект в зависимости — смена проекта перезапрашивает
 * данные, и забыть об этом на очередном экране нельзя по построению. Пока
 * проекта нет (грузится / их вообще нет) — пустой список, а не запрос без
 * фильтра: панель не показывает чужие данные, даже мельком.
 */
export function useProjectList<T>(fetcher: (project: string) => Promise<T[]>, deps: unknown[]) {
  const project = useProjectSlug();
  return useData<T[]>(
    () => (project === null ? Promise.resolve([]) : fetcher(project)),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- deps задаёт экран, проект добавляем мы
    [...deps, project],
  );
}

/**
 * То же сужение, но поверх `useAsync` вместо `useData` — для экранов, которые
 * сознательно НЕ висят на SSE. Отличается только базовым хуком: слаг, две
 * трактовки «проекта нет» и автодобавление проекта в deps — общие.
 *
 * Зачем отдельный хук, а не useProjectList: `useData` перечитывает данные на
 * каждое событие флота (плюс поллинг 5–30с). Алерты на это подписывать нельзя —
 * это прокси к vmalert, и «алерты не должны перечитываться на каждое событие
 * флота» записано в шапке lib/useAsync.ts как решение. Взять useProjectList
 * означало бы молча поменять частоту опроса апстрима ради переиспользования.
 */
export function useProjectAsync<T>(fetcher: (project: string) => Promise<T[]>, deps: unknown[]) {
  const project = useProjectSlug();
  return useAsync<T[]>(
    () => (project === null ? Promise.resolve([]) : fetcher(project)),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- deps задаёт экран, проект добавляем мы
    [...deps, project],
  );
}

/**
 * Проект события. Берётся из ПОЛЯ, которое отдаёт мастер (колонка
 * events.project_id, эпик #968), а не угадывается по payload: раньше проект
 * несли лишь некоторые виды, и событие без него показывалось всегда.
 * Пусто — платформенное событие: бекап, ротация CA, сессия панели.
 */
export function eventProjectOf(e: ApiEvent): string | undefined {
  return e.project !== undefined && e.project !== '' ? e.project : undefined;
}

/**
 * Фильтр для событий, пришедших ЖИВЫМ СТРИМОМ.
 *
 * Список приезжает уже суженным сервером (`GET /v1/events?project=`), и
 * клиентского фильтра там больше нет. Стрим — единственное исключение, и оно
 * НЕ в том, что «сервер стрим не сужает»: с #999 сужает — но по ПРИВЯЗКЕ КЛЮЧА
 * (`GET /v1/events/stream` отдаёт события проекта, к которому привязан ключ
 * сессии). Сессия панели ходит непривязанным admin-ключом, поэтому для неё
 * стрим приходит платформенным — весь, и резать его больше нечем.
 *
 * Попросить сужение параметром панель не может: стрим один на сессию и
 * открывается в LiveProvider, который по устройству дерева стоит ВЫШЕ
 * ProjectProvider и выбранного проекта не знает. Пересобирать дерево
 * провайдеров ради этого дороже, чем оставить здесь три строки — но правило то
 * же самое, что на сервере: уходит явно чужое, платформенное остаётся.
 *
 * `null` (проекта ещё нет) и `''` (провайдера нет вовсе — изолированный
 * юнит-тест экрана) значат одно: проектного измерения не существует, резать
 * нечем. Обе трактовки — те же, что у useProjectSlug().
 */
export function keepForProject(events: ApiEvent[], project: string | null): ApiEvent[] {
  if (project === null || project === '') return events;
  return events.filter((e) => {
    const p = eventProjectOf(e);
    return p === undefined || p === project;
  });
}

