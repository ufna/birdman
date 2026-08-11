// Лента последних событий: стартовый снимок из GET /v1/events, дальше —
// живое дополнение из SSE-стрима (дедупликация по id).

import { useEffect, useState } from 'react';
import { api } from '../lib/api';
import type { ApiEvent } from '../lib/api';
import { useLive } from '../lib/live';
import { useEnv, eventEnvOf, keepForEnv } from '../lib/env';
import { useProject, keepForProject } from '../lib/project';
import { shortId, summarizePayload } from '../lib/format';
import { useT, useFormat } from '../lib/i18n';
import type { I18nContextValue } from '../lib/i18n';
import { StateBadge, toneOfEventKind } from './Badge';
import { EmptyState, LoadingRow } from './ui';

const FEED_CAP = 60;

export function EventsFeed() {
  const { subscribe } = useLive();
  const { selected } = useEnv();
  const { selected: project } = useProject();
  const { t } = useT();
  const fmt = useFormat();
  const [events, setEvents] = useState<ApiEvent[] | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    api
      .listEvents(40, project)
      .then((list) => {
        if (!cancelled) setEvents(list);
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
    // project в deps: сменился проект — перезапрашиваем ленту, иначе на экране
    // осталась бы выдача прошлого среза.
  }, [project]);

  useEffect(
    () =>
      subscribe((e) => {
        setEvents((prev) => {
          const base = prev ?? [];
          if (base.some((x) => x.id === e.id)) return base;
          return [e.event, ...base].slice(0, FEED_CAP);
        });
      }),
    [subscribe],
  );

  if (failed) return <EmptyState>{t('events.feedUnavailable')}</EmptyState>;
  if (events === null) return <LoadingRow />;
  // env-фильтр (environments v1 §8, M13) — то же правило, что экран Events:
  // события БЕЗ env видны только в «All»; version_promoted несёт env в to_env.
  // Проектный фильтр поверх него нужен ТОЛЬКО для событий из живого стрима:
  // список сервер уже сузил сам (#985), а стрим один на сессию и о выбранном
  // проекте не знает (см. keepForProject).
  const visible = keepForProject(keepForEnv(events, selected, eventEnvOf), project);
  if (visible.length === 0) {
    return <EmptyState>{events.length > 0 ? t('events.emptyFilter') : t('events.none')}</EmptyState>;
  }

  return (
    <ul className="max-h-[420px] divide-y divide-line overflow-y-auto">
      {visible.map((e) => (
        <li key={e.id} className="flex items-start gap-3 px-4 py-2">
          <span className="tabular shrink-0 pt-0.5 font-mono text-xs text-muted">{fmt.clock(e.ts)}</span>
          <StateBadge state={e.kind} tone={toneOfEventKind(e.kind)} domain="event" />
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
