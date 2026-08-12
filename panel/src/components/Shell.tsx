// Каркас после логина: навигация, live-индикатор стрима, переключатели
// языка и темы, выход. До 1280px страница не скроллится по горизонтали —
// таблицы скроллятся внутри карточек. <768px навигация сворачивается в
// гамбургер-дровер (Radix Dialog: фокус-трап, Esc, возврат фокуса).

/* eslint-disable react-refresh/only-export-components -- navItemsFor — чистый состав меню по правам сессии, тестируется отдельно от оболочки. Правило про гранулярность Fast Refresh в dev-сервере, не про корректность; разносить файл по модулям ради него дороже, чем оно стоит. Политика — в eslint.config.js. */

import { useState } from 'react';
import type { ReactNode } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import * as Dialog from '@radix-ui/react-dialog';
import { useLive } from '../lib/live';
import { useEnv } from '../lib/env';
import { useProject } from '../lib/project';
import { canAdmin, useSession } from '../lib/session';
import type { SessionInfo } from '../lib/api';
import { useTheme } from '../lib/theme';
import { useT } from '../lib/i18n';
import type { Lang, MessageKey } from '../lib/i18n';
import { useCriticalAlerts } from '../lib/useCriticalAlerts';
import type { CriticalAlerts } from '../lib/useCriticalAlerts';
import { Brand } from './ui';

type NavIcon = 'overview' | 'fleet' | 'matches' | 'deploys' | 'events' | 'stats' | 'cost' | 'alerts' | 'logs' | 'backups' | 'access';

export interface NavItem {
  path: string;
  key: MessageKey;
  icon: NavIcon;
  /** Только для admin-сессии (эндпоинты admin-scoped) — иначе скрыт. */
  adminOnly?: boolean;
}

const NAV_ITEMS: NavItem[] = [
  { path: '/', key: 'nav.overview', icon: 'overview' },
  { path: '/fleet', key: 'nav.fleet', icon: 'fleet' },
  { path: '/matches', key: 'nav.matches', icon: 'matches' },
  { path: '/deploys', key: 'nav.deploys', icon: 'deploys' },
  { path: '/events', key: 'nav.events', icon: 'events' },
  { path: '/stats', key: 'nav.stats', icon: 'stats' },
  { path: '/cost', key: 'nav.cost', icon: 'cost' },
  { path: '/alerts', key: 'nav.alerts', icon: 'alerts' },
  // Логи v1: readonly-видимый флит-поиск по VictoriaLogs — БЕЗ adminOnly
  // (в отличие от /access ниже), см. navItemsFor.
  { path: '/logs', key: 'nav.logs', icon: 'logs' },
  { path: '/backups', key: 'nav.backups', icon: 'backups', adminOnly: true },
  { path: '/access', key: 'nav.access', icon: 'access', adminOnly: true },
];

/** Пункты навигации для сессии: admin-only скрыты у не-admin. Чистая — тест scope-гейта. */
export function navItemsFor(session: SessionInfo | null | undefined): NavItem[] {
  return NAV_ITEMS.filter((it) => it.adminOnly !== true || (session != null && canAdmin(session)));
}

function isActive(item: string, path: string): boolean {
  return item === '/' ? path === '/' : path.startsWith(item);
}

/** Бейдж активных critical-алертов на пункте «Алерты». Пульс на росте
 *  (ключ = alarmKey перезапускает анимацию), уважает prefers-reduced-motion. */
function CriticalBadge({ critical, label }: { critical: CriticalAlerts; label: string }) {
  return (
    <span
      key={critical.alarmKey}
      aria-label={label}
      className="critical-pulse tabular inline-flex min-w-[1.1rem] shrink-0 items-center justify-center rounded-full bg-dead px-1 text-[10px] leading-none font-semibold text-white"
    >
      {critical.count > 99 ? '99+' : critical.count}
    </span>
  );
}

