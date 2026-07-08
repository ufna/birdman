// Хелперы экрана «Алерты». Правила и active приходят из vmalert (master
// проксирует); когда vmalert_url не задан, master отвечает 503
// alerts_unconfigured, а при недоступном апстриме — 502 upstream. Оба случая —
// не ошибка панели, а «мягкое» состояние (как metrics.ts для VM). Описания
// алертов приходят с бэка как есть (сейчас по-русски из vmalert) — их НЕ
// переводим, показываем контент как пришёл.

import { ApiError } from './api';

export type AlertsUnavailable = 'unconfigured' | 'upstream';

/**
 * Мягко ли недоступны алерты: vmalert не настроен (503 alerts_unconfigured) или
 * апстрим не отвечает (502/504 upstream/bad_gateway). Иначе null — это обычная
 * ошибка (её показывает ErrorNote).
 */
export function alertsUnavailable(e: unknown): AlertsUnavailable | null {
  if (!(e instanceof ApiError)) return null;
  if (e.status === 503 && e.code === 'alerts_unconfigured') return 'unconfigured';
  if (e.status === 502 || e.status === 504 || e.code === 'upstream' || e.code === 'bad_gateway') {
    return 'upstream';
  }
  return null;
}
