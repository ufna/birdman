// Типизированный клиент публичного REST API master (docs/specs/master.md §6).
// Панель — обычный клиент API, без приватных лазеек (принцип 1 спеки панели).
// Auth — сессионная cookie (см. session.tsx); каждый не-GET несёт CSRF-заголовок.

export type Scope = 'admin' | 'deploy' | 'matchmaking' | 'allocate' | 'readonly';

export interface SessionInfo {
  scopes: Scope[];
  name: string;
  /**
   * Привязка ключа, которым вошли (master §6 «Сессия сообщает привязку ключа»,
   * tracker #1000). Отсутствует у глобального/admin-ключа — поле additive, у
   * master'а до #1000 его нет вовсе, поэтому проверять надо наличие, а не
   * пустоту. Панель по нему называет ЧЕСТНУЮ причину 403: привязанный ключ
   * гейтится не скоупом (readonly у него есть), а привязкой.
   *
   * Тип — тот же `KeyBinding` (объявлен ниже, рядом с `ApiKey`), что и у
   * создания ключа: одна сущность — один тип. Форма записи при этом РАЗНАЯ:
   * `/v1/apikeys` кладёт `project`/`env` плоско в тело ключа, сессия — во
   * вложенное `binding`. См. `docs/specs/master.md`, абзац «Сессия сообщает
   * привязку ключа», там эта цена названа.
   */
  binding?: KeyBinding;
}

export interface NodeInfo {
  id: string;
  project: string;
  region: string;
  /** Окружение ноды (environments v1); новые ноды входят как dev (master models.go). */
  env: string;
  hostname: string;
  public_ip: string;
  capacity_slots: number;
  agent_version: string;
  state: 'active' | 'draining' | 'quarantine' | 'down' | 'dead';
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
  /** Окружение версии (environments v1); заменило прежний лейбл channel
   *  (master store/models.go Version.Env). Скоуп active/deprecated — per (project, env). */
  env: string;
  /** registered | prepulling | active | deprecated | disabled (master store/deploy.go). */
  state: VersionState | string;
  created_at: string;
  /** Проставляется при демоте active → deprecated; отсчёт reap_ttl_min от неё. */
  deprecated_at?: string;
  /** Provenance: id исходной версии, если версия создана промоутом dev→prod
   *  (environments v1 §4). Отсутствует у обычной регистрации. */
  promoted_from?: string;
}

/** Проект (мультипроект W1): верхнее измерение платформы — всё (ноды, версии,
 *  флоты, окружения, матчи) принадлежит ровно одному проекту. GET /v1/projects
 *  (readonly) отдаёт их старейшим первым; правка match_size — admin (PUT). */
export interface ProjectInfo {
  id: string;
  slug: string;
  match_size: number;
  created_at: string;
}

/** Окружение проекта (environments v1 §2): измерение платформы. Ведёт поведение
 *  флаг `production` (bool), не имя: production=true запрещает auto_deploy
 *  (guardrail в БД+API) и снимает лимит ретеншна. GET /v1/environments (readonly);
 *  create/patch/delete — admin. */
export interface Environment {
  project: string;
  name: string;
  production: boolean;
  auto_deploy: boolean;
  retention_keep: number;
  created_at: string;
}

/** Тело POST /v1/environments. production&&auto_deploy → 400; имена all/global → 400. */
export interface EnvironmentInput {
  project: string;
  name: string;
  production?: boolean;
  auto_deploy?: boolean;
  retention_keep?: number;
}

/** Тело PATCH /v1/environments/{project}/{name}: только изменённые поля; имя
 *  иммутабельно. Включение auto_deploy при production=true → 400 (guardrail). */
export interface EnvironmentPatch {
  production?: boolean;
  auto_deploy?: boolean;
  retention_keep?: number;
}

/** Состав окружения (GET /v1/environments/{p}/{n}/usage): что удаление снесёт.
 *  `nodes` — блокирующее: ноду нельзя осиротить, её сначала переводят в другое
 *  окружение. `api_keys` — живые привязанные ключи: их каскад отзовёт. */
