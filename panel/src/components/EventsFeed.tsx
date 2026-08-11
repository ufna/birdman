// Лента последних событий: стартовый снимок из GET /v1/events, дальше —
// живое дополнение из SSE-стрима (дедупликация по id).

import { useEffect, useState } from 'react';
import { api } from '../lib/api';
import type { ApiEvent } from '../lib/api';
import { useLive } from '../lib/live';
import { useEnv, eventEnvOf, keepForEnv } from '../lib/env';
import { useFeedScope, scopeProject, keepForProject } from '../lib/project';
import { shortId, summarizePayload } from '../lib/format';
import { useT, useFormat } from '../lib/i18n';
import type { I18nContextValue } from '../lib/i18n';
import { EventScopeChip, StateBadge, toneOfEventKind } from './Badge';
import { EmptyState, LoadingRow } from './ui';

const FEED_CAP = 60;

export function EventsFeed() {
  const { subscribe } = useLive();
  const { selected } = useEnv();
  // Не `useProject().selected`: тот не различает «проект неизвестен» и «проектов
  // нет», а лента обязана вести себя по-разному — см. FeedScope.
  const scope = useFeedScope();
  const { t } = useT();
  const fmt = useFormat();
  const [events, setEvents] = useState<ApiEvent[] | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    // Проект неизвестен (список грузится / не приехал) — молчим: голый запрос
    // привёз бы события ВСЕХ проектов. «Проектов нет вовсе» — другой случай:
    // сужать нечем, чужого не существует, и платформенные события обязаны быть
    // видны (свежая установка — ровно то место, где их и смотрят).
    if (scope.kind === 'wait') {
      setEvents(null);
      setFailed(false);
      return;
    }
    let cancelled = false;
    setFailed(false);
    api
      .listEvents(40, scopeProject(scope))
      .then((list) => {
        if (!cancelled) setEvents(list);
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
    // scope в deps: сменился проект — перезапрашиваем ленту, иначе на экране
    // осталась бы выдача прошлого среза.
  }, [scope]);

  // Живой стрим — единственный не суженный сервером источник (см.
  // keepForProject). Чужое отсеиваем НА ВХОДЕ, ровно как экран Событий: иначе
  // события соседнего проекта занимают места в окне FEED_CAP и вытесняют свои
  // ещё до показа.
  useEffect(
    () =>
      subscribe((e) => {
        if (keepForProject([e.event], scopeProject(scope)).length === 0) return;
        setEvents((prev) => {
          const base = prev ?? [];
          if (base.some((x) => x.id === e.id)) return base;
          return [e.event, ...base].slice(0, FEED_CAP);
        });
      }),
    [subscribe, scope],
  );

  if (failed) return <EmptyState>{t('events.feedUnavailable')}</EmptyState>;
  if (events === null) return <LoadingRow />;
  // env-фильтр (environments v1 §8, M13) — то же правило, что экран Events:
  // события БЕЗ env видны только в «All»; version_promoted несёт env в to_env.
  // Проектного фильтра здесь НЕТ: список сужает сервер (#985), а события стрима
  // отсеяны на входе выше — второй копии правила в показе быть не должно.
  const visible = keepForEnv(events, selected, eventEnvOf);
  if (visible.length === 0) {
    return <EmptyState>{events.length > 0 ? t('events.emptyFilter') : t('events.none')}</EmptyState>;
  }

  return (
    <ul className="max-h-[420px] divide-y divide-line overflow-y-auto">
      {visible.map((e) => (
        <li key={e.id} className="flex items-start gap-3 px-4 py-2">
          <span className="tabular shrink-0 pt-0.5 font-mono text-xs text-muted">{fmt.clock(e.ts)}</span>
          <StateBadge state={e.kind} tone={toneOfEventKind(e.kind)} domain="event" />
          {/* Честная подпись платформенного события: оно остаётся видимым при
              любом выбранном проекте (сужение не скрывающее). */}
          <EventScopeChip event={e} />
          <span className="min-w-0 flex-1 truncate pt-0.5 text-xs text-muted">
            {refsOf(e, t)}
            {Object.keys(e.payload).length > 0 && (
              <span className="text-ink/80"> {summarizePayload(e.payload)}</span>
            )}
          </span>
        </li>
      ))}
    </ul>
  );
}

function refsOf(e: ApiEvent, t: I18nContextValue['t']): string {
  const refs: string[] = [];
  if (e.node_id !== undefined) refs.push(`${t('ref.node')} ${shortId(e.node_id)}`);
  if (e.server_id !== undefined) refs.push(`${t('ref.srv')} ${shortId(e.server_id)}`);
  if (e.match_id !== undefined) refs.push(`${t('ref.match')} ${shortId(e.match_id)}`);
  return refs.length > 0 ? `${refs.join(' · ')} · ` : '';
}
