// Счётчик активных critical-алертов (не-muted) для бейджа в навигации Shell.
// Поллинг раз в 30с + коалесированный refetch по SSE-событиям флота (новый
// node_quarantine и т.п. — повод перечитать, но всплеск событий схлопывается в
// один запрос). Пульс/звук — только на РОСТЕ числа и не на первом чтении.

import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from './api';
import { alertSoundEnabled, countCriticalAlerts, playAlertBeep } from './alerts';
import { useLive } from './live';

const POLL_MS = 30_000;
const SSE_COALESCE_MS = 2_000;

export interface CriticalAlerts {
  count: number;
  /** Инкрементится при РОСТЕ числа — ключом перезапускаем CSS-пульс бейджа. */
  alarmKey: number;
}

export function useCriticalAlerts(): CriticalAlerts {
  const { subscribe } = useLive();
  const [count, setCount] = useState(0);
  const [alarmKey, setAlarmKey] = useState(0);
  const prev = useRef(0);
  const seeded = useRef(false);

  const load = useCallback(() => {
    api
      .alertsActive()
      .then((alerts) => {
        const n = countCriticalAlerts(alerts);
        setCount(n);
        // Пульс/звук только на росте и НЕ на первом чтении — иначе пульсируем на
        // каждой перезагрузке страницы с уже горящим алертом.
        if (seeded.current && n > prev.current) {
          setAlarmKey((k) => k + 1);
          if (alertSoundEnabled()) playAlertBeep();
        }
        prev.current = n;
        seeded.current = true;
      })
      .catch(() => {
        // Алерты не настроены / vmalert недоступен → бейджа просто нет.
        setCount(0);
        prev.current = 0;
      });
  }, []);

  // Первое чтение + страховочный поллинг.
  useEffect(() => {
    load();
    const timer = setInterval(load, POLL_MS);
    return () => {
      clearInterval(timer);
    };
  }, [load]);

  // SSE: любое событие флота — повод перечитать, но коалесим всплеск в один запрос.
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | null = null;
    const unsubscribe = subscribe(() => {
      if (timer !== null) return;
      timer = setTimeout(() => {
        timer = null;
        load();
      }, SSE_COALESCE_MS);
    });
    return () => {
      unsubscribe();
      if (timer !== null) clearTimeout(timer);
    };
  }, [subscribe, load]);

  return { count, alarmKey };
}
