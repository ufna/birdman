// Статистика (П2): обзорные агрегаты за 7/30/90 дней (GET /v1/stats/overview).
// Матчи/день и игроки/день — стек по регионам; пик CCU и средняя длительность —
// одиночные ряды; распределение по версиям — доли (единый цвет версии); fill-rate
// матчмейкера показан из ДВУХ источников: истинное queue→match из гистограммы
// birdman_mm_time_to_match_seconds (metrics-proxy) и прокси allocation→start (из
// matches). Ряды UTC и зеро-филлены на бэке. Живое обновление: refetch по SSE с
// дебансом (useLiveAsync) — новый матч подтягивает свежие числа/графики.

import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { api } from '../lib/api';
import type { StatsOverview, TimeToMatch } from '../lib/api';
import { useLiveAsync } from '../lib/live';
import { useT, useFormat } from '../lib/i18n';
import { toSimpleColumns, toStackModel, versionShareModel } from '../lib/stats';
import { timeToMatchQuantileQuery } from '../lib/metrics';
import { useInstantQuery } from '../lib/useMetrics';
import type { MetricStatus } from '../lib/useMetrics';
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
import { BarChart, ChartHeading, PeriodSelect, ShareBars } from '../components/charts';

const PERIODS = [7, 30, 90];

export function Stats() {
  const [days, setDays] = useState(7);
  const ov = useLiveAsync(() => api.statsOverview(days), [days]);
  // Показываем данные, только если они за ЗАПРОШЕННЫЙ период; иначе (первая
  // загрузка или смена периода) — скелетон под финальную раскладку.
  const ready = ov.data !== undefined && ov.data.days === days;

  if (ov.error !== undefined && ov.data === undefined) {
    return (
      <div className="flex flex-col gap-4">
        <Header days={days} setDays={setDays} />
        <ErrorNote error={ov.error} retry={ov.reload} />
      </div>
    );
  }
  return (
    <div className="flex flex-col gap-4">
      <Header days={days} setDays={setDays} />
      {ready && ov.data !== undefined ? <StatsBody ov={ov.data} /> : <StatsSkeleton />}
    </div>
  );
}

function Header({ days, setDays }: { days: number; setDays: (d: number) => void }) {
  const { t } = useT();
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div className="flex items-center gap-2">
        <h1 className="text-lg font-semibold">{t('nav.stats')}</h1>
        <span className="rounded border border-line px-1.5 py-0.5 font-mono text-[10px] text-muted">{t('stats.utc')}</span>
      </div>
      <PeriodSelect value={days} onChange={setDays} options={PERIODS} />
    </div>
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

function StatsBody({ ov }: { ov: StatsOverview }) {
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
        <StatCard label={t('stats.card.peakCcu')} value={ov.peak_ccu} detail={t('stats.card.peakCcuDetail')} />
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
          <ChartHeading title={t('stats.peakCcuPerDay')} note={t('stats.peakCcuNote')} />
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
 */
function FillRateCard({ ttm, days, refetchKey }: { ttm: TimeToMatch; days: number; refetchKey: string }) {
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
        <TtmSource label={t('stats.ttm.srcAlloc')} note={t('stats.ttm.note', { count: ttm.samples })}>
          <PercentileRow p50={dur(ttm.p50_seconds)} p95={dur(ttm.p95_seconds)} />
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

function TtmSource({ label, note, children }: { label: string; note: string; children: ReactNode }) {
  return (
    <div>
      <div className="mb-1.5 font-mono text-xs font-medium text-ink">{label}</div>
      {children}
      <p className="mt-1.5 text-xs text-muted">{note}</p>
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
