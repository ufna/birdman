// Микро-роутер: history API + popstate. Трёх экранов П0 не хватает,
// чтобы оправдать зависимость от полноценного роутера; SPA-fallback
// в master отдаёт index.html для любого пути.

import { useCallback, useEffect, useState } from 'react';

export function usePath(): [string, (p: string) => void] {
  const [path, setPath] = useState(() => window.location.pathname);

  useEffect(() => {
    const onPop = () => {
      setPath(window.location.pathname);
    };
    window.addEventListener('popstate', onPop);
    return () => {
      window.removeEventListener('popstate', onPop);
    };
  }, []);

  const navigate = useCallback((p: string) => {
    if (p !== window.location.pathname) {
      window.history.pushState(null, '', p);
      setPath(p);
    }
  }, []);

  return [path, navigate];
}
