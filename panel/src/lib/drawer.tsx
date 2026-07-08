// Дровер деталей дедика: любой экран (Флот, Матчи) открывает его по server_id
// через useServerDrawer().open(id) — без прокидывания пропсов. Хост рендерит
// сам дровер и живёт внутри LiveProvider (детали используют useData/SSE).

import { createContext, useCallback, useContext, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { ServerDrawer } from '../components/ServerDrawer';

interface DrawerContextValue {
  open: (serverId: string) => void;
}

const DrawerContext = createContext<DrawerContextValue | null>(null);

export function DrawerProvider({ children }: { children: ReactNode }) {
  const [serverId, setServerId] = useState<string | null>(null);
  const open = useCallback((id: string) => {
    setServerId(id);
  }, []);
  const close = useCallback(() => {
    setServerId(null);
  }, []);
  const value = useMemo(() => ({ open }), [open]);
  return (
    <DrawerContext.Provider value={value}>
      {children}
      <ServerDrawer serverId={serverId} onClose={close} />
    </DrawerContext.Provider>
  );
}

export function useServerDrawer(): DrawerContextValue {
  const ctx = useContext(DrawerContext);
  if (ctx === null) throw new Error('useServerDrawer вне DrawerProvider');
  return ctx;
}
