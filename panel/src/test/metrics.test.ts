import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../lib/api';
import {
  formatMetric,
  matchesRunningQuery,
  parseMatrix,
  parseVector,
  playersOnlineQuery,
  queryInstant,
  queryRange,
  queueDepthQuery,
  serverMetricQueries,
  serversByStateQuery,
  timeToMatchQuantileQuery,
  toAlignedData,
  utilizationRatioQuery,
  utilizationSeriesModel,
} from '../lib/metrics';
import type { MetricSeries } from '../lib/metrics';

describe('parseMatrix', () => {
  it('разбирает VM matrix в серии: подписи, имя, числовые точки', () => {
    const body = {
      status: 'success',
      data: {
        resultType: 'matrix',
        result: [
          {
            metric: { __name__: 'birdman_server_players', server_id: 'srv-a' },
            values: [
              [1720000000, '3'],
              [1720000060, '5'],
            ],
          },
        ],
      },
    };
    const series = parseMatrix(body);
    expect(series).toHaveLength(1);
    expect(series[0].name).toBe('birdman_server_players');
    expect(series[0].labels.server_id).toBe('srv-a');
    expect(series[0].points).toEqual([
      [1720000000, 3],
      [1720000060, 5],
    ]);
  });

  it('нечисловые значения (NaN/+Inf) становятся null — разрыв линии', () => {
    const body = {
      data: {
        result: [{ metric: {}, values: [[1, 'NaN'], [2, '+Inf'], [3, '2.5']] }],
      },
    };
    const [s] = parseMatrix(body);
    expect(s.points).toEqual([
      [1, null],
      [2, null],
      [3, 2.5],
    ]);
  });

  it('пустой/битый ответ → []', () => {
    expect(parseMatrix(null)).toEqual([]);
    expect(parseMatrix({})).toEqual([]);
    expect(parseMatrix({ data: { result: 'nope' } })).toEqual([]);
    expect(parseMatrix({ status: 'error', error: 'boom' })).toEqual([]);
  });
});

describe('parseVector (instant-запрос histogram_quantile)', () => {
  it('разбирает VM vector: подписи + одна точка value', () => {
    const body = {
      status: 'success',
      data: {
        resultType: 'vector',
        result: [
          { metric: { __name__: 'ttm' }, value: [1720000000, '15.5'] },
          { metric: { quantile: '0.95' }, value: [1720000000, '80'] },
        ],
      },
    };
    const v = parseVector(body);
    expect(v).toHaveLength(2);
    expect(v[0]).toEqual({ labels: { __name__: 'ttm' }, name: 'ttm', value: 15.5 });
    expect(v[1].value).toBe(80);
  });

  it('NaN/+Inf (гистограмма без данных) → value=null', () => {
    const body = { data: { result: [{ metric: {}, value: [1, 'NaN'] }, { metric: {}, value: [2, '+Inf'] }] } };
    expect(parseVector(body).map((s) => s.value)).toEqual([null, null]);
  });

  it('пустой/битый ответ → []', () => {
    expect(parseVector(null)).toEqual([]);
    expect(parseVector({})).toEqual([]);
    expect(parseVector({ data: { result: [] } })).toEqual([]);
    expect(parseVector({ data: { result: [{ metric: {} }] } })).toEqual([{ labels: {}, name: '', value: null }]);
  });
});

describe('query builders (утилизация / истинный time-to-match)', () => {
  it('serversByStateQuery — сумма birdman_servers по состоянию', () => {
    expect(serversByStateQuery()).toBe('sum by (state) (birdman_servers)');
  });
  it('timeToMatchQuantileQuery — histogram_quantile по гистограмме за N дней', () => {
    expect(timeToMatchQuantileQuery(0.5, 7)).toBe(
      'histogram_quantile(0.5, sum by (le) (increase(birdman_mm_time_to_match_seconds_bucket[7d])))',
    );
    expect(timeToMatchQuantileQuery(0.95, 30)).toContain('histogram_quantile(0.95,');
    expect(timeToMatchQuantileQuery(0.95, 30)).toContain('[30d]');
  });
});

