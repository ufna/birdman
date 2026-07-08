// Подписка на GET /v1/events/stream (SSE поверх fetch: EventSource не даёт
// ни контроля реконнекта, ни подписки на произвольные `event:`-имена).
// Реконнект с экспоненциальной паузой и курсором after_id — события между
// обрывами не теряются (master поддерживает Last-Event-ID/after_id).

import type { ApiEvent } from './api';

export interface StreamFrame {
  id?: number;
  kind?: string;
  data?: string;
}

export type StreamStatus = 'connecting' | 'live' | 'down' | 'unauthorized';

/**
 * Инкрементальный парсер SSE-фреймов: скармливайте чанки как пришли,
 * назад — законченные фреймы (разделитель — пустая строка). Комментарии
 * (`: keepalive`) и `retry:` игнорируются.
 */
export function createSSEParser(): (chunk: string) => StreamFrame[] {
  let buf = '';
  return (chunk: string): StreamFrame[] => {
    buf += chunk;
    const frames: StreamFrame[] = [];
    let idx: number;
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const raw = buf.slice(0, idx);
      buf = buf.slice(idx + 2);
      const frame = parseFrame(raw);
      if (frame !== null) frames.push(frame);
    }
    return frames;
  };
}

function parseFrame(raw: string): StreamFrame | null {
  const frame: StreamFrame = {};
  const dataLines: string[] = [];
  let seen = false;
  for (const line of raw.split('\n')) {
    if (line.startsWith(':')) continue;
    if (line.startsWith('id: ')) {
      const id = Number(line.slice(4));
      if (Number.isFinite(id)) frame.id = id;
      seen = true;
    } else if (line.startsWith('event: ')) {
      frame.kind = line.slice(7);
      seen = true;
    } else if (line.startsWith('data: ')) {
      dataLines.push(line.slice(6));
      seen = true;
    }
  }
  if (!seen) return null;
  if (dataLines.length > 0) frame.data = dataLines.join('\n');
  return frame;
}

export interface StreamEvent {
  id: number;
  kind: string;
  event: ApiEvent;
}

export interface SubscribeOptions {
  onEvent: (e: StreamEvent) => void;
  onStatus: (s: StreamStatus) => void;
  /** База реконнект-паузы, мс (тесты ставят меньше). */
  minRetryMs?: number;
}

/** Подписывается на стрим событий; возвращает функцию отписки. */
export function subscribeEvents(opts: SubscribeOptions): () => void {
  const minRetry = opts.minRetryMs ?? 1000;
  const maxRetry = 15000;
  let stopped = false;
  let ctrl = new AbortController();
  let lastId: number | undefined;

  const sleep = (ms: number) =>
    new Promise<void>((resolve) => {
      setTimeout(resolve, ms);
    });

  async function readLoop(body: ReadableStream<Uint8Array>): Promise<void> {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    const parse = createSSEParser();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return;
      for (const frame of parse(decoder.decode(value, { stream: true }))) {
        if (frame.id !== undefined) lastId = frame.id;
        if (frame.id === undefined || frame.kind === undefined || frame.data === undefined) continue;
        try {
          opts.onEvent({ id: frame.id, kind: frame.kind, event: JSON.parse(frame.data) as ApiEvent });
        } catch {
          // битый data-фрейм не роняет стрим
        }
      }
    }
  }

  async function run(): Promise<void> {
    let retry = minRetry;
    while (!stopped) {
      opts.onStatus('connecting');
      try {
        const url = lastId !== undefined ? `/v1/events/stream?after_id=${lastId}` : '/v1/events/stream';
        const res = await fetch(url, {
          credentials: 'same-origin',
          headers: { Accept: 'text/event-stream' },
          signal: ctrl.signal,
        });
        if (res.status === 401) {
          opts.onStatus('unauthorized'); // сессия умерла — наверх, на login
          return;
        }
        if (!res.ok || res.body === null) throw new Error(`stream: ${res.status}`);
        opts.onStatus('live');
        retry = minRetry;
        await readLoop(res.body); // до обрыва соединения
      } catch {
        if (stopped) return;
      }
      if (stopped) return;
      opts.onStatus('down');
      await sleep(retry);
      retry = Math.min(retry * 2, maxRetry);
    }
  }

  void run();
  return () => {
    stopped = true;
    ctrl.abort();
    ctrl = new AbortController();
  };
}
