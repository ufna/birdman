// Единое пустое/статусное состояние графиков метрик (MetricChart,
// UtilizationChart): «метрики не настроены» (VM off) / «недоступна» / «нет
// данных» / загрузка / ошибка — панель не падает, а объясняет.
//
// Весь текст здесь — СВОЙ, из каталога (tracker #996). До этой карточки жёсткая
// ошибка подставлялась как `t('metric.error', {error})`, где `error` —
// `ApiError.message`, то есть `${code}: ${detail}` из тела мастера: в русском
// интерфейсе на графике появлялась английская строка мастера или VM. Теперь
// сюда приходит только машинный КОД (см. MetricErrorCode в lib/useMetrics.ts),
// а формулировку выбирает панель.

import { useT } from '../lib/i18n';
import type { MessageKey } from '../lib/i18n';
import { useBindingRefusal } from '../lib/session';
import type { MetricErrorCode, MetricStatus } from '../lib/useMetrics';

/**
 * Коды, у которых действие оператора ОТЛИЧАЕТСЯ от «сообщить о поломке», —
 * только им положен свой текст. Всё прочее (`bad_response`, `bad_data` от VM,
 * `http_5xx`) для оператора одно и то же: сломалось на стороне платформы,
 * чинить не ему, — и различается лишь кодом, который общий текст показывает.
 *
 * `unauthorized` (401): кука сессии живёт 24ч, а график дозапрашивается каждые
 * 15с. Достижимость у него УЖЕ, чем кажется, и это надо знать читателю: статус
 * ошибки поднимается только пока у графика ЕЩЁ НЕТ данных (`useQueryRange`
 * пишет его внутри `prev === null`), то есть текст видит тот, кто открыл график
 * заново — дровер дедика, смена периода, перезагрузка страницы; на графике с
 * уже нарисованными точками 401 проходит молча. Панель здесь только НАЗЫВАЕТ
 * причину: из сессии протухшую куку выбивает SSE-стрим (`lib/live.tsx` зовёт
 * invalidate на 401), но лишь при следующем ПОДКЛЮЧЕНИИ — `/v1/events/stream`
 * аутентифицируется один раз на коннекте, поэтому под живым стримом логаута не
 * происходит вовсе. Ни `lib/api.ts`, ни `lib/metrics.ts` 401 не обрабатывают —
 * предсуществующее, в #996 не менялось, заведено отдельной картой.
 */
// Тип с `| undefined` намеренно: `noUncheckedIndexedAccess` в tsconfig НЕ
// включён, и без него `CODE_MESSAGE[code]` считался бы всегда определённым —
// проверка ниже была бы верной лишь в рантайме, а компилятор её не видел бы.
const CODE_MESSAGE: Record<string, MessageKey | undefined> = {
  unauthorized: 'metric.err.expired',
};

export function MetricMessage({
  status,
  hasData,
  errorCode = null,
  height,
}: {
  status: MetricStatus;
  hasData: boolean;
  /** Машинный код жёсткой ошибки (MetricErrorCode): null — запрос не дошёл. */
  errorCode?: MetricErrorCode;
  height: number;
}) {
  const { t } = useT();
  // Ветка на 403 (tracker #1000): без неё новый статус отрисовался бы пустотой.
  // Текст — общий с ErrorNote, из сессии; сырой `error` сюда не попадает.
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
              ? errorText(errorCode, t)
              : !hasData
                ? t('metric.noData')
                : '';
  return (
    <div className="flex items-center justify-center px-3 text-center text-xs text-muted" style={{ height }}>
      {text}
    </div>
  );
}

/**
 * Код → текст каталога. `null` (запрос не дошёл) и `unauthorized` называются
 * своими словами, остальное — общий текст с кодом в скобках: сам код панель
 * показывает сознательно, он не переводится наравне с semver/id (panel.md §1
 * п.6) и остаётся единственным, за что можно зацепиться в баг-репорте. Текста
 * мастера (`ApiError.detail`) в этой функции нет ни в одной ветке — его сюда
 * структурно не довозят.
 */
function errorText(code: MetricErrorCode, t: (key: MessageKey, params?: Record<string, string | number>) => string): string {
  if (code === null) return t('metric.err.offline');
  const own = CODE_MESSAGE[code];
  return own !== undefined ? t(own) : t('metric.error', { code });
}
