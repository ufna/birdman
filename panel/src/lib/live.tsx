// Live-слой: одна SSE-подписка на приложение (lib/sse.ts) + шина событий
// для экранов. Данные обновляются по событиям стрима, а при упавшем стриме
// экраны переходят на частый поллинг (см. useData ниже).

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react';
import type { ReactNode } from 'react';
import { subscribeEvents } from './sse';
import type { StreamEvent, StreamStatus } from './sse';
import { useSession } from './session';

type Listener = (e: StreamEvent) => void;

interface LiveContextValue {
  status: StreamStatus;
  /** Подписка на события стрима; возвращает отписку. */
  subscribe: (fn: Listener) => () => void;
}

// Экспортируется для юнит-тестов (инъекция управляемого subscribe, как FALLBACK
// в i18n). В приложении используется только через <LiveProvider>/useLive.
export const LiveContext = createContext<LiveContextValue | null>(null);

export function LiveProvider({ children }: { children: ReactNode }) {
  const { session, invalidate } = useSession();
  const [status, setStatus] = useState<StreamStatus>('connecting');
  const listeners = useRef<Set<Listener>>(new Set());

  useEffect(() => {
    if (session === null || session === undefined) return;
    const unsubscribe = subscribeEvents({
      onEvent: (e) => {
        listeners.current.forEach((fn) => {
          fn(e);
        });
      },
      onStatus: (s) => {
        setStatus(s);
        if (s === 'unauthorized') invalidate();
      },
    });
    return unsubscribe;
  }, [session, invalidate]);

  const subscribe = useCallback((fn: Listener) => {
    listeners.current.add(fn);
    return () => {
      listeners.current.delete(fn);
    };
  }, []);

  const value = useMemo(() => ({ status, subscribe }), [status, subscribe]);
  return <LiveContext.Provider value={value}>{children}</LiveContext.Provider>;
}

// Фолбэк без провайдера: стрим «недоступен», подписка — no-op. Реальное
// приложение всегда обёрнуто в <LiveProvider>; фолбэк нужен изолированным
// юнит-тестам экранов (как FALLBACK в i18n) — данные грузятся по deps, живого
// рефетча просто нет.
const FALLBACK_LIVE: LiveContextValue = { status: 'down', subscribe: () => () => {} };

export function useLive(): LiveContextValue {
  return useContext(LiveContext) ?? FALLBACK_LIVE;
}

interface DataState<T> {
  data?: T;
  error?: Error;
  loading: boolean;
  /** Время (мс) последнего УСПЕШНОГО чтения — для индикатора «обновлено N назад». */
  updatedAt?: number;
}

/**
 * Данные экрана: загрузка сразу, refetch по каждому SSE-событию (с
 * коалесингом 400мс) и страховочный поллинг — редкий при живом стриме,
 * частый при упавшем. Прошлые данные не сбрасываются на время refetch.
 */
export function useData<T>(fetcher: () => Promise<T>, deps: unknown[]): DataState<T> & { reload: () => void } {
  const { status, subscribe } = useLive();
  const [state, setState] = useState<DataState<T>>({ loading: true });
  const load = useCallback(() => {
    let stale = false;
    fetcher()
      .then((data) => {
        if (!stale) setState({ data, loading: false, updatedAt: Date.now() });
      })
      .catch((error: unknown) => {
        if (!stale)
          setState((s) => ({
            ...s,
            error: error instanceof Error ? error : new Error(String(error)),
            loading: false,
          }));
      });
    return () => {
      stale = true;
    };
    // deps задаёт вызывающий экран (фильтры и т.п.)
  }, deps);

  useEffect(() => load(), [load]);

  // SSE: любое событие флота — повод перечитать (коалесинг всплесков).
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | null = null;
    const unsubscribe = subscribe(() => {
      if (timer !== null) return;
      timer = setTimeout(() => {
        timer = null;
        load();
      }, 400);
    });
    return () => {
      unsubscribe();
      if (timer !== null) clearTimeout(timer);
    };
  }, [subscribe, load]);

  // Страховочный поллинг: SSE жив — раз в 30с, упал — раз в 5с.
  useEffect(() => {
    const interval = status === 'live' ? 30000 : 5000;
    const t = setInterval(load, interval);
    return () => {
      clearInterval(t);
    };
  }, [status, load]);

  const reload = useCallback(() => {
    load();
  }, [load]);
  return { ...state, reload };
}

/**
 * Как useAsync (период/фильтр в deps, прошлые данные не сбрасываются), НО с
 * мягким живым обновлением: любое SSE-событие флота дозапрашивает данные с
 * КРУПНЫМ дебансом (по умолчанию 3с). Для Stats: новый матч/изменение флота
 * подтягивает свежие агрегаты, но всплеск событий коалесится в один запрос —
 * не спамим тяжёлыми /v1/stats/*. Отдельного страховочного поллинга нет:
 * ручной селектор периода + SSE достаточно для агрегатов.
 */
export function useLiveAsync<T>(
  fetcher: () => Promise<T>,
  deps: unknown[],
  opts: { debounceMs?: number } = {},
): DataState<T> & { reload: () => void } {
  const debounceMs = opts.debounceMs ?? 3000;
  const { subscribe } = useLive();
  const [state, setState] = useState<DataState<T>>({ loading: true });

  const load = useCallback(() => {
    let stale = false;
    setState((s) => ({ ...s, loading: true }));
    fetcher()
      .then((data) => {
        if (!stale) setState({ data, loading: false, updatedAt: Date.now() });
      })
      .catch((error: unknown) => {
        if (!stale)
          setState((s) => ({
            data: s.data,
            loading: false,
            error: error instanceof Error ? error : new Error(String(error)),
          }));
      });
    return () => {
      stale = true;
    };
    // deps задаёт вызывающий экран (период и т.п.)
  }, deps);

  useEffect(() => load(), [load]);

  // SSE: коалесим всплеск событий в один отложенный refetch (trailing-окно).
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | null = null;
    const unsubscribe = subscribe(() => {
      if (timer !== null) return;
      timer = setTimeout(() => {
        timer = null;
        load();
      }, debounceMs);
    });
    return () => {
      unsubscribe();
      if (timer !== null) clearTimeout(timer);
    };
  }, [subscribe, load, debounceMs]);

  const reload = useCallback(() => {
    load();
  }, [load]);
  return { ...state, reload };
}
