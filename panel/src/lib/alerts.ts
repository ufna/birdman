// Хелперы экрана «Алерты». Правила и active приходят из vmalert (master
// проксирует); когда vmalert_url не задан, master отвечает 503
// alerts_unconfigured, а при недоступном апстриме — 502 upstream. Оба случая —
// не ошибка панели, а «мягкое» состояние (как metrics.ts для VM). Описания
// алертов приходят с бэка двуязычно: `description` (EN, каноничный) и
// опциональный `description_ru` — выбираем текст по локали с фоллбэком на EN
// (alertDescription). Обвязку UI переводим через каталог как обычно.

import { ApiError } from './api';
import { apiErrorMessage } from './apiError';
import type { ActiveAlert, AlertScope } from './api';
import type { I18nContextValue, Lang } from './i18n';

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

/**
 * Двуязычный выбор описания алерта: под RU-локалью берём непустой
 * `description_ru`, иначе — каноничный `description` (EN). Фоллбэк обязателен:
 * self-host-операторы пишут свои правила без `description_ru`.
 */
export function alertDescription(a: { description: string; description_ru?: string }, lang: Lang): string {
  if (lang === 'ru' && a.description_ru !== undefined && a.description_ru !== '') {
    return a.description_ru;
  }
  return a.description;
}

// --- проектная область алерта (мультипроект, трекер #956) ---

/** Общая часть активного алерта и записи истории, из которой видна область. */
export interface AlertScoped {
  project?: string;
  scope?: AlertScope;
}

/**
 * Область алерта: платформенный или проектный — ЛИБО undefined, если мастер о
 * ней ничего не сказал.
 *
 * Три случая, и третий — несущий. Поля `project`/`scope` additive (#955): master,
 * который ещё не перевыкачен, не отдаёт НИ ОДНОГО из них. Соблазн считать
 * «нет project → платформенный» здесь ошибочен: на старом мастере такая подпись
 * навесилась бы на ВСЕ алерты, включая проектные, и оператор читал бы честную с
 * виду подпись, которая ничего не значит. Поэтому неизвестность остаётся
 * неизвестностью, а панель в этом случае просто молчит.
 *
 * `scope` от мастера главнее вывода из `project`: это его собственное решение
 * (docs/specs/master.md §6), панель его не пересчитывает. Вывод из непустого
 * `project` — страховка для чужого прокси, который донёс лейбл, но не scope.
 */
export function alertScopeOf(a: AlertScoped): AlertScope | undefined {
  if (a.scope === 'platform' || a.scope === 'project') return a.scope;
  if (a.project !== undefined && a.project !== '') return 'project';
  return undefined;
}

/**
 * Платформенный ли алерт — то есть нужна ли ему честная подпись. Такой алерт
 * виден при ЛЮБОМ выбранном проекте (сужение не скрывающее, см. keepForProject
 * и master keepAlertForProject), и без подписи оператор решит, что упавший
 * мастер или кончающийся диск относятся к текущему проекту.
 */
export function isPlatformAlert(a: AlertScoped): boolean {
  return alertScopeOf(a) === 'platform';
}

/** Проект алерта для показа рядом с region/node; пусто/нет поля → undefined. */
export function alertProjectOf(a: AlertScoped): string | undefined {
  return a.project !== undefined && a.project !== '' ? a.project : undefined;
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

/**
 * Нормализация составляющей цели mute'а (регион, проект): пустая строка или
 * пробелы → undefined, то есть «все». Одна функция на обе оси — правило общее:
 * пустая строка никогда не является целью сама по себе, это неаккуратно
 * записанный wildcard (и master нормализует ровно так же, normalizeMuteTarget).
 */
export function normalizeMuteLabel(value: string | undefined): string | undefined {
  const trimmed = value?.trim();
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
  }
  return apiErrorMessage(e, t, { forbidden: 'confirm.err.forbidden', generic: 'confirm.err.generic' });
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
