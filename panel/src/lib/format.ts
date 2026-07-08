// Форматирование времени/идентификаторов. Всё, что тикает в UI
// (heartbeat age, длительность матча, аптайм), проходит через эти функции.
// Подписи локале-зависимы (lang), но по умолчанию — русские, чтобы чистые
// функции оставались стабильной точкой тестирования.

import type { Lang } from './i18n';

/** 8 первых символов uuid — как в логах master. */
export function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

// Подписи единиц длительности по языку. Это сам locale-слой форматтера (как
// locales/), поэтому кириллица здесь легитимна — помечено i18n-allow для guard.
const DURATION_UNITS: Record<Lang, { d: string; h: string; m: string; s: string }> = {
  ru: { d: 'д', h: 'ч', m: 'м', s: 'с' }, // i18n-allow: locale duration units
  en: { d: 'd', h: 'h', m: 'm', s: 's' },
};

// Обёртка «… назад» / «… ago» вокруг длительности.
const AGO: Record<Lang, (d: string) => string> = {
  ru: (d) => `${d} назад`, // i18n-allow: locale relative-time suffix
  en: (d) => `${d} ago`,
};

/**
 * Короткая длительность: "12с", "3м 05с", "2ч 14м", "3д 6ч" (en: 12s/3m 05s…).
 * Отрицательное (рассинхрон часов) прижимается к нулю.
 */
export function formatAge(ms: number, lang: Lang = 'ru'): string {
  const u = DURATION_UNITS[lang];
  const total = Math.max(0, Math.floor(ms / 1000));
  const s = total % 60;
  const m = Math.floor(total / 60) % 60;
  const h = Math.floor(total / 3600) % 24;
  const d = Math.floor(total / 86400);
  if (d > 0) return `${d}${u.d} ${h}${u.h}`;
  if (h > 0) return `${h}${u.h} ${m}${u.m}`;
  if (m > 0) return `${m}${u.m} ${String(s).padStart(2, '0')}${u.s}`;
  return `${s}${u.s}`;
}

/** Относительное прошлое: "5с назад" / "5s ago". */
export function formatAgo(ms: number, lang: Lang = 'ru'): string {
  return AGO[lang](formatAge(ms, lang));
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

/** "14:02:57" в локальном времени (24ч) — колонка времени в ленте событий. */
export function formatClock(iso: string, lang: Lang = 'ru'): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString(lang === 'ru' ? 'ru-RU' : 'en-GB', { hour12: false });
}

const EN_STAMP = new Intl.DateTimeFormat('en-US', {
  month: 'short',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  hourCycle: 'h23',
});

/** Компактная метка истории: "07.07 14:02" (ru) / "Jul 7, 14:02" (en). */
export function formatStamp(iso: string, lang: Lang = 'ru'): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  if (lang === 'en') return EN_STAMP.format(d);
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
