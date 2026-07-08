// Хелперы экрана «Алерты». Правила и active приходят из vmalert (master
// проксирует); когда vmalert_url не задан, master отвечает 503
// alerts_unconfigured, а при недоступном апстриме — 502 upstream. Оба случая —
// не ошибка панели, а «мягкое» состояние (как metrics.ts для VM). Описания
// алертов приходят с бэка как есть (сейчас по-русски из vmalert) — их НЕ
// переводим, показываем контент как пришёл.

import { ApiError } from './api';
import type { ActiveAlert } from './api';
import type { I18nContextValue } from './i18n';

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

// --- mute вью-модель (чистые функции — точки тестирования) ---

/** Пресеты срока действия mute: относительные окна + «бессрочно». */
export type MutePreset = '1h' | '8h' | '24h' | 'forever';
export const MUTE_PRESETS: MutePreset[] = ['1h', '8h', '24h', 'forever'];

const PRESET_MS: Record<Exclude<MutePreset, 'forever'>, number> = {
  '1h': 3_600_000,
  '8h': 8 * 3_600_000,
  '24h': 24 * 3_600_000,
};

/**
 * ISO-время истечения для пресета от `now` (мс); 'forever' → undefined (поле
 * expires_at опускается — бессрочный mute).
 */
export function presetExpiry(preset: MutePreset, now: number): string | undefined {
  if (preset === 'forever') return undefined;
  return new Date(now + PRESET_MS[preset]).toISOString();
}

/**
 * Сколько активных critical-алертов НЕ заглушено — источник числа для бейджа в
 * навигации. undefined/пусто → 0; muted и не-critical не считаются.
 */
export function countCriticalAlerts(alerts: ActiveAlert[] | undefined): number {
  if (alerts === undefined) return 0;
  return alerts.filter((a) => a.severity === 'critical' && a.muted !== true).length;
}

/** Нормализация региона mute к тексту: пустой/undefined → null (все регионы). */
export function normalizeMuteRegion(region: string | undefined): string | undefined {
  const trimmed = region?.trim();
  return trimmed !== undefined && trimmed !== '' ? trimmed : undefined;
}

/**
 * Человекочитаемая ошибка mute-действия: 400 (плохой запрос), 404 (mute уже
 * снят), 409 (конфликт), 403 (нет прав) → локализованные подписи; прочее — код.
 */
export function muteErrorMessage(e: unknown, t: I18nContextValue['t']): string {
  if (e instanceof ApiError) {
    if (e.status === 400) return t('alerts.mute.err.bad');
    if (e.status === 404) return t('alerts.mute.err.gone');
    if (e.status === 409) return t('alerts.mute.err.conflict');
    if (e.status === 403) return t('confirm.err.forbidden');
    return e.detail ?? e.code;
  }
  return t('confirm.err.generic');
}

// --- флажок звука нового critical (по умолчанию выкл) ---

export const ALERT_SOUND_KEY = 'birdman.alertSound';

export function alertSoundEnabled(): boolean {
  try {
    return localStorage.getItem(ALERT_SOUND_KEY) === '1';
  } catch {
    return false;
  }
}

export function setAlertSoundEnabled(on: boolean): void {
  try {
    localStorage.setItem(ALERT_SOUND_KEY, on ? '1' : '0');
  } catch {
    /* приватный режим — выбор не переживёт перезагрузку */
  }
}

/**
 * Короткий сигнал через WebAudio (без внешних ассетов — CSP-safe). Тихий бип,
 * играет только если включён флажок; ошибки (нет AudioContext / политика
 * автоплея) молча гасим — звук опционален.
 */
export function playAlertBeep(): void {
  try {
    const Ctx =
      window.AudioContext ?? (window as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
    if (Ctx === undefined) return;
    const ctx = new Ctx();
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = 'sine';
    osc.frequency.value = 880;
    gain.gain.setValueAtTime(0.0001, ctx.currentTime);
    gain.gain.exponentialRampToValueAtTime(0.08, ctx.currentTime + 0.02);
    gain.gain.exponentialRampToValueAtTime(0.0001, ctx.currentTime + 0.35);
    osc.connect(gain).connect(ctx.destination);
    osc.start();
    osc.stop(ctx.currentTime + 0.36);
    osc.onended = () => {
      void ctx.close();
    };
  } catch {
    /* звук недоступен — не критично */
  }
}
