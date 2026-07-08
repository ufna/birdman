// Единое пустое/статусное состояние графиков метрик (MetricChart,
// UtilizationChart): «метрики не настроены» (VM off) / «недоступна» / «нет
// данных» / загрузка / ошибка — панель не падает, а объясняет.

import { useT } from '../lib/i18n';
import type { MetricStatus } from '../lib/useMetrics';

export function MetricMessage({
  status,
  hasData,
  error = '',
  height,
}: {
  status: MetricStatus;
  hasData: boolean;
  error?: string;
  height: number;
}) {
  const { t } = useT();
  const text =
    status === 'loading'
      ? t('metric.loading')
      : status === 'unconfigured'
        ? t('metric.unconfigured')
        : status === 'unreachable'
          ? t('metric.unreachable')
          : status === 'error'
            ? t('metric.error', { error })
            : !hasData
              ? t('metric.noData')
              : '';
  return (
    <div className="flex items-center justify-center px-3 text-center text-xs text-muted" style={{ height }}>
      {text}
    </div>
  );
}