export interface EnvironmentUsage {
  versions: number;
  fleets: number;
  nodes: number;
  servers: number;
  matches: number;
  api_keys: number;
}

/** Состав проекта (GET /v1/projects/{slug}/usage). `nodes` считает только живые:
 *  выведенные каскадятся вместе с проектом и удалению не мешают. */
export interface ProjectUsage {
  environments: number;
  versions: number;
  fleets: number;
  /** Живые ноды — блокируют удаление. */
  nodes: number;
  /** Выведенные (dead) — удалению не мешают, но делают проект непустым:
   *  их история уезжает вместе с ним, поэтому спрашивается подтверждение. */
  retired_nodes: number;
  servers: number;
  matches: number;
  api_keys: number;
}

/** Тело ответа 200 у DELETE непустого проекта — что именно снёс каскад. */
export interface ProjectDeleted {
  slug: string;
  environments: number;
  versions: number;
  fleets: number;
  matches: number;
  servers: number;
  api_keys_revoked: number;
}

/** Тело ответа 200 у DELETE непустого окружения — что именно снёс каскад. */
export interface EnvironmentDeleted {
  name: string;
  production: boolean;
  versions: number;
  fleets: number;
  matches: number;
  servers: number;
  api_keys_revoked: number;
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
  /** Слаг владельца события (эпик #968). Пусто/нет — платформенное событие
   *  (бекапы, CA, сессии панели): оно принадлежит установке, а не проекту. */
  project?: string;
  node_id?: string;
  server_id?: string;
  match_id?: string;
  version_id?: string;
  payload: Record<string, unknown>;
}

// --- П2: статистика / cost (GET /v1/stats/*, master httpapi/stats.go) ---
// Все ряды UTC, побито по дням и зеро-филлено (пустые дни = 0, не пропуски);
// у каждого ряда явный unit.

/** Точка простого ряда {дата, значение}. */
export interface StatPoint {
  date: string; // YYYY-MM-DD, UTC
  value: number;
}

export interface SimpleSeries {
  unit: string;
  points: StatPoint[];
}

/** День стек-ряда: total + разбивка по ключам (все ключи присутствуют, 0-filled). */
export interface StackedPoint {
  date: string;
  total: number;
  values: Record<string, number>;
}

export interface StackedSeries {
  unit: string;
  keys: string[]; // ключи стека (регионы/версии), отсортированы
  points: StackedPoint[];
}

export interface VersionShare {
  version: string;
  matches: number;
  share: number; // 0..1
}

/** Fill-rate: перцентили time-to-match. Источник/note приходят с бэка. */
export interface TimeToMatch {
  p50_seconds: number | null;
  p95_seconds: number | null;
  samples: number;
  source: string;
  note: string;
}

export interface StatsOverview {
  days: number;
  timezone: string;
  generated_at: string;
  matches_per_day: StackedSeries;
  players_per_day: StackedSeries;
  peak_ccu_per_day: SimpleSeries;
  peak_ccu: number;
  avg_match_duration_seconds: number | null;
  avg_match_duration_per_day: SimpleSeries;
  version_distribution: VersionShare[];
  time_to_match: TimeToMatch;
}

/** Снапшот утилизации региона (allocated/ready/draining vs ёмкость активных тачек). */
export interface RegionUtil {
  region: string;
  capacity_slots: number;
  allocated: number;
  ready: number;
  draining: number;
}

export interface StatsCost {
  days: number;
  timezone: string;
  generated_at: string;
  slot_hours_per_day_by_region: StackedSeries;
  slot_hours_per_day_by_version: StackedSeries;
  slot_hours_total: number;
  utilization: RegionUtil[];
  utilization_note: string;
}

// --- П2: алерты (GET /v1/alerts/*, master проксирует vmalert + лог-синк) ---

/** Правило vmalert: имя, severity, выражение, «for», состояние, описание.
 *  `description` — EN (каноничный); `description_ru` — опциональный RU-перевод
 *  (панель выбирает по локали с фоллбэком на `description`). */
