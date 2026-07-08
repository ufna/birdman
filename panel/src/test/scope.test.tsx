import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { SessionInfo } from '../lib/api';
import { canAdmin, canDeploy, canRead } from '../lib/session';
import { ConfirmButton } from '../components/ConfirmDialog';

// jsdom не реализует scrollIntoView, который может дёрнуть Radix при фокусе.
HTMLElement.prototype.scrollIntoView = () => {};

const sess = (...scopes: SessionInfo['scopes']): SessionInfo => ({ scopes, name: 'k' });

describe('скоуп-гейт: helpers', () => {
  it('canDeploy: deploy или admin', () => {
    expect(canDeploy(sess('deploy'))).toBe(true);
    expect(canDeploy(sess('admin'))).toBe(true); // admin включает остальные
    expect(canDeploy(sess('readonly'))).toBe(false);
    expect(canDeploy(sess('matchmaking'))).toBe(false);
  });
  it('canAdmin: только admin', () => {
    expect(canAdmin(sess('admin'))).toBe(true);
    expect(canAdmin(sess('deploy'))).toBe(false);
    expect(canAdmin(sess('readonly'))).toBe(false);
  });
  it('canRead: readonly или admin', () => {
    expect(canRead(sess('readonly'))).toBe(true);
    expect(canRead(sess('admin'))).toBe(true);
    expect(canRead(sess('deploy'))).toBe(false);
  });
});

// Экраны гейтят кнопки этим паттерном: {canDeploy(session) && <ConfirmButton/>}.
function DeployGate({ session }: { session: SessionInfo }) {
  return <div>{canDeploy(session) && <ConfirmButton label="Развернуть" title="t" description="d" confirmLabel="OK" onConfirm={async () => {}} />}</div>;
}
function DrainGate({ session }: { session: SessionInfo }) {
  return <div>{canAdmin(session) && <ConfirmButton label="Drain" tone="dead" title="t" description="d" confirmLabel="OK" onConfirm={async () => {}} />}</div>;
}

describe('скоуп-гейт: видимость кнопок', () => {
  it('readonly не видит Deploy и Drain', () => {
    render(<DeployGate session={sess('readonly')} />);
    render(<DrainGate session={sess('readonly')} />);
    expect(screen.queryByRole('button', { name: 'Развернуть' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Drain' })).toBeNull();
  });
  it('deploy видит Deploy, но не Drain (Drain — admin-only)', () => {
    render(<DeployGate session={sess('deploy')} />);
    render(<DrainGate session={sess('deploy')} />);
    expect(screen.getByRole('button', { name: 'Развернуть' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Drain' })).toBeNull();
  });
  it('admin видит и Deploy, и Drain', () => {
    render(<DeployGate session={sess('admin')} />);
    render(<DrainGate session={sess('admin')} />);
    expect(screen.getByRole('button', { name: 'Развернуть' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Drain' })).toBeTruthy();
  });
});

describe('ConfirmButton', () => {
  it('открывает диалог и вызывает onConfirm по подтверждению', async () => {
    const onConfirm = vi.fn().mockResolvedValue(undefined);
    render(
      <ConfirmButton label="Drain" tone="dead" title="Вывести n1?" description="описание" confirmLabel="Подтвердить" onConfirm={onConfirm} />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Drain' }));
    expect(await screen.findByText('Вывести n1?')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Подтвердить' }));
    await waitFor(() => {
      expect(onConfirm).toHaveBeenCalledTimes(1);
    });
  });
});
