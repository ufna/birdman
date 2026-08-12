// Алерты (П2): master проксирует vmalert (правила + active) и отдаёт историю из
// alerts.log. Правила/active — read-only (это состояние vmalert). Заглушки
// (mute) — панельное подавление + аудит: admin ставит/снимает mute, readonly
// только видит. Секции: Заглушённые, Активные, Правила, История (newest-first).
// Описания алертов приходят с бэка двуязычно: description (EN) + description_ru
// (опционально) — выбираем по локали с фоллбэком на EN (alertDescription).
// Обвязку UI переводим через каталог как обычно.
//
// Проектное измерение (мультипроект, трекер #956, шаг 2/3 эпика #950). Активные
// и История сужаются СЕРВЕРНЫМ ?project= (useProjectAsync), Правила — нет: это
// каталог конфигурации, у правила проекта нет вовсе. Сужение НЕ СКРЫВАЮЩЕЕ:
// алерт уходит с экрана, только если его project явно чужой, а платформенный
// (MasterDown/NodeDown/DiskHigh*/CertExpiry/Backup*) виден при любом выборе — и
// подписан чипом, иначе оператор прочитает «мастер лёг» как беду текущего
// проекта. Молча спрятать такой алерт — худший исход этого экрана.
//
// Заглушки тоже знают про проект (трекер #957, шаг 3/3): область mute'а — это
// проект И регион, и оба наследуются от алерта, с которого mute поставлен.
// Проектный mute кроет только свой проект, платформенный алерт им не заглушить
// — иначе оператор проекта А тушил бы общий сигнал проекту Б.

import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import * as Dialog from '@radix-ui/react-dialog';
import { api } from '../lib/api';
import { apiErrorMessage } from '../lib/apiError';
import type { ActiveAlert, AlertEvent, AlertMute, AlertRule } from '../lib/api';
import {
  MUTE_PRESETS,
  alertDescription,
  alertProjectOf,
  alertSoundEnabled,
  alertsUnavailable,
  isPlatformAlert,
  muteErrorMessage,
  normalizeMuteLabel,
  presetExpiry,
  setAlertSoundEnabled,
} from '../lib/alerts';
import type { AlertScoped, MutePreset } from '../lib/alerts';
import { useAsync } from '../lib/useAsync';
import { useProject, useProjectAsync } from '../lib/project';
import { canAdmin, useBindingRefusal, useSession } from '../lib/session';
import { useT, useFormat } from '../lib/i18n';
import type { BoundFormat, MessageKey } from '../lib/i18n';
import { useToast } from '../components/Toast';
import { DataTable } from '../components/DataTable';
import { StateBadge, toneOfAlertActive, toneOfSeverity } from '../components/Badge';
import { ConfirmButton } from '../components/ConfirmDialog';
import { Card, CardHeader, EmptyState, ErrorNote, LoadingRow } from '../components/ui';

const HISTORY_LIMITS = [20, 50, 100];

export function Alerts() {
  const { t } = useT();
  const { session } = useSession();
  const { selected } = useProject();
  const mayMute = session != null && canAdmin(session);
  const [limit, setLimit] = useState(50);
  // Активные и история — через useProjectAsync: слаг уезжает в ?project=, а сам
  // проект попадает в deps, так что его смена ПЕРЕСЧИТЫВАЕТ данные (грабли #948
  // — там проект забыли в deps, и экран показывал прежний проект).
  const active = useProjectAsync((project) => api.alertsActive({ project }), []);
  const history = useProjectAsync((project) => api.alertHistory({ limit, project }), [limit]);
  // Правила — каталог конфигурации, у правила проекта нет. Список заглушек тоже
  // НЕ сужается: mute'ы с этого шага (#957) знают про проект, но список
  // показывает их все — заглушка соседа объясняет, почему его алерт молчит, а
  // спрятать её значило бы оставить оператора наедине с тишиной без причины.
  const rules = useAsync(() => api.alertRules(), []);
  const mutes = useAsync(() => api.alertMutes(), []);
  // Подписи про сужение показываем, только когда сужать есть по чему.
  const narrowed = selected !== null;

  // После постановки/снятия mute перечитываем всё, чей вид зависит от заглушек:
  // список mutes + флаги muted на активных и в истории.
  const refreshMuted = () => {
    mutes.reload();
    active.reload();
    history.reload();
  };

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-lg font-semibold">{t('nav.alerts')}</h1>
        <div className="flex items-center gap-3">
          <SoundToggle />
          <span className="rounded border border-line px-1.5 py-0.5 text-[11px] text-muted">{t('alerts.readonly')}</span>
        </div>
      </div>

      <MutedSection mutes={mutes.data} mayMute={mayMute} reload={refreshMuted} />
      <ActiveSection
        active={active.data}
        error={active.error}
        loading={active.data === undefined && active.loading}
        reload={active.reload}
        mayMute={mayMute}
        onMuted={refreshMuted}
        narrowed={narrowed}
      />
      <RulesSection
        rules={rules.data}
        error={rules.error}
        loading={rules.data === undefined && rules.loading}
        reload={rules.reload}
        mayMute={mayMute}
        onMuted={refreshMuted}
        narrowed={narrowed}
      />
      <HistorySection
        events={history.data}
        error={history.error}
        loading={history.data === undefined && history.loading}
        reload={history.reload}
        limit={limit}
        setLimit={setLimit}
        mayMute={mayMute}
        onMuted={refreshMuted}
      />
    </div>
  );
}

