// Один общий секундный тик на приложение — heartbeat age, длительности и
// аптаймы «идут» синхронно и без лишних таймеров.

import { useSyncExternalStore } from 'react';

let now = Date.now();
const subscribers = new Set<() => void>();
let timer: ReturnType<typeof setInterval> | null = null;

function subscribe(fn: () => void): () => void {
  subscribers.add(fn);
  if (timer === null) {
    now = Date.now(); // первый подписчик — снапшот не должен быть протухшим
    timer = setInterval(() => {
      now = Date.now();
      subscribers.forEach((s) => {
        s();
      });
    }, 1000);
  }
  return () => {
    subscribers.delete(fn);
    if (subscribers.size === 0 && timer !== null) {
      clearInterval(timer);
      timer = null;
    }
  };
}

/** Текущее время (мс), обновляется раз в секунду. */
export function useNow(): number {
  return useSyncExternalStore(subscribe, () => now);
}
