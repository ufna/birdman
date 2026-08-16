// Логи демо: ответ /v1/logs/query в формате VictoriaLogs — ndjson, по объекту
// на строку, обязательные `_time`/`_msg` плюс поля потока (panel lib/
// logsHistory.ts: parseLogLines). Живой хвост (/v1/servers/{id}/logs) — тот же
// текст, но потоком.
import type { Fleet } from './fleet';
import { PROJECT, rng } from './fleet';

/** Контекст строки лога: матч и текущая заполненность дедика. */
interface LineCtx {
  match: string;
  players: number;
}

/** Строки, которые пишет дедик: старт матча, тики, входы/выходы, слив. */
const TEMPLATES: ((c: LineCtx) => string)[] = [
  (c) => `match ${c.match} started, ${String(c.players)} players expected`,
  (c) => `player joined (${String(c.players)}/16)`,
  () => 'tick 12.4ms avg over 600 frames',
  (c) => `player left (${String(c.players)}/16)`,
  () => 'snapshot flushed, 41 entities',
  (c) => `match ${c.match} ended, duration 9m41s`,
  () => 'graceful drain requested, deadline 120s',
  () => 'tick 11.8ms avg over 600 frames',
];

/**
 * ndjson последних `limit` строк по флоту. Время убывает от «сейчас» — панель
 * сортирует по `_time` desc и показывает свежие сверху.
 */
export function logLines(fleet: Fleet, now: number, limit: number): string {
  const r = rng(0x109);
  const live = fleet.servers.filter((s) => s.state === 'allocated' || s.state === 'draining');
  const nodeById = new Map(fleet.nodes.map((n) => [n.id, n]));
  const matchOfServer = new Map(fleet.matches.map((m) => [m.server_id, m]));
  const out: string[] = [];
  for (let i = 0; i < limit; i++) {
    const s = live[(i * 3) % live.length];
    const node = nodeById.get(s.node_id);
    const match = matchOfServer.get(s.id);
    const tpl = TEMPLATES[i % TEMPLATES.length];
    out.push(
      JSON.stringify({
        _time: new Date(now - i * 1400 - Math.floor(r() * 900)).toISOString(),
        _msg: tpl({ match: match?.id.slice(0, 8) ?? 'pending', players: 6 + Math.floor(r() * 10) }),
        server_id: s.id,
        node: node?.hostname ?? '',
        region: s.region,
        project: PROJECT,
        env: node?.env ?? 'prod',
      }),
    );
  }
  return `${out.join('\n')}\n`;
}

/** Живой хвост логов одного дедика: строка раз в секунду, пока читают. */
export function logStreamResponse(
  fleet: Fleet,
  serverId: string,
  signal: AbortSignal | undefined,
  everyMs = 1000,
): Response {
  const encoder = new TextEncoder();
  const server = fleet.servers.find((s) => s.id === serverId) ?? fleet.servers[0];
  const match = fleet.matches.find((m) => m.server_id === server.id);
  let i = 0;
  let timer: ReturnType<typeof setInterval> | undefined;

  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      const stop = () => {
        if (timer !== undefined) clearInterval(timer);
        timer = undefined;
        try {
          controller.close();
        } catch {
          // уже закрыт читателем — нормальный путь отписки
        }
      };
      timer = setInterval(() => {
        const tpl = TEMPLATES[i % TEMPLATES.length];
        const line = `${new Date().toISOString()} ${tpl({ match: match?.id.slice(0, 8) ?? 'pending', players: 8 + (i % 8) })}\n`;
        i++;
        try {
          controller.enqueue(encoder.encode(line));
        } catch {
          stop();
        }
      }, everyMs);
      signal?.addEventListener('abort', stop, { once: true });
    },
    cancel() {
      if (timer !== undefined) clearInterval(timer);
      timer = undefined;
    },
  });

  return new Response(body, { status: 200, headers: { 'Content-Type': 'text/plain' } });
}