/** Флажок звука нового critical (клиентская настройка бейджа; по умолчанию выкл). */
function SoundToggle() {
  const { t } = useT();
  const [on, setOn] = useState(() => alertSoundEnabled());
  return (
    <label className="inline-flex cursor-pointer items-center gap-1.5 text-[11px] text-muted">
      <input
        type="checkbox"
        checked={on}
        onChange={(e) => {
          setOn(e.target.checked);
          setAlertSoundEnabled(e.target.checked);
        }}
      />
      {t('alerts.sound')}
    </label>
  );
}

/** Мягкая подпись для «алерты не настроены / апстрим недоступен», иначе null. */
function useSoftNote(error?: Error): string | null {
  const { t } = useT();
  const kind = alertsUnavailable(error);
  if (kind === 'unconfigured') return t('alerts.unconfigured');
  if (kind === 'upstream') return t('alerts.upstream');
  return null;
}

/** Общий мелкий чип-подпись (область mute, «muted», «платформенный»). */
function Chip({ title, children }: { title?: string; children: ReactNode }) {
  return (
    <span title={title} className="rounded border border-line px-1.5 py-0.5 text-[10px] tracking-wide text-muted uppercase">
      {children}
    </span>
  );
}

/**
 * Область действия mute — ОБЕ оси: проект и регион, каждая либо конкретная,
 * либо «все». Проект показываем всегда, в том числе «все проекты»: mute без
 * проекта глушит алерт и соседям, и оператор обязан видеть это до нажатия, а
 * не узнавать по тишине в чужом Discord.
 */
function ScopeChip({ project, region }: { project: string | null; region: string | null }) {
  const { t } = useT();
  return (
    <>
      <Chip>{project ?? t('alerts.mute.allProjects')}</Chip>
      <Chip>{region ?? t('alerts.mute.allRegions')}</Chip>
    </>
  );
}

/** Маленький бейдж «muted» на приглушённом алерте. */
function MutedChip() {
  const { t } = useT();
  return <Chip>{t('alerts.mute.badge')}</Chip>;
}

/**
 * Честная подпись «платформенный» — на алертах, у которых нет проекта. Ставится
 * ТОЛЬКО когда мастер сказал про область явно (alertScopeOf): мастер без
 * additive-полей #955 не даёт повода подписывать ничего, и панель молчит,
 * вместо того чтобы навесить подпись на все алерты подряд.
 */
function PlatformChip({ alert }: { alert: AlertScoped }) {
  const { t } = useT();
  if (!isPlatformAlert(alert)) return null;
  return <Chip title={t('alerts.scope.platformHint')}>{t('alerts.scope.platform')}</Chip>;
}

/** Подпись про не скрывающее сужение — под шапкой сужаемой секции. */
function ScopeNote() {
  const { t } = useT();
  return <p className="border-b border-line px-4 py-2 text-xs text-muted">{t('alerts.scope.note')}</p>;
}

/** «Где горит»: проект (если он есть) рядом с регионом и нодой. */
function whereOf(a: AlertScoped & { region: string; node: string }): string {
  return [alertProjectOf(a), a.region, a.node].filter((x) => x !== undefined && x !== '').join(' · ') || '—';
}

// --- Заглушённые (mutes) ---

