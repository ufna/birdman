// Статистика (П2 + П1 "Статистика v1"): выбор окна решает режим. 12ч/24ч/3д —
// live-режим, операционные ряды напрямую из VictoriaMetrics (metrics-proxy);
// 7д/30д — product-режим, обзорные агрегаты (GET /v1/stats/overview), как и
// раньше. В product-режиме: матчи/день и игроки/день — стек по регионам; пик
// CCU и средняя длительность — одиночные ряды; распределение по версиям —
// доли (единый цвет версии). Fill-rate матчмейкера показан из ДВУХ источников
// в ОБОИХ режимах: истинное queue→match из гистограммы
// birdman_mm_time_to_match_seconds (metrics-proxy) и прокси allocation→start
// (из matches, /v1/stats/overview). Ряды UTC и зеро-филлены на бэке. Живое
// обновление: refetch по SSE с дебансом (useLiveAsync) — новый матч
// подтягивает свежие числа/графики.

import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { api } from '../lib/api';
import type { StatsOverview, TimeToMatch } from '../lib/api';
import { useLiveAsync } from '../lib/live';
import { useEnv } from '../lib/env';
import { useProject } from '../lib/project';
import { useT, useFormat } from '../lib/i18n';
import type { MessageKey } from '../lib/i18n';
import { toSimpleColumns, toStackModel, versionShareModel } from '../lib/stats';
import {
  matchesRunningQuery,
  playersOnlineQuery,
  queueDepthQuery,
  timeToMatchQuantileQuery,
  utilizationRatioQuery,
} from '../lib/metrics';
import { useInstantQuery } from '../lib/useMetrics';
import type { MetricStatus } from '../lib/useMetrics';
import { DEFAULT_RANGE_KEY, STATS_RANGES, rangeByKey } from '../lib/statsRange';
import type { StatsRange } from '../lib/statsRange';
import {
  Card,
  CardHeader,
  ChartSkeleton,
  ErrorNote,
  Skeleton,
  SkeletonRegion,
  StatCard,
  StatCardSkeleton,
} from '../components/ui';
import { BarChart, ChartHeading, RangeSelect, ShareBars } from '../components/charts';
import { MetricChart } from '../components/MetricChart';
import { UtilizationChart } from '../components/UtilizationChart';

/**
 * Дни-эквивалент периода для api.statsOverview()/гистограммы TTM в ОБОИХ
 * режимах: product — как задан диапазоном; live — окно в днях, округлённое
 * вверх, не меньше 1 (12ч/24ч → 1д, 3д → 3д). Простое, единое место: и запрос
 * "истинного" TTM (histogram_quantile), и alloc→start прокси из /v1/stats/
 * overview используют один и тот же days для live-окна.
 */
function effectiveDays(range: StatsRange): number {
  if (range.mode === 'product') return range.days ?? 30;
  return Math.max(1, Math.ceil((range.windowMs ?? 0) / 86_400_000));
}

