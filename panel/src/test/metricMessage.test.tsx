// Графики метрик не показывают текст мастера (tracker #996).
//
// До этой карточки `MetricMessage` подставлял жёсткую ошибку как
// `t('metric.error', {error})`, где `error` — `ApiError.message`, то есть
// `${code}: ${detail}`, собранный из тела мастера (`lib/api.ts`) или VM
// (`lib/metrics.ts`). Сообщение ВСЕГДА английское, поэтому в русском интерфейсе
// на графике появлялась строка вида `forbidden: key is bound to game/dev: raw
// query proxy is global-key only` — нарушение правила «панель 100% EN+RU, без
// сквозных строк мастера» (panel.md §1 п.6).
//
// #1000 закрыл только 403-ветку (свой статус `forbidden`); ВСЕ прочие коды
// продолжали ехать сырыми. Здесь держатся обе стороны:
//  * ни при одном коде текст мастера не доходит до DOM — причём не «мы не
//    забыли обернуть», а структурно: канал сузили до кода (см. ниже тест на
//    useQueryRange, который проверяет это на настоящем ответе мастера);
//  * новый текст НЕ растёкся на осмысленные состояния — «метрики не настроены»,
//    «VM недоступна», «нет данных» и честный 403 остались собой.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, render, renderHook, screen } from '@testing-library/react';
import type { ReactElement, ReactNode } from 'react';
import type { SessionInfo } from '../lib/api';
import { I18nProvider } from '../lib/i18n';
import { SessionContext } from '../lib/session';
import { useQueryRange } from '../lib/useMetrics';
import { MetricMessage } from '../components/MetricMessage';
import { MetricChart } from '../components/MetricChart';

/** Привязанный readonly-ключ: скоуп есть, отказ приходит по привязке (#1000). */
const BOUND: SessionInfo = { scopes: ['readonly'], name: 'ro-bound', binding: { project: 'game', env: 'dev' } };
/** Непривязанный: поле binding master не присылает вовсе (additive). */
const UNBOUND: SessionInfo = { scopes: ['readonly'], name: 'ro-global' };

/** Проза, которую мастер кладёт в `detail` 403 по привязке (`deploy.go`,
 *  `metrics.go`: «key is bound to …: raw query proxy is global-key only»). */
const MASTER_PROSE = 'key is bound to game/dev: raw query proxy is global-key only';
/** Проза VictoriaMetrics в теле `{"status":"error","error":…}`. */
const VM_PROSE = 'parse error: unexpected character after top-level expression';

