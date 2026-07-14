// Компактный бейдж окружения (environments v1 §8) — общий кросс-скрин компонент:
// используется в Deploys (версии, карточка env), Fleet (колонка ноды) и Access
// (привязка ключа project/env). Вынесен из screens/Deploys.tsx (p4), чтобы не
// тянуть импорт между экранами.
import { useT } from '../lib/i18n';

/** Компактный бейдж окружения версии/строки (не StateBadge — это не «состояние»). */
export function EnvTag({ env, production }: { env: string; production?: boolean }) {
  const { t } = useT();
  return (
    <span
      title={production ? t('env.productionTitle') : undefined}
      className="inline-flex items-center gap-1 rounded border border-line px-1.5 py-0.5 font-mono text-[11px] text-muted"
    >
      {production === true && <span aria-hidden className="size-1.5 rounded-full bg-warn" />}
      {env}
    </span>
  );
}