function MutedSection({ mutes, mayMute, reload }: { mutes?: AlertMute[]; mayMute: boolean; reload: () => void }) {
  const { t } = useT();
  const fmt = useFormat();
  // Показываем секцию, только когда есть активные mutes (иначе не засоряем).
  if (mutes === undefined || mutes.length === 0) return null;
  return (
    <Card>
      <CardHeader title={t('alerts.mute.section')} aside={<span className="tabular font-mono text-xs text-muted">{mutes.length}</span>} />
      <p className="border-b border-line px-4 py-2 text-xs text-muted">{t('alerts.mute.semantics')}</p>
      <ul className="flex flex-col divide-y divide-line">
        {mutes.map((m) => (
          <li key={m.id} className="flex flex-wrap items-start justify-between gap-3 px-4 py-3">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <span className="font-mono text-sm font-medium">{m.alertname}</span>
                <ScopeChip project={m.project} region={m.region} />
              </div>
              {m.note !== '' && <p className="mt-1 text-xs text-muted">{m.note}</p>}
              <p className="mt-1 text-[11px] text-muted">{t('alerts.mute.byWhen', { who: m.created_by, when: fmt.stamp(m.created_at) })}</p>
            </div>
            <div className="flex shrink-0 flex-col items-end gap-1.5 text-right">
              <span className="font-mono text-[11px] text-muted">
                {m.expires_at !== null ? t('alerts.mute.expires', { when: fmt.stamp(m.expires_at) }) : t('alerts.mute.noExpiry')}
              </span>
              {mayMute && <UnmuteButton mute={m} onDone={reload} />}
            </div>
          </li>
        ))}
      </ul>
    </Card>
  );
}

/** Снятие mute — confirm + человекочитаемая обработка 404 (уже снят) и т.п. */
function UnmuteButton({ mute, onDone }: { mute: AlertMute; onDone: () => void }) {
  const { t } = useT();
  const toast = useToast();
  return (
    <ConfirmButton
      label={t('alerts.mute.unmute')}
      title={t('alerts.mute.unmute.title', { name: mute.alertname })}
      description={t('alerts.mute.unmute.desc')}
      confirmLabel={t('alerts.mute.unmute')}
      errorOverride={(e) => muteErrorMessage(e, t)}
      onConfirm={async () => {
        await api.deleteAlertMute(mute.id);
        toast.success(t('alerts.mute.toast.removed', { name: mute.alertname }));
        onDone();
      }}
    />
  );
}

// --- Кнопка + диалог постановки mute (admin) ---

/**
 * Кнопка и диалог постановки mute. Область (проект + регион) НАСЛЕДУЕТСЯ от
 * самого алерта, выбора у оператора нет — и это несущее решение, а не
 * упрощение UI:
 *  • у проектного алерта mute всегда своего проекта — оператор проекта А не
 *    может нечаянно заглушить соседа;
 *  • у платформенного алерта проекта нет вовсе, значит и mute у него без
 *    проекта: поставить платформенному сигналу ПРОЕКТНУЮ заглушку просто нечем
 *    (мастер такой mute всё равно не засчитал бы — матч по проекту строгий),
 *    зато диалог честно показывает «все проекты», и оператор видит, что глушит
 *    сигнал всем, ДО нажатия.
 * Правила (каталог конфигурации) мьютятся без проекта и без региона — у
 * правила ни того, ни другого нет, они живут внутри expr.
 */
