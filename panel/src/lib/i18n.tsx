// Lightweight in-house i18n (no heavy deps). Two catalogs (en canonical, ru
// translation), a t()/tp() pair, a provider and hooks.
//
// Language resolution on first visit: an explicit choice in localStorage
// (birdman.lang) wins; otherwise navigator.language(s) starting with "ru" →
// RU, everything else → EN (the default). A manual switch persists and from
// then on auto-detection never overrides it.

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { en } from './locales/en';
import type { MessageKey } from './locales/en';
import { ru } from './locales/ru';
import { formatAge, formatAgo, formatClock, formatStamp } from './format';

export type Lang = 'en' | 'ru';
export type { MessageKey };
export type MessageParams = Record<string, string | number>;

export const LANG_STORAGE_KEY = 'birdman.lang';

const DICTS: Record<Lang, Record<string, string>> = { en, ru };
const EN_ANY = en as Record<string, string>;

/** navigator.language(s) → Lang. Honours preference order: the first entry
 *  that starts with "ru" or "en" decides; unknown languages fall through to
 *  the EN default. */
export function detectLang(nav: Pick<Navigator, 'language' | 'languages'> = navigator): Lang {
  const list = nav.languages && nav.languages.length > 0 ? nav.languages : [nav.language];
  for (const l of list) {
    if (typeof l !== 'string') continue;
    const low = l.toLowerCase();
    if (low.startsWith('ru')) return 'ru';
    if (low.startsWith('en')) return 'en';
  }
  return 'en';
}

/** Explicit choice persisted by the switcher, or null if never chosen. */
export function storedLang(): Lang | null {
  try {
    const v = localStorage.getItem(LANG_STORAGE_KEY);
    return v === 'en' || v === 'ru' ? v : null;
  } catch {
    return null;
  }
}

/** Stored choice wins; otherwise auto-detect (EN default). */
export function resolveInitialLang(): Lang {
  return storedLang() ?? detectLang();
}

function interpolate(tpl: string, params?: MessageParams): string {
  if (params === undefined) return tpl;
  return tpl.replace(/\{(\w+)\}/g, (m, k: string) => (k in params ? String(params[k]) : m));
}

function pluralCategory(lang: Lang, count: number): string {
  return new Intl.PluralRules(lang).select(count);
}

export interface I18nContextValue {
  lang: Lang;
  setLang: (l: Lang) => void;
  /** Translate a key; {placeholders} filled from params. */
  t: (key: MessageKey, params?: MessageParams) => string;
  /** Plural-aware translate: picks `${base}.${category}` by count (auto-added
   *  as the `count` param). */
  tp: (base: string, count: number, params?: MessageParams) => string;
  /** Whether a key exists in the canonical catalog (for dynamic fallbacks). */
  has: (key: string) => boolean;
}

function makeI18n(lang: Lang, setLang: (l: Lang) => void): I18nContextValue {
  const dict = DICTS[lang];
  return {
    lang,
    setLang,
    t: (key, params) => interpolate(dict[key] ?? EN_ANY[key] ?? key, params),
    tp: (base, count, params) => {
      const key = `${base}.${pluralCategory(lang, count)}`;
      const tpl = dict[key] ?? dict[`${base}.other`] ?? EN_ANY[key] ?? EN_ANY[`${base}.other`] ?? base;
      return interpolate(tpl, { count, ...params });
    },
    has: (key) => key in en,
  };
}

const I18nContext = createContext<I18nContextValue | null>(null);

// Used when a component is rendered without a provider (only in unit tests that
// mount a leaf in isolation). The real app always wraps in <I18nProvider>.
const FALLBACK = makeI18n(resolveInitialLang(), () => {});

export function I18nProvider({ children, initialLang }: { children: ReactNode; initialLang?: Lang }) {
  const [lang, setLangState] = useState<Lang>(() => initialLang ?? resolveInitialLang());

  const setLang = useCallback((l: Lang) => {
    try {
      localStorage.setItem(LANG_STORAGE_KEY, l);
    } catch {
      /* private mode — choice won't survive reload */
    }
    setLangState(l);
  }, []);

  useEffect(() => {
    document.documentElement.lang = lang;
  }, [lang]);

  const value = useMemo(() => makeI18n(lang, setLang), [lang, setLang]);
  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

export function useI18n(): I18nContextValue {
  return useContext(I18nContext) ?? FALLBACK;
}

/** Primary hook: `const { t } = useT()` (also exposes tp, lang, setLang, has). */
export function useT(): I18nContextValue {
  return useI18n();
}

export interface BoundFormat {
  /** Compact duration: "3м 05с" / "3m 05s". */
  age: (ms: number) => string;
  /** Relative past: "5с назад" / "5s ago". */
  ago: (ms: number) => string;
  /** Wall clock "14:02:57". */
  clock: (iso: string) => string;
  /** Compact stamp "07.07 14:02" / "Jul 7, 14:02". */
  stamp: (iso: string) => string;
}

/** Locale-bound formatters — labels follow the current language. */
export function useFormat(): BoundFormat {
  const { lang } = useI18n();
  return useMemo(
    () => ({
      age: (ms: number) => formatAge(ms, lang),
      ago: (ms: number) => formatAgo(ms, lang),
      clock: (iso: string) => formatClock(iso, lang),
      stamp: (iso: string) => formatStamp(iso, lang),
    }),
    [lang],
  );
}
