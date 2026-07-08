// Статистика (П2): обзорные агрегаты за 7/30/90 дней (GET /v1/stats/overview).
// Матчи/день и игроки/день — стек по регионам; пик CCU и средняя длительность —
// одиночные ряды; распределение по версиям — доли; fill-rate матчмейкера
// (time-to-match p50/p95). Все ряды UTC и зеро-филлены на бэке.

import { useMemo, useState } from 'react';
import { api } from '../lib/api';
import type { StatsOverview } from '../lib/api';
import { useAsync } from '../lib/useAsync';
import { useT, useFormat } from '../lib/i18n';
import { toSimpleColumns, toStackModel, versionShareModel } from '../lib/stats';
import { Card, CardHeader, ErrorNote, LoadingRow, StatCard } from '../components/ui';
import { BarChart, ChartHeading, PeriodSelect, ShareBars } from '../components/charts';

const PERIODS = [7, 30, 90];

export function Stats() {
  const [days, setDays] = useState(7);
  const ov = useAsync(() => api.statsOverview(days), [days]);

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
      {ov.data === undefined ? <LoadingRow /> : <StatsBody ov={ov.data} />}
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
        <Card>
          <CardHeader title={t('stats.fillRate')} />
          <div className="flex flex-col gap-3 p-4">
            <div className="grid grid-cols-2 gap-3">
              <Metric label={t('stats.ttm.p50')} value={dur(ttm.p50_seconds)} />
              <Metric label={t('stats.ttm.p95')} value={dur(ttm.p95_seconds)} />
            </div>
            <p className="text-xs text-muted">{t('stats.ttm.note', { count: ttm.samples })}</p>
            <p className="font-mono text-[11px] text-muted">{t('stats.ttm.source')}: {ttm.source}</p>
          </div>
        </Card>
      </div>
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
