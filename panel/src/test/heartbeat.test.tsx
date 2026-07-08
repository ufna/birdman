import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { HeartbeatCell } from '../screens/Fleet';

describe('HeartbeatCell', () => {
  it('свежий heartbeat — зелёный и с возрастом', () => {
    const iso = new Date(Date.now() - 3_000).toISOString();
    render(<HeartbeatCell iso={iso} />);
    const el = screen.getByText(/с назад$/);
    expect(el.parentElement?.className).toContain('text-good');
  });

  it('протухший heartbeat — красный', () => {
    const iso = new Date(Date.now() - 42_000).toISOString();
    render(<HeartbeatCell iso={iso} />);
    // 42с ± секундный тик — важен тон, не точное значение.
    expect(screen.getByText(/4\dс назад/).parentElement?.className).toContain('text-dead');
  });

  it('нода ещё не репортила — «не был»', () => {
    render(<HeartbeatCell />);
    expect(screen.getByText('не был').parentElement?.className).toContain('text-dead');
  });
});
