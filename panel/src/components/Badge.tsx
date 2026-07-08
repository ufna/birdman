// Статусные бейджи. Статусные цвета зарезервированы: good/warn/dead —
// только состояния, accent — «в работе» (allocated), neutral — прошлое и
// неизвестное. Цвет никогда не единственный носитель смысла: всегда есть
// точка-маркер и текстовая метка состояния.

export type Tone = 'good' | 'warn' | 'dead' | 'accent' | 'neutral';

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

/** Тон события ленты по kind (models.go master). */
export function toneOfEventKind(kind: string): Tone {
  switch (kind) {
    case 'node_quarantine':
    case 'server_failed':
    case 'crash_loop':
      return 'dead';
    case 'allocation_failed':
      return 'warn';
    case 'node_recovered':
      return 'good';
    case 'node_created':
    case 'version_registered':
    case 'fleet_updated':
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

export function StateBadge({ state, tone }: { state: string; tone: Tone }) {
  return (
    <span
      data-tone={tone}
      className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium whitespace-nowrap ${toneClasses[tone]}`}
    >
      <span aria-hidden className="size-1.5 rounded-full bg-current" />
      {state}
    </span>
  );
}
