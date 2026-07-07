// Форматирование времени/идентификаторов. Всё, что тикает в UI
// (heartbeat age, длительность матча, аптайм), проходит через эти функции.

/** 8 первых символов uuid — как в логах master. */
export function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

/**
 * Короткая длительность: "12с", "3м 05с", "2ч 14м", "3д 6ч".
 * Отрицательное (рассинхрон часов) прижимается к нулю.
 */
export function formatAge(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const s = total % 60;
  const m = Math.floor(total / 60) % 60;
  const h = Math.floor(total / 3600) % 24;
  const d = Math.floor(total / 86400);
  if (d > 0) return `${d}д ${h}ч`;
  if (h > 0) return `${h}ч ${m}м`;
  if (m > 0) return `${m}м ${String(s).padStart(2, '0')}с`;
  return `${s}с`;
}

export type HeartbeatTone = 'good' | 'warn' | 'dead';

/**
 * Тон heartbeat age: агент шлёт ~2с, карантин с 10с (protocol.md §1) —
 * зелёный до 5с, жёлтый до 10с, дальше красный. null — нода ещё не
 * репортила.
 */
export function heartbeatTone(ageMs: number | null): HeartbeatTone {
  if (ageMs === null) return 'dead';
  if (ageMs < 5000) return 'good';
  if (ageMs < 10000) return 'warn';
  return 'dead';
}

/** "14:02:57" в локальном времени — колонка времени в ленте событий. */
export function formatClock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString('ru-RU', { hour12: false });
}

/** "07.07 14:02" — компактная метка для истории. */
export function formatStamp(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  const dd = String(d.getDate()).padStart(2, '0');
  const mm = String(d.getMonth() + 1).padStart(2, '0');
  const hh = String(d.getHours()).padStart(2, '0');
  const mi = String(d.getMinutes()).padStart(2, '0');
  return `${dd}.${mm} ${hh}:${mi}`;
}

/** Возраст ISO-метки относительно now (мс). */
export function ageOf(iso: string | undefined, now: number): number | null {
  if (iso === undefined) return null;
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return null;
  return now - t;
}

/**
 * Компактная выжимка payload события: "region=eu · reason=no_capacity".
 * Вложенные значения сворачиваются в JSON, длина ограничивается.
 */
export function summarizePayload(payload: Record<string, unknown>, maxLen = 90): string {
  const parts: string[] = [];
  for (const [k, v] of Object.entries(payload)) {
    const val = typeof v === 'object' && v !== null ? JSON.stringify(v) : String(v);
    parts.push(`${k}=${val}`);
  }
  const s = parts.join(' · ');
  return s.length > maxLen ? `${s.slice(0, maxLen - 1)}…` : s;
}
