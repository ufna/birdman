// Флот: таблица тачек с живым heartbeat age; раскрытие строки — дедики
// этой тачки (state, версия, игроки, порт, аптайм).

import { useCallback, useMemo, useState } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import * as Dialog from '@radix-ui/react-dialog';
import { api } from '../lib/api';
import { apiErrorMessage } from '../lib/apiError';
import type { ApiEvent, Environment, GameServer, NodeInfo } from '../lib/api';
import { useData } from '../lib/live';
import { useEnv, keepForEnv } from '../lib/env';
import { useProject, useProjectList } from '../lib/project';
import { canAdmin, useSession } from '../lib/session';
import { EnvTag } from '../components/EnvTag';
import { useServerDrawer } from '../lib/drawer';
import { useNow } from '../lib/useNow';
import { ageOf, heartbeatTone, shortId } from '../lib/format';
import { useT, useFormat } from '../lib/i18n';
import type { I18nContextValue } from '../lib/i18n';
import { DataTable } from '../components/DataTable';
import { StateBadge, toneOfNodeState, toneOfServerState } from '../components/Badge';
import { ConfirmButton } from '../components/ConfirmDialog';
import { useToast } from '../components/Toast';
import { Card, CardHeader, ErrorNote, LoadingRow } from '../components/ui';

const LIVE_SERVER_STATES = new Set(['creating', 'ready', 'allocated', 'draining']);