function MuteButton({
  alertname,
  region,
  project,
  onMuted,
}: {
  alertname: string;
  region?: string;
  project?: string;
  onMuted: () => void;
}) {
  const { t } = useT();
  const fmt = useFormat();
  const toast = useToast();
  const bound = useBindingRefusal();
  const [open, setOpen] = useState(false);
  const [note, setNote] = useState('');
  const [mode, setMode] = useState<MutePreset | 'custom'>('8h');
  const [customAt, setCustomAt] = useState('');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const scopeRegion = normalizeMuteLabel(region);
  const scopeProject = normalizeMuteLabel(project);

  const reset = () => {
    setNote('');
    setMode('8h');
    setCustomAt('');
    setPending(false);
    setError(null);
  };

  const expiresAt = (): string | undefined => {
    if (mode === 'custom') return customAt !== '' ? new Date(customAt).toISOString() : undefined;
    return presetExpiry(mode, Date.now());
  };

  const submit = () => {
    if (pending) return;
    setPending(true);
    setError(null);
    api
      .createAlertMute({
        alertname,
        region: scopeRegion,
        project: scopeProject,
        note: note.trim() !== '' ? note.trim() : undefined,
        expires_at: expiresAt(),
      })
      .then(() => {
        setPending(false);
        setOpen(false);
        reset();
        toast.success(t('alerts.mute.toast.created', { name: alertname }));
        onMuted();
      })
      .catch((e: unknown) => {
        setPending(false);
        // Не ConfirmDialog: общий словарь зовём сами, mute-статусы отдаём
        // уточнением. refusal — чтобы привязанный ключ и здесь читал правду
        // про причину 403, а не «недостаточно прав» (tracker #1010).
        setError(
          apiErrorMessage(e, t, {
            refusal: bound,
            forbidden: 'confirm.err.forbidden',
            generic: 'confirm.err.generic',
            override: (err) => muteErrorMessage(err, t),
          }),
        );
      });
  };

  const seg = 'rounded-lg border px-2.5 py-1 text-xs font-medium transition-colors';
  const preview = expiresAt();

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return;
        setOpen(o);
        if (!o) reset();
      }}
    >
      <Dialog.Trigger asChild>
        <button
          type="button"
          className="rounded-lg border border-line px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:text-ink"
        >
          {t('alerts.mute.action')}
        </button>
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content
          className="fixed top-1/2 left-1/2 z-50 w-[min(32rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl"
          onEscapeKeyDown={(e) => {
            if (pending) e.preventDefault();
          }}
        >
          <Dialog.Title className="text-base font-semibold">{t('alerts.mute.dialog.title', { name: alertname })}</Dialog.Title>
          <Dialog.Description className="mt-1 text-sm text-muted">{t('alerts.mute.semantics')}</Dialog.Description>

          <div className="mt-4 flex flex-col gap-3">
            <div className="flex items-center gap-2 rounded-lg border border-line bg-paper px-3 py-2 text-sm">
              <span className="font-mono font-medium">{alertname}</span>
              <ScopeChip project={scopeProject ?? null} region={scopeRegion ?? null} />
            </div>

            <label className="flex flex-col gap-1 text-sm font-medium">
              {t('alerts.mute.note')}
              <input
                autoFocus
                value={note}
                onChange={(e) => {
                  setNote(e.target.value);
                }}
                placeholder={t('alerts.mute.notePlaceholder')}
                className="rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal placeholder:text-muted"
              />
            </label>

            <fieldset className="flex flex-col gap-2">
              <legend className="mb-1 text-sm font-medium">{t('alerts.mute.expiresLabel')}</legend>
              <div className="flex flex-wrap gap-1.5">
                {([...MUTE_PRESETS, 'custom'] as (MutePreset | 'custom')[]).map((p) => {
                  const activeMode = mode === p;
                  return (
                    <button
                      key={p}
                      type="button"
                      onClick={() => {
                        setMode(p);
                      }}
                      aria-pressed={activeMode}
                      className={`${seg} ${activeMode ? 'border-accent bg-mark text-accent-ink' : 'border-line text-muted hover:text-ink'}`}
                    >
                      {t(`alerts.mute.preset.${p}` as MessageKey)}
                    </button>
                  );
                })}
              </div>
              {mode === 'custom' && (
                <input
                  type="datetime-local"
                  value={customAt}
                  onChange={(e) => {
                    setCustomAt(e.target.value);
                  }}
                  className="mt-1 rounded-lg border border-line bg-paper px-3 py-2 text-sm"
                  aria-label={t('alerts.mute.expiresLabel')}
                />
              )}
              <p className="text-xs text-muted">
                {preview !== undefined ? t('alerts.mute.expiresPreview', { when: fmt.stamp(preview) }) : t('alerts.mute.noExpiry')}
              </p>
            </fieldset>

            {error !== null && (
              <p role="alert" className="rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
                {error}
              </p>
            )}
          </div>

          <div className="mt-5 flex justify-end gap-2">
            <Dialog.Close asChild>
              <button type="button" disabled={pending} className="rounded-lg border border-line px-3 py-1.5 text-sm text-muted hover:text-ink disabled:opacity-40">
                {t('common.cancel')}
              </button>
            </Dialog.Close>
            <button
              type="button"
              disabled={pending}
              onClick={submit}
              className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-60"
            >
              {pending ? t('common.running') : t('alerts.mute.action')}
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

// --- Активные ---

