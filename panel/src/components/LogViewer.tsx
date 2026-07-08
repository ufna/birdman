// Логи дедика: live tail (follow) + скачивание файлом. Работает и для
// умерших дедиков (reaped/failed) — их логи и .gz живут на ноде ретенцию
// (ops.go). Стрим держим через AbortController; переключение follow/tail
// перезапускает соединение.

import { useEffect, useRef, useState } from 'react';
import { streamServerLogs, downloadServerLogs } from '../lib/logs';
import type { LogStreamStatus } from '../lib/logs';
import { shortId } from '../lib/format';
import { useT } from '../lib/i18n';
import type { MessageKey } from '../lib/i18n';

// Мягкий предел буфера, чтобы длинный tail не съедал память вкладки.
const MAX_CHARS = 400_000;
const TAIL_OPTIONS = [200, 1000, 5000];

/** initialFollow=false — для завершённых сущностей (матч finished): не тейлим. */
export function LogViewer({ serverId, initialFollow = true }: { serverId: string; initialFollow?: boolean }) {
  const { t } = useT();
  const [text, setText] = useState('');
  const [status, setStatus] = useState<LogStreamStatus>('connecting');
  const [follow, setFollow] = useState(initialFollow);
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
          {t('logs.tail')}
          <select
            aria-label={t('logs.tailAria')}
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
          {t('logs.follow')}
        </label>
        <button
          type="button"
          onClick={download}
          disabled={downloading}
          className="rounded-md border border-line px-2 py-0.5 text-xs text-muted transition-colors hover:text-ink disabled:opacity-50"
        >
          {downloading ? t('logs.preparing') : t('logs.download')}
        </button>
      </div>
      <pre
        ref={preRef}
        onScroll={onScroll}
        className="tabular h-72 overflow-auto rounded-lg border border-line bg-paper p-3 font-mono text-xs leading-relaxed whitespace-pre text-ink"
      >
        {text === '' ? (
          <span className="text-muted">{t(emptyKey(status))}</span>
        ) : (
          text
        )}
      </pre>
    </div>
  );
}

/** Подпись пустого лога по статусу: gone/ended → «нет/недоступно», error →
 *  ошибка, иначе — ждём строки. */
function emptyKey(status: LogStreamStatus): MessageKey {
  if (status === 'gone') return 'logs.gone';
  if (status === 'ended') return 'logs.empty';
  if (status === 'error') return 'logs.error';
  return 'logs.waiting';
}

function LogStatus({ status, follow }: { status: LogStreamStatus; follow: boolean }) {
  const { t } = useT();
  const view: Record<LogStreamStatus, { cls: string; key: MessageKey }> = {
    connecting: { cls: 'bg-warn', key: 'logs.connecting' },
    streaming: { cls: 'bg-good', key: follow ? 'logs.liveTail' : 'logs.reading' },
    ended: { cls: 'bg-muted', key: 'logs.done' },
    gone: { cls: 'bg-muted', key: 'logs.gone' },
    error: { cls: 'bg-dead', key: 'logs.errorShort' },
    unauthorized: { cls: 'bg-dead', key: 'live.noAccess' },
  };
  const v = view[status];
  return (
    <span className="inline-flex items-center gap-2 font-mono text-xs text-muted">
      <span aria-hidden className={`size-2 rounded-full ${v.cls} ${status === 'streaming' && follow ? 'live-dot-on' : ''}`} />
      {t(v.key)}
    </span>
  );
}