export interface AlertRule {
  name: string;
  group: string;
  severity: string;
  expr: string;
  for: string;
  state: string; // inactive|pending|firing
  description: string;
  description_ru?: string;
}

/**
 * Область алерта (мультипроект, трекер #955): `platform` — алерт не привязан к
 * проекту (MasterDown, NodeDown, DiskHigh, CertExpiry, BackupStale/Failed),
 * `project` — привязан. Master выводит область из НАЛИЧИЯ лейбла `project` у
 * метрики, на которой построен expr.
 */
export type AlertScope = 'project' | 'platform';

/** Активный (firing) алерт из vmalert. `muted` — подавлен ли mute'ом (панель).
 *  `description` — EN; `description_ru` — опциональный RU (фоллбэк на EN).
 *  `project`/`scope` — additive-поля #955: их НЕТ у мастера, который ещё не
 *  перевыкачен, поэтому оба опциональны (см. alertScopeOf в lib/alerts.ts). */
export interface ActiveAlert {
  name: string;
  severity: string;
  region: string;
  node: string;
  state: string;
  active_at: string;
  value: string;
  description: string;
  description_ru?: string;
  muted?: boolean;
  project?: string;
  scope?: AlertScope;
}

/** Срабатывание из истории (alerts.log). active — ещё ли горит (по endsAt).
 *  `description` — EN; `description_ru` — опциональный RU (фоллбэк на EN).
 *  `project`/`scope` — additive-поля #955, опциональны (см. ActiveAlert). */
export interface AlertEvent {
  name: string;
  severity: string;
  region: string;
  node: string;
  startsAt: string;
  endsAt: string;
  description: string;
  description_ru?: string;
  active: boolean;
  received_at?: string;
  muted?: boolean;
  project?: string;
  scope?: AlertScope;
}

/**
 * Заглушка алерта (mute): подавляет показ в панели, зеркалится в alertmanager
 * silence и ведёт аудит. region=null — все регионы; project=null — все проекты
 * (#957); expires_at=null — бессрочно. Апсертится по тройке
 * (alertname, region, project).
 *
 * Проектный mute кроет ТОЛЬКО свой проект — платформенный алерт (у него
 * проекта нет) им не заглушить, иначе оператор одного проекта тушил бы общий
 * сигнал всем. Чтобы заглушить платформенный алерт, mute ставится без проекта.
 */
export interface AlertMute {
  id: string;
  alertname: string;
  region: string | null;
  project: string | null;
  note: string;
  created_at: string;
  expires_at: string | null;
  created_by: string;
}

/** Тело создания mute: region/project/note/expires_at опциональны (пустое = все/бессрочно). */
export interface AlertMuteInput {
  alertname: string;
  region?: string;
  project?: string;
  note?: string;
  expires_at?: string;
}

// --- П2: API-ключи (GET/POST/DELETE /v1/apikeys, admin-only) ---

export interface ApiKey {
  id: string;
  name: string;
  scopes: Scope[];
  created_at: string;
  revoked_at: string | null;
  /** Опциональная привязка (project, env) — строго парой (environments v1 §5).
   *  Отсутствует у глобального ключа (NULL-привязка = пре-env поведение). */
  project?: string;
  env?: string;
}

/** Привязка ключа к (project, env) — строго парой (environments v1 §5). */
export interface KeyBinding {
  project: string;
  env: string;
}

/** Ответ POST /v1/apikeys: ключ + секрет (показывается РОВНО один раз). */
export interface CreatedApiKey {
  key: ApiKey;
  secret: string;
}

// --- Реестры (registries v2, GET/POST/PATCH/DELETE /v1/registries, admin-only)
// (docs/superpowers/specs/2026-07-10-registries-v2-design.md §4). Токен
// write-only: POST/PATCH его принимают, но ни GET, ни события никогда его не
// возвращают — RegistryInfo структурно не имеет поля token (master/internal/
// store/registries.go: Registry без Token; RegistryCred с токеном — только
// внутри master, для agentlink).

/** Тип реестра (v2): определяет форму кредов/валидацию/подсказку в панели.
 *  Мастер нормализует любой тип в docker-basic-auth (gar → username=_json_key). */
