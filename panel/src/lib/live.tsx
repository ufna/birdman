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

const LiveContext = createContext<LiveContextValue | null>(null);

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

export function useLive(): LiveContextValue {
  const ctx = useContext(LiveContext);
  if (ctx === null) throw new Error('useLive must be used within a LiveProvider');
  return ctx;
}

interface DataState<T> {
  data?: T;
  error?: Error;
  loading: boolean;
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
        if (!stale) setState({ data, loading: false });
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
