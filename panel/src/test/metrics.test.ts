import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../lib/api';
import { formatMetric, parseMatrix, queryRange, serverMetricQueries, toAlignedData } from '../lib/metrics';

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
