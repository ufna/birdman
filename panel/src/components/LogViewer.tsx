// Логи дедика: live tail (follow) + скачивание файлом. Работает и для
// умерших дедиков (reaped/failed) — их логи и .gz живут на ноде ретенцию
// (ops.go). Стрим держим через AbortController; переключение follow/tail
// перезапускает соединение.

import { useEffect, useRef, useState } from 'react';
import { streamServerLogs, downloadServerLogs } from '../lib/logs';
import type { LogStreamStatus } from '../lib/logs';
import { shortId } from '../lib/format';

// Мягкий предел буфера, чтобы длинный tail не съедал память вкладки.
const MAX_CHARS = 400_000;
const TAIL_OPTIONS = [200, 1000, 5000];

export function LogViewer({ serverId }: { serverId: string }) {
  const [text, setText] = useState('');
  const [status, setStatus] = useState<LogStreamStatus>('connecting');
  const [follow, setFollow] = useState(true);
  const [tail, setTail] = useState(1000);
  const [downloading, setDownloading] = useState(false);
  const preRef = useRef<HTMLPreElement>(null);
  const stickToBottom = useRef(true);

  useEffect(() => {
    setText('');
    const ctrl = new AbortController();
    void streamServerLogs(serverId, {
      tail,
      follow,
      signal: ctrl.signal,
      onStatus: setStatus,
      onText: (chunk) => {
        setText((prev) => {
          const next = prev + chunk;
          return next.length > MAX_CHARS ? next.slice(next.indexOf('\n', next.length - MAX_CHARS) + 1) : next;
        });
      },
    }).catch(() => {
      /* статус уже 'error' из onStatus */
    });
    return () => {
      ctrl.abort();
    };
  }, [serverId, follow, tail]);

  // Автопрокрут вниз, пока пользователь у нижнего края.
  useEffect(() => {
    const el = preRef.current;
    if (el !== null && follow && stickToBottom.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [text, follow]);

  const onScroll = () => {
    const el = preRef.current;
    if (el === null) return;
    stickToBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  };

  const download = () => {
    setDownloading(true);
    downloadServerLogs(serverId, undefined, shortId(serverId))
      .catch(() => {
        /* тихо: кнопка вернётся в исходное */
      })
      .finally(() => {
        setDownloading(false);
      });
  };

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <LogStatus status={status} follow={follow} />
        <label className="ml-auto flex items-center gap-1.5 text-xs text-muted">
          tail
          <select
            aria-label="Сколько последних строк тянуть"
            className="rounded-md border border-line bg-card px-1.5 py-0.5 text-xs"
            value={tail}
            onChange={(e) => {
              setTail(Number(e.target.value));
            }}
          >
            {TAIL_OPTIONS.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <label className="flex items-center gap-1.5 text-xs text-muted">
          <input
            type="checkbox"
            checked={follow}
            onChange={(e) => {
              setFollow(e.target.checked);
            }}
          />
          follow
        </label>
        <button
          type="button"
          onClick={download}
          disabled={downloading}
          className="rounded-md border border-line px-2 py-0.5 text-xs text-muted transition-colors hover:text-ink disabled:opacity-50"
        >
          {downloading ? 'Готовим…' : 'Скачать'}
        </button>
      </div>
      <pre
        ref={preRef}
        onScroll={onScroll}
        className="tabular h-72 overflow-auto rounded-lg border border-line bg-paper p-3 font-mono text-xs leading-relaxed whitespace-pre text-ink"
      >
        {text === '' ? (
          <span className="text-muted">
            {status === 'ended' ? 'Логов нет.' : status === 'error' ? 'Не удалось получить логи.' : 'Ждём строки…'}
          </span>
        ) : (
          text
        )}
      </pre>
    </div>
  );
}

function LogStatus({ status, follow }: { status: LogStreamStatus; follow: boolean }) {
  const view: Record<LogStreamStatus, { cls: string; label: string }> = {
    connecting: { cls: 'bg-warn', label: 'подключение…' },
    streaming: { cls: 'bg-good', label: follow ? 'live tail' : 'читаем…' },
    ended: { cls: 'bg-muted', label: 'готово' },
    error: { cls: 'bg-dead', label: 'ошибка' },
    unauthorized: { cls: 'bg-dead', label: 'нет доступа' },
  };
  const v = view[status];
  return (
    <span className="inline-flex items-center gap-2 font-mono text-xs text-muted">
      <span aria-hidden className={`size-2 rounded-full ${v.cls} ${status === 'streaming' && follow ? 'live-dot-on' : ''}`} />
      {v.label}
    </span>
  );
}
