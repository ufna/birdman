// Логи дедика через master-прокси (GET /v1/servers/{id}/logs, ops.go):
// chunked text/plain. follow=1 — live tail (стрим до дисконнекта), без follow —
// конечный ответ для скачивания. Работает и для reaped/failed дедиков.

import { ApiError, serverLogsPath } from './api';

/** Минимальный контракт ридера тела ответа — чтобы тестировать без сети. */
export interface ByteReader {
  read: () => Promise<{ done: boolean; value?: Uint8Array }>;
}

/**
 * Выкачивает текстовый стрим из ридера, декодируя UTF-8 потоково (чанк может
 * рвать многобайтовый символ) и отдавая непустые куски в onText. Возвращает
 * промис, завершающийся по концу стрима. Чистая относительно сети — точка
 * тестирования чтения логов.
 */
export async function pumpTextStream(
  reader: ByteReader,
  onText: (chunk: string) => void,
  decoder: TextDecoder = new TextDecoder(),
): Promise<void> {
  for (;;) {
    const { done, value } = await reader.read();
    if (done) {
      const tail = decoder.decode(); // флаш незавершённых байтов
      if (tail !== '') onText(tail);
      return;
    }
    if (value !== undefined && value.length > 0) {
      const text = decoder.decode(value, { stream: true });
      if (text !== '') onText(text);
    }
  }
}

// 'gone' — сервер вычищен (404): логи истекли/дедик реапнут; UI показывает
// аккуратное пустое состояние, а не ошибку.
export type LogStreamStatus = 'connecting' | 'streaming' | 'ended' | 'error' | 'unauthorized' | 'gone';

export interface StreamLogsOptions {
  tail?: number;
  follow?: boolean;
  onText: (chunk: string) => void;
  onStatus?: (s: LogStreamStatus) => void;
  signal?: AbortSignal;
}

/**
 * Открывает лог-стрим дедика и качает его в onText. При follow держит
 * соединение до отмены через signal; без follow дочитывает до EOF. Отписка —
 * через переданный AbortSignal (abort → стрим закрывается, master отменяет
 * tail на агенте).
 */
export async function streamServerLogs(id: string, opts: StreamLogsOptions): Promise<void> {
  const status = opts.onStatus ?? (() => {});
  status('connecting');
  let res: Response;
  try {
    res = await fetch(serverLogsPath(id, { follow: opts.follow, tail: opts.tail }), {
      credentials: 'same-origin',
      signal: opts.signal,
    });
  } catch (e) {
    if (opts.signal?.aborted) {
      status('ended');
      return;
    }
    status('error');
    throw e;
  }
  if (res.status === 401) {
    status('unauthorized');
    return;
  }
  // 404 — дедик/логи вычищены (ретенция, reaped server_id): не ошибка стрима,
  // а «больше недоступно». Тело не читаем, наверх не бросаем.
  if (res.status === 404) {
    status('gone');
    return;
  }
  if (!res.ok || res.body === null) {
    status('error');
    const detail = await res.text().catch(() => '');
    throw new ApiError(res.status, 'logs_failed', detail.slice(0, 160));
  }
  status('streaming');
  try {
    await pumpTextStream(res.body.getReader(), opts.onText);
    status('ended');
  } catch (e) {
    if (opts.signal?.aborted) {
      status('ended');
      return;
    }
    status('error');
    throw e;
  }
}

/**
 * Скачивает логи дедика файлом (тот же эндпоинт без follow → Blob → <a
 * download>). Возвращает промис, чтобы UI мог показать «готовим…».
 */
export async function downloadServerLogs(id: string, tail?: number, shortId = id): Promise<void> {
  const res = await fetch(serverLogsPath(id, { tail }), { credentials: 'same-origin' });
  if (!res.ok) {
    const detail = await res.text().catch(() => '');
    throw new ApiError(res.status, 'logs_failed', detail.slice(0, 160));
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = `dedic-${shortId}.log`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}
