// Чистые хелперы деталей матча (фича 2): длительность и окно метрик. Держим
// отдельно от компонента — это точки юнит-тестирования (маппинг данных матча
// в дровер).

import type { Match } from './api';
import type { MetricRange } from './metrics';

/** Опорное начало матча: started_at, иначе created_at (pending ещё без старта). */
function startMs(m: Match): number | null {
  const s = new Date(m.started_at ?? m.created_at).getTime();
  return Number.isNaN(s) ? null : s;
}

/**
 * Длительность матча в мс: finished/aborted — `ended_at − started_at`; живой
 * (running/pending) — тикающая `now − started_at`. null — нет валидного старта.
 */
export function matchDurationMs(m: Match, now: number): number | null {
  const s = startMs(m);
  if (s === null) return null;
  if (m.ended_at !== undefined) {
    const e = new Date(m.ended_at).getTime();
    if (!Number.isNaN(e)) return Math.max(0, e - s);
  }
  return Math.max(0, now - s);
}

/** Матч завершён (есть ended_at или терминальное состояние). */
export function isMatchOver(m: Match): boolean {
  return m.ended_at !== undefined || m.state === 'finished' || m.state === 'aborted';
}

/**
 * Окно метрик матча (unix-сек): finished — статичное [started_at, ended_at];
 * живой — [started_at] с открытым концом (график добавляет now). null — нет
 * server_id (вычищен) или невалидный старт → метрики не запрашиваем.
 */
export function matchMetricRange(m: Match): MetricRange | null {
  if (m.server_id === '') return null;
  const s = startMs(m);
  if (s === null) return null;
  const start = Math.floor(s / 1000);
  if (m.ended_at !== undefined) {
    const e = new Date(m.ended_at).getTime();
    if (!Number.isNaN(e)) return { start, end: Math.max(start + 1, Math.ceil(e / 1000)) };
  }
  return { start };
}