export type RegistryType = 'ghcr' | 'gar' | 'generic';

export interface RegistryInfo {
  id: string;
  host: string;
  type: RegistryType;
  username: string;
  note: string;
  created_at: string;
  updated_at: string;
}

/** Тело POST /v1/registries: upsert по (нормализованному) host — тот же host
 *  заменяет type/username/token/note целиком. `username` опционален: для gar его
 *  не шлём (мастер форсит `_json_key`). */
export interface RegistryInput {
  host: string;
  type: RegistryType;
  username?: string;
  token: string;
  note?: string;
}

/** Тело PATCH /v1/registries/{id}: частичное обновление по id. host неизменяем
 *  (в теле не принимается). token опционален: пустой/отсутствует → секрет не
 *  меняется; задан → ротация. */
export interface RegistryPatch {
  type?: RegistryType;
  username?: string;
  token?: string;
  note?: string;
}

// --- Бекапы (Backups v1, GET/PATCH /v1/backups/settings, GET /v1/backups/runs,
// POST /v1/backups/run, POST /v1/backups/s3/test; admin-only). Дампы Postgres
// силами master: расписание + локальный ретеншн + S3-оффсайт. s3_secret_key
// write-only: PATCH его принимает, но GET его никогда не возвращает — вместо
// секрета отдаётся флаг has_s3_secret (задан ли он).

export interface BackupSettings {
  enabled: boolean;
  interval_hours: number;
  retention_local: number;
  s3_enabled: boolean;
  s3_endpoint: string;
  s3_region: string;
  s3_bucket: string;
  s3_prefix: string;
  s3_access_key: string;
  has_s3_secret: boolean;
  retention_s3: number;
  updated_at: string;
}

/** Тело PATCH /v1/backups/settings: только изменённые поля. s3_secret_key
 *  write-only — не задан = секрет цел (keep); задан = ротация. */
export interface BackupSettingsPatch {
  enabled?: boolean;
  interval_hours?: number;
  retention_local?: number;
  s3_enabled?: boolean;
  s3_endpoint?: string;
  s3_region?: string;
  s3_bucket?: string;
  s3_prefix?: string;
  s3_access_key?: string;
  s3_secret_key?: string; // write-only: не задан = keep
  retention_s3?: number;
}

/** Прогон бекапа из истории (GET /v1/backups/runs). result=running — ещё идёт
 *  (finished_at=null, size_bytes=null); error — текст ошибки (пустой, если ок). */