export function Fleet() {
  const { t, tp } = useT();
  const nodes = useProjectList((project) => api.listNodes({ project }), []);
  const servers = useProjectList((project) => api.listServers({ project }), []);
  const versions = useProjectList((project) => api.listVersions({ project }), []);
  // Причина карантина — из последнего события node_quarantine ноды
  // (server-side фильтра по node_id у /v1/events нет — фильтруем клиентом).
  // Проектного сужения тут не нужно: лента используется как lookup
  // node_id → причина карантина, а сами ноды уже сужены по проекту — событие
  // чужой ноды просто никогда не будет запрошено.
  const events = useData(() => api.listEvents(500), []);
  const { session } = useSession();
  const mayAdmin = session != null && canAdmin(session);
  const { selected, environments } = useEnv();
  const { selected: project } = useProject();
  const [expandedId, setExpandedId] = useState<string | null>(null);
  // useCallback, а не голая функция: `columns` ниже зовёт isProdEnv, и раньше
  // в его deps стоял транзитивный `environments` — то есть замыкание хелпера
  // отслеживалось вручную. Работало, но ровно на этой ручной цепочке и подорвался
  // #948. Мемоизированный хелпер в deps проверяется линтером сам.
  const isProdEnv = useCallback(
    (envName: string) => environments.find((e) => e.name === envName)?.production === true,
    [environments],
  );

  const semverOf = useMemo(
    () => new Map((versions.data ?? []).map((v) => [v.id, v.semver])),
    [versions.data],
  );
  const quarantineReason = useMemo(() => latestQuarantineReasons(events.data ?? []), [events.data]);
  const serversByNode = useMemo(() => {
    const m = new Map<string, GameServer[]>();
    for (const s of servers.data ?? []) {
      const list = m.get(s.node_id) ?? [];
      list.push(s);
      m.set(s.node_id, list);
    }
    for (const list of m.values()) list.sort((a, b) => a.port - b.port);
    return m;
  }, [servers.data]);

  // Выведенные из флота (dead) по умолчанию скрыты: это ноды, про которые
  // оператор уже сказал «бокса нет», и держать их в списке вечно — то же, что
  // не иметь возможности их убрать. Скрываем ТОЛЬКО dead: down и quarantine
  // остаются на виду, их оператор как раз и должен видеть.
  const [showRetired, setShowRetired] = useState(false);
  const allNodes = useMemo(
    () =>
      keepForEnv([...(nodes.data ?? [])], selected, (n) => n.env).sort(
        (a, b) => a.region.localeCompare(b.region) || a.hostname.localeCompare(b.hostname),
      ),
    [nodes.data, selected],
  );
  const retiredCount = useMemo(() => allNodes.filter((n) => n.state === 'dead').length, [allNodes]);
  const sortedNodes = useMemo(
    () => (showRetired ? allNodes : allNodes.filter((n) => n.state !== 'dead')),
    [allNodes, showRetired],
  );

  const columns = useMemo<ColumnDef<NodeInfo, unknown>[]>(
    () => [
      {
        id: 'host',
        header: t('col.node'),
        cell: ({ row }) => (
          <div className="min-w-36">
            <div className="font-medium">{row.original.hostname}</div>
            <div className="font-mono text-xs text-muted">
              {shortId(row.original.id)} · {row.original.public_ip}
            </div>
          </div>
        ),
      },
      { id: 'region', header: t('col.region'), cell: ({ row }) => <span className="font-mono text-xs">{row.original.region}</span> },
      {
        id: 'env',
        header: t('col.env'),
        cell: ({ row }) => <EnvTag env={row.original.env} production={isProdEnv(row.original.env)} />,
      },
      {
        id: 'state',
        header: t('col.state'),
        cell: ({ row }) => (
          <div className="flex flex-col gap-1">
            <StateBadge state={row.original.state} tone={toneOfNodeState(row.original.state)} domain="node" />
            {row.original.state === 'quarantine' && quarantineReason.get(row.original.id) !== undefined && (
              <span className="max-w-40 truncate text-[11px] text-dead" title={quarantineReason.get(row.original.id)}>
                {quarantineReason.get(row.original.id)}
              </span>
            )}
            {row.original.state === 'draining' && (
              <DrainProgress
                playing={(serversByNode.get(row.original.id) ?? []).filter((s) => s.state === 'allocated').length}
              />
            )}
          </div>
        ),
      },
      {
        id: 'slots',
        header: t('col.slots'),
        cell: ({ row }) => (
          <SlotsCell
            busy={(serversByNode.get(row.original.id) ?? []).filter((s) => LIVE_SERVER_STATES.has(s.state)).length}
            total={row.original.capacity_slots}
          />
        ),
      },
      {
        id: 'heartbeat',
        header: t('col.heartbeat'),
        cell: ({ row }) => <HeartbeatCell iso={row.original.last_heartbeat_at} />,
      },
      {
        id: 'agent',
        header: t('col.agent'),
        cell: ({ row }) => (
          <span className="font-mono text-xs text-muted">
            {row.original.agent_version !== '' ? row.original.agent_version : '—'}
          </span>
        ),
      },
      {
        id: 'actions',
        header: '',
        cell: ({ row }) =>
          mayAdmin ? <NodeActions node={row.original} onDone={nodes.reload} /> : null,
      },
      {
        id: 'chevron',
        header: '',
        cell: ({ row }) => (
          <svg
            viewBox="0 0 16 16"
            aria-hidden
            className={`size-3.5 text-muted transition-transform ${expandedId === row.original.id ? 'rotate-90' : ''}`}
          >
            <path d="M6 4l4 4-4 4" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
          </svg>
        ),
      },
    ],
    [t, serversByNode, expandedId, quarantineReason, mayAdmin, nodes.reload, isProdEnv],
  );

  const error = nodes.error ?? servers.error ?? versions.error;
  if (error !== undefined && nodes.data === undefined) {
    return <ErrorNote error={error} retry={nodes.reload} />;
  }

  return (
    <Card>
      <CardHeader
        title={t('nav.fleet')}
        aside={
          <div className="flex items-center gap-3">
            {/* Чекбокс появляется только когда выведенные ноды есть: иначе он
                предлагал бы показать пустоту и мозолил глаза на здоровом флоте. */}
            {retiredCount > 0 && (
              <label className="flex cursor-pointer items-center gap-1.5 text-xs text-muted">
                <input
                  type="checkbox"
                  checked={showRetired}
                  onChange={(e) => {
                    setShowRetired(e.target.checked);
                  }}
                  className="size-3.5 accent-[var(--accent)]"
                />
                {tp('fleet.showRetired', retiredCount)}
              </label>
            )}
            <span className="font-mono text-xs text-muted">{tp('fleet.nodesCount', sortedNodes.length)}</span>
          </div>
        }
      />
      {nodes.data === undefined ? (
        <LoadingRow />
      ) : (
        <DataTable
          columns={columns}
          data={sortedNodes}
          rowId={(n) => n.id}
          empty={
            <>
              {project !== null ? t('fleet.emptyInProject', { project }) : t('fleet.emptyPre')}{' '}
              <span className="font-mono">POST /v1/nodes</span>
            </>
          }
          expandedId={expandedId}
          onRowClick={(n) => {
            setExpandedId((cur) => (cur === n.id ? null : n.id));
          }}
          rowLabel={(n) => t('fleet.toggleNode', { host: n.hostname })}
          renderExpanded={(n) => (
            <NodeServers servers={serversByNode.get(n.id) ?? []} semverOf={semverOf} />
          )}
        />
      )}
    </Card>
  );
}

function SlotsCell({ busy, total }: { busy: number; total: number }) {
  const ratio = total > 0 ? Math.min(1, busy / total) : 0;
  return (
    <div className="flex items-center gap-2">
      <span className="tabular font-mono text-xs">
        {busy}/{total}
      </span>
      <span className="h-1.5 w-16 overflow-hidden rounded-full bg-line" aria-hidden>
        <span
          className={`block h-full rounded-full ${ratio >= 1 ? 'bg-warn' : 'bg-accent'}`}
          style={{ width: `${String(Math.round(ratio * 100))}%` }}
        />
      </span>
    </div>
  );
}

