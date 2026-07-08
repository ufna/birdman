// Каркас после логина: навигация, live-индикатор стрима, переключатели
// языка и темы, выход. До 1280px страница не скроллится по горизонтали —
// таблицы скроллятся внутри карточек.

import type { ReactNode } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import { useLive } from '../lib/live';
import { useSession } from '../lib/session';
import { useTheme } from '../lib/theme';
import { useT } from '../lib/i18n';
import type { Lang, MessageKey } from '../lib/i18n';
import { Brand } from './ui';

const navItems: { path: string; key: MessageKey }[] = [
  { path: '/', key: 'nav.overview' },
  { path: '/fleet', key: 'nav.fleet' },
  { path: '/matches', key: 'nav.matches' },
  { path: '/deploys', key: 'nav.deploys' },
  { path: '/events', key: 'nav.events' },
];

function isActive(item: string, path: string): boolean {
  return item === '/' ? path === '/' : path.startsWith(item);
}

function NavLinks({ path, navigate, row = false }: { path: string; navigate: (p: string) => void; row?: boolean }) {
  const { t } = useT();
  return (
    <nav className={row ? 'flex items-center gap-1' : 'flex flex-col gap-1'} aria-label={t('nav.sections')}>
      {navItems.map((item) => {
        const active = isActive(item.path, path);
        return (
          <a
            key={item.path}
            href={item.path}
            aria-current={active ? 'page' : undefined}
            onClick={(e) => {
              e.preventDefault();
              navigate(item.path);
            }}
            className={`rounded-lg px-3 py-1.5 text-sm transition-colors ${
              active
                ? 'bg-mark font-medium text-accent-ink'
                : 'text-muted hover:bg-paper hover:text-ink'
            }`}
          >
            {t(item.key)}
          </a>
        );
      })}
    </nav>
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
