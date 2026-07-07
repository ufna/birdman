import { ThemeProvider } from './lib/theme';
import { SessionProvider, canRead, useSession } from './lib/session';
import { LiveProvider } from './lib/live';
import { usePath } from './lib/usePath';
import { Shell } from './components/Shell';
import { Brand, Card } from './components/ui';
import { Login } from './screens/Login';
import { Overview } from './screens/Overview';
import { Fleet } from './screens/Fleet';
import { Matches } from './screens/Matches';

export default function App() {
  return (
    <ThemeProvider>
      <SessionProvider>
        <Root />
      </SessionProvider>
    </ThemeProvider>
  );
}

function Root() {
  const { session, logout } = useSession();

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
          <p>
            У ключа <span className="font-mono">{session.name}</span> нет скоупа{' '}
            <span className="font-mono">readonly</span> — панели нечего показать.
          </p>
          <button
            type="button"
            onClick={() => {
              void logout();
            }}
            className="mt-4 rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white hover:opacity-90"
          >
            Войти другим ключом
          </button>
        </Card>
      </div>
    );
  }
  return (
    <LiveProvider>
      <Routed />
    </LiveProvider>
  );
}

function Routed() {
  const [path, navigate] = usePath();
  const screen = path.startsWith('/fleet') ? (
    <Fleet />
  ) : path.startsWith('/matches') ? (
    <Matches />
  ) : (
    <Overview />
  );
  return (
    <Shell path={path} navigate={navigate}>
      {screen}
    </Shell>
  );
}
