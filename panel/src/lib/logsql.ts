// LogsQL-билдеры для панели: чистые функции, собирающие строку запроса к
// VictoriaLogs через master-прокси GET /v1/logs/query (Task 1, ops.go
// handleLogsQuery). Не знают ни про сеть, ни про React — единственная точка
// тестирования LogsQL-синтаксиса; lib/logsHistory.ts просто передаёт готовую
// строку в query= как есть.

/**
 * Экранирует значение LogsQL-строки/фильтра: `\` → `\\`, `"` → `\"` (именно в
 * этом порядке — иначе экранирующий бэкслэш сам попадёт под вторую замену).
 */
export function escapeStreamValue(v: string): string {
  return v.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

// Обе точки входа (История дедика и флит-поиск) читают ленту «новые сверху» —
// как live-tail в LogViewer, только по историческим данным.
const SORT_DESC = ' | sort by (_time) desc';

/** trim(); пустое/из одних пробелов → undefined («поле не задано»). */
function nonBlank(v: string | undefined): string | undefined {
  const t = v?.trim() ?? '';
  return t === '' ? undefined : t;
}

/**
 * История одного дедика: стрим-фильтр по server_id (всегда) + опциональная
 * полнотекстовая фраза. serverId для reaped/мёртвого дедика — просто строка
 * (в /v1/servers его уже может не быть — история именно этим и ценна).
 */
export function serverHistoryQuery(serverId: string, text?: string): string {
  let q = `{server_id="${escapeStreamValue(serverId)}"}`;
  const t = nonBlank(text);
  if (t !== undefined) q += ` "${escapeStreamValue(t)}"`;
  return q + SORT_DESC;
}

/** Флит-поиск (экран «Логи»): любая комбинация region/node/server_id + текст. */
export interface FleetFilter {
  text?: string;
  region?: string;
  node?: string;
  serverId?: string;
}

/**
 * Стрим-фильтры из заданных полей (`{region="eu",node="n1"}`), следом —
 * опциональная текстовая фраза. Ни полей, ни текста нет → `*` (весь поток,
 * LogsQL matches-all). Всегда суффикс сортировки.
 */
export function fleetSearchQuery(f: FleetFilter): string {
  const pairs: string[] = [];
  const region = nonBlank(f.region);
  const node = nonBlank(f.node);
  const serverId = nonBlank(f.serverId);
  if (region !== undefined) pairs.push(`region="${escapeStreamValue(region)}"`);
  if (node !== undefined) pairs.push(`node="${escapeStreamValue(node)}"`);
  if (serverId !== undefined) pairs.push(`server_id="${escapeStreamValue(serverId)}"`);

  let q = pairs.length > 0 ? `{${pairs.join(',')}}` : '';
  const text = nonBlank(f.text);
  if (text !== undefined) {
    const phrase = `"${escapeStreamValue(text)}"`;
    q = q === '' ? phrase : `${q} ${phrase}`;
  } else if (q === '') {
    q = '*';
  }
  return q + SORT_DESC;
}