export function Stats() {
  const [rangeKey, setRangeKey] = useState(DEFAULT_RANGE_KEY);
  const range = rangeByKey(rangeKey);
  const days = effectiveDays(range);
  const { selected } = useEnv();
  const { selected: project } = useProject();
  // env сужает историю через ?env= (environments v1 §7, I5). CCU остаётся
  // глобальным платформенным пиком — StatsBody подписывает его «platform-wide».
  //
  // Ответ /v1/stats/* НЕ несёт env, поэтому помечаем данные окружением, за
  // которое их запросили: «готовность» обязана учитывать и период, И env —
  // иначе при переключении чипа мгновение показываются данные ПРЕЖНЕГО
  // окружения как готовые (follow-up p3).
  const ov = useLiveAsync(
    () =>
      api
        .statsOverview(days, { project: project ?? undefined, env: selected ?? undefined })
        .then((data) => ({ env: selected, project, data })),
    [days, selected, project],
  );
  // Данные годны, только если они за ЗАПРОШЕННЫЙ период и ЗАПРОШЕННЫЙ env;
  // иначе (первая загрузка, смена периода/режима/окружения) — скелетон под
  // финальную раскладку.
  const readyOv =
    ov.data !== undefined && ov.data.env === selected && ov.data.project === project && ov.data.data.days === days
      ? ov.data.data
      : undefined;

  // Полноэкранная ошибка — ТОЛЬКО в product-режиме: там /v1/stats/overview и
  // есть страница. В live-режиме VM-панели (MetricChart/UtilizationChart) и
  // истинный TTM читаются напрямую из metrics-proxy и не зависят от product-
  // API — гейтить их ошибкой overview нельзя (Critical bug: раньше падение
  // /v1/stats/overview гасило ВСЮ страницу даже при здоровой VM в дефолтном
  // 24h/live виде). Деградацию своей (alloc→start) строки live обрабатывает
  // сам FillRateCard — см. LiveBody.
  if (range.mode === 'product' && ov.error !== undefined && ov.data === undefined) {
    return (
      <div className="flex flex-col gap-4">
        <Header rangeKey={rangeKey} setRangeKey={setRangeKey} />
        <ErrorNote error={ov.error} retry={ov.reload} />
      </div>
    );
  }
  return (
    <div className="flex flex-col gap-4">
      <Header rangeKey={rangeKey} setRangeKey={setRangeKey} />
      {range.mode === 'live' ? (
        <LiveBody range={range} days={days} ov={{ ready: readyOv, error: ov.error }} />
      ) : readyOv !== undefined ? (
        <StatsBody ov={readyOv} envActive={selected !== null} projectScoped={project !== null} />
      ) : (
        <StatsSkeleton />
      )}
    </div>
  );
}

function Header({ rangeKey, setRangeKey }: { rangeKey: string; setRangeKey: (k: string) => void }) {
  const { t } = useT();
  const options = STATS_RANGES.map((r) => ({ value: r.key, label: t(r.labelKey) }));
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div className="flex items-center gap-2">
        <h1 className="text-lg font-semibold">{t('nav.stats')}</h1>
        <span className="rounded border border-line px-1.5 py-0.5 font-mono text-[10px] text-muted">{t('stats.utc')}</span>
      </div>
      <RangeSelect value={rangeKey} onChange={setRangeKey} options={options} ariaLabel={t('stats.range.aria')} />
    </div>
  );
}

/**
 * Live-режим (12ч/24ч/3д): гранулярные операционные гейджи через
 * VictoriaMetrics (metrics-proxy) — онлайн/матчи в игре/очередь/утилизация —
 * плюс ряд занятости по состояниям (как в Cost) и fill-rate-карточка (общая с
 * product-режимом, см. ambiguity resolution #3: TTM-окно ≤30д всегда в
 * пределах ретеншена VM). Деградацию VM даёт сам MetricChart/UtilizationChart.
 *
 * `ov` — состояние /v1/stats/overview (для вторичной alloc→start строки
 * fill-rate-карточки), НЕ гейт: пока он ещё грузится (нет ни данных, ни
 * ошибки) — показываем FillRateSkeleton, как и раньше; если он осел с ошибкой
 * (или данные за другой период/env) — FillRateCard всё равно монтируется,
 * просто его alloc-строка деградирует (Critical bug fix: раньше это было
 * «навечно скелетон», а VM-панели выше вообще не зависят от ov и всегда
 * рендерятся).
 */