export interface BackupRun {
  id: number;
  started_at: string;
  finished_at: string | null;
  kind: 'scheduled' | 'manual';
  result: 'running' | 'ok' | 'error';
  size_bytes: number | null;
  s3_uploaded: boolean;
  error: string;
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

async function request<T>(method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE', path: string, body?: unknown): Promise<T> {
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
  listNodes: (f: { project?: string; env?: string } = {}) =>
    request<{ nodes: NodeInfo[] }>('GET', `/v1/nodes${qs(f)}`).then((r) => r.nodes),
  listServers: (f: { state?: string; region?: string; project?: string } = {}) =>
    request<{ servers: GameServer[] }>('GET', `/v1/servers${qs(f)}`).then((r) => r.servers),
  listVersions: (f: { project?: string; env?: string } = {}) =>
    request<{ versions: VersionInfo[] }>('GET', `/v1/versions${qs(f)}`).then((r) => r.versions),
  listMatches: (f: MatchFilter = {}) =>
    request<{ matches: Match[] }>('GET', `/v1/matches${qs(f)}`).then((r) => r.matches),
  getMatch: (id: string) =>
    request<{ match: Match }>('GET', `/v1/matches/${encodeURIComponent(id)}`).then((r) => r.match),
  /** Лента событий. `project` сужает СЕРВЕРНО и не скрывающе: уходят события
   *  явно чужого проекта, платформенные видны под любым фильтром (#985). */
  listEvents: (limit = 50, project?: string | null) =>
    request<{ events: ApiEvent[] }>('GET', `/v1/events${qs({ limit, project: project ?? undefined })}`).then(
      (r) => r.events,
    ),

  // --- П1: операции (скоуп deploy/admin; кнопки скрыты у readonly) ---

  /** Мягкий деплой версии: 202 prepulling / 200 active (master §5). */
  deploy: (versionId: string) =>
    request<{ deploy: DeployStatus }>('POST', '/v1/deploy', { version_id: versionId }).then((r) => r.deploy),
  /** Откат: deprecated ↔ active за секунды. project опускаем при единственном;
   *  env обязателен, когда у проекта >1 env с deprecated-окном (environments v1 §3). */
  rollback: (body: { project?: string; env?: string; region?: string } = {}) =>
    request<{ rollback: RollbackResult }>('POST', '/v1/rollback', body).then((r) => r.rollback),
  /** Промоут версии в другой env (environments v1 §4): регистрация в to_env (тот же
   *  image_ref, provenance promoted_from) + немедленный deploy-пайплайн. 202/200. */
  promote: (versionId: string, toEnv: string) =>
    request<{ version: VersionInfo; deploy: DeployStatus }>('POST', '/v1/promote', {
      version_id: versionId,
      to_env: toEnv,
    }),
  /** Вывод тачки из ротации (admin). */
  drainNode: (id: string) =>
    request<{ node: NodeInfo }>('POST', `/v1/nodes/${encodeURIComponent(id)}/drain`).then((r) => r.node),
  undrainNode: (id: string) =>
    request<{ node: NodeInfo }>('POST', `/v1/nodes/${encodeURIComponent(id)}/undrain`).then((r) => r.node),
  /** Вывод ноды из флота навсегда: state → dead. 409, если у ноды есть живые
   *  дедики — такую выводит drain, ревокация оборвала бы матч. */
  revokeNode: (id: string) =>
    request<{ node: NodeInfo }>('POST', `/v1/nodes/${encodeURIComponent(id)}/revoke`).then((r) => r.node),

  // --- П2: статистика / cost (скоуп readonly) ---

  /** Агрегаты обзора за N дней (matches/players/CCU/версии/fill-rate).
   *  `env` (environments v1 §7, I5) сужает историю, но НЕ пик CCU — он
   *  остаётся пиком всего среза. `project` (мультипроект W3) сужает ВСЁ,
   *  включая пик: проекты — непересекающиеся тенанты. Опции объектом, а не
   *  двумя строками подряд: перепутать их местами было бы слишком легко. */
  statsOverview: (days: number, f: { project?: string; env?: string } = {}) =>
    request<StatsOverview>('GET', `/v1/stats/overview${qs({ days, ...f })}`),
  /** Слото-часы per регион/версия + утилизация за N дней. Фильтры — как в overview. */
  statsCost: (days: number, f: { project?: string; env?: string } = {}) =>
    request<StatsCost>('GET', `/v1/stats/cost${qs({ days, ...f })}`),

  // --- Проекты (мультипроект W1) ---

  /** Список проектов (readonly), старейший первым — источник селектора проекта. */
  listProjects: () => request<{ projects: ProjectInfo[] }>('GET', '/v1/projects').then((r) => r.projects),
  /** Явное создание проекта (admin): 409, если слаг занят. В отличие от
   *  upsertProject, который на опечатке молча перезаписал бы чужой match_size. */
  createProject: (slug: string, matchSize: number) =>
    request<{ project: ProjectInfo }>('POST', '/v1/projects', { slug, match_size: matchSize }).then((r) => r.project),
  /** Правка размера матча (admin). Тот же PUT, что и раньше. */
  upsertProject: (slug: string, matchSize: number) =>
    request<{ project: ProjectInfo }>('PUT', `/v1/projects/${encodeURIComponent(slug)}`, {
      match_size: matchSize,
    }).then((r) => r.project),
  /** Состав проекта (admin) — показывается в диалоге удаления ДО подтверждения. */
  projectUsage: (slug: string) =>
    request<{ usage: ProjectUsage }>('GET', `/v1/projects/${encodeURIComponent(slug)}/usage`).then((r) => r.usage),
  /** Удалить проект вместе с содержимым (admin): 409, пока есть живые ноды;
   *  непустой требует confirm, равного слагу; пустой сносится без него (204). */
  deleteProject: (slug: string, confirm: string) =>
    request<{ deleted: ProjectDeleted } | undefined>('DELETE', `/v1/projects/${encodeURIComponent(slug)}`, {
      confirm,
    }).then((r) => r?.deleted),

  // --- Окружения (environments v1 §2): список — readonly, CRUD — admin ---

  /** Список окружений проекта (readonly). project ОБЯЗАТЕЛЕН: панель всегда
   *  знает выбранный проект (мультипроект W1), и опциональность параметра
   *  оставляла бы открытым путь к sole-резолву на стороне master — тому
   *  самому, который при нескольких проектах отвечал 400. */
  listEnvironments: (project: string) =>
    request<{ environments: Environment[] }>('GET', `/v1/environments${qs({ project })}`).then((r) => r.environments),
  /** Создать окружение (admin). 201; production&&auto_deploy или all/global → 400; дубль → 409. */
  createEnvironment: (body: EnvironmentInput) =>
    request<{ environment: Environment }>('POST', '/v1/environments', body).then((r) => r.environment),
  /** Правка флагов окружения (admin). Включение auto_deploy при production → 400. */
  patchEnvironment: (project: string, name: string, body: EnvironmentPatch) =>
    request<{ environment: Environment }>(
      'PATCH',
      `/v1/environments/${encodeURIComponent(project)}/${encodeURIComponent(name)}`,
      body,
    ).then((r) => r.environment),
  /** Состав окружения (readonly) — панель показывает его в диалоге удаления. */
  environmentUsage: (project: string, name: string) =>
    request<{ usage: EnvironmentUsage }>(
      'GET',
      `/v1/environments/${encodeURIComponent(project)}/${encodeURIComponent(name)}/usage`,
    ).then((r) => r.usage),
  /** Удалить окружение ВМЕСТЕ с содержимым (admin): версии/флоты/матчи/серверы
   *  сносятся каскадом, привязанные ключи отзываются. Требования сервера:
   *  ноль нод (иначе 409 — сначала переведите их) и confirm, ТОЧНО равный имени
   *  (иначе 400). Пустое окружение сервер удалит и без confirm (204 → undefined). */
  deleteEnvironment: (project: string, name: string, confirm: string) =>
    request<{ deleted: EnvironmentDeleted } | undefined>(
      'DELETE',
      `/v1/environments/${encodeURIComponent(project)}/${encodeURIComponent(name)}`,
      { confirm },
    ).then((r) => r?.deleted),
  /** Перевод ноды в другой env (admin, environments v1 §2): PATCH пустой ноде. */
  setNodeEnv: (id: string, env: string) =>
    request<{ node: NodeInfo }>('PATCH', `/v1/nodes/${encodeURIComponent(id)}`, { env }).then((r) => r.node),

  // --- П2: алерты (скоуп readonly; master проксирует vmalert) ---

  /** Каталог правил. Сознательно БЕЗ ?project=: у правила проекта нет — он живёт
   *  внутри текста expr, и фильтр по несуществующему лейблу вернул бы пустой
   *  каталог для любого проекта (master: handleAlertRules). */
  alertRules: () => request<{ rules: AlertRule[] }>('GET', '/v1/alerts/rules').then((r) => r.rules),
  /** Активные. `project` — НЕ скрывающее сужение (#955): алерт уходит, только
   *  если его project ЯВНО чужой; платформенный виден при любом выборе. */
  alertsActive: (opts: { project?: string } = {}) =>
    request<{ alerts: ActiveAlert[] }>('GET', `/v1/alerts/active${qs({ project: opts.project })}`).then(
      (r) => r.alerts,
    ),
  /** История. `project` — то же не скрывающее сужение; master применяет его ДО
   *  отсечения по limit, иначе записи проекта съедали бы соседи. */
  alertHistory: (opts: { limit: number; project?: string }) =>
    request<{ alerts: AlertEvent[] }>(
      'GET',
      `/v1/alerts/history${qs({ limit: opts.limit, project: opts.project })}`,
    ).then((r) => r.alerts),

  // Заглушки алертов (readonly читает; создание/снятие — admin). all=1 включает истёкшие.
  alertMutes: (all = false) =>
    request<{ mutes: AlertMute[] }>('GET', `/v1/alerts/mutes${qs({ all: all ? 1 : undefined })}`).then(
      (r) => r.mutes ?? [],
    ),
  createAlertMute: (body: AlertMuteInput) =>
    request<{ mute: AlertMute }>('POST', '/v1/alerts/mutes', body).then((r) => r.mute),
  deleteAlertMute: (id: string) =>
    request<void>('DELETE', `/v1/alerts/mutes/${encodeURIComponent(id)}`),

  // --- П2: API-ключи (admin-only) ---

  listApiKeys: () => request<{ apikeys: ApiKey[] }>('GET', '/v1/apikeys').then((r) => r.apikeys),
  /** Создаёт ключ; секрет в ответе показывается ровно один раз. `binding`
   *  (environments v1 §5) — опциональная привязка (project, env) строго парой:
   *  несовместима со скоупом admin (400), требует существования env. */
  createApiKey: (name: string, scopes: Scope[], binding?: KeyBinding) =>
    request<CreatedApiKey>('POST', '/v1/apikeys', { name, scopes, ...binding }),
  /** Отзыв ключа (409 last_admin_key — нельзя отозвать последний admin). */
  revokeApiKey: (id: string) =>
    request<{ key: ApiKey }>('DELETE', `/v1/apikeys/${encodeURIComponent(id)}`).then((r) => r.key),
  /** Purge (hard-delete) уже отозванного ключа: 409 not_revoked на активном,
   *  404 на неизвестном/повторном вызове (registries v1 design §6). */
  purgeApiKey: (id: string) =>
    request<void>('DELETE', `/v1/apikeys/${encodeURIComponent(id)}${qs({ purge: 'true' })}`),

  // --- Реестры (admin-only) ---

  listRegistries: () => request<{ registries: RegistryInfo[] }>('GET', '/v1/registries').then((r) => r.registries),
  /** Создаёт или (тот же host) заменяет type/username/token/note. */
  upsertRegistry: (body: RegistryInput) =>
    request<{ registry: RegistryInfo }>('POST', '/v1/registries', body).then((r) => r.registry),
  /** Частичная правка по id: host неизменяем; пустой token → секрет цел, иначе
   *  ротация (registries v2 design §2). */
  patchRegistry: (id: string, body: RegistryPatch) =>
    request<{ registry: RegistryInfo }>('PATCH', `/v1/registries/${encodeURIComponent(id)}`, body).then((r) => r.registry),
  deleteRegistry: (id: string) => request<void>('DELETE', `/v1/registries/${encodeURIComponent(id)}`),

  // --- Бекапы (admin-only) ---

  getBackupSettings: () =>
    request<{ settings: BackupSettings }>('GET', '/v1/backups/settings').then((r) => r.settings),
  /** PATCH только изменённых полей; s3_secret_key задаём лишь при ротации. */
  patchBackupSettings: (body: BackupSettingsPatch) =>
    request<{ settings: BackupSettings }>('PATCH', '/v1/backups/settings', body).then((r) => r.settings),
  listBackupRuns: (limit = 50) =>
    request<{ runs: BackupRun[] }>('GET', `/v1/backups/runs${qs({ limit })}`).then((r) => r.runs),
  /** Ручной прогон: 202 {started:true} | 409 conflict (прогон уже идёт). */
  runBackupNow: () => request<{ started: boolean }>('POST', '/v1/backups/run'),
  /** Проверка СОХРАНЁННОЙ S3-конфигурации: 200 {ok:true} | 400 s3_test_failed. */
  testBackupS3: () => request<{ ok: boolean }>('POST', '/v1/backups/s3/test'),
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
