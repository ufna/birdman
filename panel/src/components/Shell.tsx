// Каркас после логина: навигация, live-индикатор стрима, переключатель
// темы, выход. До 1280px страница не скроллится по горизонтали — таблицы
// скроллятся внутри карточек.

import type { ReactNode } from 'react';
import * as Tooltip from '@radix-ui/react-tooltip';
import { useLive } from '../lib/live';
import { useSession } from '../lib/session';
import { useTheme } from '../lib/theme';
import { Brand } from './ui';

const navItems = [
  { path: '/', label: 'Overview' },
  { path: '/fleet', label: 'Флот' },
  { path: '/matches', label: 'Матчи' },
];

function isActive(item: string, path: string): boolean {
  return item === '/' ? path === '/' : path.startsWith(item);
}

function NavLinks({ path, navigate, row = false }: { path: string; navigate: (p: string) => void; row?: boolean }) {
  return (
    <nav className={row ? 'flex items-center gap-1' : 'flex flex-col gap-1'} aria-label="Разделы">
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
            {item.label}
          </a>
        );
      })}
    </nav>
  );
}

function LiveIndicator() {
  const { status } = useLive();
  const view = {
    live: { cls: 'bg-good', dotAnim: true, label: 'live' },
    connecting: { cls: 'bg-warn', dotAnim: false, label: 'подключение…' },
    down: { cls: 'bg-dead', dotAnim: false, label: 'переподключение…' },
    unauthorized: { cls: 'bg-dead', dotAnim: false, label: 'нет доступа' },
  }[status];
  return (
    <span className="inline-flex items-center gap-2 font-mono text-xs text-muted" title="Состояние стрима событий">
      <span aria-hidden className={`size-2 rounded-full ${view.cls} ${view.dotAnim ? 'live-dot-on' : ''}`} />
      {view.label}
    </span>
  );
}

function ThemeToggle() {
  const { theme, toggle } = useTheme();
  const label = theme === 'dark' ? 'Светлая тема' : 'Тёмная тема';
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
  return (
    <div className="flex items-center justify-between gap-2">
      <span className="truncate font-mono text-xs text-muted" title="Имя API-ключа сессии">
        {session?.name ?? ''}
      </span>
      <button
        type="button"
        onClick={() => {
          void logout();
        }}
        className="rounded-lg border border-line px-2.5 py-1 text-xs text-muted hover:text-ink"
      >
        Выйти
      </button>
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
              <ThemeToggle />
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
              <ThemeToggle />
            </div>
          </header>
          <main className="mx-auto w-full max-w-[1400px] flex-1 px-4 py-5 md:px-6">{children}</main>
        </div>
      </div>
    </Tooltip.Provider>
  );
}