function LiveBody({ range, days, ov }: { range: StatsRange; days: number; ov: OverviewState }) {
  const { t } = useT();
  const windowMs = range.windowMs ?? 24 * 60 * 60_000;
  const readyOv = ov.ready;
  return (
    <div className="flex flex-col gap-4">
      <div className="grid gap-3 sm:grid-cols-2">
        <MetricChart query={playersOnlineQuery()} title={t('stats.live.online')} unit="int" windowMs={windowMs} />
        <MetricChart query={matchesRunningQuery()} title={t('stats.live.matchesRunning')} unit="int" windowMs={windowMs} />
        <MetricChart query={queueDepthQuery()} title={t('stats.live.queue')} unit="int" windowMs={windowMs} />
        <MetricChart query={utilizationRatioQuery()} title={t('stats.live.util')} unit="percent" windowMs={windowMs} />
      </div>
      <Card>
        <CardHeader
          title={t('stats.live.utilOverTime')}
          aside={<span className="font-mono text-xs text-muted">{t('stats.live.utilOverTimeNote')}</span>}
        />
        <UtilizationChart windowMs={windowMs} />
      </Card>
      {readyOv !== undefined ? (
        <FillRateCard ttm={readyOv.time_to_match} days={days} refetchKey={readyOv.generated_at} />
      ) : ov.error !== undefined ? (
        <FillRateCard ttm={undefined} days={days} refetchKey={`live-err-${String(days)}`} />
      ) : (
        <FillRateSkeleton />
      )}
    </div>
  );
}

/** Состояние /v1/stats/overview, нужное LiveBody: ГОДНЫЕ данные (за текущий
 *  период и env) либо ошибка — гейт готовности считает Stats выше. */
interface OverviewState {
  ready?: StatsOverview;
  error?: Error;
}

/** Скелетон fill-rate карточки (live-режим, пока не готов ov для alloc→start прокси). */
function FillRateSkeleton() {
  return (
    <SkeletonRegion>
      <Card className="p-4">
        <Skeleton className="mb-3 h-4 w-40" />
        <Skeleton className="h-24 w-full" rounded="rounded-lg" />
      </Card>
    </SkeletonRegion>
  );
}

/** Скелетон Stats: 4 карточки + 4 графика + распределение/fill-rate. */
function StatsSkeleton() {
  return (
    <SkeletonRegion>
      <div className="flex flex-col gap-4">
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
          {[0, 1, 2, 3].map((i) => (
            <StatCardSkeleton key={i} />
          ))}
        </div>
        <div className="grid gap-4 lg:grid-cols-2">
          {[0, 1, 2, 3].map((i) => (
            <ChartSkeleton key={i} />
          ))}
        </div>
        <div className="grid gap-4 lg:grid-cols-2">
          {[0, 1].map((i) => (
            <Card key={i} className="p-4">
              <Skeleton className="mb-3 h-4 w-40" />
              <Skeleton className="h-24 w-full" rounded="rounded-lg" />
            </Card>
          ))}
        </div>
      </div>
    </SkeletonRegion>
  );
}

/** Подпись пика CCU при активном env-фильтре: «весь» — это проект, если он
 *  выбран (мультипроект W3 дал пику проектное измерение), иначе платформа.
 *  Вынесено функцией, потому что используется в двух местах и разъехаться им
 *  нельзя: карточка и заголовок графика описывают ОДНО число. */
function ccuScopeKey(projectScoped: boolean): MessageKey {
  return projectScoped ? 'stats.ccu.projectWide' : 'stats.ccu.platformWide';
}

