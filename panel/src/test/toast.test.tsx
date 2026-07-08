import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { I18nProvider } from '../lib/i18n';
import { ToastProvider, useToast } from '../components/Toast';

function Harness() {
  const toast = useToast();
  return (
    <div>
      <button type="button" onClick={() => { toast.success('Saved ok'); }}>ok</button>
      <button type="button" onClick={() => { toast.error('Bad thing'); }}>err</button>
    </div>
  );
}

const wrap = () =>
  render(
    <I18nProvider initialLang="en">
      <ToastProvider>
        <Harness />
      </ToastProvider>
    </I18nProvider>,
  );

afterEach(() => {
  vi.useRealTimers();
});

describe('Toast', () => {
  it('успех → role=status; закрытие по кнопке убирает тост', async () => {
    wrap();
    fireEvent.click(screen.getByText('ok'));
    const toast = await screen.findByRole('status');
    expect(toast.textContent).toContain('Saved ok');
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    await waitFor(() => {
      expect(screen.queryByText('Saved ok')).toBeNull();
    });
  });

  it('ошибка → role=alert (ассертивно)', async () => {
    wrap();
    fireEvent.click(screen.getByText('err'));
    const toast = await screen.findByRole('alert');
    expect(toast.textContent).toContain('Bad thing');
  });

  it('стопка: два тоста одновременно', async () => {
    wrap();
    fireEvent.click(screen.getByText('ok'));
    fireEvent.click(screen.getByText('err'));
    expect(await screen.findByText('Saved ok')).toBeTruthy();
    expect(screen.getByText('Bad thing')).toBeTruthy();
  });

  it('авто-скрытие по таймауту', () => {
    vi.useFakeTimers();
    try {
      wrap();
      act(() => {
        fireEvent.click(screen.getByText('ok'));
      });
      expect(screen.getByText('Saved ok')).toBeTruthy();
      act(() => {
        vi.advanceTimersByTime(4600);
      });
      expect(screen.queryByText('Saved ok')).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });
});
