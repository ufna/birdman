import { ThemeProvider } from './lib/theme';
import { I18nProvider, useT } from './lib/i18n';
import { SessionProvider, canAdmin, canRead, useSession } from './lib/session';
import { LiveProvider } from './lib/live';
import { EnvProvider } from './lib/env';
import { ProjectProvider } from './lib/project';
import { DrawerProvider } from './lib/drawer';
import { ToastProvider } from './components/Toast';
import { usePath } from './lib/usePath';
import { Shell, sectionOf } from './components/Shell';
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

/**
 * Экспортируется для тестов (как LiveContext в lib/live.tsx): роутер — это
 * место, где путь превращается в экран, и пин «под-путь получает экран и
 * скоуп ОДНОГО раздела» иначе не написать. В приложении рендерится только
 * из Root.
 */
export function Routed() {
  const [path, navigate] = usePath();
  const { session } = useSession();
  const mayAdmin = session != null && canAdmin(session);
  // Экран выбирает РАЗДЕЛ пути, а не сырой путь: sectionOf режет по границе
  // сегмента, поэтому под-путь `/logs/x` — те же Логи, а посторонний
  // `/logsomething` — Обзор (голый startsWith отдавал его Логам). Ту же
  // функцию спрашивают подсветка нава и классификация скоупа, так что
  // разъехаться им теперь негде (tracker #1109).
  const section = sectionOf(path);
  const screen =
    section === '/fleet' ? (
      <Fleet />
    ) : section === '/matches' ? (
      <Matches />
    ) : section === '/deploys' ? (
      <Deploys navigate={navigate} />
    ) : section === '/events' ? (
      <Events />
    ) : section === '/stats' ? (
      <Stats />
    ) : section === '/cost' ? (
      <Cost />
    ) : section === '/alerts' ? (
      <Alerts />
    ) : section === '/logs' ? (
      <Logs />
    ) : section === '/backups' ? (
      // Бекапы — admin-only: не-admin по прямому URL уводим на Обзор (в нав его нет).
      mayAdmin ? (
        <Backups />
      ) : (
        <Overview />
      )
    ) : section === '/access' ? (
      // Доступ — admin-only: не-admin по прямому URL уводим на Обзор (в нав его нет).
      mayAdmin ? (
        <Access />
      ) : (
        <Overview />
      )
    ) : (
      <Overview />
    );
  return (
    <Shell path={path} navigate={navigate}>
      {screen}
    </Shell>
  );
}
