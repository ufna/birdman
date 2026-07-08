// Каркас после логина: навигация, live-индикатор стрима, переключатели
// языка и темы, выход. До 1280px страница не скроллится по горизонтали —
// таблицы скроллятся внутри карточек.

import type { ReactNode } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import { useLive } from '../lib/live';
import { canAdmin, useSession } from '../lib/session';
import type { SessionInfo } from '../lib/api';
import { useTheme } from '../lib/theme';
import { useT } from '../lib/i18n';
import type { Lang, MessageKey } from '../lib/i18n';
import { Brand } from './ui';

type NavIcon = 'overview' | 'fleet' | 'matches' | 'deploys' | 'events' | 'stats' | 'cost' | 'alerts' | 'access';

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
  { path: '/access', key: 'nav.access', icon: 'access', adminOnly: true },
];

/** Пункты навигации для сессии: admin-only скрыты у не-admin. Чистая — тест scope-гейта. */
export function navItemsFor(session: SessionInfo | null | undefined): NavItem[] {
  return NAV_ITEMS.filter((it) => it.adminOnly !== true || (session != null && canAdmin(session)));
}

function isActive(item: string, path: string): boolean {
  return item === '/' ? path === '/' : path.startsWith(item);
}

function NavLinks({ path, navigate, row = false }: { path: string; navigate: (p: string) => void; row?: boolean }) {
  const { t } = useT();
  const { session } = useSession();
  const items = navItemsFor(session);
  return (
    <nav
      className={row ? 'flex min-w-0 items-center gap-0.5 overflow-x-auto' : 'flex flex-col gap-1'}
      aria-label={t('nav.sections')}
    >
      {items.map((item) => {
        const active = isActive(item.path, path);
        const label = t(item.key);
        return (
          <a
            key={item.path}
            href={item.path}
            aria-current={active ? 'page' : undefined}
            aria-label={row ? label : undefined}
            title={row ? label : undefined}
            onClick={(e) => {
              e.preventDefault();
              navigate(item.path);
            }}
            className={`flex shrink-0 items-center gap-2 rounded-lg text-sm transition-colors ${
              row ? 'px-2 py-1.5' : 'px-3 py-1.5'
            } ${active ? 'bg-mark font-medium text-accent-ink' : 'text-muted hover:bg-paper hover:text-ink'}`}
          >
            <NavGlyph icon={item.icon} />
            {!row && label}
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
    access: <path d="M14 7a4 4 0 1 0-3.5 6L9 14.5l1.5 1.5L9 17.5l1.5 1.5L13 16l-1.5-1.5.9-.9A4 4 0 0 0 14 7Zm.5 2.5h.01" />,
  };
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden className="size-4 shrink-0">
      {p[icon]}
    </svg>
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

export function Shell({ path, navigate, children }: { path: string; navigate: (p: string) => void; children: ReactNode }) {
  return (
    <Tooltip.Provider delayDuration={300}>
      <div className="flex min-h-screen">
        {/* Боковая колонка ≥md */}
        <aside className="sticky top-0 hidden h-screen w-56 shrink-0 flex-col gap-6 border-r border-line bg-card px-4 py-5 md:flex">
          <Brand />
          <NavLinks path={path} navigate={navigate} />
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
        </aside>

        <div className="flex min-w-0 flex-1 flex-col">
          {/* Верхняя панель <md */}
          <header className="sticky top-0 z-10 flex items-center justify-between gap-3 border-b border-line bg-card px-4 py-3 md:hidden">
            <Brand />
            <NavLinks path={path} navigate={navigate} row />
            <div className="flex items-center gap-2">
              <LiveIndicator />
              <LangToggle />
              <ThemeToggle />
            </div>
          </header>
          <main className="mx-auto w-full max-w-[1400px] flex-1 px-4 py-5 md:px-6">{children}</main>
        </div>
      </div>
    </Tooltip.Provider>
  );
}
