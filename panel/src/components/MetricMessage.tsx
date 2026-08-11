// Единое пустое/статусное состояние графиков метрик (MetricChart,
// UtilizationChart): «метрики не настроены» (VM off) / «недоступна» / «нет
// данных» / загрузка / ошибка — панель не падает, а объясняет.

import { useT } from '../lib/i18n';
import { useBindingRefusal } from '../lib/session';
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
  // Ветка на 403 (tracker #1000): без неё новый статус отрисовался бы пустотой.
  // Текст — общий с ErrorNote, из сессии; сырой `error` сюда не попадает.
  // Соседняя строка t('metric.error', {error}) — предмет tracker #996, не тронута.
  const bound = useBindingRefusal();
  const text =
    status === 'loading'
      ? t('metric.loading')
      : status === 'unconfigured'
        ? t('metric.unconfigured')
        : status === 'unreachable'
          ? t('metric.unreachable')
          : status === 'forbidden'
            ? (bound ?? t('ui.err.forbidden'))
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