function NavLinks({
  path,
  navigate,
  critical,
}: {
  path: string;
  navigate: (p: string) => void;
  critical: CriticalAlerts;
}) {
  const { t } = useT();
  const { session } = useSession();
  const items = navItemsFor(session);
  return (
    <nav className="flex flex-col gap-1" aria-label={t('nav.sections')}>
      {items.map((item) => {
        const active = isActive(item.path, path);
        const showBadge = item.icon === 'alerts' && critical.count > 0;
        return (
          <a
            key={item.path}
            href={item.path}
            aria-current={active ? 'page' : undefined}
            onClick={(e) => {
              e.preventDefault();
              navigate(item.path);
            }}
            className={`flex items-center gap-2 rounded-lg px-3 py-1.5 text-sm transition-colors ${
              active ? 'bg-mark font-medium text-accent-ink' : 'text-muted hover:bg-paper hover:text-ink'
            }`}
          >
            <NavGlyph icon={item.icon} />
            <span className="min-w-0 flex-1 truncate">{t(item.key)}</span>
            {showBadge && <CriticalBadge critical={critical} label={t('alerts.critical.badge', { count: critical.count })} />}
          </a>
        );
      })}
    </nav>
  );
}

/** Инлайновые нав-иконки (CSP-safe, как в Login): 24-viewBox, stroke=currentColor. */
function NavGlyph({ icon }: { icon: NavIcon }) {
  const p: Record<NavIcon, ReactNode> = {
    overview: <path d="M4 4h6v6H4V4Zm10 0h6v6h-6V4ZM4 14h6v6H4v-6Zm10 0h6v6h-6v-6Z" />,
    fleet: <path d="M4 5h16v5H4V5Zm0 9h16v5H4v-5ZM7.5 7.5h.01M7.5 16.5h.01" />,
    matches: <path d="M6 12a6 6 0 0 1 6-6 6 6 0 0 1 6 6 6 6 0 0 1-6 6 6 6 0 0 1-6-6Zm4.5-2.5 4 2.5-4 2.5v-5Z" />,
    deploys: <path d="M12 3v12m0-12 4 4m-4-4-4 4M5 21h14" />,
    events: <path d="M4 6h16M4 12h16M4 18h10" />,
    stats: <path d="M5 20V10m6 10V4m6 16v-7M3 20h18" />,
    cost: <path d="M12 3a4 9 0 0 0 0 18 4 9 0 0 0 0-18Zm-8 6h16M4 15h16" />,
    alerts: <path d="M12 4a5 5 0 0 0-5 5c0 5-2 6-2 6h14s-2-1-2-6a5 5 0 0 0-5-5Zm-2 15a2 2 0 0 0 4 0" />,
    logs: <path d="M10 4a6 6 0 1 0 0 12 6 6 0 0 0 0-12Z M17 17l4 4" />,
    backups: <path d="M12 3c3.9 0 7 1.3 7 3s-3.1 3-7 3-7-1.3-7-3 3.1-3 7-3ZM5 6v6c0 1.7 3.1 3 7 3s7-1.3 7-3V6M5 12v6c0 1.7 3.1 3 7 3s7-1.3 7-3v-6" />,
    access: <path d="M14 7a4 4 0 1 0-3.5 6L9 14.5l1.5 1.5L9 17.5l1.5 1.5L13 16l-1.5-1.5.9-.9A4 4 0 0 0 14 7Zm.5 2.5h.01" />,
  };
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden className="size-4 shrink-0">
      {p[icon]}
    </svg>
  );
}

/**
 * Селектор проекта (мультипроект W1) — верхнее измерение панели: всё, что
 * показывают экраны, живёт внутри ВЫБРАННОГО проекта, режима «все проекты» нет.
 *
 * Три формы вместо одной, потому что у них разный смысл: несколько проектов —
 * нативный `<select>` (доступен с клавиатуры, не разъезжается на десятке
 * проектов и не тянет зависимость); ровно один — статическая подпись (какой
 * проект открыт, видно всегда, но выбирать не из чего); ни одного — чип с
 * подсказкой, что проект заведётся сам при регистрации первой ноды/версии
 * (ensureProject), а не «пусто» без объяснений.
 */
