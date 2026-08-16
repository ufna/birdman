// Демо-роутер: подменяет window.fetch и отвечает на публичный REST панели
// сгенерированным флотом. Живёт ТОЛЬКО во второй точке входа (demo.html) —
// в прод-бандл (index.html) не входит, см. гейт маркера в panel/build.sh.
//
// Флот собирается ОДИН раз на страницу: панель поллит и переспрашивает по SSE,
// и пересборка на каждый запрос заставила бы цифры дрожать под курсором.
import type { NodeInfo, SessionInfo } from '../lib/api';
import {
  alertHistory,
  alertMutes,
  alertRules,
  activeAlerts,
  apiKeys,
  backupRuns,
  backupSettings,
  registries,
  statsCost,
  statsOverview,
} from './admin';
import { buildFleet, PROJECT } from './fleet';
import type { Fleet } from './fleet';
import { DEMO_MARKER } from './marker';
import { matrixFor, vectorFor } from './series';
import { streamResponse } from './stream';

/** Ответ-JSON с проставленным Content-Type (панель читает text() → JSON.parse). */
export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const SESSION: SessionInfo = { name: `demo-admin (${DEMO_MARKER})`, scopes: ['admin'] };

let fleetCache: Fleet | null = null;
function fleet(): Fleet {
  fleetCache ??= buildFleet(Date.now());
  return fleetCache;
}

/** Метод запроса; панель шлёт его в init, GET — по умолчанию. */
function methodOf(input: RequestInfo | URL, init?: RequestInit): string {
  if (init?.method !== undefined) return init.method.toUpperCase();
  if (typeof input !== 'string' && !(input instanceof URL)) return input.method.toUpperCase();
  return 'GET';
}

function urlOf(input: RequestInfo | URL): URL {
  const raw = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
  return new URL(raw, 'http://demo.invalid');
}

/** Сужение по проекту: чужой проект — пусто, «все проекты» — всё. */
function keepProject<T>(items: T[], want: string | null, of: (x: T) => string): T[] {
  return want === null ? items : items.filter((x) => of(x) === want);
}

/** Сужение по окружению; `null` — «все». */
function keepEnv<T>(items: T[], want: string | null, of: (x: T) => string | undefined): T[] {
  return want === null ? items : items.filter((x) => of(x) === want);
}

function num(q: URLSearchParams, key: string, fallback: number): number {
  const raw = q.get(key);
  const v = raw === null ? NaN : Number(raw);
  return Number.isFinite(v) ? v : fallback;
}

/** GET-ручки. Возвращает `undefined`, если путь не наш. */
function handleGet(path: string, q: URLSearchParams, f: Fleet, now: number): unknown | undefined {
  const project = q.get('project');
  const env = q.get('env');

  switch (path) {
    case '/v1/session':
      return SESSION;
    case '/v1/ca':
      return { pem: '-----BEGIN CERTIFICATE-----\ndemo\n-----END CERTIFICATE-----\n' };
    case '/v1/nodes':
      return { nodes: keepEnv(keepProject(f.nodes, project, (n) => n.project), env, (n) => n.env) };
    case '/v1/servers': {
      let servers = keepProject(f.servers, project, () => PROJECT);
      const state = q.get('state');
      const region = q.get('region');
      if (state !== null) servers = servers.filter((s) => s.state === state);
      if (region !== null) servers = servers.filter((s) => s.region === region);
      return { servers };
    }
    case '/v1/versions':
      return { versions: keepEnv(keepProject(f.versions, project, (v) => v.project), env, (v) => v.env) };
    case '/v1/matches': {
      let matches = keepProject(f.matches, project, (m) => m.project);
      const state = q.get('state');
      const region = q.get('region');
      if (state !== null) matches = matches.filter((m) => m.state === state);
      if (region !== null) matches = matches.filter((m) => m.region === region);
      const offset = num(q, 'offset', 0);
      return { matches: matches.slice(offset, offset + num(q, 'limit', 100)) };
    }
    case '/v1/events': {
      const events = keepProject(f.events, project, (e) => e.project ?? '');
      return { events: events.slice(0, num(q, 'limit', 50)) };
    }
    case '/v1/projects':
      return { projects: f.projects };
    case '/v1/environments':
      return { environments: keepProject(f.environments, project, (e) => e.project) };
    case '/v1/stats/overview':
      return statsOverview(num(q, 'days', 7), now);
    case '/v1/stats/cost':
      return statsCost(num(q, 'days', 7), now, f);
    case '/v1/alerts/rules':
      return { rules: alertRules() };
    case '/v1/alerts/active':
      return { alerts: activeAlerts(now) };
    case '/v1/alerts/history':
      return { alerts: alertHistory(now).slice(0, num(q, 'limit', 100)) };
    case '/v1/alerts/mutes':
      return { mutes: alertMutes(now) };
    case '/v1/apikeys':
      return { apikeys: apiKeys(now) };
    case '/v1/registries':
      return { registries: registries(now) };
    case '/v1/backups/settings':
      return { settings: backupSettings(now) };
    case '/v1/backups/runs':
      return { runs: backupRuns(now).slice(0, num(q, 'limit', 20)) };
    default:
      break;
  }

  const match = /^\/v1\/matches\/([^/]+)$/.exec(path);
  if (match !== null) {
    const m = f.matches.find((x) => x.id === match[1]);
    return m === undefined ? undefined : { match: m };
  }
  const projectUsage = /^\/v1\/projects\/([^/]+)\/usage$/.exec(path);
  if (projectUsage !== null) {
    const slug = decodeURIComponent(projectUsage[1]);
    const nodes = f.nodes.filter((n) => n.project === slug);
    return {
      usage: {
        environments: f.environments.filter((e) => e.project === slug).length,
        versions: f.versions.filter((v) => v.project === slug).length,
        fleets: nodes.length === 0 ? 0 : 3,
        nodes: nodes.filter((n) => n.state !== 'dead').length,
        retired_nodes: nodes.filter((n) => n.state === 'dead').length,
        servers: slug === PROJECT ? f.servers.length : 0,
        matches: f.matches.filter((m) => m.project === slug).length,
        api_keys: slug === PROJECT ? 1 : 0,
      },
    };
  }
  const envUsage = /^\/v1\/environments\/([^/]+)\/([^/]+)\/usage$/.exec(path);
  if (envUsage !== null) {
    const slug = decodeURIComponent(envUsage[1]);
    const name = decodeURIComponent(envUsage[2]);
    const versions = f.versions.filter((v) => v.project === slug && v.env === name);
    const ids = new Set(versions.map((v) => v.id));
    return {
      usage: {
        versions: versions.length,
        fleets: versions.length === 0 ? 0 : 3,
        nodes: f.nodes.filter((n) => n.project === slug && n.env === name).length,
        servers: f.servers.filter((s) => ids.has(s.version_id)).length,
        matches: f.matches.filter((m) => ids.has(m.version_id)).length,
        api_keys: name === 'prod' && slug === PROJECT ? 1 : 0,
      },
    };
  }
  return undefined;
}

