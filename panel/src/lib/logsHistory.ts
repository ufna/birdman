// Клиент истории логов: GET /v1/logs/query (master-прокси к VictoriaLogs,
// docs/superpowers/specs/2026-07-09-centralized-logs-design.md §3). ndjson →
// строки; мягкая деградация «хранилище не настроено» / «недоступно» —
// зеркально fetchVM (lib/metrics.ts:141). Live-tail (lib/logs.ts,
// /v1/servers/{id}/logs) — отдельный путь, не трогаем.

import { ApiError, qs } from './api';
// Только ТИП (строковый союз, не React) — как в lib/logsql.ts и lib/metrics.ts:
// на рантайме импорт стирается, цикла lib→i18n→locales не возникает.
import type { MessageKey } from './i18n';

/** Одна строка лога: время + сообщение (VictoriaLogs _time/_msg) + остальные
 *  стрим-поля (server_id/node/region и что ещё пришло) как есть, строками. */
export interface LogLine {
  time: string;
  msg: string;
  fields: Record<string, string>;
}

/** Почему истории нет, хотя запрос дошёл и панель не считает это ошибкой:
 *  логи не настроены на этом master (503 `logs_unconfigured`); VictoriaLogs
 *  настроена, но лежит (502/504 или тело `upstream`); апстрим не разбирает
 *  ручку сужения, поэтому привязанному ключу отказано вместо выдачи всего
 *  флота (503 `logs_narrowing_unsupported`, tracker #1007). */
export type LogsUnavailableReason = 'unconfigured' | 'upstream' | 'narrowing';

/** Успех — строки; либо мягкая недоступность (см. LogsUnavailableReason).
 *  Прочие ошибки панель не глотает — see queryLogs. */
export type LogsResult =
  | { kind: 'ok'; lines: LogLine[] }
  | { kind: 'unavailable'; reason: LogsUnavailableReason };

/**
 * Причина мягкой недоступности → строка каталога. Карта ОДНА на всех
 * потребителей проксии (`screens/Logs.tsx` — флит-поиск, `components/
 * LogsPanel.tsx` — вкладка «История» в дровере) намеренно: до tracker #1076
 * каждый из них решал это своим тернарником `reason === 'unconfigured' ? … :
 * 'logs.unavailable'`, и добавленная причина показалась бы оператору чужим
 * текстом «хранилище недоступно» — молча, потому что else-ветка всегда
 * что-нибудь да рисует. `Record<LogsUnavailableReason, MessageKey>` эту
 * тихую ветку убирает: причина без своей строки — ошибка КОМПИЛЯЦИИ.
 *
 * `logs.narrowing` — близнец `metric.err.narrowing` (MetricMessage.tsx), но
 * со своей ручкой (`extra_stream_filters`) и своей опцией конфига
 * (`victorialogs_url`): чинить оператору не ключ и не панель, а апстрим, и
 * сказать это должен экран — прозу master'а панель в UI не довозит (#996).
 */
export const LOGS_UNAVAILABLE_MESSAGE: Record<LogsUnavailableReason, MessageKey> = {
  unconfigured: 'logs.unconfigured',
  upstream: 'logs.unavailable',
  narrowing: 'logs.narrowing',
};

/**
 * Разбирает ndjson-ответ /v1/logs/query в строки. Каждая непустая строка —
 * отдельный JSON-объект; _time/_msg выделяются в одноимённые поля, остальное
 * (server_id/node/region и т.п.) — в fields как строки. Битые строки (не
 * JSON, не объект, без _time) молча пропускаются — один плохой байт не
 * должен гасить всю историю.
 */
export function parseLogLines(text: string): LogLine[] {
  const out: LogLine[] = [];
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (line === '') continue;
    let obj: unknown;
    try {
      obj = JSON.parse(line);
    } catch {
      continue;
    }
    if (typeof obj !== 'object' || obj === null || Array.isArray(obj)) continue;
    const { _time, _msg, ...rest } = obj as Record<string, unknown>;
    if (typeof _time !== 'string') continue;
    const fields: Record<string, string> = {};
    for (const [k, v] of Object.entries(rest)) {
      if (typeof v === 'string') fields[k] = v;
      else if (v !== undefined && v !== null) fields[k] = String(v);
    }
    out.push({ time: _time, msg: typeof _msg === 'string' ? _msg : '', fields });
  }
  return out;
}

/**
 * Пытается вытащить {error} из тела ответа. Успешное тело — ndjson (может
 * быть много строк) и целиком как JSON не парсится; ошибочное тело мастера —
 * ровно один JSON-объект {error, detail}. Поэтому: не похоже на объект —
 * сразу undefined; похоже, но не распарсилось (например, several ndjson
 * lines) — тоже undefined, не бросаем.
 */
function tryParseErrorCode(text: string): string | undefined {
  const trimmed = text.trim();
  if (!trimmed.startsWith('{')) return undefined;
  try {
    return (JSON.parse(trimmed) as { error?: string }).error;
  } catch {
    return undefined;
  }
}

export interface QueryLogsArgs {
  query: string;
  /** unix-секунды. */
  start?: number;
  end?: number;
  limit?: number;
  signal?: AbortSignal;
}

/**
 * GET /v1/logs/query — история/поиск по LogsQL. 503 + error=logs_unconfigured
 * → мягкая недоступность (VictoriaLogs не настроена на этом master); 502/504
 * (или тело error=upstream) → недоступность апстрима (VL настроена, но не
 * отвечает); 503 + error=logs_narrowing_unsupported → апстрим не разбирает
 * `extra_stream_filters`, и master отказывает привязанному ключу вместо того,
 * чтобы отдать ему весь флот (tracker #1007) — эти случаи панель показывает
 * мягко, не ошибкой. Прочие !ok (кривой LogsQL и т.п.) — ApiError.
 */
export async function queryLogs(args: QueryLogsArgs): Promise<LogsResult> {
  const path = `/v1/logs/query${qs({
    query: args.query,
    start: args.start,
    end: args.end,
    limit: args.limit,
  })}`;
  const res = await fetch(path, { credentials: 'same-origin', signal: args.signal });
  const text = await res.text();
  const errCode = tryParseErrorCode(text);
  if (res.status === 503 && errCode === 'logs_unconfigured') {
    return { kind: 'unavailable', reason: 'unconfigured' };
  }
  // Штатный исход #1007 для привязанного ключа, а не поломка логов: код
  // проверяется вместе со статусом (как у logs_unconfigured выше) — 503 без
  // тела или с чужим кодом по-прежнему жёсткая ошибка.
  if (res.status === 503 && errCode === 'logs_narrowing_unsupported') {
    return { kind: 'unavailable', reason: 'narrowing' };
  }
  if (res.status === 502 || res.status === 504 || errCode === 'upstream') {
    return { kind: 'unavailable', reason: 'upstream' };
  }
  if (!res.ok) {
    throw new ApiError(res.status, errCode ?? `http_${res.status}`, text.slice(0, 160));
  }
  return { kind: 'ok', lines: parseLogLines(text) };
}
