// Простой fetch-хук для П2-экранов: загрузка по смене зависимостей (период,
// лимит), ручной reload. В отличие от useData (live.tsx) НЕ подписан на SSE —
// статистика/кост/алерты не должны перечитываться на каждое событие флота;
// обновление — по смене периода или кнопкой. Прошлые данные на время
// перезагрузки не сбрасываются (нет мигания).

import { useCallback, useEffect, useRef, useState } from 'react';

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
  const gen = useRef(0);

  const load = useCallback(() => {
    const mine = ++gen.current;
    setState((s) => ({ ...s, loading: true }));
    fetcher()
      .then((data) => {
        if (mine === gen.current) setState({ data, loading: false });
      })
      .catch((error: unknown) => {
        if (mine === gen.current)
          setState((s) => ({
            data: s.data,
            error: error instanceof Error ? error : new Error(String(error)),
            loading: false,
          }));
      });
    return () => {
      gen.current++;
    };
    // deps задаёт вызывающий экран (период/лимит и т.п.)
    // eslint-disable-next-line react-hooks/exhaustive-deps -- ДВЕ находки, обе по устройству хука. (1) deps не литерал: их прокидывает вызывающий экран — в этом весь смысл `useAsync(fetcher, deps)`; сами call-site'ы линтер проверяет через additionalHooks в eslint.config.js, так что дыра закрыта на уровне выше. (2) `fetcher` не может быть в deps: это свежая стрелка каждый рендер, зависимость от неё = перезапрос в бесконечном цикле.
  }, deps);

  useEffect(() => load(), [load]);

  const reload = useCallback(() => {
    load();
  }, [load]);
  return { ...state, reload };
}
