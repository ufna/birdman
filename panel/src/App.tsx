import { ThemeProvider } from './lib/theme';
import { I18nProvider, useT } from './lib/i18n';
import { SessionProvider, canAdmin, canRead, useSession } from './lib/session';
import { LiveProvider } from './lib/live';
import { EnvProvider } from './lib/env';
import { DrawerProvider } from './lib/drawer';
import { ToastProvider } from './components/Toast';
import { usePath } from './lib/usePath';
import { Shell } from './components/Shell';
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
        <EnvProvider>
          <DrawerProvider>
            <Routed />
          </DrawerProvider>
        </EnvProvider>
      </ToastProvider>
    </LiveProvider>
  );
}

function Routed() {
  const [path, navigate] = usePath();
  const { session } = useSession();
  const mayAdmin = session != null && canAdmin(session);
  const screen = path.startsWith('/fleet') ? (
    <Fleet />
  ) : path.startsWith('/matches') ? (
    <Matches />
  ) : path.startsWith('/deploys') ? (
    <Deploys navigate={navigate} />
  ) : path.startsWith('/events') ? (
    <Events />
  ) : path.startsWith('/stats') ? (
    <Stats />
  ) : path.startsWith('/cost') ? (
    <Cost />
  ) : path.startsWith('/alerts') ? (
    <Alerts />
  ) : path.startsWith('/logs') ? (
    <Logs />
  ) : path.startsWith('/backups') ? (
    // Бекапы — admin-only: не-admin по прямому URL уводим на Обзор (в нав его нет).
    mayAdmin ? <Backups /> : <Overview />
  ) : path.startsWith('/access') ? (
    // Доступ — admin-only: не-admin по прямому URL уводим на Обзор (в нав его нет).
    mayAdmin ? <Access /> : <Overview />
  ) : (
    <Overview />
  );
  return (
    <Shell path={path} navigate={navigate}>
      {screen}
    </Shell>
  );
}
