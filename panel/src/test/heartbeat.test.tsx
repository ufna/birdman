import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { ReactElement } from 'react';
import { I18nProvider } from '../lib/i18n';
import { HeartbeatCell } from '../screens/Fleet';

// HeartbeatCell локализована; рендерим в RU-провайдере, чтобы проверять
// именно русские подписи (тон — суть теста — от языка не зависит).
const renderRu = (ui: ReactElement) => render(<I18nProvider initialLang="ru">{ui}</I18nProvider>);

describe('HeartbeatCell', () => {
  it('свежий heartbeat — зелёный и с возрастом', () => {
    const iso = new Date(Date.now() - 3_000).toISOString();
    renderRu(<HeartbeatCell iso={iso} />);
    const el = screen.getByText(/с назад$/);
    expect(el.parentElement?.className).toContain('text-good');
  });

  it('протухший heartbeat — красный', () => {
    const iso = new Date(Date.now() - 42_000).toISOString();
    renderRu(<HeartbeatCell iso={iso} />);
    // 42с ± секундный тик — важен тон, не точное значение.
    expect(screen.getByText(/4\dс назад/).parentElement?.className).toContain('text-dead');
  });

  it('нода ещё не репортила — «не был»', () => {
    renderRu(<HeartbeatCell />);
    expect(screen.getByText('не был').parentElement?.className).toContain('text-dead');
  });
});
