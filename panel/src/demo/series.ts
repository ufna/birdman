// Ряды метрик демо: ответы /v1/metrics/query_range и /v1/metrics/query в
// формате VictoriaMetrics/Prometheus (панель разбирает их parseMatrix/
// parseVector в lib/metrics.ts).
//
// Два обещания, ради которых этот файл существует отдельно от фикстур:
//
//  1. ОКНО ЗАПОЛНЯЕТСЯ ЦЕЛИКОМ. Точки считаются под пришедшие start/end/step,
//     а не берутся из готового массива, поэтому 12 ч, 24 ч, 3 д, 7 д и 30 д
//     одинаково полны. Пустой правый край на скриншоте — худшее, что может
//     показать витрина.
//  2. ПРАВЫЙ КРАЙ СХОДИТСЯ СО СНАПШОТОМ. Значение в точке `end` равно тому,
//     что Обзор считает по флоту (игроки, идущие матчи, дедики по состояниям).
//     Иначе карточка «583 игрока» соседствовала бы с графиком, который в тот
//     же момент показывает 820.
import { fleetSnapshot, REGIONS } from './fleet';

/** Суточная форма: 0 в провале (07:00 UTC), 1 на пике (19:00 UTC). */
function shape(unixSec: number): number {
  const h = (unixSec % 86400) / 3600;
  return 0.5 - 0.5 * Math.cos((2 * Math.PI * (h - 7)) / 24);
}

/** Детерминированный шум по метке времени: одна и та же точка при каждом
 *  дозапросе даёт одно значение, иначе линия дрожала бы на поллинге. */
function noise(unixSec: number, salt: number): number {
  let t = (unixSec ^ (salt * 0x9e3779b1)) >>> 0;
  t = Math.imul(t ^ (t >>> 15), 1 | t);
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
  return (((t ^ (t >>> 14)) >>> 0) / 4294967296) * 2 - 1;
}

/** Метки времени окна: включая оба конца, шаг step. */
function stamps(start: number, end: number, step: number): number[] {
  const out: number[] = [];
  for (let t = start; t <= end; t += step) out.push(t);
  if (out[out.length - 1] !== end) out.push(end);
  return out;
}

/**
 * Волна, привязанная правым краем к `target`: в точке `end` ровно target,
 * в провале суток — target * trough. `jitter` — амплитуда шума в долях.
 */
function wave(t: number, end: number, target: number, trough: number, jitter: number, salt: number): number {
  const at = (x: number) => trough + (1 - trough) * shape(x);
  const scale = target / Math.max(at(end), 0.05);
  // На самом правом крае шум гасится: последняя точка графика — это «сейчас», и
  // она обязана совпасть с числом, которое Обзор считает по флоту. Разъехавшись
  // на 4 %, два скриншота витрины начинают противоречить друг другу.
  const jit = t >= end ? 0 : jitter;
  return at(t) * scale * (1 + noise(t, salt) * jit);
}

interface Series {
  labels: Record<string, string>;
  points: [number, string][];
}

function matrix(result: Series[]): unknown {
  return {
    status: 'ok',
    data: {
      resultType: 'matrix',
      result: result.map((s) => ({ metric: s.labels, values: s.points })),
    },
  };
}

function vector(samples: { labels: Record<string, string>; at: number; value: number }[]): unknown {
  return {
    status: 'ok',
    data: {
      resultType: 'vector',
      result: samples.map((s) => ({ metric: s.labels, value: [s.at, String(s.value)] as [number, string] })),
    },
  };
}

/** Ряд целых чисел с суточной формой. */
function intSeries(
  ts: number[],
  end: number,
  target: number,
  trough: number,
  jitter: number,
  salt: number,
  labels: Record<string, string> = {},
): Series {
  return {
    labels,
    points: ts.map((t) => [t, String(Math.max(0, Math.round(wave(t, end, target, trough, jitter, salt))))]),
  };
}

