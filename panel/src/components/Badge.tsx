// Статусные бейджи. Статусные цвета зарезервированы: good/warn/dead —
// только состояния, accent — «в работе» (allocated), neutral — прошлое и
// неизвестное. Цвет никогда не единственный носитель смысла: всегда есть
// точка-маркер и текстовая метка состояния.

import { useT } from '../lib/i18n';
import type { MessageKey } from '../lib/i18n';

export type Tone = 'good' | 'warn' | 'dead' | 'accent' | 'neutral';

/** Домены человекочитаемых подписей (коды остаются в API). Каждый домен — свой
 *  префикс ключа в каталоге; неизвестный код → фолбэк на сам код (badge без
 *  domain тоже показывает сырой код). */
export type StateDomain =
  | 'node'
  | 'server'
  | 'match'
  | 'version'
  | 'event'
  | 'severity'
  | 'keystatus'
  | 'alertstate';

const KEY_PREFIX: Record<StateDomain, string> = {
  node: 'state.node.',
  server: 'state.server.',
  match: 'state.match.',
  version: 'state.version.',
  event: 'event.',
  severity: 'severity.',
  keystatus: 'keystatus.',
  alertstate: 'alertstate.',
};

/** Все виды событий ленты (константы EventXxx в master store/*.go). Единый
 *  источник для дропдауна фильтра, подписей event.<kind> и guard-теста. */
export const EVENT_KINDS = [
  // node_down (итерация 5, follow-ups): master помечает ноду недоступной, когда
  // quarantine молчит дольше node_down_after_min — и state ноды → down, и событие
  // ленты того же kind (store/models.go: EventNodeDown, рядом с quarantine).
  'node_created', 'node_quarantine', 'node_down', 'node_recovered', 'node_drain', 'node_undrain',
  // mTLS agentlink v1 (docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md
  // §7): node_enrolled — первый обмен node_token→серт; node_cert_renewed —
  // ротация клиентского серта по живой mTLS-сессии.
  'node_enrolled', 'node_cert_renewed',
  'server_failed', 'server_recovered', 'server_drain', 'crash_loop', 'allocation_failed',
  'version_registered', 'version_disabled', 'fleet_updated', 'deploy_started',
  'deploy_node_pulled', 'deploy_activated', 'deploy_failed', 'deploy_rolled_back',
  'agent_upgrade', 'agent_upgrade_succeeded', 'agent_upgrade_failed',
  // registries v1 (docs/superpowers/specs/2026-07-09-registries-design.md
  // §4/§6): apikey_created/apikey_revoked existed in master before this set
  // but had no label here — cheap backfill alongside the three new kinds.
  // registry_updated — PATCH-правка записи (registries v2 design §2).
  'registry_upserted', 'registry_updated', 'registry_removed', 'apikey_created', 'apikey_revoked', 'apikey_purged',
  // Backups v1 (2026-07-13-backups-admin-v1-design.md §2): прогон упал.
  'backup_failed',
] as const;

export function toneOfNodeState(state: string): Tone {
  switch (state) {
    case 'active':
      return 'good';
    case 'draining':
      return 'warn';
    case 'quarantine':
    case 'down': // молчит дольше node_down_after_min — активный простой, красный как quarantine (не терминальный dead)
      return 'dead';
    case 'dead':
      return 'neutral';
    default:
      return 'neutral';
  }
}

export function toneOfServerState(state: string): Tone {
  switch (state) {
    case 'ready':
      return 'good';
    case 'allocated':
      return 'accent';
    case 'creating':
    case 'draining':
      return 'warn';
    case 'failed':
      return 'dead';
    case 'reaped':
      return 'neutral';
    default:
      return 'neutral';
  }
}

export function toneOfMatchState(state: string): Tone {
  switch (state) {
    case 'running':
      return 'good';
    case 'pending':
      return 'warn';
    case 'finished':
      return 'neutral';
    case 'aborted':
      return 'dead';
    default:
      return 'neutral';
  }
}

/**
 * Тон состояния версии (store/deploy.go: registered → prepulling → active →
 * deprecated → disabled). active — good; prepulling — warn (идёт прогрев);
 * deprecated — accent (ещё в окне мультиверсий, доигрывает); registered/
 * disabled — neutral.
 */
export function toneOfVersionState(state: string): Tone {
  switch (state) {
    case 'active':
      return 'good';
    case 'prepulling':
      return 'warn';
    case 'deprecated':
      return 'accent';
    case 'registered':
    case 'disabled':
      return 'neutral';
    default:
      return 'neutral';
  }
}

/**
 * Тон severity алерта (vmalert labels.severity, ops.md §1): critical → dead,
 * warning → warn, всё прочее (info и т.п.) — neutral. Цвет не единственный
 * носитель: у бейджа всегда есть подпись severity.<code>.
 */
export function toneOfSeverity(severity: string): Tone {
  switch (severity) {
    case 'critical':
      return 'dead';
    case 'warning':
      return 'warn';
    default:
      return 'neutral';
  }
}

/** Тон статуса API-ключа: активен → good, отозван → neutral (прошлое). */
export function toneOfKeyStatus(revoked: boolean): Tone {
  return revoked ? 'neutral' : 'good';
}

/** Тон состояния алерта из истории: горит (active) → dead, погас → neutral. */
export function toneOfAlertActive(active: boolean): Tone {
  return active ? 'dead' : 'neutral';
}

/** Тон события ленты по kind (models.go master). */
export function toneOfEventKind(kind: string): Tone {
  switch (kind) {
    case 'node_quarantine':
    case 'node_down':
    case 'server_failed':
    case 'crash_loop':
    case 'deploy_failed':
    case 'agent_upgrade_failed':
    case 'backup_failed':
      return 'dead';
    case 'allocation_failed':
    case 'node_drain':
    case 'version_disabled':
    case 'server_drain':
      return 'warn';
    case 'node_recovered':
    case 'server_recovered':
    case 'node_undrain':
    case 'node_enrolled':
    case 'deploy_activated':
    case 'agent_upgrade_succeeded':
      return 'good';
    case 'node_created':
    case 'node_cert_renewed':
    case 'version_registered':
    case 'fleet_updated':
    case 'deploy_started':
    case 'deploy_node_pulled':
    case 'deploy_rolled_back':
    case 'agent_upgrade':
    case 'registry_upserted':
    case 'registry_updated':
    case 'apikey_created':
      return 'accent';
    case 'registry_removed':
    case 'apikey_revoked':
      return 'warn';
    default:
      return 'neutral';
  }
}

const toneClasses: Record<Tone, string> = {
  good: 'bg-good-bg text-good',
  warn: 'bg-warn-bg text-warn',
  dead: 'bg-dead-bg text-dead',
  accent: 'bg-mark text-accent-ink',
  neutral: 'border border-line text-muted',
};

/**
 * Бейдж состояния/события. С `domain` показывает переведённую подпись
 * (ключ `<prefix><code>`, фолбэк — сырой код для незнакомых значений) и держит
 * сырой код в `title`; без `domain` — сырой код.
 */
export function StateBadge({ state, tone, domain }: { state: string; tone: Tone; domain?: StateDomain }) {
  const { t, has } = useT();
  const key = domain !== undefined ? `${KEY_PREFIX[domain]}${state}` : undefined;
  const label = key !== undefined && has(key) ? t(key as MessageKey) : state;
  return (
    <span
      data-tone={tone}
      title={domain !== undefined ? state : undefined}
      className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium whitespace-nowrap ${toneClasses[tone]}`}
    >
      <span aria-hidden className="size-1.5 rounded-full bg-current" />
      {label}
    </span>
  );
}