/** Мутации: правдоподобный 200 той же формы, что у master. */
function handleMutation(method: string, path: string, f: Fleet, now: number): unknown | undefined {
  const setNodeState = (id: string, state: NodeInfo['state']): unknown => {
    const node = f.nodes.find((n) => n.id === id);
    if (node === undefined) return undefined;
    node.state = state;
    return { node };
  };

  if (method === 'DELETE' && path === '/v1/session') return {};
  if (method === 'POST' && path === '/v1/deploy') {
    const version = f.versions.find((v) => v.semver === '1.15.0');
    return { deploy: { version, state: 'active', pending_nodes: 0 } };
  }
  if (method === 'POST' && path === '/v1/rollback') {
    const version = f.versions.find((v) => v.semver === '1.14.3');
    return { rollback: { version, regions: ['ap-southeast', 'eu-central', 'us-east'], old_semver: '1.15.0' } };
  }
  if (method === 'POST' && path === '/v1/promote') {
    const version = f.versions.find((v) => v.semver === '1.15.1');
    return { version, deploy: { version, state: 'prepulling', pending_nodes: 4 } };
  }
  const nodeAction = /^\/v1\/nodes\/([^/]+)\/(drain|undrain|revoke)$/.exec(path);
  if (method === 'POST' && nodeAction !== null) {
    const state = nodeAction[2] === 'drain' ? 'draining' : nodeAction[2] === 'undrain' ? 'active' : 'dead';
    return setNodeState(nodeAction[1], state);
  }
  if (method === 'POST' && path === '/v1/backups/run') return { started: true };
  if (method === 'POST' && path === '/v1/backups/s3/test') return { ok: true };
  if (method === 'PATCH' && path === '/v1/backups/settings') return { settings: backupSettings(now) };
  if (method === 'POST' && path === '/v1/alerts/mutes') return { mute: alertMutes(now)[0] };
  if (method === 'DELETE' && /^\/v1\/alerts\/mutes\//.test(path)) return {};
  if (method === 'POST' && path === '/v1/apikeys') {
    return { key: apiKeys(now)[0], secret: `bmk_demo_${DEMO_MARKER}` };
  }
  if (method === 'DELETE' && /^\/v1\/apikeys\//.test(path)) return { key: apiKeys(now)[3] };
  if ((method === 'POST' || method === 'PATCH') && /^\/v1\/registries/.test(path)) {
    return { registry: registries(now)[0] };
  }
  if (method === 'DELETE' && /^\/v1\/registries\//.test(path)) return {};
  return undefined;
}

export async function demoFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  const url = urlOf(input);
  const path = url.pathname;
  const method = methodOf(input, init);
  const now = Date.now();
  const f = fleet();

  if (method === 'GET') {
    if (path === '/v1/events/stream') return streamResponse(f, init?.signal ?? undefined);
    if (path === '/v1/metrics/query_range') {
      const q = url.searchParams;
      const nowSec = Math.floor(now / 1000);
      return jsonResponse(
        matrixFor(
          q.get('query') ?? '',
          num(q, 'start', nowSec - 3600),
          num(q, 'end', nowSec),
          // step приходит строкой вида "300s" — секунды нужны числом.
          Number.parseInt(q.get('step') ?? '60', 10) || 60,
        ),
      );
    }
    if (path === '/v1/metrics/query') {
      const q = url.searchParams;
      return jsonResponse(vectorFor(q.get('query') ?? '', num(q, 'time', Math.floor(now / 1000))));
    }
    const body = handleGet(path, url.searchParams, f, now);
    if (body !== undefined) return jsonResponse(body);
  } else {
    const body = handleMutation(method, path, f, now);
    if (body !== undefined) return jsonResponse(body);
  }

  console.error(`demo: необслуженная ручка ${method} ${path} — экран останется пустым`);
  return jsonResponse({ error: 'demo_unhandled', detail: path }, 500);
}

/** Ставит демо-fetch на window. Вызывается ДО бутстрапа приложения. */
export function installDemoFetch(): void {
  window.fetch = demoFetch as typeof window.fetch;
}