function renderAs(ui: ReactElement, session: SessionInfo | null, lang: 'en' | 'ru' = 'en') {
  const value = { session, login: async () => {}, logout: async () => {}, invalidate: () => {} };
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <I18nProvider initialLang={lang}>
        <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
      </I18nProvider>
    );
  }
  return render(<Wrapper>{ui}</Wrapper>);
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('MetricMessage: жёсткая ошибка объясняется каталогом, а не телом мастера', () => {
  it('RU: неизвестный код → русский текст и КОД, без английской прозы', () => {
    renderAs(<MetricMessage status="error" hasData={false} errorCode="internal" height={120} />, UNBOUND, 'ru');
    expect(screen.getByText('Метрики недоступны (ошибка internal).')).toBeTruthy();
    // Ни куска сообщения мастера: до #996 здесь стояло `internal: <проза>`.
    expect(document.body.textContent).not.toContain(MASTER_PROSE);
    expect(document.body.textContent).not.toContain('unavailable');
  });

  it('EN: тот же код → английский текст того же смысла (паритет каталогов)', () => {
    renderAs(<MetricMessage status="error" hasData={false} errorCode="internal" height={120} />, UNBOUND, 'en');
    expect(screen.getByText('Metrics unavailable (error internal).')).toBeTruthy();
  });

  it('код VM (bad_data) виден оператору — за него цепляется баг-репорт', () => {
    renderAs(<MetricMessage status="error" hasData={false} errorCode="bad_data" height={120} />, UNBOUND, 'ru');
    expect(screen.getByText(/bad_data/)).toBeTruthy();
    expect(document.body.textContent).not.toContain(VM_PROSE);
  });

  it('код с именем из Object.prototype не даёт ПУСТОГО графика (tracker #1021)', () => {
    // `constructor` проходит санитайзер (/^[a-z][a-z0-9_]{0,39}$/), а
    // CODE_MESSAGE['constructor'] достаёт через прототип функцию Object:
    // проверка `!== undefined` истинна, t() получает не MessageKey, и на
    // графике не остаётся НИЧЕГО. Лечится Object.hasOwn.
    renderAs(<MetricMessage status="error" hasData={false} errorCode="constructor" height={120} />, UNBOUND, 'ru');
    expect(screen.getByText(/Метрики недоступны \(ошибка constructor\)/)).toBeTruthy();
  });

  it('401 unauthorized → свой текст про истёкшую сессию, а не общий «ошибка …»', () => {
    renderAs(<MetricMessage status="error" hasData={false} errorCode="unauthorized" height={120} />, UNBOUND, 'ru');
    expect(screen.getByText('Сессия истекла — войдите заново, чтобы видеть метрики.')).toBeTruthy();
    // Иначе оператор читал бы `unauthorized: missing or invalid API key`
    // (master auth.go) и гадал, при чём тут ключ, если он вошёл.
    expect(document.body.textContent).not.toContain('unauthorized');
    expect(document.body.textContent).not.toContain('missing or invalid API key');
  });

  it('401 по-английски — тот же смысл, ключ есть в обоих каталогах', () => {
    renderAs(<MetricMessage status="error" hasData={false} errorCode="unauthorized" height={120} />, UNBOUND, 'en');
    expect(screen.getByText('The session has expired — sign in again to see metrics.')).toBeTruthy();
  });

  it('запрос не дошёл (кода нет вовсе) → «не удалось связаться с master», а не пустота', () => {
    renderAs(<MetricMessage status="error" hasData={false} errorCode={null} height={120} />, UNBOUND, 'ru');
    expect(screen.getByText('Не удалось связаться с master — проверьте соединение.')).toBeTruthy();
  });

  it('кода не передали вовсе (значение по умолчанию) → тот же текст, не «(ошибка undefined)»', () => {
    renderAs(<MetricMessage status="error" hasData={false} height={120} />, UNBOUND, 'ru');
    expect(screen.getByText('Не удалось связаться с master — проверьте соединение.')).toBeTruthy();
    expect(document.body.textContent).not.toContain('undefined');
  });
});

describe('MetricMessage: новый текст не подменил собой осмысленные состояния', () => {
  // Вторая сторона мутации: легко «починить» карточку, залив общим текстом
  // ошибки всё подряд. Тогда оператор перестал бы отличать «VM не настроена на
  // этом master» (чинится конфигом) от «VM лежит» (чинится перезапуском) и от
  // «нет данных за период» (не поломка вовсе).
  const CASES: { status: 'unconfigured' | 'unreachable'; ru: string }[] = [
    { status: 'unconfigured', ru: 'Метрики не настроены на этом master (victoriametrics_url пуст).' },
    { status: 'unreachable', ru: 'VictoriaMetrics недоступна — данных сейчас нет.' },
  ];

  for (const c of CASES) {
    it(`${c.status} → прежний собственный текст, без кода ошибки`, () => {
      renderAs(<MetricMessage status={c.status} hasData={false} errorCode="internal" height={120} />, UNBOUND, 'ru');
      expect(screen.getByText(c.ru)).toBeTruthy();
      expect(document.body.textContent).not.toContain('internal');
    });
  }

  it('ok без точек → «нет данных за период», а не ошибка', () => {
    renderAs(<MetricMessage status="ok" hasData={false} errorCode="internal" height={120} />, UNBOUND, 'ru');
    expect(screen.getByText('Нет данных за выбранный период.')).toBeTruthy();
  });

  it('loading → «загрузка», код ошибки прошлого прогона не проступает', () => {
    renderAs(<MetricMessage status="loading" hasData={false} errorCode="internal" height={120} />, UNBOUND, 'ru');
    expect(screen.getByText('Загрузка метрик…')).toBeTruthy();
    expect(document.body.textContent).not.toContain('internal');
  });
});

