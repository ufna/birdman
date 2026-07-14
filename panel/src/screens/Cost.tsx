// Cost-view (П2): слото-часы как прокси стоимости железа (GET /v1/stats/cost).
// Слото-часы per регион и per версия per день (стек + итог) и снапшот
// утилизации тачек (allocated/ready/draining vs ёмкость активных нод). Единицы
// явные (slot-hours), время UTC.

import { useMemo, useState } from 'react';
import { api } from '../lib/api';
import type { StatsCost } from '../lib/api';
import { useAsync } from '../lib/useAsync';
import { useEnv } from '../lib/env';
import { useT } from '../lib/i18n';
import { toStackModel, utilizationModel, versionColor } from '../lib/stats';
import {
  Card,
  CardHeader,
  ChartSkeleton,
  EmptyState,
  ErrorNote,
  Skeleton,
  SkeletonRegion,
  StatCard,
  StatCardSkeleton,
} from '../components/ui';
import { BarChart, ChartHeading, PeriodSelect, UtilBar } from '../components/charts';
import { UtilizationChart } from '../components/UtilizationChart';

/** Окно графика утилизации во времени, часов (совпадает с UtilizationChart). */
const UTIL_WINDOW_HOURS = 6;

// Потолок — 30 дней (Task 6, "Статистика v1"): 90д снят. Совпадает с
// горизонтом роллапов master (match_stats_daily, ≤30д) и ретеншеном VM —
// /v1/stats/cost на этих окнах читает роллапы, не сканирует сырые matches.
const PERIODS = [7, 30];

/** Слото-часы: 2 знака для мелких, 1 для десятков, целое для сотен+. */
function fmtHours(v: number): string {
  if (v >= 100) return String(Math.round(v));
  if (v >= 10) return v.toFixed(1);
  return v.toFixed(2);
}

export function Cost() {
  const [days, setDays] = useState(7);
  const { selected } = useEnv();
  const cost = useAsync(() => api.statsCost(days, selected ?? undefined), [days, selected]);
  // Данные показываем, только если они за ЗАПРОШЕННЫЙ период; иначе (первая
  // загрузка или смена периода) — скелетон: раскладка держится, без «прыжка».
  const ready = cost.data !== undefined && cost.data.days === days;

  if (cost.error !== undefined && cost.data === undefined) {
    return (
      <div className="flex flex-col gap-4">
        <Header days={days} setDays={setDays} />
        <ErrorNote error={cost.error} retry={cost.reload} />
      </div>
    );
  }
  return (
    <div className="flex flex-col gap-4">
      <Header days={days} setDays={setDays} />
      {ready && cost.data !== undefined ? <CostBody cost={cost.data} /> : <CostSkeleton />}
    </div>
  );
}

/** Скелетон Cost: карточка итога + два графика + утилизация — под финальную раскладку. */
function CostSkeleton() {
  return (
    <SkeletonRegion>
      <div className="flex flex-col gap-4">
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
          <StatCardSkeleton />
        </div>
        <div className="grid gap-4 lg:grid-cols-2">
          <ChartSkeleton />
          <ChartSkeleton />
        </div>
        <Card className="p-4">
          <Skeleton className="mb-3 h-4 w-40" />
          <Skeleton rounded="rounded-lg" className="h-40 w-full" />
        </Card>
      </div>
    </SkeletonRegion>
  );
}

function Header({ days, setDays }: { days: number; setDays: (d: number) => void }) {
  const { t } = useT();
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div className="flex items-center gap-2">
        <h1 className="text-lg font-semibold">{t('nav.cost')}</h1>
        <span className="rounded border border-line px-1.5 py-0.5 font-mono text-[10px] text-muted">{t('stats.utc')}</span>
      </div>
      <PeriodSelect value={days} onChange={setDays} options={PERIODS} />
    </div>
  );
}

function CostBody({ cost }: { cost: StatsCost }) {
  const { t } = useT();
  const byRegion = useMemo(() => toStackModel(cost.slot_hours_per_day_by_region), [cost.slot_hours_per_day_by_region]);
  // Стек по версиям красим ЕДИНЫМ цветом версии (хэш semver), чтобы совпадал
  // с распределением версий на Stats и с версиями в Matches.
  const byVersion = useMemo(
    () => toStackModel(cost.slot_hours_per_day_by_version, (k) => versionColor(k)),
    [cost.slot_hours_per_day_by_version],
  );
  const util = useMemo(() => utilizationModel(cost.utilization), [cost.utilization]);

  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <StatCard
          label={t('cost.total')}
          value={<span className="tabular">{fmtHours(cost.slot_hours_total)}</span>}
          detail={t('cost.totalDetail', { unit: t('cost.unit'), days: cost.days })}
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card className="p-4">
          <ChartHeading title={t('cost.byRegion')} note={t('cost.unit')} />
          <BarChart
            columns={byRegion.columns}
            max={byRegion.max}
            keys={byRegion.keys}
            formatValue={fmtHours}
            empty={byRegion.empty}
            ariaLabel={t('cost.byRegion')}
            emptyLabel={t('stats.noData')}
          />
        </Card>
        <Card className="p-4">
          <ChartHeading title={t('cost.byVersion')} note={t('cost.unit')} />
          <BarChart
            columns={byVersion.columns}
            max={byVersion.max}
            keys={byVersion.keys}
            formatValue={fmtHours}
            empty={byVersion.empty}
            ariaLabel={t('cost.byVersion')}
            emptyLabel={t('stats.noData')}
          />
        </Card>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader title={t('cost.utilization')} aside={<span className="font-mono text-xs text-muted">{t('cost.utilSnapshot')}</span>} />
          {util.length === 0 ? (
            <EmptyState>{t('cost.noUtil')}</EmptyState>
          ) : (
            <div className="flex flex-col gap-4 p-4">
              {util.map((u) => (
                <UtilBar key={u.region} row={u} />
              ))}
              <p className="text-xs text-muted">{t('cost.utilNote')}</p>
            </div>
          )}
        </Card>
        <Card>
          <CardHeader
            title={t('cost.utilOverTime')}
            aside={<span className="font-mono text-xs text-muted">{t('cost.utilOverTimeNote', { hours: UTIL_WINDOW_HOURS })}</span>}
          />
          <UtilizationChart windowMs={UTIL_WINDOW_HOURS * 60 * 60_000} />
        </Card>
      </div>
    </div>
  );
}