export function ProjectSelector({ stacked = false }: { stacked?: boolean } = {}) {
  const { projects, selected, setSelected, loading, error, reload } = useProject();
  const { t } = useT();
  if (error !== undefined) return <ProjectUnavailableChip retry={reload} />;
  // Пока список едет, не мигаем «проектов нет» — это разные состояния.
  if (loading && projects.length === 0) return null;
  return (
    <div
      role="group"
      aria-label={t('project.switch')}
      className={stacked ? 'flex flex-col items-stretch gap-1.5' : 'flex flex-wrap items-center gap-1.5'}
    >
      <span aria-hidden className="text-xs font-medium tracking-wide text-muted uppercase">
        {t('project.switch')}
      </span>
      {projects.length === 0 ? (
        <span title={t('project.none.hint')} className="rounded-lg border border-line px-2.5 py-1 text-xs text-muted">
          {t('project.none')}
        </span>
      ) : projects.length === 1 ? (
        <span className="rounded-lg border border-line px-2.5 py-1 font-mono text-xs font-medium text-ink">
          {projects[0].slug}
        </span>
      ) : (
        // appearance-none + своя шеврон-иконка: нативная отрисовка селекта
        // выбивается из плоского стиля панели, но сам <select> остаётся
        // нативным — клавиатура и скринридеры работают даром.
        <span className={`relative inline-flex items-center ${stacked ? 'w-full' : ''}`}>
          <select
            aria-label={t('project.switch')}
            value={selected ?? ''}
            onChange={(e) => {
              setSelected(e.target.value);
            }}
            className={`appearance-none rounded-lg border border-line bg-card py-1 pr-7 pl-2.5 font-mono text-xs font-medium text-ink transition-colors hover:border-accent focus:border-accent focus:outline-none ${stacked ? 'w-full' : ''}`}
          >
            {projects.map((p) => (
              <option key={p.slug} value={p.slug}>
                {p.slug}
              </option>
            ))}
          </select>
          <svg
            viewBox="0 0 16 16"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.8"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden
            className="pointer-events-none absolute right-2 size-3 text-muted"
          >
            <path d="M4 6.5 8 10.5l4-4" />
          </svg>
        </span>
      )}
    </div>
  );
}

/** Список проектов не приехал: селектора нет — говорим об этом и даём повтор. */
function ProjectUnavailableChip({ retry }: { retry: () => void }) {
  const { t } = useT();
  const hint = t('project.unavailable.hint');
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <span aria-hidden className="text-xs font-medium tracking-wide text-muted uppercase">
        {t('project.switch')}
      </span>
      <button
        type="button"
        onClick={retry}
        title={hint}
        aria-label={`${t('project.unavailable')} — ${hint}`}
        className="inline-flex items-center gap-1.5 rounded-lg border border-warn/40 bg-warn-bg/40 px-2.5 py-1 text-xs font-medium text-warn transition-opacity hover:opacity-80"
      >
        <span aria-hidden className="size-1.5 rounded-full bg-warn" />
        {t('project.unavailable')}
      </button>
    </div>
  );
}

/**
 * Глобальный переключатель окружения (environments v1 §8): чипы из
 * GET /v1/environments (non-production сначала, потом production, потом «All»),
 * выбор персистится (useEnv). Скрыт, пока окружений нет/не загрузились —
 * тогда фильтр по env панели и так no-op. Экраны сужают данные по selected.
 *
 * Список недоступен (ошибка API) → вместо чипов ненавязчивый чип-предупреждение
 * (follow-up p2): фильтр в этот момент принудительно «All» (env.tsx), и молчать
 * об этом нельзя — иначе непонятно, почему переключателя нет.
 */
