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
 * место, где пара (путь, сессия) превращается в экран, и пин «экран и скоуп
 * приезжают из ОДНОГО раздела» иначе не написать — ни для под-пути (#1109),
 * ни для admin-only раздела у не-admin (#1111). В приложении рендерится
 * только из Root.
 */
export function Routed() {
  const [path, navigate] = usePath();
  const { session } = useSession();
  // Экран выбирает не сырой путь, а РАЗДЕЛ пары (путь, сессия). Резка по
  // границе сегмента: под-путь `/logs/x` — те же Логи, а посторонний
  // `/logsomething` — Обзор (голый startsWith отдавал его Логам, #1109).
  // Права: admin-only раздел (`adminOnly` в NAV_ITEMS — сегодня `/backups` и
  // `/access`) у не-admin деградирует в Обзор прямо здесь, поэтому веток
  // `mayAdmin ? … : <Overview/>` ниже больше нет: гейт выводится из состава
  // нава, а не переписывается руками на каждый раздел (tracker #1111).
  // Ту же функцию спрашивают подсветка нава и классификация скоупа, так что
  // разъехаться им негде — ни по пути, ни по сессии.
  const section = effectiveSectionOf(path, session);
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
      <Backups />
    ) : section === '/access' ? (
      <Access />
    ) : (
      <Overview />
    );
  return (
    <Shell path={path} navigate={navigate}>
      {screen}
    </Shell>
  );
}