/** Drain/Undrain + перевод в другой env (admin, confirm). stopPropagation —
 *  чтобы клик по кнопке не разворачивал строку. У dead-ноды действий нет:
 *  мастер откажет и в дрейне, и в переводе (409). */
function NodeActions({ node, onDone }: { node: NodeInfo; onDone: () => void }) {
  const { t } = useT();
  const { environments } = useEnv();
  if (node.state === 'dead') return null;
  const draining = node.state === 'draining';
  // Цели перевода — все окружения проекта, кроме текущего. Список недоступен/
  // пуст → переводить некуда, кнопки нет (как и Promote на Deploys).
  const moveTargets = environments.filter((e) => e.name !== node.env);
  return (
    <div
      onClick={(e) => {
        e.stopPropagation();
      }}
      className="flex justify-end gap-2"
    >
      {/* Вывод из флота — только у ноды, которая уже не работает: active-ноду
          сначала выводят дрейном (иначе ревокация обрывает живые матчи, и мастер
          всё равно ответит 409). Тон dead: действие терминальное, обратного
          пути из панели нет. */}
      {node.state !== 'active' && (
        <ConfirmButton
          label={t('fleet.revoke')}
          tone="dead"
          title={t('fleet.revoke.title', { host: node.hostname })}
          description={t('fleet.revoke.desc')}
          confirmLabel={t('fleet.revoke')}
          onConfirm={async () => {
            await api.revokeNode(node.id);
            onDone();
          }}
        />
      )}
      {moveTargets.length > 0 && <MoveEnvDialog node={node} targets={moveTargets} onDone={onDone} />}
      {draining ? (
        <ConfirmButton
          label={t('fleet.undrain')}
          title={t('fleet.undrain.title', { host: node.hostname })}
          description={t('fleet.undrain.desc')}
          confirmLabel={t('fleet.undrain')}
          onConfirm={async () => {
            await api.undrainNode(node.id);
            onDone();
          }}
        />
      ) : (
        <ConfirmButton
          label={t('fleet.drain')}
          tone="dead"
          title={t('fleet.drain.title', { host: node.hostname })}
          description={t('fleet.drain.desc')}
          confirmLabel={t('fleet.drain')}
          onConfirm={async () => {
            await api.drainNode(node.id);
            onDone();
          }}
        />
      )}
    </div>
  );
}

/**
 * Перевод ноды в другое окружение (environments v1 §2): PATCH /v1/nodes/{id}
 * {env}, admin. Цели — окружения проекта, кроме текущего. Мастер не двигает
 * ноду с живыми дедиками (409 «has live servers, drain it first») — показываем
 * это инлайн внятной подсказкой «сначала Drain», а не сырым текстом ошибки;
 * 400 показываем как есть (detail мастера).
 */
function MoveEnvDialog({ node, targets, onDone }: { node: NodeInfo; targets: Environment[]; onDone: () => void }) {
  const { t } = useT();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [target, setTarget] = useState(targets[0]?.name ?? '');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = () => {
    if (target === '' || pending) return;
    setPending(true);
    setError(null);
    api
      .setNodeEnv(node.id, target)
      .then(() => {
        setPending(false);
        setOpen(false);
        toast.success(t('fleet.moveEnv.toast', { host: node.hostname, env: target }));
        onDone();
      })
      .catch((e: unknown) => {
        setPending(false);
        setError(moveErrorText(e, t));
      });
  };

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return; // не закрывать на время запроса
        setOpen(o);
        if (o) {
          setTarget(targets[0]?.name ?? '');
          setError(null);
        }
      }}
    >
      <Dialog.Trigger asChild>
        <button
          type="button"
          className="rounded-lg border border-line px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:text-ink"
        >
          {t('fleet.moveEnv')}
        </button>
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content className="fixed top-1/2 left-1/2 z-50 w-[min(28rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl">
          <Dialog.Title className="text-base font-semibold">
            {t('fleet.moveEnv.title', { host: node.hostname })}
          </Dialog.Title>
          <Dialog.Description className="mt-1 text-sm text-muted">{t('fleet.moveEnv.desc')}</Dialog.Description>
          <label className="mt-4 flex flex-col gap-1 text-sm font-medium">
            {t('fleet.moveEnv.target')}
            <select
              value={target}
              onChange={(e) => {
                setTarget(e.target.value);
              }}
              className="rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal"
            >
              {targets.map((e) => (
                <option key={e.name} value={e.name}>
                  {e.name}
                </option>
              ))}
            </select>
          </label>
          <p className="mt-1.5 font-mono text-xs text-muted">{t('fleet.moveEnv.current', { env: node.env })}</p>
          {error !== null && (
            <p role="alert" className="mt-3 rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
              {error}
            </p>
          )}
          <div className="mt-5 flex justify-end gap-2">
            <Dialog.Close asChild>
              <button
                type="button"
                disabled={pending}
                className="rounded-lg border border-line px-3 py-1.5 text-sm text-muted hover:text-ink disabled:opacity-40"
              >
                {t('common.cancel')}
              </button>
            </Dialog.Close>
            <button
              type="button"
              disabled={target === '' || pending}
              onClick={submit}
              className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
            >
              {pending ? t('common.running') : t('fleet.moveEnv.confirm')}
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/** Текст ошибки перевода: 409 (живые дедики / dead-нода) → подсказка про drain;
 *  прочее API — общий словарь (#1005: раньше хвост печатал detail мастера как
 *  есть); не-API — общий текст. */
