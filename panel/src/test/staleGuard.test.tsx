// #969: ответ ПРОШЛОГО среза не должен затирать данные текущего.
//
// Guard в data-хуках был локальным флагом в замыкании load(), а взводил его
// дизпоузер — использовавшийся ровно в одном месте, в useEffect. Все остальные
// вызовы load() (SSE-рефетч, страховочный поллинг, кнопка «Повторить») его
// выбрасывали, поэтому их запросы не помечались устаревшими никогда: сменил
// проект — и ответ старого доезжал в состояние нового.
//
// Тест воспроизводит именно этот путь: reload() при висящем первом запросе.
// На старом коде поздний ответ побеждал; теперь он отбрасывается по поколению.

import { describe, expect, it } from 'vitest';
import { act, render, screen } from '@testing-library/react';
import { useAsync } from '../lib/useAsync';

function Probe({ fetcher }: { fetcher: () => Promise<string> }) {
  const { data, reload } = useAsync(fetcher, []);
  return (
    <div>
      <span data-testid="value">{data ?? '—'}</span>
      <button type="button" onClick={reload}>
        reload
      </button>
    </div>
  );
}

describe('useAsync: ответ прошлого запроса не затирает свежий', () => {
  it('reload при висящем запросе — поздний ответ отбрасывается', async () => {
    const resolvers: ((v: string) => void)[] = [];
    const fetcher = () =>
      new Promise<string>((resolve) => {
        resolvers.push(resolve);
      });

    render(<Probe fetcher={fetcher} />);
    // Первый запрос (из эффекта) висит; жмём «Повторить» — второй тоже висит.
    await act(async () => {
      screen.getByRole('button', { name: 'reload' }).click();
    });
    expect(resolvers.length).toBe(2);

    // Свежий приходит первым, устаревший — вторым (ровно та гонка из карточки).
    await act(async () => {
      resolvers[1]('НОВЫЙ');
    });
    expect(screen.getByTestId('value').textContent).toBe('НОВЫЙ');

    await act(async () => {
      resolvers[0]('СТАРЫЙ');
    });
    expect(screen.getByTestId('value').textContent).toBe('НОВЫЙ');
  });
});