export function EnvChips({ stacked = false }: { stacked?: boolean } = {}) {
  const { environments, selected, setSelected, error, reload } = useEnv();
  const { t } = useT();
  if (error !== undefined) return <EnvUnavailableChip retry={reload} />;
  if (environments.length === 0) return null;
  const chipCls = (active: boolean) =>
    `inline-flex items-center gap-1.5 rounded-lg border px-2.5 py-1 text-xs font-medium transition-colors ${
      active ? 'border-accent bg-mark text-accent-ink' : 'border-line text-muted hover:text-ink'
    }`;
  return (
    <div
      role="group"
      aria-label={t('env.switch')}
      className={stacked ? 'flex flex-col items-start gap-1.5' : 'flex flex-wrap items-center gap-1.5'}
    >
      <span aria-hidden className="text-xs font-medium tracking-wide text-muted uppercase">
        {t('env.switch')}
      </span>
      {/* Чипы всегда в своей строке-обёртке: в сайдбаре они переносятся под
          подписью, в ряду — стоят рядом с ней. Одна структура на оба режима. */}
      <div className="flex flex-wrap items-center gap-1.5">
        {environments.map((e) => {
        const active = selected === e.name;
        return (
          <button
            key={e.name}
            type="button"
            aria-pressed={active}
            title={e.production ? t('env.productionTitle') : undefined}
            onClick={() => {
              setSelected(e.name);
            }}
            className={chipCls(active)}
          >
              {e.production && <span aria-hidden className="size-1.5 rounded-full bg-warn" />}
              <span className="font-mono">{e.name}</span>
            </button>
          );
        })}
        <button
          type="button"
          aria-pressed={selected === null}
          onClick={() => {
            setSelected(null);
          }}
          className={chipCls(selected === null)}
        >
          {t('env.all')}
        </button>
      </div>
    </div>
  );
}

/**
 * Скоуп панели — проект и окружение. Живёт в сайдбаре (и в мобильном меню), а
 * не над контентом: это верхнее измерение ВСЕГО, что показывают экраны, и
 * читаться должно сверху вниз — система → на что смотрю → раздел.
 *
 * Показывать селектор, который ни на что не влияет, — врать пользователю.
 * Но «влияет» спрашивается ПО КАЖДОЙ ОСИ ОТДЕЛЬНО, а не про скоуп целиком:
 * одномерное множество путей (прежнее SCOPELESS_PATHS) этого не выражало, и
 * потому принцип исполнялся только наполовину — чипы окружения рендерились
 * вхолостую сразу на трёх экранах (tracker #1106). Замерено по коду:
 *
 * ПРОЕКТ жив везде, кроме Бекапов (настройки и история бекапов у мастера одни,
 * ручки не принимают ?project= — master.md §6). В Админке он НУЖЕН, и не ради
 * одного диалога: им кормятся привязка создаваемого ключа, отметка текущего
 * проекта в «Проектах» и вся секция «Окружения» (её таблица — окружения
 * ВЫБРАННОГО проекта, add/edit/delete гейтятся project !== null). На «Логах»
 * сам поиск проект не несёт (LogsQL строится без него, сужает сервер по
 * привязке ключа), но клик по строке результата открывает дровер дедика, а тот
 * резолвит дедик и его таймлайн в ПРОЕКТЕ — выбор решает, увидит оператор мету
 * дедика или «нет в списке».
 *
 * ОКРУЖЕНИЕ мертво на Бекапах, Алертах, Логах и в Админке. Алерты живут в осях
 * проект×регион (mute — тоже), Логи не читают env вовсе, а в Админке оба
 * потребителя env берут СПИСОК окружений и проект, но никогда выбранное
 * значение: диалог привязки ключа выбирает окружение своим дропдауном.
 *
 * Инвариант, который держит отдельный тест: PROJECTLESS ⊆ ENVLESS — окружения
 * принадлежат проекту, поэтому экран, слепой к проекту, слеп и к его
 * окружениям. Бекапы — по-прежнему единственный, где не рендерится ничего.
 */
