// Мелкие строительные блоки: карточка, заголовок, пустые/ошибочные
// состояния, метка «идёт загрузка».

import { useEffect, useRef, useState } from 'react';
import type { CSSProperties, ReactNode } from 'react';
import { ApiError } from '../lib/api';
import { useT } from '../lib/i18n';
import { useBindingRefusal } from '../lib/session';

export function Card({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <section className={`rounded-xl border border-line bg-card ${className}`}>{children}</section>
  );
}

export function CardHeader({ title, aside }: { title: string; aside?: ReactNode }) {
  return (
    <header className="flex items-baseline justify-between gap-3 border-b border-line px-4 py-3">
      <h2 className="text-sm font-semibold">{title}</h2>
      {aside}
    </header>
  );
}

/** Стат-карточка Overview: подпись, большое число, деталь. */
export function StatCard({
  label,
  value,
  detail,
}: {
  label: string;
  value: ReactNode;
  detail?: ReactNode;
}) {
  return (
    <Card className="px-4 py-3">
      <div className="text-xs font-medium tracking-wide text-muted uppercase">{label}</div>
      <div className="tabular mt-1 text-2xl font-semibold">{value}</div>
      {detail !== undefined && <div className="mt-1 text-xs text-muted">{detail}</div>}
    </Card>
  );
}

export function EmptyState({ children }: { children: ReactNode }) {
  return <div className="px-4 py-10 text-center text-sm text-muted">{children}</div>;
}

/**
 * Живое число: короткая вспышка тона при изменении значения (обновление по SSE
 * заметно без «прыжка» — ширина стабильна за счёт tabular-nums у StatCard).
 * Только для примитивов (число/строка): сравнение по значению. Первый рендер не
 * вспыхивает. Пересборкой ключа перезапускаем CSS-анимацию.
 */
export function LiveValue({ value }: { value: string | number }) {
  const [gen, setGen] = useState(0);
  const prev = useRef<string | number>(value);
  useEffect(() => {
    if (!Object.is(prev.current, value)) {
      prev.current = value;
      setGen((g) => g + 1);
    }
  }, [value]);
  return (
    <span key={gen} className={gen > 0 ? 'value-flash' : undefined}>
      {value}
    </span>
  );
}

export function ErrorNote({ error, retry }: { error: Error; retry?: () => void }) {
  const { t } = useT();
  // 403 объясняем по СЕССИИ, а не догадкой: привязан ключ — причина привязка,
  // иначе прежний текст про скоуп (tracker #1000, lib/session.tsx).
  const bound = useBindingRefusal();
  const detail =
    error instanceof ApiError && error.status === 403
      ? (bound ?? t('ui.err.forbidden'))
      : error.message;
  return (
    <div className="flex items-center justify-between gap-3 rounded-lg bg-dead-bg px-4 py-2.5 text-sm text-dead">
      <span>{t('ui.err.loadFailed', { detail })}</span>
      {retry !== undefined && (
        <button
          type="button"
          onClick={retry}
          className="shrink-0 rounded-md border border-current px-2 py-0.5 text-xs font-medium hover:opacity-80"
        >
          {t('common.retry')}
        </button>
      )}
    </div>
  );
}

export function LoadingRow({ label }: { label?: string }) {
  const { t } = useT();
  return <div className="px-4 py-10 text-center text-sm text-muted">{label ?? t('common.loading')}</div>;
}

/**
 * Плейсхолдер-шиммер на время первой загрузки / смены периода — держит
 * раскладку, чтобы контент не «прыгал» при появлении. Декоративен (aria-hidden);
 * доступную подпись «идёт загрузка» несёт контейнер (role=status).
 */
export function Skeleton({
  className = '',
  rounded = 'rounded-md',
  style,
}: {
  className?: string;
  rounded?: string;
  style?: CSSProperties;
}) {
  return <div aria-hidden style={style} className={`skeleton ${rounded} ${className}`} />;
}

/** Карточка-скелетон под StatCard: подпись + крупное число + деталь. */
export function StatCardSkeleton() {
  return (
    <Card className="px-4 py-3">
      <Skeleton className="h-3 w-20" />
      <Skeleton className="mt-2 h-7 w-16" />
      <Skeleton className="mt-2 h-3 w-24" />
    </Card>
  );
}

/** Скелетон графика в карточке (заголовок + область построения). */
export function ChartSkeleton({ title, height = 172 }: { title?: string; height?: number }) {
  return (
    <Card className="p-4">
      {title !== undefined ? (
        <div className="mb-3 text-sm font-semibold">{title}</div>
      ) : (
        <Skeleton className="mb-3 h-4 w-40" />
      )}
      <Skeleton rounded="rounded-lg" className="w-full" style={{ height }} />
    </Card>
  );
}

/** Обёртка, помечающая зону скелетонов для скринридера (role=status + label). */
export function SkeletonRegion({ children }: { children: ReactNode }) {
  const { t } = useT();
  return (
    <div role="status" aria-label={t('common.loading')} aria-busy="true">
      {children}
    </div>
  );
}

/** Логотип: две «птичьи» галки + моно-словомарка. */
export function Brand({ compact = false }: { compact?: boolean }) {
  return (
    <span className="inline-flex items-center gap-2">
      <svg viewBox="0 0 24 24" fill="none" aria-hidden className="size-5">
        <path
          d="M3 14L12 8L21 14"
          className="stroke-accent"
          strokeWidth="2.6"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        <path
          d="M7 19.5L12 16L17 19.5"
          className="stroke-accent-ink"
          strokeWidth="2.2"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
      {!compact && <span className="font-mono text-base font-semibold tracking-tight">birdman</span>}
    </span>
  );
}
