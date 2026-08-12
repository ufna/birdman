import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

// jsdom не даёт matchMedia (нужен uPlot при импорте и theme.tsx) и
// ResizeObserver (uPlot при создании графика) — минимальные заглушки.
if (typeof window.matchMedia !== 'function') {
  window.matchMedia = (query: string): MediaQueryList =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList;
}
if (typeof globalThis.ResizeObserver === 'undefined') {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver;
}

// Канвас: jsdom не реализует getContext вовсе (без пакета `canvas`) — он
// печатает «Not implemented» и возвращает null. Для тестов, где график только
// ПУСТОЙ, это безобидно; но как только uPlot получает данные и рисует, его
// асинхронный `_commit` падает на `ctx.clearRect` НЕПЕРЕХВАЧЕННОЙ ошибкой.
//
// Цена этого — не «шум в выводе», а красный прогон: vitest считает unhandled
// error падением РАНА, при том что все тесты в нём passed. Поймано ровно так
// (tracker #1011 → #1012): локально `npm test | grep 'Tests '` показывал
// «480 passed», а код возврата был 1, и CI закономерно покраснел. Смотреть
// надо код возврата, а не строку в выводе.
//
// Заглушка — Proxy: любой метод контекста no-op, а немногие, от кого ждут
// значение, отвечают правдоподобно. Так шим не приходится дополнять каждый
// раз, когда uPlot вызовет ещё один метод.
// Path2D — из того же ряда: uPlot строит им пути, jsdom его не определяет вовсе.
if (typeof globalThis.Path2D === 'undefined') {
  globalThis.Path2D = class {
    addPath() {}
    arc() {}
    arcTo() {}
    bezierCurveTo() {}
    closePath() {}
    ellipse() {}
    lineTo() {}
    moveTo() {}
    quadraticCurveTo() {}
    rect() {}
    roundRect() {}
  } as unknown as typeof Path2D;
}

if (typeof HTMLCanvasElement !== 'undefined') {
  const stubCtx = (canvas: HTMLCanvasElement): unknown => {
    const own: Record<string, unknown> = {
      canvas,
      measureText: () => ({ width: 0 }),
      createLinearGradient: () => ({ addColorStop: () => {} }),
      getImageData: () => ({ data: new Uint8ClampedArray(4) }),
    };
    return new Proxy(own, {
      get: (t, prop: string | symbol) => (prop in t ? t[prop as string] : () => undefined),
      set: (t, prop: string | symbol, value: unknown) => {
        t[prop as string] = value;
        return true;
      },
    });
  };
  HTMLCanvasElement.prototype.getContext = function getContext(this: HTMLCanvasElement, type: string) {
    return type === '2d' ? (stubCtx(this) as CanvasRenderingContext2D) : null;
  } as HTMLCanvasElement['getContext'];
}

afterEach(() => {
  cleanup();
});