/** Селектор `{server_id="…"}` из PromQL одного дедика, если он там есть. */
function serverIdOf(query: string): string | null {
  return /server_id="([^"]+)"/.exec(query)?.[1] ?? null;
}

export function matrixFor(query: string, start: number, end: number, step: number): unknown {
  const ts = stamps(start, end, step);
  const snap = fleetSnapshot();

  if (query.includes('birdman_players_online')) {
    return matrix([intSeries(ts, end, snap.playersOnline, 0.22, 0.05, 1, { __name__: 'birdman_players_online' })]);
  }
  if (query.includes('birdman_matches_running')) {
    return matrix([intSeries(ts, end, snap.matchesRunning, 0.22, 0.06, 2, { __name__: 'birdman_matches_running' })]);
  }
  if (query.includes('birdman_mm_queue_depth')) {
    // Очередь мелкая и всплесками: на пике заметна, ночью около нуля.
    const targets: Record<string, number> = { 'eu-central': 5, 'us-east': 3, 'ap-southeast': 2 };
    return matrix(
      [...REGIONS].map((region, i) => intSeries(ts, end, targets[region], 0.05, 0.55, 10 + i, { region })),
    );
  }
  if (query.includes('birdman_node_capacity_slots')) {
    // Доля 0..1: панель форматирует её в проценты сама (Unit='percent').
    const target = snap.allocated / snap.capacitySlots;
    return matrix([
      {
        labels: {},
        points: ts.map((t) => [t, wave(t, end, target, 0.28, 0.04, 3).toFixed(4)] as [number, string]),
      },
    ]);
  }
  if (query.includes('by (state)') && query.includes('birdman_servers')) {
    const draining: Series = {
      labels: { state: 'draining' },
      // Слив начался с демотом версии 1.14.2 — до него дренящихся дедиков не
      // было вовсе. Ступенька на стеке и есть «выкат без обрыва матчей».
      points: ts.map((t) => [t, String(end - t <= snap.deprecatedAgoSec ? snap.draining : 0)] as [number, string]),
    };
    return matrix([
      intSeries(ts, end, snap.allocated, 0.22, 0.05, 4, { state: 'allocated' }),
      intSeries(ts, end, snap.ready, 0.72, 0.08, 5, { state: 'ready' }),
      draining,
      intSeries(ts, end, Math.max(snap.creating, 1), 0.3, 0.7, 6, { state: 'creating' }),
    ]);
  }

  const serverId = serverIdOf(query);
  if (serverId !== null) {
    const s = snap.serverById(serverId);
    if (s === undefined) return matrix([]);
    const labels = { server_id: serverId };
    if (query.includes('birdman_server_players')) {
      return matrix([intSeries(ts, end, s.players, 0.35, 0.12, 7, labels)]);
    }
    if (query.includes('birdman_server_tick_ms')) {
      return matrix([
        { labels, points: ts.map((t) => [t, (s.tickMs * (1 + noise(t, 8) * 0.12)).toFixed(2)] as [number, string]) },
      ]);
    }
    if (query.includes('birdman_container_cpu_seconds_total')) {
      return matrix([
        { labels, points: ts.map((t) => [t, (0.74 + noise(t, 9) * 0.18).toFixed(3)] as [number, string]) },
      ]);
    }
    if (query.includes('birdman_container_memory_bytes')) {
      return matrix([
        {
          labels,
          points: ts.map((t) => [t, String(Math.round(486_000_000 * (1 + noise(t, 12) * 0.06)))] as [number, string]),
        },
      ]);
    }
  }

  return matrix([]);
}

export function vectorFor(query: string, at: number): unknown {
  const q = /histogram_quantile\(([0-9.]+)/.exec(query);
  if (q !== null) {
    // Перцентили time-to-match: те же числа, что и в /v1/stats/overview,
    // иначе Fill-rate спорил бы сам с собой на одном экране.
    const value = Number(q[1]) >= 0.9 ? 21.8 : 7.4;
    return vector([{ labels: {}, at, value }]);
  }
  return vector([]);
}
