/* eslint-disable react-refresh/only-export-components -- SECTION_SCREENS — таблица «раздел → экран», которую читает роутер в этом же файле; её же читает пин соответствия «пункт нава ⇄ экран» (tracker #1118). Разносить таблицу и её единственного потребителя по модулям ради гранулярности Fast Refresh в dev-сервере дороже, чем оно стоит. Политика — в eslint.config.js. */

import type { ReactNode } from 'react';
import { ThemeProvider } from './lib/theme';
import { I18nProvider, useT } from './lib/i18n';
import { SessionProvider, canRead, useSession } from './lib/session';
import { LiveProvider } from './lib/live';
import { EnvProvider } from './lib/env';
import { ProjectProvider } from './lib/project';
import { DrawerProvider } from './lib/drawer';
import { ToastProvider } from './components/Toast';
import { usePath } from './lib/usePath';
import { Shell, effectiveSectionOf } from './components/Shell';
import type { SectionPath } from './components/Shell';
import { Brand, Card } from './components/ui';
import { Login } from './screens/Login';
import { Overview } from './screens/Overview';
import { Fleet } from './screens/Fleet';
import { Matches } from './screens/Matches';
import { Deploys } from './screens/Deploys';
import { Events } from './screens/Events';
import { Stats } from './screens/Stats';
import { Cost } from './screens/Cost';
import { Alerts } from './screens/Alerts';
import { Logs } from './screens/Logs';
import { Backups } from './screens/Backups';
import { Access } from './screens/Access';

export default function App() {
  return (
    <ThemeProvider>
      <I18nProvider>
        <SessionProvider>
          <Root />
        </SessionProvider>
      </I18nProvider>
    </ThemeProvider>
  );
}

function Root() {
  const { session, logout } = useSession();
  const { t } = useT();

  // Пробуем существующую cookie — без мигания формой логина.
  if (session === undefined) {
    return (
      <div className="flex min-h-screen items-center justify-center">
        <Brand />
      </div>
    );
  }
  if (session === null) return <Login />;
  if (!canRead(session)) {
    return (
      <div className="flex min-h-screen items-center justify-center px-4">
        <Card className="max-w-sm p-5 text-sm">
          <p>{t('app.noScope', { name: session.name })}</p>
          <button
            type="button"
            onClick={() => {
              void logout();
            }}
            className="mt-4 rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white hover:opacity-90"
          >
            {t('app.switchKey')}
          </button>
        </Card>
      </div>
    );
  }
  return (
    <LiveProvider>
      <ToastProvider>
        {/* Проект снаружи окружения: окружения принадлежат проекту, и
            EnvProvider читает выбранный проект из ProjectContext. */}
        <ProjectProvider>
          <EnvProvider>
            <DrawerProvider>
              <Routed />
            </DrawerProvider>
          </EnvProvider>
        </ProjectProvider>
      </ToastProvider>
    </LiveProvider>
  );
}

/** Что рендерить в разделе. Функция от `navigate`, а не готовый узел: экрану
 *  Деплоев нужна навигация, а замыкать её в модульной константе нельзя. */
type ScreenOf = (navigate: (p: string) => void) => ReactNode;

/**
 * Экраны разделов — ОДНА таблица «корень раздела → что рендерить», которую и
 * читает роутер ниже. Была цепочка `section === '…' ? … :` в теле роутера, и
 * соответствие «пункт нава ⇄ ветка роутера» держалось на договорённости:
 * сегодня ветка есть у всех десяти корней, но пункт, добавленный БЕЗ ветки,
 * провалился бы в `else` — нав подсвечивал бы «Secrets», а на экране был бы
 * Обзор, и заметить это было нечем (tracker #1118).
 *
 * Тип `Record<SectionPath, …>` превращает эту договорённость в проверку
 * КОМПИЛЯТОРА: `SectionPath` выведён из литералов `NAV_ITEMS` (Shell.tsx), а
 * `Record` требует ключ на КАЖДЫЙ из них — новый пункт нава без записи здесь
 * роняет `tsc`, лишний ключ без пункта нава роняет его же. Инвариант #1111
 * («эффективный раздел ВСЕГДА есть в наве своей сессии») — это первая
 * половина соответствия; таблица — вторая.
 */
export const SECTION_SCREENS: Record<SectionPath, ScreenOf> = {
  '/fleet': () => <Fleet />,
  '/matches': () => <Matches />,
  '/deploys': (navigate) => <Deploys navigate={navigate} />,
  '/events': () => <Events />,
  '/stats': () => <Stats />,
  '/cost': () => <Cost />,
  '/alerts': () => <Alerts />,
  '/logs': () => <Logs />,
  '/backups': () => <Backups />,
  '/access': () => <Access />,
};

/**
 * Экспортируется для тестов (как LiveContext в lib/live.tsx): здесь пара
 * (путь, сессия) превращается в РАЗДЕЛ и экран, и пин «экран и скоуп приезжают
 * из ОДНОГО раздела» иначе не написать — ни для под-пути (#1109), ни для
 * admin-only раздела у не-admin (#1111). В приложении рендерится только из
 * Root — и только после его гейта `canRead`: сессия без `readonly`/`admin`
 * сюда не доходит вовсе, так что права режутся в двух местах, а не в одном.
 */
export function Routed() {
  const [path, navigate] = usePath();
  const { session } = useSession();
  // Экран выбирает не сырой путь, а РАЗДЕЛ пары (путь, сессия). Резка по
  // границе сегмента: под-путь `/logs/x` — те же Логи, а посторонний
  // `/logsomething` — Обзор (голый startsWith отдавал его Логам, #1109).
  // Права: admin-only раздел (`adminOnly` в NAV_ITEMS — сегодня `/backups` и
  // `/access`) у не-admin деградирует в Обзор прямо здесь, поэтому веток
  // `mayAdmin ? … : <Overview/>` ниже больше нет: гейт РАЗДЕЛОВ выводится из
  // состава нава, а не переписывается руками на каждый (tracker #1111).
  // Гейт ДОСТУПА к панели целиком — отдельный и по-прежнему свой, `canRead`
  // в Root выше.
  // Ту же функцию спрашивают подсветка нава и классификация скоупа, так что
  // разъехаться им негде — ни по пути, ни по сессии.
  const section = effectiveSectionOf(path, session);
  // Экран берётся из SECTION_SCREENS выше. Индексируем через `string`, а не
  // через `SectionPath`: `section` — это Обзор ИЛИ корень раздела, и `undefined`
  // здесь легитимен ровно для Обзора (`/` и всё неизвестное).
  const make = (SECTION_SCREENS as Partial<Record<string, ScreenOf>>)[section];
  const screen = make === undefined ? <Overview /> : make(navigate);
  return (
    <Shell path={path} navigate={navigate}>
      {screen}
    </Shell>
  );
}