function moveErrorText(e: unknown, t: I18nContextValue['t']): string {
  return apiErrorMessage(e, t, {
    forbidden: 'confirm.err.forbidden',
    generic: 'fleet.moveEnv.err.generic',
    byStatus: { 400: 'ui.err.badRequest', 409: 'fleet.moveEnv.err.conflict' },
  });
}

/** Индикация опустошения draining-ноды: сколько allocated ещё доигрывает. */
function DrainProgress({ playing }: { playing: number }) {
  const { t, tp } = useT();
  return (
    <span className="font-mono text-[11px] text-warn">
      {playing > 0 ? tp('fleet.drain.playing', playing) : t('fleet.drain.empty')}
    </span>
  );
}

/** node_id → причина последнего карантина (payload.reason события node_quarantine). */
function latestQuarantineReasons(events: ApiEvent[]): Map<string, string> {
  const m = new Map<string, string>();
  // events отсортированы новыми вперёд — первое совпадение и есть последнее.
  for (const e of events) {
    if (e.kind !== 'node_quarantine' || e.node_id === undefined || m.has(e.node_id)) continue;
    const reason = e.payload.reason ?? e.payload.detail;
    if (typeof reason === 'string') m.set(e.node_id, reason);
  }
  return m;
}

/** Живой возраст heartbeat: тикает каждую секунду, тон — по свежести. */
export function HeartbeatCell({ iso }: { iso?: string }) {
  const now = useNow();
  const { t } = useT();
  const fmt = useFormat();
  const age = ageOf(iso, now);
  const tone = heartbeatTone(age);
  const toneText = { good: 'text-good', warn: 'text-warn', dead: 'text-dead' }[tone];
  return (
    <span className={`inline-flex items-center gap-1.5 font-mono text-xs ${toneText}`}>
      <span aria-hidden className="size-1.5 rounded-full bg-current" />
      <span className="tabular">{age === null ? t('fleet.hb.never') : fmt.ago(age)}</span>
    </span>
  );
}

function NodeServers({ servers, semverOf }: { servers: GameServer[]; semverOf: Map<string, string> }) {
  const now = useNow();
  const { open } = useServerDrawer();
  const { t } = useT();
  const fmt = useFormat();
  if (servers.length === 0) {
    return <p className="py-2 text-xs text-muted">{t('fleet.node.noServers')}</p>;
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-line bg-card">
      <table className="w-full min-w-[560px] text-xs">
        <thead>
          <tr className="border-b border-line text-muted">
            <th className="px-3 py-2 text-left font-medium">{t('col.dedic')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.state')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.version')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.players')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.port')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.uptime')}</th>
            <th className="px-3 py-2 text-left font-medium">{t('col.match')}</th>
          </tr>
        </thead>
        <tbody>
          {servers.map((s) => (
            <tr
              key={s.id}
              onClick={() => {
                open(s.id);
              }}
              className="cursor-pointer border-b border-line transition-colors last:border-0 hover:bg-paper"
              title={t('server.openDetailsFull')}
            >
              <td className="px-3 py-2 font-mono text-accent-ink underline-offset-2 hover:underline">{shortId(s.id)}</td>
              <td className="px-3 py-2">
                <StateBadge state={s.state} tone={toneOfServerState(s.state)} domain="server" />
              </td>
              <td className="px-3 py-2 font-mono">{semverOf.get(s.version_id) ?? shortId(s.version_id)}</td>
              <td className="tabular px-3 py-2 font-mono">{s.players}</td>
              <td className="tabular px-3 py-2 font-mono">{s.port}</td>
              <td className="tabular px-3 py-2 font-mono">{fmt.age(Math.max(0, now - new Date(s.created_at).getTime()))}</td>
              <td className="px-3 py-2 font-mono text-muted">
                {s.match_id !== undefined ? shortId(s.match_id) : '—'}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
