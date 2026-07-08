// Типизированный клиент публичного REST API master (docs/specs/master.md §6).
// Панель — обычный клиент API, без приватных лазеек (принцип 1 спеки панели).
// Auth — сессионная cookie (см. session.tsx); каждый не-GET несёт CSRF-заголовок.

export type Scope = 'admin' | 'deploy' | 'matchmaking' | 'allocate' | 'readonly';

export interface SessionInfo {
  scopes: Scope[];
  name: string;
}

export interface NodeInfo {
  id: string;
  project: string;
  region: string;
  hostname: string;
  public_ip: string;
  capacity_slots: number;
  agent_version: string;
  state: 'active' | 'draining' | 'quarantine' | 'dead';
  last_heartbeat_at?: string;
  created_at: string;
}

export interface GameServer {
  id: string;
  node_id: string;
  version_id: string;
  region: string;
  state: 'creating' | 'ready' | 'allocated' | 'draining' | 'failed' | 'reaped';
  port: number;
  players: number;
  tick_ms?: number;
  match_id?: string;
  created_at: string;
  updated_at: string;
}

export type VersionState = 'registered' | 'prepulling' | 'active' | 'deprecated' | 'disabled';

export interface VersionInfo {
  id: string;
  project: string;
  semver: string;
  image_ref: string;
  channel: string;
  /** registered | prepulling | active | deprecated | disabled (master store/deploy.go). */
  state: VersionState | string;
  created_at: string;
  /** Проставляется при демоте active → deprecated; отсчёт reap_ttl_min от неё. */
  deprecated_at?: string;
}

/** Ответ POST /v1/deploy: {deploy: Status} (deploy.Status в master). */
export interface DeployStatus {
  version: VersionInfo;
  /** Состояние после вызова: prepulling | active. */
  state: string;
  /** Сколько тачек ещё греется (0, когда флип уже произошёл). */
  pending_nodes: number;
}

/** Ответ POST /v1/rollback: {rollback: {...}}. */
export interface RollbackResult {
  version: VersionInfo;
  regions: string[];
  old_semver: string;
}

export type MatchState = 'pending' | 'running' | 'finished' | 'aborted';

export interface Match {
  id: string;
  project: string;
  region: string;
  state: MatchState;
  version_id: string;
  semver: string;
  server_id: string;
  host: string;
  port: number;
  server_players: number;
  players_peak: number;
  started_at?: string;
  ended_at?: string;
  created_at: string;
}

export interface ApiEvent {
  id: number;
  ts: string;
  kind: string;
  node_id?: string;
  server_id?: string;
  match_id?: string;
  version_id?: string;
  payload: Record<string, unknown>;
}

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly code: string,
    public readonly detail?: string,
  ) {
    super(detail ? `${code}: ${detail}` : code);
    this.name = 'ApiError';
  }
}

/** Собирает query string, пропуская пустые значения. */
export function qs(params: Record<string, string | number | undefined>): string {
  const parts: string[] = [];
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === '') continue;
    parts.push(`${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`);
  }
  return parts.length > 0 ? `?${parts.join('&')}` : '';
}

async function request<T>(method: 'GET' | 'POST' | 'DELETE', path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  // SameSite=Lax + кастомный заголовок — CSRF-защита v0 (master session.go).
  if (method !== 'GET') headers['X-Birdman-Csrf'] = '1';

  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  const text = await res.text();
  let data: unknown;
  if (text !== '') {
    try {
      data = JSON.parse(text);
    } catch {
      throw new ApiError(res.status, 'bad_response', text.slice(0, 160));
    }
  }
  if (!res.ok) {
    const e = data as { error?: string; detail?: string } | undefined;
    throw new ApiError(res.status, e?.error ?? `http_${res.status}`, e?.detail);
  }
  return data as T;
}

// type, не interface: у type-алиасов есть неявная индекс-сигнатура для qs().
export type MatchFilter = {
  state?: MatchState;
  region?: string;
  project?: string;
  limit?: number;
  offset?: number;
};

export const api = {
  // Сессия (login/logout панели).
  createSession: (apiKey: string) => request<SessionInfo>('POST', '/v1/session', { api_key: apiKey }),
  getSession: () => request<SessionInfo>('GET', '/v1/session'),
  deleteSession: () => request<void>('DELETE', '/v1/session'),

  // Read-only списки П0.
  listNodes: () => request<{ nodes: NodeInfo[] }>('GET', '/v1/nodes').then((r) => r.nodes),
  listServers: (f: { state?: string; region?: string; project?: string } = {}) =>
    request<{ servers: GameServer[] }>('GET', `/v1/servers${qs(f)}`).then((r) => r.servers),
  listVersions: () => request<{ versions: VersionInfo[] }>('GET', '/v1/versions').then((r) => r.versions),
  listMatches: (f: MatchFilter = {}) =>
    request<{ matches: Match[] }>('GET', `/v1/matches${qs(f)}`).then((r) => r.matches),
  getMatch: (id: string) =>
    request<{ match: Match }>('GET', `/v1/matches/${encodeURIComponent(id)}`).then((r) => r.match),
  listEvents: (limit = 50) =>
    request<{ events: ApiEvent[] }>('GET', `/v1/events${qs({ limit })}`).then((r) => r.events),

  // --- П1: операции (скоуп deploy/admin; кнопки скрыты у readonly) ---

  /** Мягкий деплой версии: 202 prepulling / 200 active (master §5). */
  deploy: (versionId: string) =>
    request<{ deploy: DeployStatus }>('POST', '/v1/deploy', { version_id: versionId }).then((r) => r.deploy),
  /** Откат: deprecated ↔ active за секунды. project опускаем при единственном. */
  rollback: (body: { project?: string; region?: string } = {}) =>
    request<{ rollback: RollbackResult }>('POST', '/v1/rollback', body).then((r) => r.rollback),
  /** Вывод тачки из ротации (admin). */
  drainNode: (id: string) =>
    request<{ node: NodeInfo }>('POST', `/v1/nodes/${encodeURIComponent(id)}/drain`).then((r) => r.node),
  undrainNode: (id: string) =>
    request<{ node: NodeInfo }>('POST', `/v1/nodes/${encodeURIComponent(id)}/undrain`).then((r) => r.node),
};

/**
 * URL стрима логов дедика (chunked text/plain). follow=1 — live tail; без
 * follow — конечный ответ для скачивания. Работает и для reaped/failed.
 */
export function serverLogsPath(id: string, opts: { follow?: boolean; tail?: number } = {}): string {
  return `/v1/servers/${encodeURIComponent(id)}/logs${qs({
    follow: opts.follow ? 1 : undefined,
    tail: opts.tail,
  })}`;
}
