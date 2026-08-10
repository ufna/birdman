// Тема: системная по умолчанию, явный выбор хранится в localStorage и
// ставится как data-theme на <html> (до первого рендера — см. index.html).

/* eslint-disable react-refresh/only-export-components -- useTheme и cssVar рядом со своим провайдером темы. Правило про гранулярность Fast Refresh в dev-сервере, не про корректность; разносить файл по модулям ради него дороже, чем оно стоит. Политика — в eslint.config.js. */

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';

export type Theme = 'light' | 'dark';

interface ThemeContextValue {
  theme: Theme; // фактическая (для перерисовки графиков)
  toggle: () => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

const STORAGE_KEY = 'birdman.theme';

function systemTheme(): Theme {
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function storedTheme(): Theme | null {
  try {
    const t = localStorage.getItem(STORAGE_KEY);
    return t === 'light' || t === 'dark' ? t : null;
  } catch {
    return null;
  }
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setTheme] = useState<Theme>(() => storedTheme() ?? systemTheme());
  const [explicit, setExplicit] = useState<boolean>(() => storedTheme() !== null);

  // Пока выбор не сделан — следуем за системой.
  useEffect(() => {
    if (explicit) return;
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const onChange = () => {
      setTheme(systemTheme());
    };
    mq.addEventListener('change', onChange);
    return () => {
      mq.removeEventListener('change', onChange);
    };
  }, [explicit]);

  useEffect(() => {
    if (explicit) {
      document.documentElement.dataset.theme = theme;
    } else {
      delete document.documentElement.dataset.theme;
    }
  }, [theme, explicit]);

  const toggle = useCallback(() => {
    setTheme((t) => {
      const next: Theme = t === 'dark' ? 'light' : 'dark';
      try {
        localStorage.setItem(STORAGE_KEY, next);
      } catch {
        /* приватный режим — тема не переживёт перезагрузку */
      }
      return next;
    });
    setExplicit(true);
  }, []);

  const value = useMemo(() => ({ theme, toggle }), [theme, toggle]);
  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (ctx === null) throw new Error('useTheme must be used within a ThemeProvider');
  return ctx;
}

/** Значение CSS-переменной темы (для canvas-графиков uPlot). */
export function cssVar(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}
