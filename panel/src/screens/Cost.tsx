// Cost-view (П2): слото-часы как прокси стоимости железа (GET /v1/stats/cost).
// Слото-часы per регион и per версия per день (стек + итог) и снапшот
// утилизации тачек (allocated/ready/draining vs ёмкость активных нод). Единицы
// явные (slot-hours), время UTC.

import { useMemo, useState } from 'react';
import { api } from '../lib/api';
import type { StatsCost } from '../lib/api';
import { useAsync } from '../lib/useAsync';
import { useT } from '../lib/i18n';
import { toStackModel, utilizationModel } from '../lib/stats';
import { Card, CardHeader, EmptyState, ErrorNote, LoadingRow, StatCard } from '../components/ui';
import { BarChart, ChartHeading, PeriodSelect, UtilBar } from '../components/charts';

const PERIODS = [7, 30, 90];

/** Слото-часы: 2 знака для мелких, 1 для десятков, целое для сотен+. */
function fmtHours(v: number): string {
  if (v >= 100) return String(Math.round(v));
  if (v >= 10) return v.toFixed(1);
  return v.toFixed(2);
}

export function Cost() {
  const [days, setDays] = useState(7);
  const cost = useAsync(() => api.statsCost(days), [days]);

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
      {cost.data === undefined ? <LoadingRow /> : <CostBody cost={cost.data} />}
    </div>
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
  const byVersion = useMemo(() => toStackModel(cost.slot_hours_per_day_by_version), [cost.slot_hours_per_day_by_version]);
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
    </div>
  );
}
