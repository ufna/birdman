// Детали матча (дровер справа): метаданные (регион, версия, длительность, пик
// игроков, начало/конец), статистика (tick_ms и игроки за окно матча) и логи
// дедика. Переиспользует паттерн ServerDrawer + MetricChart + LogViewer. Всё
// на публичном API: GET /v1/matches/{id}, metrics-proxy, /v1/servers/{id}/logs.
//
// Живой матч обновляется по SSE (useData рефетчит на события стрима); finished —
// статичен. Грациозная деградация: у старых матчей server_id может быть вычищен
// или логи/метрики истечь — показываем аккуратные пустые состояния, не спиннер.

import * as Dialog from '@radix-ui/react-dialog';
import * as Tabs from '@radix-ui/react-tabs';
import { api } from '../lib/api';
import type { Match } from '../lib/api';
import { useData } from '../lib/live';
import { useNow } from '../lib/useNow';
import { useT, useFormat } from '../lib/i18n';
import { shortId } from '../lib/format';
import { versionColor } from '../lib/stats';
import { matchDurationMs, matchMetricRange, isMatchOver } from '../lib/match';
import { matchMetricQueries } from '../lib/metrics';
import { StateBadge, toneOfMatchState } from './Badge';
import { MetricChart } from './MetricChart';
import { LogViewer } from './LogViewer';
import { EmptyState, ErrorNote, LoadingRow } from './ui';

export function MatchDrawer({ matchId, onClose }: { matchId: string | null; onClose: () => void }) {
  return (
    <Dialog.Root
      open={matchId !== null}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed top-0 right-0 z-50 flex h-screen w-[min(42rem,100vw)] flex-col border-l border-line bg-card shadow-2xl focus:outline-none"
        >
          {matchId !== null ? <MatchBody matchId={matchId} /> : <HiddenTitle />}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function HiddenTitle() {
  const { t } = useT();
  return <Dialog.Title className="sr-only">{t('col.match')}</Dialog.Title>;
}

function MatchBody({ matchId }: { matchId: string }) {
  const { t } = useT();
  const match = useData(() => api.getMatch(matchId), [matchId]);
  const m = match.data;

  return (
    <>
      <header className="flex items-start justify-between gap-3 border-b border-line px-5 py-4">
        <div className="min-w-0">
          <Dialog.Title className="flex items-center gap-2 text-base font-semibold">
            <span className="font-mono">
              {t('col.match')} {shortId(matchId)}
            </span>
            {m !== undefined && <StateBadge state={m.state} tone={toneOfMatchState(m.state)} domain="match" />}
          </Dialog.Title>
          {m !== undefined && !isMatchOver(m) && (
            <p className="mt-1 inline-flex items-center gap-1.5 text-xs text-good">
              <span aria-hidden className="size-1.5 rounded-full bg-good live-dot-on" />
              {t('match.liveHint')}
            </p>
          )}
        </div>
        <Dialog.Close asChild>
          <button
            type="button"
            aria-label={t('common.close')}
            className="shrink-0 rounded-lg border border-line p-1.5 text-muted hover:text-ink"
          >
            <svg viewBox="0 0 16 16" className="size-4" aria-hidden>
              <path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
            </svg>
          </button>
        </Dialog.Close>
      </header>

      {m === undefined ? (
        match.error !== undefined ? (
          <div className="p-5">
            <ErrorNote error={match.error} retry={match.reload} />
          </div>
        ) : (
          <LoadingRow />
        )
      ) : (
        <MatchContent m={m} />
      )}
    </>
  );
}

function MatchContent({ m }: { m: Match }) {
  const { t } = useT();
  const range = matchMetricRange(m);
  const queries = matchMetricQueries(m.server_id);

  return (
    <>
      <MatchMeta m={m} />
      <Tabs.Root defaultValue="stats" className="flex min-h-0 flex-1 flex-col">
        <Tabs.List className="flex gap-1 border-b border-line px-5 py-2" aria-label={t('match.sections')}>
          {[
            { v: 'stats', l: t('match.tab.stats') },
            { v: 'logs', l: t('match.tab.logs') },
          ].map((tab) => (
            <Tabs.Trigger
              key={tab.v}
              value={tab.v}
              className="rounded-md px-3 py-1 text-sm text-muted transition-colors data-[state=active]:bg-mark data-[state=active]:font-medium data-[state=active]:text-accent-ink"
            >
              {tab.l}
            </Tabs.Trigger>
          ))}
        </Tabs.List>

        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
          <Tabs.Content value="stats">
            {range === null ? (
              <EmptyState>{t('match.noServer')}</EmptyState>
            ) : (
              <>
                <p className="mb-3 text-xs text-muted">{t('match.statsNote')}</p>
                <div className="grid gap-3 sm:grid-cols-2">
                  {queries.map((q) => (
                    <MetricChart key={q.key} query={q.expr} title={t(q.titleKey)} unit={q.unit} range={range} />
                  ))}
                </div>
              </>
            )}
          </Tabs.Content>
          <Tabs.Content value="logs">
            {m.server_id === '' ? (
              <EmptyState>{t('match.logs.gone')}</EmptyState>
            ) : (
              <LogViewer serverId={m.server_id} initialFollow={!isMatchOver(m)} />
            )}
          </Tabs.Content>
        </div>
      </Tabs.Root>
    </>
  );
}

/** Метаданные матча: тикающая длительность у живого, статичная у завершённого. */
function MatchMeta({ m }: { m: Match }) {
  const { t, tp } = useT();
  const fmt = useFormat();
  const now = useNow();
  const durationMs = matchDurationMs(m, now);

  const rows: { label: string; value: string; color?: string }[] = [
    { label: t('col.region'), value: m.region },
    { label: t('col.version'), value: m.semver, color: versionColor(m.semver) },
    { label: t('col.duration'), value: durationMs === null ? '—' : fmt.age(durationMs) },
    { label: t('col.playersPeak'), value: tp('common.playersCount', m.players_peak) },
    { label: t('match.started'), value: fmt.stamp(m.started_at ?? m.created_at) },
    { label: t('match.ended'), value: m.ended_at !== undefined ? fmt.stamp(m.ended_at) : '—' },
    { label: t('col.server'), value: m.server_id !== '' ? `${shortId(m.server_id)} · ${m.host}:${m.port}` : '—' },
  ];

  return (
    <dl className="grid grid-cols-2 gap-x-6 gap-y-2.5 border-b border-line px-5 py-4 sm:grid-cols-3">
      {rows.map((r) => (
        <div key={r.label} className="min-w-0">
          <dt className="text-xs tracking-wide text-muted uppercase">{r.label}</dt>
          <dd className="tabular mt-0.5 flex items-center gap-1.5 truncate font-mono text-sm">
            {r.color !== undefined && (
              <span aria-hidden className="size-2 shrink-0 rounded-[2px]" style={{ background: r.color }} />
            )}
            <span className="truncate">{r.value}</span>
          </dd>
        </div>
      ))}
    </dl>
  );
}
