// Клиент истории логов: GET /v1/logs/query (master-прокси к VictoriaLogs,
// docs/superpowers/specs/2026-07-09-centralized-logs-design.md §3). ndjson →
// строки; мягкая деградация «хранилище не настроено» / «недоступно» —
// зеркально fetchVM (lib/metrics.ts:141). Live-tail (lib/logs.ts,
// /v1/servers/{id}/logs) — отдельный путь, не трогаем.

import { ApiError, qs } from './api';

/** Одна строка лога: время + сообщение (VictoriaLogs _time/_msg) + остальные
 *  стрим-поля (server_id/node/region и что ещё пришло) как есть, строками. */
export interface LogLine {
  time: string;
  msg: string;
  fields: Record<string, string>;
}

/** Успех — строки; либо мягкая недоступность: логи не настроены на этом
 *  master (503 logs_unconfigured) или VictoriaLogs настроена, но лежит
 *  (502/504 upstream). Прочие ошибки панель не глотает — see queryLogs. */
export type LogsResult =
  | { kind: 'ok'; lines: LogLine[] }
  | { kind: 'unavailable'; reason: 'unconfigured' | 'upstream' };

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
 * отвечает) — эти случаи панель показывает мягко, не ошибкой. Прочие !ok
 * (кривой LogsQL и т.п.) — ApiError.
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
  if (res.status === 502 || res.status === 504 || errCode === 'upstream') {
    return { kind: 'unavailable', reason: 'upstream' };
  }
  if (!res.ok) {
    throw new ApiError(res.status, errCode ?? `http_${res.status}`, text.slice(0, 160));
  }
  return { kind: 'ok', lines: parseLogLines(text) };
}
