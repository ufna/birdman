// Статусные бейджи. Статусные цвета зарезервированы: good/warn/dead —
// только состояния, accent — «в работе» (allocated), neutral — прошлое и
// неизвестное. Цвет никогда не единственный носитель смысла: всегда есть
// точка-маркер и текстовая метка состояния.

import { useT } from '../lib/i18n';
import type { MessageKey } from '../lib/i18n';

export type Tone = 'good' | 'warn' | 'dead' | 'accent' | 'neutral';

/** Домены человекочитаемых подписей состояний (коды остаются в API). Событийные
 *  kind'ы — технические токены, их не переводим (badge без domain = сырой код). */
export type StateDomain = 'node' | 'server' | 'match' | 'version';

export function toneOfNodeState(state: string): Tone {
  switch (state) {
    case 'active':
      return 'good';
    case 'draining':
      return 'warn';
    case 'quarantine':
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

/** Тон события ленты по kind (models.go master). */
export function toneOfEventKind(kind: string): Tone {
  switch (kind) {
    case 'node_quarantine':
    case 'server_failed':
    case 'crash_loop':
    case 'deploy_failed':
    case 'agent_upgrade_failed':
      return 'dead';
    case 'allocation_failed':
    case 'node_drain':
    case 'version_disabled':
    case 'server_drain':
      return 'warn';
    case 'node_recovered':
    case 'server_recovered':
    case 'node_undrain':
    case 'deploy_activated':
    case 'agent_upgrade_succeeded':
      return 'good';
    case 'node_created':
    case 'version_registered':
    case 'fleet_updated':
    case 'deploy_started':
    case 'deploy_node_pulled':
    case 'deploy_rolled_back':
    case 'agent_upgrade':
      return 'accent';
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
 * Бейдж состояния. С `domain` показывает переведённую подпись
 * (`state.<domain>.<code>`, фолбэк — сырой код для незнакомых значений); без
 * `domain` — сырой код (событийные kind'ы).
 */
export function StateBadge({ state, tone, domain }: { state: string; tone: Tone; domain?: StateDomain }) {
  const { t, has } = useT();
  const key = domain !== undefined ? `state.${domain}.${state}` : undefined;
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