const PROJECTLESS_PATHS = new Set(['/backups']);
const ENVLESS_PATHS = new Set(['/backups', '/alerts', '/logs', '/access']);

/** Влияет ли ВЫБОР ПРОЕКТА на то, что показывает экран этого пути. */
export function pathUsesProject(path: string): boolean {
  return !PROJECTLESS_PATHS.has(path);
}

/** Влияет ли ВЫБОР ОКРУЖЕНИЯ на то, что показывает экран этого пути. */
export function pathUsesEnv(path: string): boolean {
  return !ENVLESS_PATHS.has(path);
}

/** Есть ли на этом пути хоть одна живая ось — то есть рисовать ли блок вообще. */
export function pathUsesScope(path: string): boolean {
  return pathUsesProject(path) || pathUsesEnv(path);
}

export function ScopePicker({ path }: { path: string }) {
  if (!pathUsesScope(path)) return null;
  return (
    <div className="flex flex-col gap-3">
      {pathUsesProject(path) && <ProjectSelector stacked />}
      {pathUsesEnv(path) && <EnvChips stacked />}
    </div>
  );
}

/**
 * Текущий скоуп текстом в мобильной шапке. На узком экране сайдбара нет, а
 * переключатель уезжает в выдвижное меню — без этой подписи пропал бы ответ на
 * вопрос «в каком я проекте», который до переезда всегда был на экране.
 * Не интерактивен намеренно: переключение — в меню, здесь только факт.
 *
 * Половинки те же, что у ScopePicker: подпись не называет окружение там, где
 * его выбор ни на что не влияет, — иначе индикатор врал бы ровно тем же
 * способом, что и спрятанные чипы. Проект — первая половина: пока его нет,
 * называть нечего вовсе (а без проекта нет и окружений — см. инвариант выше).
 */
export function ScopeIndicator({ path }: { path: string }) {
  const { selected: project } = useProject();
  const { selected: env } = useEnv();
  const { t } = useT();
  if (!pathUsesProject(path) || project === null) return null;
  return (
    <span className="truncate font-mono text-xs text-muted" title={t('scope.current')}>
      {project}
      {pathUsesEnv(path) && (
        <>
          <span aria-hidden className="px-1 text-line">
            /
          </span>
          {env ?? t('env.all')}
        </>
      )}
    </span>
  );
}

/**
 * Деградация списка окружений (follow-up p2): чипов нет, фильтр отключён —
 * говорим об этом одним чипом с тултипом (title) и даём повторить запрос
 * кликом. Причина теперь ровно одна — список не приехал: прежняя ветка
 * «мультипроект» (400 several projects exist) недостижима, панель спрашивает
 * окружения только с явным ?project= выбранного проекта (мультипроект W1).
 */
function EnvUnavailableChip({ retry }: { retry: () => void }) {
  const { t } = useT();
  const hint = t('env.unavailable.hint');
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <span aria-hidden className="text-xs font-medium tracking-wide text-muted uppercase">
        {t('env.switch')}
      </span>
      <button
        type="button"
        onClick={retry}
        title={hint}
        aria-label={`${t('env.unavailable')} — ${hint}`}
        className="inline-flex items-center gap-1.5 rounded-lg border border-warn/40 bg-warn-bg/40 px-2.5 py-1 text-xs font-medium text-warn transition-opacity hover:opacity-80"
      >
        <span aria-hidden className="size-1.5 rounded-full bg-warn" />
        {t('env.unavailable')}
      </button>
    </div>
  );
}

