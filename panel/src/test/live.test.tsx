import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { LiveContext, useLiveAsync } from '../lib/live';
import type { StreamEvent } from '../lib/sse';

// Живой refetch Stats: всплеск SSE-событий коалесится в ОДИН отложенный запрос
// (дебанс), а не спамит тяжёлые /v1/stats/*. Драйвим управляемым LiveContext.

const fakeEvent: StreamEvent = { id: 1, kind: 'server_drain', event: { id: 1, ts: 't', kind: 'server_drain', payload: {} } };

function makeLive() {
  const listeners = new Set<(e: StreamEvent) => void>();
  const value = {
    status: 'live' as const,
    subscribe: (fn: (e: StreamEvent) => void) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
  };
  const emit = () => listeners.forEach((fn) => fn(fakeEvent));
  const wrap = ({ children }: { children: ReactNode }) => (
    <LiveContext.Provider value={value}>{children}</LiveContext.Provider>
  );
  return { emit, wrap };
}

describe('useLiveAsync — дебанс/коалесинг живого refetch', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('всплеск событий в окне → один refetch; событие после окна → ещё один', async () => {
    let calls = 0;
    const fetcher = () => {
      calls += 1;
      return Promise.resolve(calls);
    };
    const { emit, wrap: Wrap } = makeLive();
    function Probe() {
      const s = useLiveAsync(fetcher, [], { debounceMs: 3000 });
      return <span>{s.data ?? '-'}</span>;
    }

    await act(async () => {
      render(
        <Wrap>
          <Probe />
        </Wrap>,
      );
    });
    expect(calls).toBe(1); // загрузка на маунте

    // Три события подряд в пределах окна — коалесятся в один отложенный refetch.
    act(() => {
      emit();
      emit();
      emit();
    });
    expect(calls).toBe(1); // таймер ещё не сработал
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });
    expect(calls).toBe(2); // ровно один refetch на всплеск

    // Новое событие после окна → отдельный refetch.
    act(() => {
      emit();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });
    expect(calls).toBe(3);
  });

  it('без событий лишних запросов нет (нет фонового поллинга)', async () => {
    let calls = 0;
    const fetcher = () => {
      calls += 1;
      return Promise.resolve(calls);
    };
    const { wrap: Wrap } = makeLive();
    function Probe() {
      useLiveAsync(fetcher, [], { debounceMs: 3000 });
      return null;
    }
    await act(async () => {
      render(
        <Wrap>
          <Probe />
        </Wrap>,
      );
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(calls).toBe(1); // только начальная загрузка
  });
});
