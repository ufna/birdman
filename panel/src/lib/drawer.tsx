// Дроверы деталей: любой экран открывает детали дедика по server_id
// (useServerDrawer().open) или детали матча по match_id (useMatchDrawer().open)
// — без прокидывания пропсов. Хост рендерит оба дровера и живёт внутри
// LiveProvider (детали используют useData/SSE).

import { createContext, useCallback, useContext, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { ServerDrawer } from '../components/ServerDrawer';
import { MatchDrawer } from '../components/MatchDrawer';

interface OpenById {
  open: (id: string) => void;
}

const ServerDrawerContext = createContext<OpenById | null>(null);
const MatchDrawerContext = createContext<OpenById | null>(null);

export function DrawerProvider({ children }: { children: ReactNode }) {
  const [serverId, setServerId] = useState<string | null>(null);
  const [matchId, setMatchId] = useState<string | null>(null);

  const server = useMemo<OpenById>(() => ({ open: (id) => setServerId(id) }), []);
  const match = useMemo<OpenById>(() => ({ open: (id) => setMatchId(id) }), []);

  const closeServer = useCallback(() => setServerId(null), []);
  const closeMatch = useCallback(() => setMatchId(null), []);

  return (
    <ServerDrawerContext.Provider value={server}>
      <MatchDrawerContext.Provider value={match}>
        {children}
        <ServerDrawer serverId={serverId} onClose={closeServer} />
        <MatchDrawer matchId={matchId} onClose={closeMatch} />
      </MatchDrawerContext.Provider>
    </ServerDrawerContext.Provider>
  );
}

export function useServerDrawer(): OpenById {
  const ctx = useContext(ServerDrawerContext);
  if (ctx === null) throw new Error('useServerDrawer вне DrawerProvider');
  return ctx;
}

export function useMatchDrawer(): OpenById {
  const ctx = useContext(MatchDrawerContext);
  if (ctx === null) throw new Error('useMatchDrawer вне DrawerProvider');
  return ctx;
}