describe('live query builders (Task 3, "Статистика v1" — гранулярные окна 12ч/24ч/3д)', () => {
  it('playersOnlineQuery — общий счётчик игроков онлайн', () => {
    expect(playersOnlineQuery()).toBe('birdman_players_online');
  });
  it('matchesRunningQuery — общий счётчик матчей в игре', () => {
    expect(matchesRunningQuery()).toBe('birdman_matches_running');
  });
  it('queueDepthQuery суммирует по региону', () => {
    expect(queueDepthQuery()).toBe('sum by (region)(birdman_mm_queue_depth)');
  });
  it('utilizationRatioQuery делит allocated на capacity с защитой от нуля', () => {
    expect(utilizationRatioQuery()).toContain('birdman_node_capacity_slots');
    expect(utilizationRatioQuery()).toContain('clamp_min');
    expect(utilizationRatioQuery()).toBe(
      'sum(birdman_servers{state="allocated"}) / clamp_min(sum(birdman_node_capacity_slots), 1)',
    );
  });
});

describe('utilizationSeriesModel (query_range matrix → ряды утилизации)', () => {
  const mk = (state: string, pts: [number, number | null][]): MetricSeries => ({ labels: { state }, name: '', points: pts });

  it('маппит по состоянию в стабильном порядке (allocated→ready→draining→creating) с тонами', () => {
    const series = [
      mk('ready', [[10, 2], [20, 3]]),
      mk('allocated', [[10, 1], [20, 4]]),
    ];
    const out = utilizationSeriesModel(series);
    expect(out.map((s) => s.state)).toEqual(['allocated', 'ready']); // порядок канонический, не входной
    expect(out[0]).toMatchObject({ state: 'allocated', color: 'var(--accent)', labelKey: 'stats.util.allocated' });
    expect(out[1]).toMatchObject({ state: 'ready', color: 'var(--good)', labelKey: 'stats.util.ready' });
    expect(out[0].points).toEqual([[10, 1], [20, 4]]);
  });

  it('состояния вне «живых» (failed/reaped) отбрасываются', () => {
    const out = utilizationSeriesModel([mk('failed', [[1, 5]]), mk('reaped', [[1, 9]]), mk('draining', [[1, 1]])]);
    expect(out.map((s) => s.state)).toEqual(['draining']);
  });

  it('пусто / без точек → []', () => {
    expect(utilizationSeriesModel([])).toEqual([]);
    expect(utilizationSeriesModel([mk('allocated', [])])).toEqual([]);
  });
});

describe('toAlignedData', () => {
  it('объединяет оси времени серий, дырки заполняет null', () => {
    const series = [
      { labels: {}, name: 'a', points: [[10, 1] as [number, number], [20, 2] as [number, number]] },
      { labels: {}, name: 'b', points: [[20, 9] as [number, number], [30, 8] as [number, number]] },
    ];
    const { x, ys } = toAlignedData(series);
    expect(x).toEqual([10, 20, 30]);
    expect(ys[0]).toEqual([1, 2, null]);
    expect(ys[1]).toEqual([null, 9, 8]);
  });

  it('нет серий → пустые оси', () => {
    expect(toAlignedData([])).toEqual({ x: [], ys: [] });
  });
});

describe('formatMetric', () => {
  it('null → тире', () => {
    expect(formatMetric(null, 'int')).toBe('—');
  });
  it('байты в человекочитаемые единицы', () => {
    expect(formatMetric(1048576, 'bytes')).toBe('1.0 MiB');
    expect(formatMetric(512, 'bytes')).toBe('512 B');
  });
  it('ядра — два знака, tick — компактно', () => {
    expect(formatMetric(0.5, 'cores')).toBe('0.50');
    expect(formatMetric(16.5, 'ms')).toBe('16.5');
    expect(formatMetric(120, 'ms')).toBe('120');
  });
  it('percent (Task 3, ambiguity resolution #1) — доля 0..1 в проценты, округление до целого', () => {
    expect(formatMetric(0.5, 'percent')).toBe('50%');
    expect(formatMetric(0.333, 'percent')).toBe('33%');
    expect(formatMetric(1, 'percent')).toBe('100%');
    expect(formatMetric(null, 'percent')).toBe('—');
  });
});

