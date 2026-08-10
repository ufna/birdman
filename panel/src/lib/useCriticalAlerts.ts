// Счётчик активных critical-алертов (не-muted) для бейджа в навигации Shell.
// Поллинг раз в 30с + коалесированный refetch по SSE-событиям флота (новый
// node_quarantine и т.п. — повод перечитать, но всплеск событий схлопывается в
// один запрос). Пульс/звук — только на РОСТЕ числа и не на первом чтении.
//
// Считаем ПО ТОМУ ЖЕ срезу, что показывает экран Алертов (трекер #956): тот же
// не скрывающий ?project=, поэтому платформенный critical бейдж не теряет, а
// critical соседнего проекта в него не попадает. Иначе бейдж «3» и экран, где
// их два, расходились бы — а бейдж существует ровно затем, чтобы отправить
// оператора на экран.
//
// Раз число живёт по срезу, то и ДЕЛЬТА-состояние (с чем сравнивать) живёт по
// срезу (трекер #964): «стало больше» имеет смысл только внутри одного
// проекта. Сравнение с числом соседа давало ложный пульс на каждом
// переключении — а сигнал, который врёт, оператор перестаёт замечать.

import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from './api';
import { alertSoundEnabled, countCriticalAlerts, playAlertBeep } from './alerts';
import { useLive } from './live';
import { useProjectSlug } from './project';

const POLL_MS = 30_000;
const SSE_COALESCE_MS = 2_000;

export interface CriticalAlerts {
  count: number;
  /** Инкрементится при РОСТЕ числа — ключом перезапускаем CSS-пульс бейджа. */
  alarmKey: number;
}

export function useCriticalAlerts(): CriticalAlerts {
  const { subscribe } = useLive();
  // '' — проектного измерения нет (нет провайдера), фильтр не шлём; null —
  // проекта ещё нет, тогда и считать нечего (как пустой список у экранов).
  const project = useProjectSlug();
  const [count, setCount] = useState(0);
  const [alarmKey, setAlarmKey] = useState(0);
  // Прошлое чтение ВМЕСТЕ с проектом, которому оно принадлежит: null — сравнивать
  // не с чем (первое чтение, смена проекта, потерянная связь с vmalert). Одно
  // поле вместо пары prev+seeded именно затем, чтобы «с чем сравниваем» нельзя
  // было отделить от «чей это срез» и получить сравнение через проект.
  const seen = useRef<{ project: string; count: number } | null>(null);
  // Поколение запроса: смена проекта ничего не отменяет, и ответ ПРОШЛОГО
  // проекта приходит уже при новом выборе. Без guard'а он и число рисует чужое,
  // и дельту травит — следующий настоящий critical оказывается «не больше
  // прошлого» и молча съедается. Тот же монотонный счётчик, что в screens/Logs.tsx.
  const seq = useRef(0);

  const load = useCallback(() => {
    const gen = ++seq.current;
    if (project === null) {
      setCount(0);
      seen.current = null;
      return;
    }
    api
      .alertsActive({ project })
      .then((alerts) => {
        if (gen !== seq.current) return; // ответ на устаревший запрос — игнорируем
        const n = countCriticalAlerts(alerts);
        setCount(n);
        // Пульс/звук только на росте ВНУТРИ того же проекта и НЕ на первом
        // чтении — иначе пульсируем на каждой перезагрузке страницы с уже
        // горящим алертом и на каждом переключении проекта.
        const last = seen.current;
        if (last !== null && last.project === project && n > last.count) {
          setAlarmKey((k) => k + 1);
          if (alertSoundEnabled()) playAlertBeep();
        }
        seen.current = { project, count: n };
      })
      .catch(() => {
        // Алерты не настроены / vmalert недоступен → бейджа просто нет. Дельту
        // обнуляем в «нечего сравнивать», а не в «было ноль»: после блипа
        // vmalert те же три давно горящих алерта не должны читаться как три
        // новых.
        if (gen !== seq.current) return;
        setCount(0);
        seen.current = null;
      });
  }, [project]);

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
