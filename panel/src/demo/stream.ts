// Живая лента демо: GET /v1/events/stream. Панель читает стрим через fetch +
// body.getReader() (lib/sse.ts), поэтому отдаём именно ReadableStream, а не
// строку — иначе индикатор в подвале навсегда останется «reconnecting…».
//
// Сначала хвост ленты одним куском, дальше по событию раз в everyMs. Таймер
// снимается по abort: StrictMode монтирует эффект дважды, и без этого каждая
// перемонтировка оставляла бы висящий интервал.
import type { ApiEvent } from '../lib/api';
import type { Fleet } from './fleet';
import { PROJECT } from './fleet';

/** Сколько последних событий уходит клиенту сразу при подключении. */
const TAIL = 14;

/** Кадр SSE в формате master: id + имя события + JSON-тело. */
function frame(e: ApiEvent): string {
  return `id: ${e.id}\nevent: ${e.kind}\ndata: ${JSON.stringify(e)}\n\n`;
}

/** Ядра синтетических событий — тем же набором, что и хвост ленты. */
const LIVE_KINDS = ['match_start', 'match_end', 'server_ready', 'match_start', 'match_end'] as const;

export function streamResponse(fleet: Fleet, signal: AbortSignal | undefined, everyMs = 2000): Response {
  const encoder = new TextEncoder();
  const tail = fleet.events.slice(0, TAIL).reverse(); // старые первыми, как у master
  let nextId = fleet.events[0].id + 1;
  // Начинаем не с нуля: иначе первое же живое событие повторяет верхнюю строку
  // хвоста тем же дедиком и матчем, и лента открывается видимым дублем.
  let tick = 7;
  let timer: ReturnType<typeof setInterval> | undefined;

  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(encoder.encode(tail.map(frame).join('')));

      const stop = () => {
        if (timer !== undefined) clearInterval(timer);
        timer = undefined;
        try {
          controller.close();
        } catch {
          // уже закрыт читателем — это нормальный путь отписки
        }
      };

      timer = setInterval(() => {
        const kind = LIVE_KINDS[tick % LIVE_KINDS.length];
        const server = fleet.servers[(tick * 11) % fleet.servers.length];
        const match = fleet.matches.find((m) => m.server_id === server.id) ?? fleet.matches[0];
        const node = fleet.nodes.find((n) => n.id === server.node_id);
        tick++;
        const e: ApiEvent = {
          id: nextId++,
          ts: new Date().toISOString(),
          kind,
          project: PROJECT,
          node_id: server.node_id,
          server_id: server.id,
          ...(kind.startsWith('match') ? { match_id: match.id } : {}),
          // Форма payload — та же, что у хвоста ленты (fleet.payloadFor):
          // строка ленты не должна менять состав полей на глазах у зрителя.
          payload:
            kind === 'server_ready'
              ? { port: server.port, region: server.region, hostname: node?.hostname ?? '' }
              : kind === 'match_end'
                ? { match: match.id, duration_s: 240 + ((tick * 37) % 900), players_peak: match.players_peak }
                : { match: match.id, players: match.players_peak, region: match.region },
        };
        try {
          controller.enqueue(encoder.encode(frame(e)));
        } catch {
          stop(); // читатель ушёл — гасим таймер, а не сыплем в закрытый стрим
        }
      }, everyMs);

      signal?.addEventListener('abort', stop, { once: true });
    },
    cancel() {
      if (timer !== undefined) clearInterval(timer);
      timer = undefined;
    },
  });

  return new Response(body, {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream' },
  });
}