function StatsBody({
  ov,
  envActive,
  projectScoped,
}: {
  ov: StatsOverview;
  envActive: boolean;
  projectScoped: boolean;
}) {
  const { t } = useT();
  const fmt = useFormat();
  const int = (v: number) => String(Math.round(v));
  const dur = (sec: number | null | undefined) => (sec == null ? '—' : fmt.age(sec * 1000));

  const matches = useMemo(() => toStackModel(ov.matches_per_day), [ov.matches_per_day]);
  const players = useMemo(() => toStackModel(ov.players_per_day), [ov.players_per_day]);
  const ccu = useMemo(() => toSimpleColumns(ov.peak_ccu_per_day), [ov.peak_ccu_per_day]);
  const avgDur = useMemo(() => toSimpleColumns(ov.avg_match_duration_per_day), [ov.avg_match_duration_per_day]);
  const versions = useMemo(() => versionShareModel(ov.version_distribution), [ov.version_distribution]);

  const totalMatches = matches.columns.reduce((s, c) => s + c.total, 0);
  const ttm = ov.time_to_match;
  const sharePct = (share: number) => `${(share * 100).toFixed(share >= 0.1 ? 0 : 1)}%`;

  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <StatCard label={t('stats.card.matches')} value={int(totalMatches)} detail={t('stats.card.matchesDetail', { days: ov.days })} />
        <StatCard
          label={t('stats.card.peakCcu')}
          value={ov.peak_ccu}
          detail={envActive ? t(ccuScopeKey(projectScoped)) : t('stats.card.peakCcuDetail')}
        />
        <StatCard label={t('stats.card.avgDuration')} value={dur(ov.avg_match_duration_seconds)} detail={t('stats.card.avgDurationDetail')} />
        <StatCard
          label={t('stats.card.fillRate')}
          value={
            <span className="tabular">
              {dur(ttm.p50_seconds)}
              <span className="text-base font-normal text-muted"> / {dur(ttm.p95_seconds)}</span>
            </span>
          }
          detail={t('stats.card.fillRateDetail', { count: ttm.samples })}
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card className="p-4">
          <ChartHeading title={t('stats.matchesPerDay')} note={t('stats.byRegion')} />
          <BarChart
            columns={matches.columns}
            max={matches.max}
            keys={matches.keys}
            formatValue={int}
            empty={matches.empty}
            ariaLabel={t('stats.matchesPerDay')}
            emptyLabel={t('stats.noData')}
          />
        </Card>
        <Card className="p-4">
          <ChartHeading title={t('stats.playersPerDay')} note={t('stats.byRegion')} />
          <BarChart
            columns={players.columns}
            max={players.max}
            keys={players.keys}
            formatValue={int}
            empty={players.empty}
            ariaLabel={t('stats.playersPerDay')}
            emptyLabel={t('stats.noData')}
          />
        </Card>
        <Card className="p-4">
          <ChartHeading title={t('stats.peakCcuPerDay')} note={envActive ? t(ccuScopeKey(projectScoped)) : t('stats.peakCcuNote')} />
          <BarChart
            columns={ccu.columns}
            max={ccu.max}
            formatValue={int}
            empty={ccu.empty}
            ariaLabel={t('stats.peakCcuPerDay')}
            emptyLabel={t('stats.noData')}
          />
        </Card>
        <Card className="p-4">
          <ChartHeading title={t('stats.avgDurationPerDay')} note={t('stats.avgDurationNote')} />
          <BarChart
            columns={avgDur.columns}
            max={avgDur.max}
            formatValue={(v) => fmt.age(v * 1000)}
            empty={avgDur.empty}
            ariaLabel={t('stats.avgDurationPerDay')}
            emptyLabel={t('stats.noData')}
          />
        </Card>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader title={t('stats.versionDist')} aside={<span className="font-mono text-xs text-muted">{t('stats.versionDistNote')}</span>} />
          <div className="p-4">
            <ShareBars rows={versions} sharePct={sharePct} />
          </div>
        </Card>
        <FillRateCard ttm={ttm} days={ov.days} refetchKey={ov.generated_at} />
      </div>
    </div>
  );
}

/**
 * Fill-rate из двух источников: истинное queue→match (гистограмма time-to-match
 * через metrics-proxy) и прокси allocation→start (из matches). Гистограмма
 * пуста/недоступна → деградируем на прокси с пометкой (dev: данных мало).
 * `ttm` может отсутствовать (live-режим, /v1/stats/overview ещё грузится или
 * осел с ошибкой) — тогда alloc-строка сама деградирует с пометкой
 * недоступности, а не блокирует всю карточку (истинная queue→match строка не
 * зависит от `ttm` вовсе и продолжает работать через metrics-proxy).
 */