function LiveIndicator() {
  const { status } = useLive();
  const { t } = useT();
  const view = {
    live: { cls: 'bg-good', dotAnim: true, key: 'live.live' as MessageKey },
    connecting: { cls: 'bg-warn', dotAnim: false, key: 'live.connecting' as MessageKey },
    down: { cls: 'bg-dead', dotAnim: false, key: 'live.reconnecting' as MessageKey },
    unauthorized: { cls: 'bg-dead', dotAnim: false, key: 'live.noAccess' as MessageKey },
  }[status];
  return (
    <span className="inline-flex items-center gap-2 font-mono text-xs text-muted" title={t('live.title')}>
      <span aria-hidden className={`size-2 rounded-full ${view.cls} ${view.dotAnim ? 'live-dot-on' : ''}`} />
      {t(view.key)}
    </span>
  );
}

/** Компактный переключатель EN/RU — в тон обводке темы, доступный (role=group
 *  + aria-pressed на активном языке). */
function LangToggle() {
  const { lang, setLang, t } = useT();
  return (
    <div
      role="group"
      aria-label={t('lang.switch')}
      className="inline-flex overflow-hidden rounded-lg border border-line text-xs"
    >
      {(['en', 'ru'] as Lang[]).map((l) => {
        const active = lang === l;
        return (
          <button
            key={l}
            type="button"
            onClick={() => {
              setLang(l);
            }}
            aria-pressed={active}
            className={`px-1.5 py-1 font-medium uppercase transition-colors ${
              active ? 'bg-mark text-accent-ink' : 'text-muted hover:text-ink'
            }`}
          >
            {l}
          </button>
        );
      })}
    </div>
  );
}

function ThemeToggle() {
  const { theme, toggle } = useTheme();
  const { t } = useT();
  const label = theme === 'dark' ? t('theme.toLight') : t('theme.toDark');
  return (
    <Tooltip.Root>
      <Tooltip.Trigger asChild>
        <button
          type="button"
          onClick={toggle}
          aria-label={label}
          className="rounded-lg border border-line p-1.5 text-muted hover:text-ink"
        >
          {theme === 'dark' ? (
            <svg viewBox="0 0 24 24" fill="none" className="size-4" aria-hidden>
              <circle cx="12" cy="12" r="4" stroke="currentColor" strokeWidth="1.8" />
              <path
                d="M12 3v2m0 14v2M3 12h2m14 0h2M5.6 5.6l1.4 1.4m9.9 9.9 1.4 1.4m0-12.7-1.4 1.4M7 17l-1.4 1.4"
                stroke="currentColor"
                strokeWidth="1.8"
                strokeLinecap="round"
              />
            </svg>
          ) : (
            <svg viewBox="0 0 24 24" fill="none" className="size-4" aria-hidden>
              <path
                d="M20 13.5A8 8 0 0 1 10.5 4 7 7 0 1 0 20 13.5Z"
                stroke="currentColor"
                strokeWidth="1.8"
                strokeLinejoin="round"
              />
            </svg>
          )}
        </button>
      </Tooltip.Trigger>
      <Tooltip.Portal>
        <Tooltip.Content
          sideOffset={6}
          className="rounded-md border border-line bg-card px-2 py-1 text-xs text-ink shadow-sm"
        >
          {label}
        </Tooltip.Content>
      </Tooltip.Portal>
    </Tooltip.Root>
  );
}

