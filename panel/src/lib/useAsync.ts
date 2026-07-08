// Простой fetch-хук для П2-экранов: загрузка по смене зависимостей (период,
// лимит), ручной reload. В отличие от useData (live.tsx) НЕ подписан на SSE —
// статистика/кост/алерты не должны перечитываться на каждое событие флота;
// обновление — по смене периода или кнопкой. Прошлые данные на время
// перезагрузки не сбрасываются (нет мигания).

import { useCallback, useEffect, useState } from 'react';

export interface AsyncState<T> {
  data?: T;
  error?: Error;
  loading: boolean;
}

export function useAsync<T>(
  fetcher: () => Promise<T>,
  deps: unknown[],
): AsyncState<T> & { reload: () => void } {
  const [state, setState] = useState<AsyncState<T>>({ loading: true });

  const load = useCallback(() => {
    let stale = false;
    setState((s) => ({ ...s, loading: true }));
    fetcher()
      .then((data) => {
        if (!stale) setState({ data, loading: false });
      })
      .catch((error: unknown) => {
        if (!stale)
          setState((s) => ({
            data: s.data,
            error: error instanceof Error ? error : new Error(String(error)),
            loading: false,
          }));
      });
    return () => {
      stale = true;
    };
    // deps задаёт вызывающий экран (период/лимит и т.п.)

  }, deps);

  useEffect(() => load(), [load]);

  const reload = useCallback(() => {
    load();
  }, [load]);
  return { ...state, reload };
}