function FillRateCard({ ttm, days, refetchKey }: { ttm: TimeToMatch | undefined; days: number; refetchKey: string }) {
  const { t } = useT();
  const fmt = useFormat();
  const dur = (sec: number | null | undefined) => (sec == null ? '—' : fmt.age(sec * 1000));
  const trueTtm = useTrueTimeToMatch(days, refetchKey);

  return (
    <Card>
      <CardHeader title={t('stats.fillRate')} />
      <div className="flex flex-col gap-3 p-4">
        <TtmSource label={t('stats.ttm.srcQueue')} note={t('stats.ttm.srcQueueNote')}>
          {trueTtm.status === 'loading' ? (
            <SourceNote>{t('stats.ttm.trueLoading')}</SourceNote>
          ) : trueTtm.status !== 'ok' ? (
            <SourceNote>{t('stats.ttm.trueUnavailable')}</SourceNote>
          ) : trueTtm.p50 === null && trueTtm.p95 === null ? (
            <SourceNote>{t('stats.ttm.trueDegraded')}</SourceNote>
          ) : (
            <PercentileRow p50={dur(trueTtm.p50)} p95={dur(trueTtm.p95)} />
          )}
        </TtmSource>
        <TtmSource
          label={t('stats.ttm.srcAlloc')}
          note={ttm !== undefined ? t('stats.ttm.note', { count: ttm.samples }) : undefined}
        >
          {ttm === undefined ? (
            <SourceNote>{t('stats.ttm.allocUnavailable')}</SourceNote>
          ) : (
            <PercentileRow p50={dur(ttm.p50_seconds)} p95={dur(ttm.p95_seconds)} />
          )}
        </TtmSource>
      </div>
    </Card>
  );
}

interface TrueTtm {
  status: MetricStatus;
  p50: number | null;
  p95: number | null;
}

/** Истинные p50/p95 time-to-match за период из гистограммы (два instant-запроса). */
function useTrueTimeToMatch(days: number, refetchKey: string): TrueTtm {
  const p50 = useInstantQuery({ query: timeToMatchQuantileQuery(0.5, days), refetchKey });
  const p95 = useInstantQuery({ query: timeToMatchQuantileQuery(0.95, days), refetchKey });
  const status = combineStatus(p50.status, p95.status);
  return { status, p50: p50.vector?.[0]?.value ?? null, p95: p95.vector?.[0]?.value ?? null };
}

/** Один общий статус двух запросов: незавершённый/недоступный «побеждает» ok. */
function combineStatus(a: MetricStatus, b: MetricStatus): MetricStatus {
  if (a === 'loading' || b === 'loading') return 'loading';
  if (a !== 'ok') return a;
  if (b !== 'ok') return b;
  return 'ok';
}

function TtmSource({ label, note, children }: { label: string; note?: string; children: ReactNode }) {
  return (
    <div>
      <div className="mb-1.5 font-mono text-xs font-medium text-ink">{label}</div>
      {children}
      {note !== undefined && <p className="mt-1.5 text-xs text-muted">{note}</p>}
    </div>
  );
}

function SourceNote({ children }: { children: ReactNode }) {
  return <div className="rounded-lg border border-dashed border-line px-3 py-2 text-xs text-muted">{children}</div>;
}

function PercentileRow({ p50, p95 }: { p50: string; p95: string }) {
  const { t } = useT();
  return (
    <div className="grid grid-cols-2 gap-3">
      <Metric label={t('stats.ttm.p50')} value={p50} />
      <Metric label={t('stats.ttm.p95')} value={p95} />
    </div>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-line bg-paper px-3 py-2">
      <div className="text-xs font-medium tracking-wide text-muted uppercase">{label}</div>
      <div className="tabular mt-0.5 font-mono text-xl font-semibold">{value}</div>
    </div>
  );
}