function SessionBox() {
  const { session, logout } = useSession();
  const { t, has } = useT();
  const scopeLabel = (s: string) => {
    const key = `scope.${s}`;
    return has(key) ? t(key as MessageKey) : s;
  };
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center justify-between gap-2">
        <span className="truncate font-mono text-xs text-muted" title={t('shell.keyName')}>
          {session?.name ?? ''}
        </span>
        <button
          type="button"
          onClick={() => {
            void logout();
          }}
          className="rounded-lg border border-line px-2.5 py-1 text-xs text-muted hover:text-ink"
        >
          {t('shell.logout')}
        </button>
      </div>
      {session != null && session.scopes.length > 0 && (
        <div className="flex flex-wrap gap-1" aria-label={t('shell.scopes')}>
          {session.scopes.map((s) => (
            <span
              key={s}
              title={s}
              className="rounded border border-line px-1.5 py-0.5 text-[10px] tracking-wide text-muted uppercase"
            >
              {scopeLabel(s)}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

/** Нижний блок сайдбара/дровера: стрим, язык, тема, сессия. */
function ShellFooter() {
  return (
    <div className="mt-auto flex flex-col gap-3 border-t border-line pt-4">
      <div className="flex items-center justify-between">
        <LiveIndicator />
        <div className="flex items-center gap-2">
          <LangToggle />
          <ThemeToggle />
        </div>
      </div>
      <SessionBox />
    </div>
  );
}

/** Мобильная навигация <md: гамбургер → дровер слева (Radix Dialog: фокус-трап,
 *  Esc, возврат фокуса на триггер). На кнопке — точка при активных critical. */
function MobileNav({
  path,
  navigate,
  critical,
}: {
  path: string;
  navigate: (p: string) => void;
  critical: CriticalAlerts;
}) {
  const { t } = useT();
  const [open, setOpen] = useState(false);
  const go = (p: string) => {
    navigate(p);
    setOpen(false);
  };
  return (
    <Dialog.Root open={open} onOpenChange={setOpen}>
      <Dialog.Trigger asChild>
        <button
          type="button"
          aria-label={t('nav.menu')}
          className="relative rounded-lg border border-line p-1.5 text-muted hover:text-ink"
        >
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" className="size-5" aria-hidden>
            <path d="M4 7h16M4 12h16M4 17h16" />
          </svg>
          {critical.count > 0 && (
            <span aria-hidden className="absolute -top-0.5 -right-0.5 size-2 rounded-full bg-dead ring-2 ring-card" />
          )}
        </button>
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed inset-y-0 left-0 z-50 flex w-[min(18rem,85vw)] flex-col gap-6 border-r border-line bg-card px-4 py-5 shadow-2xl focus:outline-none"
        >
          <div className="flex items-center justify-between">
            <Dialog.Title asChild>
              <span>
                <Brand />
              </span>
            </Dialog.Title>
            <Dialog.Close asChild>
              <button type="button" aria-label={t('common.close')} className="rounded-lg border border-line p-1.5 text-muted hover:text-ink">
                <svg viewBox="0 0 16 16" className="size-4" aria-hidden>
                  <path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
                </svg>
              </button>
            </Dialog.Close>
          </div>
          <ScopePicker path={path} />
          <NavLinks path={path} navigate={go} critical={critical} />
          <ShellFooter />
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

export function Shell({ path, navigate, children }: { path: string; navigate: (p: string) => void; children: ReactNode }) {
  const { t } = useT();
  const critical = useCriticalAlerts();
  return (
    <Tooltip.Provider delayDuration={300}>
      <a href="#main-content" className="skip-link">
        {t('a11y.skipToContent')}
      </a>
      <div className="flex min-h-screen">
        {/* Боковая колонка ≥md */}
        <aside className="sticky top-0 hidden h-screen w-56 shrink-0 flex-col gap-6 border-r border-line bg-card px-4 py-5 md:flex">
          <Brand />
          <ScopePicker path={path} />
          <NavLinks path={path} navigate={navigate} critical={critical} />
          <ShellFooter />
        </aside>

        <div className="flex min-w-0 flex-1 flex-col">
          {/* Верхняя панель <md */}
          <header className="sticky top-0 z-10 flex items-center justify-between gap-3 border-b border-line bg-card px-4 py-3 md:hidden">
            <Brand />
            <div className="flex min-w-0 items-center gap-3">
              <ScopeIndicator path={path} />
              <LiveIndicator />
              <MobileNav path={path} navigate={navigate} critical={critical} />
            </div>
          </header>
          <main id="main-content" tabIndex={-1} className="mx-auto flex w-full max-w-[1400px] flex-1 flex-col gap-4 px-4 py-5 focus:outline-none md:px-6">
            {children}
          </main>
        </div>
      </div>
    </Tooltip.Provider>
  );
}