function ActiveSection({
  active,
  error,
  loading,
  reload,
  mayMute,
  onMuted,
  narrowed,
}: {
  active?: ActiveAlert[];
  error?: Error;
  loading: boolean;
  reload: () => void;
  mayMute: boolean;
  onMuted: () => void;
  narrowed: boolean;
}) {
  const { t, lang } = useT();
  const fmt = useFormat();
  const soft = useSoftNote(error);
  return (
    <Card>
      <CardHeader
        title={t('alerts.active')}
        aside={active !== undefined ? <span className="tabular font-mono text-xs text-muted">{active.length}</span> : undefined}
      />
      {narrowed && <ScopeNote />}
      {loading ? (
        <LoadingRow />
      ) : soft !== null ? (
        <EmptyState>{soft}</EmptyState>
      ) : error !== undefined && active === undefined ? (
        <div className="p-4">
          <ErrorNote error={error} retry={reload} />
        </div>
      ) : active === undefined || active.length === 0 ? (
        <EmptyState>{t('alerts.noActive')}</EmptyState>
      ) : (
        <ul className="flex flex-col divide-y divide-line">
          {active.map((a, i) => (
            <li key={`${a.name}-${a.node}-${String(i)}`} className="flex flex-wrap items-start justify-between gap-3 px-4 py-3">
              <div className={`min-w-0 ${a.muted === true ? 'opacity-60' : ''}`}>
                <div className="flex flex-wrap items-center gap-2">
                  <StateBadge state={a.severity} tone={toneOfSeverity(a.severity)} domain="severity" />
                  <span className="font-mono text-sm font-medium">{a.name}</span>
                  <PlatformChip alert={a} />
                  {a.muted === true && <MutedChip />}
                </div>
                {alertDescription(a, lang) !== '' && (
                  <p className="mt-1 text-xs text-muted">{alertDescription(a, lang)}</p>
                )}
              </div>
              <div className="flex shrink-0 flex-col items-end gap-1.5 text-right">
                <div className="font-mono text-xs text-muted">{whereOf(a)}</div>
                <div className="font-mono text-xs text-muted">{fmt.stamp(a.active_at)}</div>
                {mayMute && a.muted !== true && (
                  <MuteButton alertname={a.name} region={a.region} project={alertProjectOf(a)} onMuted={onMuted} />
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

// --- Правила ---

function RulesSection({
  rules,
  error,
  loading,
  reload,
  mayMute,
  onMuted,
  narrowed,
}: {
  rules?: AlertRule[];
  error?: Error;
  loading: boolean;
  reload: () => void;
  mayMute: boolean;
  onMuted: () => void;
  narrowed: boolean;
}) {
  const { t } = useT();
  const soft = useSoftNote(error);
  const columns = useMemo<ColumnDef<AlertRule, unknown>[]>(() => {
    const cols: ColumnDef<AlertRule, unknown>[] = [
      {
        id: 'name',
        header: t('alerts.col.rule'),
        cell: ({ row }) => (
          <div className="min-w-40">
            <div className="font-mono text-sm font-medium">{row.original.name}</div>
            <div className="font-mono text-[11px] text-muted">{row.original.group}</div>
          </div>
        ),
      },
      {
        id: 'severity',
        header: t('alerts.col.severity'),
        cell: ({ row }) => <StateBadge state={row.original.severity} tone={toneOfSeverity(row.original.severity)} domain="severity" />,
      },
      {
        id: 'expr',
        header: t('alerts.col.expr'),
        cell: ({ row }) => (
          <code className="block max-w-[22rem] truncate font-mono text-xs text-muted" title={row.original.expr}>
            {row.original.expr}
          </code>
        ),
      },
      {
        id: 'for',
        header: t('alerts.col.for'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{row.original.for}</span>,
      },
    ];
    if (mayMute) {
      cols.push({
        id: 'actions',
        header: '',
        cell: ({ row }) => (
          <div className="flex justify-end">
            <MuteButton alertname={row.original.name} onMuted={onMuted} />
          </div>
        ),
      });
    }
    return cols;
  }, [t, mayMute, onMuted]);

  return (
    <Card>
      <CardHeader
        title={t('alerts.rules')}
        aside={rules !== undefined ? <span className="tabular font-mono text-xs text-muted">{rules.length}</span> : undefined}
      />
      {/* Правила НЕ сужаются по проекту — говорим это вслух, иначе отсутствие
          сужения на фоне сужённых секций прочтётся как забывчивость панели. */}
      {narrowed && <p className="border-b border-line px-4 py-2 text-xs text-muted">{t('alerts.rules.platformNote')}</p>}
      {loading ? (
        <LoadingRow />
      ) : soft !== null ? (
        <EmptyState>{soft}</EmptyState>
      ) : error !== undefined && rules === undefined ? (
        <div className="p-4">
          <ErrorNote error={error} retry={reload} />
        </div>
      ) : (
        <DataTable columns={columns} data={rules ?? []} rowId={(r) => `${r.group}/${r.name}`} empty={t('alerts.noRules')} />
      )}
    </Card>
  );
}

// --- История ---

function HistorySection({
  events,
  error,
  loading,
  reload,
  limit,
  setLimit,
  mayMute,
  onMuted,
}: {
  events?: AlertEvent[];
  error?: Error;
  loading: boolean;
  reload: () => void;
  limit: number;
  setLimit: (n: number) => void;
  mayMute: boolean;
  onMuted: () => void;
}) {
  const { t, lang } = useT();
  const fmt = useFormat();
  const columns = useMemo<ColumnDef<AlertEvent, unknown>[]>(() => {
    const cols: ColumnDef<AlertEvent, unknown>[] = [
      {
        id: 'name',
        header: t('alerts.col.alert'),
        cell: ({ row }) => (
          <div className={`min-w-48 max-w-[24rem] ${row.original.muted === true ? 'opacity-60' : ''}`}>
            <div className="flex items-center gap-2">
              <span className="font-mono text-sm font-medium">{row.original.name}</span>
              <PlatformChip alert={row.original} />
              {row.original.muted === true && <MutedChip />}
            </div>
            {alertDescription(row.original, lang) !== '' && (
              <p className="truncate text-[11px] text-muted" title={alertDescription(row.original, lang)}>
                {alertDescription(row.original, lang)}
              </p>
            )}
          </div>
        ),
      },
      {
        id: 'severity',
        header: t('alerts.col.severity'),
        cell: ({ row }) => <StateBadge state={row.original.severity} tone={toneOfSeverity(row.original.severity)} domain="severity" />,
      },
      {
        id: 'where',
        header: t('alerts.col.where'),
        cell: ({ row }) => <span className="font-mono text-xs text-muted">{whereOf(row.original)}</span>,
      },
      {
        id: 'started',
        header: t('alerts.col.started'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{stamp(fmt, row.original.startsAt)}</span>,
      },
      {
        id: 'ended',
        header: t('alerts.col.ended'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{row.original.active ? '—' : stamp(fmt, row.original.endsAt)}</span>,
      },
      {
        id: 'status',
        header: t('alerts.col.status'),
        cell: ({ row }) => <StateBadge state={row.original.active ? 'firing' : 'resolved'} tone={toneOfAlertActive(row.original.active)} domain="alertstate" />,
      },
    ];
    if (mayMute) {
      cols.push({
        id: 'actions',
        header: '',
        cell: ({ row }) =>
          row.original.muted === true ? null : (
            <div className="flex justify-end">
              <MuteButton
                alertname={row.original.name}
                region={row.original.region}
                project={alertProjectOf(row.original)}
                onMuted={onMuted}
              />
            </div>
          ),
      });
    }
    return cols;
  }, [t, fmt, lang, mayMute, onMuted]);

  const select = 'rounded-lg border border-line bg-card px-2.5 py-1.5 text-xs';
  return (
    <Card>
      <CardHeader
        title={t('alerts.history')}
        aside={
          <select
            aria-label={t('alerts.limitAria')}
            className={select}
            value={limit}
            onChange={(e) => {
              setLimit(Number(e.target.value));
            }}
          >
            {HISTORY_LIMITS.map((n) => (
              <option key={n} value={n}>
                {t('alerts.lastN', { count: n })}
              </option>
            ))}
          </select>
        }
      />
      {loading ? (
        <LoadingRow />
      ) : error !== undefined && events === undefined ? (
        <div className="p-4">
          <ErrorNote error={error} retry={reload} />
        </div>
      ) : (
        <DataTable columns={columns} data={events ?? []} rowId={(_e) => rowKey(_e)} empty={t('alerts.noHistory')} />
      )}
    </Card>
  );
}

// startsAt/received_at делают строку уникальной среди повторных доставок.
function rowKey(e: AlertEvent): string {
  return `${e.name}|${e.node}|${e.startsAt}|${e.received_at ?? ''}`;
}

// Пустой/зеро-таймстамп (alertmanager "0001-…") → «—», иначе локальный штамп.
function stamp(fmt: BoundFormat, iso: string): string {
  if (iso === '' || iso.startsWith('0001')) return '—';
  return fmt.stamp(iso);
}