describe('MetricMessage: 403 остаётся честным (регрессия #1000 внутри #996)', () => {
  it('привязанная сессия (RU) → называет привязку, а не «ошибка forbidden»', () => {
    renderAs(<MetricMessage status="forbidden" hasData={false} errorCode="forbidden" height={120} />, BOUND, 'ru');
    expect(screen.getByText(/ключ привязан к game\/dev/)).toBeTruthy();
    expect(document.body.textContent).not.toContain('forbidden');
    expect(document.body.textContent).not.toContain(MASTER_PROSE);
  });

  it('привязанная сессия (EN) → тот же смысл по-английски', () => {
    renderAs(<MetricMessage status="forbidden" hasData={false} errorCode="forbidden" height={120} />, BOUND, 'en');
    expect(screen.getByText(/your key is bound to game\/dev/)).toBeTruthy();
  });

  it('НЕпривязанная сессия → прежний текст про скоуп (поведение до #1000 цело)', () => {
    renderAs(<MetricMessage status="forbidden" hasData={false} errorCode="forbidden" height={120} />, UNBOUND, 'ru');
    expect(screen.getByText('Не хватает прав: нужен ключ со скоупом readonly или admin.')).toBeTruthy();
  });

  it('сессии ещё нет (грузится) → текст про скоуп, без падения на undefined', () => {
    renderAs(<MetricMessage status="forbidden" hasData={false} errorCode="forbidden" height={120} />, null, 'ru');
    expect(screen.getByText('Не хватает прав: нужен ключ со скоупом readonly или admin.')).toBeTruthy();
  });
});