describe('serverMetricQueries', () => {
  it('строит PromQL по server_id для players/tick/cpu/mem', () => {
    const qs = serverMetricQueries('srv-x');
    const byKey = Object.fromEntries(qs.map((q) => [q.key, q.expr]));
    expect(byKey.players).toBe('birdman_server_players{server_id="srv-x"}');
    expect(byKey.tick).toBe('birdman_server_tick_ms{server_id="srv-x"}');
    expect(byKey.cpu).toContain('rate(birdman_container_cpu_seconds_total{server_id="srv-x"}[1m])');
    expect(byKey.mem).toBe('birdman_container_memory_bytes{server_id="srv-x"}');
  });
});

describe('queryRange (metrics-proxy)', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('строит URL с query/start/end/step и разбирает серии', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ status: 'success', data: { result: [{ metric: {}, values: [[1, '2']] }] } }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    vi.stubGlobal('fetch', fetchMock);
    const res = await queryRange({ query: 'up{server_id="s"}', start: 100, end: 200, step: 15 });
    expect(res.kind).toBe('ok');
    if (res.kind === 'ok') expect(res.series[0].points).toEqual([[1, 2]]);
    const url = (fetchMock.mock.calls[0] as [string])[0];
    expect(url).toContain('/v1/metrics/query_range?');
    expect(url).toContain('query=up%7Bserver_id%3D%22s%22%7D');
    expect(url).toContain('start=100');
    expect(url).toContain('end=200');
    expect(url).toContain('step=15s');
  });

  it('VM не настроена (503 metrics_unconfigured) → unavailable/unconfigured, не бросает', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: 'metrics_unconfigured', detail: 'victoriametrics_url is not set' }), {
          status: 503,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    );
    const res = await queryRange({ query: 'up', start: 1, end: 2, step: 15 });
    expect(res).toEqual({ kind: 'unavailable', reason: 'unconfigured' });
  });

  it('VM настроена, но лежит (502 upstream, как в dev-compose) → unavailable/upstream, не бросает', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: 'upstream', detail: 'connection refused' }), {
          status: 502,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    );
    const res = await queryRange({ query: 'up', start: 1, end: 2, step: 15 });
    expect(res).toEqual({ kind: 'unavailable', reason: 'upstream' });
  });

  it('VM вернула status:error → ApiError', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ status: 'error', errorType: 'bad_data', error: 'parse error' }), {
          status: 422,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    );
    const err = await queryRange({ query: 'bad(', start: 1, end: 2, step: 15 }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
  });
});

describe('queryInstant (metrics-proxy /v1/metrics/query)', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('строит URL query/time и разбирает вектор перцентиля', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ status: 'success', data: { resultType: 'vector', result: [{ metric: {}, value: [100, '3.5'] }] } }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    vi.stubGlobal('fetch', fetchMock);
    const res = await queryInstant({ query: timeToMatchQuantileQuery(0.95, 7), time: 100 });
    expect(res.kind).toBe('ok');
    if (res.kind === 'ok') expect(res.vector[0].value).toBe(3.5);
    const url = (fetchMock.mock.calls[0] as [string])[0];
    expect(url).toContain('/v1/metrics/query?');
    expect(url).toContain('query=histogram_quantile');
    expect(url).toContain('time=100');
    expect(url).not.toContain('query_range');
  });

  it('VM не настроена (503) → unavailable/unconfigured, не бросает', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: 'metrics_unconfigured' }), { status: 503, headers: { 'Content-Type': 'application/json' } }),
      ),
    );
    expect(await queryInstant({ query: 'x' })).toEqual({ kind: 'unavailable', reason: 'unconfigured' });
  });

  it('битый PromQL (status:error) → ApiError', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ status: 'error', errorType: 'bad_data', error: 'boom' }), {
          status: 422,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    );
    const err = await queryInstant({ query: 'bad(' }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
  });
});