describe('useQueryRange: в состояние графика уезжает КОД, а не сообщение мастера', () => {
  /** Отвечает на query_range заданным статусом и телом мастера. */
  function stub(status: number, body: unknown) {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(JSON.stringify(body), { status })));
    vi.stubGlobal('fetch', fetchMock);
    return fetchMock;
  }

  it('500 с прозой в detail → errorCode = код, проза выброшена ещё в хуке', async () => {
    stub(500, { error: 'internal', detail: 'victoriametrics: dial tcp 10.0.0.5:8428: connect: connection refused' });
    const { result } = renderHook(() => useQueryRange({ query: 'up' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(result.current.status).toBe('error');
    expect(result.current.errorCode).toBe('internal');
    // Именно это и утекало на график: `${code}: ${detail}` из ApiError.message.
    expect(JSON.stringify(result.current)).not.toContain('connection refused');
  });

  it('403 по привязке → статус forbidden, а сырой detail мастера никуда не сохраняется', async () => {
    stub(403, { error: 'forbidden', detail: MASTER_PROSE });
    const { result } = renderHook(() => useQueryRange({ query: 'up' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(result.current.status).toBe('forbidden');
    expect(JSON.stringify(result.current)).not.toContain('raw query proxy');
  });

  it('VM ответила 200 со status:error → код errorType, проза парсера отброшена', async () => {
    stub(200, { status: 'error', errorType: 'bad_data', error: VM_PROSE });
    const { result } = renderHook(() => useQueryRange({ query: 'up((' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(result.current.status).toBe('error');
    expect(result.current.errorCode).toBe('bad_data');
    expect(JSON.stringify(result.current)).not.toContain('parse error');
  });

  it('fetch отверг запрос (сети нет) → errorCode null, а не текст браузера', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))));
    const { result } = renderHook(() => useQueryRange({ query: 'up' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(result.current.status).toBe('error');
    expect(result.current.errorCode).toBeNull();
    expect(JSON.stringify(result.current)).not.toContain('Failed to fetch');
  });
});

describe('Формы отказа ДВУХ апстримов: код читается из своего поля, проза не едет', () => {
  // Найдено вторым независимым проходом, и это был НАСТОЯЩИЙ пропуск: первая
  // редакция карточки чинила только 200-со-`status:error`, а живая VM отвечает
  // на отвергнутый запрос 422/400/503. Ветка `!res.ok` в `lib/metrics.ts` стояла
  // РАНЬШЕ разбора конверта VM и читала поле `error` как код — а у VM в этом
  // поле ПРОЗА (код лежит в `errorType`). То есть английский текст доезжал до
  // графика ровно как до карточки, только через другую дверь.
  function stub(status: number, body: unknown, asText = false) {
    const payload = asText ? (body as string) : JSON.stringify(body);
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(payload, { status }))));
  }

  async function load() {
    const { result } = renderHook(() => useQueryRange({ query: 'up' }));
    await act(async () => {
      await Promise.resolve();
    });
    return result;
  }

  // Что чем запинено (важно для того, кто будет это ломать):
  //  * порядок веток в `fetchVM` пинит случай VM 503 — там `errorType` это
  //    настоящий токен, и при возврате старого порядка код становится `http_503`
  //    вместо `internal`, то есть тест краснеет;
  //  * случай VM 422 порядок НЕ пинит: `errorType` там сам не токен ("422"), и
  //    санитайзер даёт `http_422` при обоих порядках. Он пинит другое — что
  //    наружу вообще не выходит предложение;
  //  * «ноль прозы в DOM» держится ОБОИМИ слоями сразу, поэтому свойство
  //    переживает регрессию любого одного из них. Это и есть смысл второго слоя.
  it('VM 422 на кривой запрос (её штатная форма) → код из errorType, не проза из error', async () => {
    stub(422, { status: 'error', errorType: '422', error: 'cannot parse query: unexpected token "((" ; expecting label filter' });
    const result = await load();
    expect(result.current.status).toBe('error');
    // errorType здесь не токен ("422"), поэтому санитайзер даёт http_422 —
    // но в любом случае НЕ предложение.
    expect(result.current.errorCode).toBe('http_422');
    expect(JSON.stringify(result.current)).not.toContain('cannot parse query');
  });

  it('VM 503 (упёрлись в maxConcurrentRequests) → тоже токен, а не жалоба VM', async () => {
    stub(503, { status: 'error', errorType: 'internal', error: 'cannot handle more than 8 concurrent search requests' });
    const result = await load();
    expect(result.current.errorCode).toBe('internal');
    expect(JSON.stringify(result.current)).not.toContain('concurrent search requests');
  });

  it('master 500 (его форма: error=код, detail=проза) → код по-прежнему из error', async () => {
    stub(500, { error: 'internal', detail: 'victoriametrics: connection refused' });
    const result = await load();
    expect(result.current.errorCode).toBe('internal');
    expect(JSON.stringify(result.current)).not.toContain('connection refused');
  });

  it('код-не-код (кто угодно ошибся конвертом) → клампится в http_<status>', async () => {
    stub(500, { error: 'something went terribly wrong: see master logs for details' });
    const result = await load();
    expect(result.current.errorCode).toBe('http_500');
    expect(JSON.stringify(result.current)).not.toContain('terribly wrong');
  });

  it('HTML-страница шлюза на 504 → мягкий gatewayDown, а не кусок HTML кодом', async () => {
    stub(504, '<html><head><title>504 Gateway Time-out</title></head><body><center>nginx</center></body></html>', true);
    const result = await load();
    // До #996 `JSON.parse` бросал раньше проверки статуса и это выглядело как
    // жёсткая ошибка `bad_response` с куском HTML в тексте.
    //
    // Статус ИМЕННО gatewayDown, а не unreachable (tracker #1021): тело не
    // разобралось, значит отвечал ШЛЮЗ, а перед кем он стоит — перед master
    // или перед VM — по одному статусу неизвестно. Единственный nginx в
    // `infra/` стоит перед МАСТЕРОМ, так что «VictoriaMetrics недоступна»
    // здесь называла заведомо не того.
    expect(result.current.status).toBe('gatewayDown');
    expect(result.current.errorCode).toBeNull();
  });

  it('502 СОБСТВЕННЫМ JSON мастера → unreachable: тут виновник известен точно', async () => {
    // Контраст к тесту выше и причина, по которой ветки две. `ops.go` отдаёт
    // `{"error":"upstream"}` — это сказал сам master, значит не смог сходить
    // именно в VictoriaMetrics, и называть её ВЕРНО.
    stub(502, JSON.stringify({ error: 'upstream', detail: 'connection refused' }), true);
    const result = await load();
    expect(result.current.status).toBe('unreachable');
    expect(result.current.errorCode).toBeNull();
  });
});

describe('Свойство, а не перечисление: ни одна форма отказа не даёт прозы в DOM', () => {
  // Ответ на замечание второго прохода: перечислительные тесты держат ровно те
  // формы, которые перечислены. Здесь проверяется СВОЙСТВО — что бы ни ответил
  // апстрим, на графике стоит текст каталога и ни одного куска чужой прозы.
  const PROSE = [
    'cannot parse query',
    'concurrent search requests',
    'connection refused',
    'missing or invalid API key',
    'raw query proxy is global-key only',
    'Gateway Time-out',
    'nginx',
    'terribly wrong',
    'Failed to fetch',
  ];
  // Все тексты, которые панель имеет право показать на графике (RU).
  const CATALOG = [
    'Метрики недоступны (ошибка ',
    'Сессия истекла — войдите заново, чтобы видеть метрики.',
    'Не удалось связаться с master — проверьте соединение.',
    'Метрики не настроены на этом master (victoriametrics_url пуст).',
    'VictoriaMetrics недоступна — данных сейчас нет.',
    'Шлюз вернул ошибку — метрики недоступны. Проверьте, что master и VictoriaMetrics подняты.',
    'Запрос отклонён: ключ привязан к',
    'Не хватает прав: нужен ключ со скоупом readonly или admin.',
    'Нет данных за выбранный период.',
  ];

  const SHAPES: { name: string; status: number; body: string }[] = [
    { name: 'VM 422 кривой запрос', status: 422, body: JSON.stringify({ status: 'error', errorType: '422', error: 'cannot parse query: unexpected token "((" ' }) },
    { name: 'VM 503 лимит конкурентности', status: 503, body: JSON.stringify({ status: 'error', errorType: 'internal', error: 'cannot handle more than 8 concurrent search requests' }) },
    { name: 'VM 200 status:error', status: 200, body: JSON.stringify({ status: 'error', errorType: 'bad_data', error: 'parse error: unexpected "("' }) },
    { name: 'master 500', status: 500, body: JSON.stringify({ error: 'internal', detail: 'victoriametrics: connection refused' }) },
    { name: 'master 401 протухшая кука', status: 401, body: JSON.stringify({ error: 'unauthorized', detail: 'missing or invalid API key' }) },
    { name: 'master 403 привязка', status: 403, body: JSON.stringify({ error: 'forbidden', detail: MASTER_PROSE }) },
    { name: 'nginx 504 HTML', status: 504, body: '<html><title>504 Gateway Time-out</title><center>nginx</center></html>' },
    // 500 с HTML — единственная форма, где `bad_response` ещё достижим (502/504
    // уходят в мягкую ветку до разбора тела). Именно на ней второй проход
    // показал, что можно вернуть прозу в UI и оставить сьют зелёным.
    { name: 'шлюз 500 HTML (путь bad_response)', status: 500, body: '<html><title>500 Internal Server Error</title><center>nginx</center></html>' },
    { name: 'мусор вместо кода', status: 500, body: JSON.stringify({ error: 'something went terribly wrong: see logs' }) },
  ];

  for (const shape of SHAPES) {
    it(`${shape.name} → текст каталога, ноль прозы`, async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(shape.body, { status: shape.status }))));
      renderAs(<MetricChart query="birdman_players_online" title="Игроки" />, BOUND, 'ru');
      await screen.findByText((s) => CATALOG.some((c) => s.startsWith(c)));
      const dom = document.body.textContent ?? '';
      for (const p of PROSE) expect(dom).not.toContain(p);
    });
  }

  it('fetch отверг запрос (сети нет) → тоже текст каталога', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))));
    renderAs(<MetricChart query="birdman_players_online" title="Игроки" />, BOUND, 'ru');
    expect(await screen.findByText('Не удалось связаться с master — проверьте соединение.')).toBeTruthy();
    expect(document.body.textContent).not.toContain('Failed to fetch');
  });
});

describe('MetricChart целиком: настоящее тело мастера → на графике русский текст', () => {
  // Сквозной тест ровно по формулировке карточки: «в русском интерфейсе на
  // графиках метрик появляется английская строка мастера». Здесь настоящий
  // компонент графика, настоящий хук и настоящий парсер ответа — подменён
  // только fetch. Компонентные тесты выше проверяют вывод по коду, хук — что
  // проза отброшена; этот замыкает цепочку до DOM.
  it('500 с прозой в detail → «Метрики недоступны (ошибка internal).», прозы нет', async () => {
    const detail = 'victoriametrics: dial tcp 10.0.0.5:8428: connect: connection refused';
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(JSON.stringify({ error: 'internal', detail }), { status: 500 }))),
    );
    renderAs(<MetricChart query="birdman_server_players" title="Игроки" />, UNBOUND, 'ru');
    expect(await screen.findByText('Метрики недоступны (ошибка internal).')).toBeTruthy();
    expect(document.body.textContent).not.toContain('connection refused');
    expect(document.body.textContent).not.toContain('dial tcp');
  });

  it('403 по привязке на том же графике → причина по-русски, без текста мастера', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(JSON.stringify({ error: 'forbidden', detail: MASTER_PROSE }), { status: 403 }))),
    );
    renderAs(<MetricChart query="birdman_server_players" title="Игроки" />, BOUND, 'ru');
    expect(await screen.findByText(/ключ привязан к game\/dev/)).toBeTruthy();
    expect(document.body.textContent).not.toContain('raw query proxy');
  });
});
