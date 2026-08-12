// Деплои: карточка-инструкция «Как залить билд» (образ в ghcr → deploy-ключ
// → регистрация версии и деплой, готовые curl), таблица версий, окно
// мультиверсий (active + deprecated и сколько дедиков каждой ещё живо),
// прогресс pre-pull по тачкам во время деплоя, кнопки Deploy/Rollback
// (confirm, только при скоупе deploy/admin). Live — через SSE
// (deploy_started/activated/failed/rolled_back). Всё на публичном API:
// GET /v1/versions, /v1/servers, /v1/nodes, POST /v1/deploy, /v1/rollback,
// /v1/events (seed прогресса).

import { useCallback, useEffect, useMemo, useState } from 'react';
import type { ColumnDef } from '@tanstack/react-table';
import * as Dialog from '@radix-ui/react-dialog';
import { api, WARN_NO_LIVE_NODES } from '../lib/api';
import type { ApiEvent, Environment, GameServer, NodeInfo, VersionInfo } from '../lib/api';
import { apiErrorMessage } from '../lib/apiError';
import { useLive } from '../lib/live';
import { useEnv, keepForEnv } from '../lib/env';
import { useProject, useProjectList, useFeedScope, scopeProject } from '../lib/project';
import { canAdmin, canDeploy, useBindingRefusal, useSession } from '../lib/session';
import { shortId } from '../lib/format';
import { useT, useFormat } from '../lib/i18n';
import { buildHowtoCtx } from '../lib/deployHowto';
import { useToast } from '../components/Toast';
import { DataTable } from '../components/DataTable';
import { DeployHowto } from '../components/DeployHowto';
import { StateBadge, toneOfVersionState } from '../components/Badge';
import { ConfirmButton } from '../components/ConfirmDialog';
import { EnvTag } from '../components/EnvTag';
import { Card, CardHeader, ErrorNote, LoadingRow } from '../components/ui';

const LIVE_SERVER_STATES = new Set(['creating', 'ready', 'allocated', 'draining']);
const HEARTBEAT_FRESH_MS = 30_000;

export function Deploys({ navigate }: { navigate: (path: string) => void }) {
  const { t } = useT();
  const versions = useProjectList((project) => api.listVersions({ project }), []);
  const servers = useProjectList((project) => api.listServers({ project }), []);
  const nodes = useProjectList((project) => api.listNodes({ project }), []);
  const progress = useDeployProgress();
  const { session } = useSession();
  const mayDeploy = session != null && canDeploy(session);
  const mayAdmin = session != null && canAdmin(session);
  const { selected, environments, reload: reloadEnvs } = useEnv();
  const { selected: project } = useProject();
  // «Скрывать disabled» по умолчанию (environments v1 §8, M11): при dev-потоке
  // список версий пухнет снятыми ретеншном строками — прячем их за тогглом.
  const [hideDisabled, setHideDisabled] = useState(true);

  const liveByVersion = useMemo(() => countLiveByVersion(servers.data ?? []), [servers.data]);
  // Все версии (без env-фильтра) — для резолва provenance promoted_from: источник
  // промоута живёт в ДРУГОМ окружении, чем показанная версия.
  const versionById = useMemo(
    () => new Map((versions.data ?? []).map((v) => [v.id, v])),
    [versions.data],
  );

  const error = versions.error ?? servers.error ?? nodes.error;
  if (error !== undefined && versions.data === undefined) {
    return <ErrorNote error={error} retry={versions.reload} />;
  }
  if (versions.data === undefined) return <LoadingRow />;

  // Клиентский фильтр по выбранному env (environments v1 §8); «All» → всё.
  const shownVersions = keepForEnv(versions.data, selected, (v) => v.env);
  // Ёмкость проекта в этом срезе (tracker #1071). `dead` не считаем: это тачки,
  // выведенные из флота руками оператора, — бокса за ними уже нет. `undefined`
  // (список не приехал/отвалился) — НЕ ноль: иначе сбой запроса рисовал бы
  // баннер «нод нет» проекту с полным флотом.
  const liveNodes =
    nodes.data === undefined ? undefined : keepForEnv(nodes.data, selected, (n) => n.env).filter((n) => n.state !== 'dead').length;
  const projects = groupByProject(shownVersions);
  const isEmpty = projects.length === 0;
  const howtoCtx = buildHowtoCtx(window.location.origin, groupByProject(versions.data));
  const prodEnvs = environments.filter((e) => e.production);
  // Карточка настроек env — только для конкретно выбранного env и admin.
  const selectedEnvObj = selected !== null ? environments.find((e) => e.name === selected) : undefined;

  const reload = () => {
    versions.reload();
    servers.reload();
  };

  return (
    <div className="flex flex-col gap-4">
      {isEmpty && <p className="text-sm text-muted">{selected !== null ? t('deploys.emptyEnv') : project !== null ? t('deploys.emptyInProject', { project }) : t('deploys.emptyPre')}</p>}
      {mayAdmin && selectedEnvObj !== undefined && (
        <EnvSettingsCard env={selectedEnvObj} onSaved={reloadEnvs} />
      )}
      <DeployHowto ctx={howtoCtx} navigate={navigate} defaultExpanded={isEmpty && selected === null} />
      {/* Имя проекта в заголовке карточки — ТОЛЬКО когда оно что-то различает.
          После серверного сужения (W2) группа всегда одна, и её заголовок
          дублировал бы селектор проекта в шапке (поймано ревью #948). */}
      {projects.map(({ project, versions: pv }) => (
        <ProjectDeploys
          key={project}
          project={project}
          showProject={projects.length > 1}
          versions={pv}
          servers={servers.data ?? []}
          nodes={nodes.data ?? []}
          liveByVersion={liveByVersion}
          progress={progress}
          mayDeploy={mayDeploy}
          environments={environments}
          prodEnvs={prodEnvs}
          versionById={versionById}
          hideDisabled={hideDisabled}
          setHideDisabled={setHideDisabled}
          liveNodes={liveNodes}
          navigate={navigate}
          reload={reload}
        />
      ))}
    </div>
  );
}

function ProjectDeploys({
  project,
  showProject,
  versions,
  servers,
  nodes,
  liveByVersion,
  progress,
  mayDeploy,
  environments,
  prodEnvs,
  versionById,
  hideDisabled,
  setHideDisabled,
  liveNodes,
  navigate,
  reload,
}: {
  project: string;
  showProject: boolean;
  versions: VersionInfo[];
  servers: GameServer[];
  nodes: NodeInfo[];
  liveByVersion: Map<string, number>;
  progress: Map<string, DeployProgress>;
  mayDeploy: boolean;
  environments: Environment[];
  prodEnvs: Environment[];
  versionById: Map<string, VersionInfo>;
  hideDisabled: boolean;
  setHideDisabled: (v: boolean) => void;
  /** Сколько не-`dead` тачек у проекта в текущем срезе окружения; `undefined` —
   *  список нод ещё не приехал (см. NoNodesBanner). */
  liveNodes: number | undefined;
  navigate: (path: string) => void;
  reload: () => void;
}) {
  const { t, tp } = useT();
  const fmt = useFormat();
  const toast = useToast();
  const active = versions.find((v) => v.state === 'active');
  const deprecated = versions.filter((v) => v.state === 'deprecated');
  const prepulling = versions.filter((v) => v.state === 'prepulling');
  const regionActive = useMemo(() => deriveRegionActive(servers, versions), [servers, versions]);
  const isProdEnv = useCallback(
    (envName: string) => environments.find((e) => e.name === envName)?.production === true,
    [environments],
  );
  const tableVersions = useMemo(
    () => (hideDisabled ? versions.filter((v) => v.state !== 'disabled') : versions),
    [versions, hideDisabled],
  );
  const disabledCount = versions.length - versions.filter((v) => v.state !== 'disabled').length;

  const columns = useMemo<ColumnDef<VersionInfo, unknown>[]>(
    () => [
      {
        id: 'semver',
        header: t('col.version'),
        cell: ({ row }) => (
          <div>
            <div className="font-mono font-medium">{row.original.semver}</div>
            <Provenance version={row.original} versionById={versionById} />
          </div>
        ),
      },
      {
        id: 'env',
        header: t('col.env'),
        cell: ({ row }) => <EnvTag env={row.original.env} production={isProdEnv(row.original.env)} />,
      },
      {
        id: 'state',
        header: t('col.state'),
        cell: ({ row }) => <StateBadge state={row.original.state} tone={toneOfVersionState(row.original.state)} domain="version" />,
      },
      {
        id: 'live',
        header: t('deploys.col.liveDedics'),
        cell: ({ row }) => {
          const n = liveByVersion.get(row.original.id) ?? 0;
          return <span className="tabular font-mono text-xs">{n > 0 ? n : '—'}</span>;
        },
      },
      {
        id: 'created',
        header: t('deploys.col.created'),
        cell: ({ row }) => <span className="tabular font-mono text-xs text-muted">{fmt.stamp(row.original.created_at)}</span>,
      },
      {
        id: 'deprecated',
        header: t('deploys.col.deprecated'),
        cell: ({ row }) => (
          <span className="tabular font-mono text-xs text-muted">
            {row.original.deprecated_at !== undefined ? fmt.stamp(row.original.deprecated_at) : '—'}
          </span>
        ),
      },
      {
        id: 'actions',
        header: '',
        cell: ({ row }) =>
          mayDeploy ? (
            <VersionActions
              version={row.original}
              envsKnown={environments.length > 0}
              prodEnvs={prodEnvs}
              isProdEnv={isProdEnv}
              onDone={reload}
            />
          ) : null,
      },
    ],
    [t, fmt, liveByVersion, mayDeploy, environments, prodEnvs, isProdEnv, versionById, reload],
  );

  return (
    <Card>
      <CardHeader
        title={showProject ? t('deploys.project', { project }) : t('nav.deploys')}
        aside={
          <div className="flex flex-wrap items-center gap-3">
            <label className="flex items-center gap-1.5 text-xs text-muted">
              <input
                type="checkbox"
                checked={hideDisabled}
                onChange={(e) => {
                  setHideDisabled(e.target.checked);
                }}
              />
              {t('deploys.hideDisabled')}
              {disabledCount > 0 && <span className="tabular font-mono">({disabledCount})</span>}
            </label>
            {mayDeploy && deprecated.length > 0 ? (
              <ConfirmButton
                label={t('deploys.rollback')}
                tone="dead"
                title={t('deploys.rollback.title', { project })}
                description={t('deploys.rollback.desc', { semver: deprecated[0].semver })}
                confirmLabel={t('deploys.rollback')}
                onConfirm={async () => {
                  const res = await api.rollback({ project, env: deprecated[0].env });
                  toast.success(t('deploys.toast.rolledBack', { project, semver: res.version.semver }));
                  reload();
                }}
              />
            ) : (
              <span className="font-mono text-xs text-muted">{tp('deploys.versionsCount', versions.length)}</span>
            )}
          </div>
        }
      />

      {active !== undefined && liveNodes === 0 && (
        <NoNodesBanner project={project} semver={active.semver} navigate={navigate} />
      )}

      <div className="grid gap-4 border-b border-line p-4 md:grid-cols-2">
        <WindowCard active={active} deprecated={deprecated} liveByVersion={liveByVersion} />
        <RegionActiveCard regionActive={regionActive} activeSemver={active?.semver} />
      </div>

      {prepulling.map((v) => (
        <PrepullBar key={v.id} version={v} progress={progress.get(v.id)} nodes={nodes} />
      ))}

      <DataTable
        columns={columns}
        data={tableVersions}
        rowId={(v) => v.id}
        empty={t('deploys.emptyProject')}
      />
    </Card>
  );
}

/** Provenance-строка: «promoted from <env>/<semver>» по promoted_from. Источник
 *  ищем среди всех версий; не нашли — показываем сам факт промоута без id. */
function Provenance({ version, versionById }: { version: VersionInfo; versionById: Map<string, VersionInfo> }) {
  const { t } = useT();
  if (version.promoted_from === undefined) return null;
  const src = versionById.get(version.promoted_from);
  const label =
    src !== undefined
      ? t('deploys.provenance', { from: `${src.env}/${src.semver}` })
      : t('deploys.provenance.unknown');
  return <div className="font-mono text-[11px] text-muted">↳ {label}</div>;
}

/** Действия строки версии: Deploy (registered/deprecated) + Promote (версии
 *  non-production env, если есть production-цель). prepulling → «warming». */
function VersionActions({
  version,
  envsKnown,
  prodEnvs,
  isProdEnv,
  onDone,
}: {
  version: VersionInfo;
  /** Список окружений загружен и не пуст. Пустой/недоступный список делает
   *  isProdEnv() слепым (всё «не production») — Promote в этом случае гейтим. */
  envsKnown: boolean;
  prodEnvs: Environment[];
  isProdEnv: (env: string) => boolean;
  onDone: () => void;
}) {
  const { t } = useT();
  const toast = useToast();
  if (version.state === 'prepulling') {
    return <span className="font-mono text-xs text-warn">{t('deploys.warming')}</span>;
  }
  const canDeployState = version.state === 'registered' || version.state === 'deprecated';
  // Цели промоута: production-окружения, КРОМЕ окружения самой версии (промоут
  // «в себя» бессмыслен; страховка на случай рассинхрона флага production).
  const targets = prodEnvs.filter((e) => e.name !== version.env);
  // Промоут доступен у deployable-состояний в non-production env (не для
  // disabled) — и только когда список окружений реально известен (follow-up p5:
  // при пустом/недоступном списке isProdEnv() врал «не production» даже для
  // prod-версий, и кнопка открывала диалог без целей).
  const canPromote =
    envsKnown &&
    !isProdEnv(version.env) &&
    (version.state === 'registered' || version.state === 'active' || version.state === 'deprecated');
  if (!canDeployState && !canPromote) return null;
  return (
    <div className="flex justify-end gap-2">
      {canPromote && <PromoteDialog version={version} prodEnvs={targets} onDone={onDone} />}
      {canDeployState && (
        <ConfirmButton
          label={t('deploys.deploy')}
          title={t('deploys.deploy.title', { semver: version.semver })}
          description={t('deploys.deploy.desc')}
          confirmLabel={t('deploys.deploy')}
          onConfirm={async () => {
            const st = await api.deploy(version.id);
            // Мастер сказал, что катить было не на что (tracker #1071) — зелёный
            // «успех» здесь и был той самой ложью: флип состоялся, дедиков не
            // будет. Говорим это в момент действия, а не оставляем догадываться.
            if (st.warning === WARN_NO_LIVE_NODES) {
              toast.info(t('deploys.toast.deployedNoNodes', { semver: version.semver }));
            } else {
              toast.success(t('deploys.toast.deployed', { semver: version.semver }));
            }
            onDone();
          }}
        />
      )}
    </div>
  );
}

/** Диалог промоута: выбор целевого production-env + confirm → POST /v1/promote. */
function PromoteDialog({
  version,
  prodEnvs,
  onDone,
}: {
  version: VersionInfo;
  prodEnvs: Environment[];
  onDone: () => void;
}) {
  const { t } = useT();
  const toast = useToast();
  // Промоут привязанного ключа отказывает ВСЕГДА (master deploy.go:164 —
  // requireBinding на целевой env), поэтому 403 тут штатный путь, а не край.
  const bound = useBindingRefusal('deploy');
  const [open, setOpen] = useState(false);
  const [target, setTarget] = useState(prodEnvs[0]?.name ?? '');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const noTargets = prodEnvs.length === 0;

  const submit = () => {
    if (target === '' || pending) return;
    setPending(true);
    setError(null);
    api
      .promote(version.id, target)
      .then(() => {
        setPending(false);
        setOpen(false);
        toast.success(t('deploys.promote.toast', { semver: version.semver, env: target }));
        onDone();
      })
      .catch((e: unknown) => {
        setPending(false);
        setError(
          apiErrorMessage(e, t, {
            refusal: bound,
            forbidden: 'confirm.err.forbidden',
            generic: 'access.environments.create.err',
            byStatus: { 400: 'ui.err.badRequest' },
          }),
        );
      });
  };

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return;
        setOpen(o);
        if (o) {
          setTarget(prodEnvs[0]?.name ?? '');
          setError(null);
        }
      }}
    >
      <Dialog.Trigger asChild>
        <button
          type="button"
          className="rounded-lg border border-line px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:text-ink"
        >
          {t('deploys.promote')}
        </button>
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content className="fixed top-1/2 left-1/2 z-50 w-[min(28rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl">
          <Dialog.Title className="text-base font-semibold">
            {t('deploys.promote.title', { semver: version.semver, from: version.env })}
          </Dialog.Title>
          <Dialog.Description className="mt-1 text-sm text-muted">{t('deploys.promote.desc', { semver: version.semver })}</Dialog.Description>
          {noTargets ? (
            <p className="mt-4 rounded-lg bg-paper px-3 py-2 text-xs text-muted">{t('deploys.promote.noTargets')}</p>
          ) : (
            <label className="mt-4 flex flex-col gap-1 text-sm font-medium">
              {t('deploys.promote.target')}
              <select
                value={target}
                onChange={(e) => {
                  setTarget(e.target.value);
                }}
                className="rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal"
              >
                {prodEnvs.map((e) => (
                  <option key={e.name} value={e.name}>
                    {e.name}
                  </option>
                ))}
              </select>
            </label>
          )}
          {error !== null && (
            <p role="alert" className="mt-3 rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
              {error}
            </p>
          )}
          <div className="mt-5 flex justify-end gap-2">
            <Dialog.Close asChild>
              <button type="button" className="rounded-lg border border-line px-3 py-1.5 text-sm text-muted hover:text-ink">
                {t('common.cancel')}
              </button>
            </Dialog.Close>
            <button
              type="button"
              disabled={noTargets || target === '' || pending}
              onClick={submit}
              className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
            >
              {pending ? t('common.running') : t('deploys.promote.confirm')}
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/** Карточка настроек выбранного env (admin): auto_deploy + retention_keep → PATCH.
 *  Guardrail: production-env не может включить auto_deploy (чекбокс заблокирован;
 *  API тоже вернёт 400). */
function EnvSettingsCard({ env, onSaved }: { env: Environment; onSaved: () => void }) {
  const { t } = useT();
  const toast = useToast();
  // PATCH окружения гейтится admin'ом, а admin с привязкой несовместим
  // (store/apikeys.go:78) — bound здесь всегда undefined. Передаём его всё
  // равно: поверхность не должна знать про сегодняшнюю форму гейта мастера.
  const bound = useBindingRefusal('deploy');
  const [autoDeploy, setAutoDeploy] = useState(env.auto_deploy);
  const [retention, setRetention] = useState(String(env.retention_keep));
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Пересинхронизировать поля при смене выбранного env (перемонтирование по key
  // не гарантируем — env-объект может обновиться после reload).
  useEffect(() => {
    setAutoDeploy(env.auto_deploy);
    setRetention(String(env.retention_keep));
    setError(null);
  }, [env.name, env.auto_deploy, env.retention_keep]);

  const retentionNum = Number(retention);
  const retentionOk = Number.isInteger(retentionNum) && retentionNum >= 0;
  const dirty = autoDeploy !== env.auto_deploy || (retentionOk && retentionNum !== env.retention_keep);

  const save = () => {
    if (!retentionOk || pending || !dirty) return;
    setPending(true);
    setError(null);
    api
      .patchEnvironment(env.project, env.name, { auto_deploy: autoDeploy, retention_keep: retentionNum })
      .then(() => {
        setPending(false);
        toast.success(t('deploys.env.toast', { env: env.name }));
        onSaved();
      })
      .catch((e: unknown) => {
        setPending(false);
        setError(
          apiErrorMessage(e, t, {
            refusal: bound,
            forbidden: 'confirm.err.forbidden',
            generic: 'access.environments.create.err',
            byStatus: { 400: 'ui.err.badRequest' },
          }),
        );
      });
  };

  return (
    <Card>
      <CardHeader
        title={t('deploys.env.settingsFor', { env: env.name })}
        aside={<EnvTag env={env.name} production={env.production} />}
      />
      <div className="flex flex-col gap-3 p-4">
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={autoDeploy}
            disabled={env.production}
            onChange={(e) => {
              setAutoDeploy(e.target.checked);
            }}
          />
          <span>{t('deploys.env.autoDeploy')}</span>
        </label>
        <p className="text-xs text-muted">{t('deploys.env.autoDeployHint')}</p>
        <label className="flex flex-col gap-1 text-sm font-medium">
          {t('deploys.env.retention')}
          <input
            type="number"
            min={0}
            value={retention}
            onChange={(e) => {
              setRetention(e.target.value);
            }}
            className="w-28 rounded-lg border border-line bg-paper px-3 py-2 text-sm font-normal"
          />
        </label>
        <p className="text-xs text-muted">{t('deploys.env.retentionHint')}</p>
        {error !== null && (
          <p role="alert" className="rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
            {error}
          </p>
        )}
        <div>
          <button
            type="button"
            disabled={!retentionOk || pending || !dirty}
            onClick={save}
            className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
          >
            {pending ? t('common.running') : t('deploys.env.save')}
          </button>
        </div>
      </div>
    </Card>
  );
}

/**
 * Недостающая ёмкость проекта (tracker #1071): версия активна, а живых тачек у
 * проекта ноль — дедиков не будет. Раньше система об этом молчала везде, и
 * причину («нода принадлежит РОВНО одному проекту», #1064/#1065) приходилось
 * искать запросом `GET /v1/nodes?project=…`.
 *
 * Почему баннер здесь, а не только на Флоте (там пустое состояние уже есть):
 * вопрос «а где дедики?» рождается на ЭТОМ экране — соседние карточки под
 * баннером рисуют «active {semver} · 0 дедиков» и «Живых дедиков нет», то есть
 * верный факт без причины. Алерт заменить это не может (проверено по
 * `infra/roles/birdman_monitoring_dev/templates/rules.yml.j2`): `NodeDown` висит
 * на heartbeat СУЩЕСТВУЮЩЕЙ ноды, `BufferEmptyAllocFail` — на попытках
 * аллокации, которых при нуле нод никто не делает, а `BufferEmptyReady*` здесь
 * как раз загорится (мастер эмитит явный нулевой ready по флиту с
 * `buffer_ready > 0`, #960) — но скажет «регион без ready-серверов» в
 * алерт-канал, то есть назовёт симптом и не тому, кто сейчас смотрит на экран.
 */
function NoNodesBanner({
  project,
  semver,
  navigate,
}: {
  project: string;
  semver: string;
  navigate: (path: string) => void;
}) {
  const { t } = useT();
  return (
    <div role="status" className="border-b border-line bg-warn-bg/60 px-4 py-3">
      <p className="text-sm font-medium text-warn">{t('deploys.noNodes.title')}</p>
      <p className="mt-1 text-xs text-muted">{t('deploys.noNodes.body', { semver, project })}</p>
      <button
        type="button"
        onClick={() => {
          navigate('/fleet');
        }}
        className="mt-2 rounded-lg border border-line bg-card px-2.5 py-1 text-xs font-medium text-ink transition-colors hover:text-accent-ink"
      >
        {t('deploys.noNodes.cta')}
      </button>
    </div>
  );
}

/** Окно мультиверсий: active + deprecated и сколько дедиков каждой ещё живо. */
function WindowCard({
  active,
  deprecated,
  liveByVersion,
}: {
  active?: VersionInfo;
  deprecated: VersionInfo[];
  liveByVersion: Map<string, number>;
}) {
  const { t, tp } = useT();
  const rows: { v: VersionInfo; role: string }[] = [];
  if (active !== undefined) rows.push({ v: active, role: 'active' });
  for (const d of deprecated) rows.push({ v: d, role: 'deprecated' });
  return (
    <div className="rounded-lg border border-line bg-paper p-3">
      <div className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">{t('deploys.window')}</div>
      {rows.length === 0 ? (
        <p className="text-sm text-muted">{t('deploys.noActive')}</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {rows.map(({ v, role }) => (
            <li key={v.id} className="flex items-center justify-between gap-3">
              <span className="flex items-center gap-2">
                <StateBadge state={role} tone={toneOfVersionState(role)} domain="version" />
                <span className="font-mono text-sm">{v.semver}</span>
              </span>
              <span className="tabular font-mono text-xs text-muted">{tp('deploys.dedicsCount', liveByVersion.get(v.id) ?? 0)}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** Активная версия по регионам — выведено из живых дедиков (GET /v1/fleets в
 *  API нет; TODO — точный per-region active_version из флит-конфигов). */
function RegionActiveCard({
  regionActive,
  activeSemver,
}: {
  regionActive: { region: string; semver: string; count: number; extra: number }[];
  activeSemver?: string;
}) {
  const { t, tp } = useT();
  return (
    <div className="rounded-lg border border-line bg-paper p-3">
      <div className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">{t('deploys.regionActive')}</div>
      {regionActive.length === 0 ? (
        <p className="text-sm text-muted">{t('deploys.noLiveDedics')}</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {regionActive.map((r) => (
            <li key={r.region} className="flex items-center justify-between gap-3">
              <span className="font-mono text-sm">{r.region}</span>
              <span className="flex items-center gap-2">
                <span className={`font-mono text-sm ${r.semver === activeSemver ? 'text-good' : 'text-warn'}`}>
                  {r.semver}
                </span>
                {r.extra > 0 && <span className="font-mono text-xs text-muted">+{r.extra}</span>}
                <span className="tabular font-mono text-xs text-muted">{tp('deploys.dedicsCount', r.count)}</span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** Прогресс pre-pull по тачкам: спрогретые ноды из событий deploy_node_pulled
 *  + remaining из последнего события; total = sprogreto + remaining, либо
 *  оценка по живым нодам, пока событий нет. */
function PrepullBar({ version, progress, nodes }: { version: VersionInfo; progress?: DeployProgress; nodes: NodeInfo[] }) {
  const { t } = useT();
  const pulled = progress?.pulled.size ?? 0;
  const estimate = nodes.filter(
    (n) => n.state === 'active' && n.last_heartbeat_at !== undefined && Date.now() - new Date(n.last_heartbeat_at).getTime() < HEARTBEAT_FRESH_MS,
  ).length;
  const total = progress?.remaining != null ? pulled + progress.remaining : Math.max(pulled, estimate);
  const ratio = total > 0 ? Math.min(1, pulled / total) : 0;

  return (
    <div className="border-b border-line bg-warn-bg/40 px-4 py-3">
      <div className="mb-2 flex items-center justify-between gap-3">
        <span className="text-sm">
          {t('deploys.prepullPre')} <span className="font-mono font-medium">{version.semver}</span> {t('deploys.prepullSuffix')}
        </span>
        <span className="tabular font-mono text-xs text-muted">
          {t('deploys.prepull.nodes', { pulled, total: total > 0 ? total : '?' })}
        </span>
      </div>
      <span className="block h-1.5 w-full overflow-hidden rounded-full bg-line" aria-hidden>
        <span className="block h-full rounded-full bg-warn transition-all" style={{ width: `${String(Math.round(ratio * 100))}%` }} />
      </span>
      {pulled > 0 && (
        <div className="mt-2 flex flex-wrap gap-1">
          {[...(progress?.pulled ?? [])].map((nid) => (
            <span key={nid} className="rounded border border-line bg-card px-1.5 py-0.5 font-mono text-[11px] text-muted">
              {shortId(nid)}
            </span>
          ))}
        </div>
      )}
      {progress === undefined && (
        <p className="mt-2 text-xs text-muted">{t('deploys.prepull.waiting')}</p>
      )}
    </div>
  );
}

// --- helpers ---

interface DeployProgress {
  pulled: Set<string>;
  remaining: number | null;
}

/**
 * Live-трекер pre-pull по версиям: seed из /v1/events + обновление по SSE.
 * deploy_started обнуляет прогресс версии, deploy_node_pulled добавляет тачку
 * и запоминает remaining, activated/failed/rolled_back закрывают прогресс.
 */
function useDeployProgress(): Map<string, DeployProgress> {
  const { subscribe } = useLive();
  const [map, setMap] = useState<Map<string, DeployProgress>>(new Map());

  const apply = useCallback((e: ApiEvent, m: Map<string, DeployProgress>) => {
    const vid = e.version_id;
    if (vid === undefined) return;
    switch (e.kind) {
      case 'deploy_started':
        m.set(vid, { pulled: new Set(), remaining: null });
        break;
      case 'deploy_node_pulled': {
        const cur = m.get(vid) ?? { pulled: new Set<string>(), remaining: null };
        if (e.node_id !== undefined) cur.pulled.add(e.node_id);
        const rem = e.payload.remaining;
        cur.remaining = typeof rem === 'number' ? rem : cur.remaining;
        m.set(vid, cur);
        break;
      }
      case 'deploy_activated':
      case 'deploy_failed':
      case 'deploy_rolled_back':
        m.delete(vid);
        break;
    }
  }, []);

  // Сужаем по проекту (tracker #1024): окно 1000 делилось между проектами, и
  // прогресс СВОЕЙ версии переставал сидироваться, когда события соседа
  // выталкивали свои за край. `wait` — не запрашиваем вовсе, иначе в это окно
  // уедет ровно голый запрос.
  const feedScope = useFeedScope();
  useEffect(() => {
    if (feedScope.kind === 'wait') return;
    let cancelled = false;
    api
      .listEvents(1000, scopeProject(feedScope))
      .then((list) => {
        if (cancelled) return;
        const m = new Map<string, DeployProgress>();
        for (const e of [...list].reverse()) apply(e, m); // oldest → newest
        setMap(m);
      })
      .catch(() => {
        /* прогресс просто не сидируется */
      });
    return () => {
      cancelled = true;
    };
  }, [apply, feedScope]);

  useEffect(
    () =>
      subscribe((e) => {
        setMap((prev) => {
          const m = new Map(prev);
          apply(e.event, m);
          return m;
        });
      }),
    [subscribe, apply],
  );

  return map;
}

function countLiveByVersion(servers: GameServer[]): Map<string, number> {
  const m = new Map<string, number>();
  for (const s of servers) {
    if (LIVE_SERVER_STATES.has(s.state)) m.set(s.version_id, (m.get(s.version_id) ?? 0) + 1);
  }
  return m;
}

function groupByProject(versions: VersionInfo[]): { project: string; versions: VersionInfo[] }[] {
  const m = new Map<string, VersionInfo[]>();
  for (const v of versions) {
    const list = m.get(v.project) ?? [];
    list.push(v);
    m.set(v.project, list);
  }
  return [...m.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([project, vs]) => ({ project, versions: vs }));
}

/** Активная версия по регионам из живых дедиков: доминирующий semver + сколько ещё версий рядом. */
function deriveRegionActive(
  servers: GameServer[],
  versions: VersionInfo[],
): { region: string; semver: string; count: number; extra: number }[] {
  const semverOf = new Map(versions.map((v) => [v.id, v.semver]));
  const byRegion = new Map<string, Map<string, number>>();
  for (const s of servers) {
    if (!LIVE_SERVER_STATES.has(s.state)) continue;
    const sv = semverOf.get(s.version_id);
    if (sv === undefined) continue;
    const byVer = byRegion.get(s.region) ?? new Map<string, number>();
    byVer.set(sv, (byVer.get(sv) ?? 0) + 1);
    byRegion.set(s.region, byVer);
  }
  return [...byRegion.entries()]
    .map(([region, byVer]) => {
      const sorted = [...byVer.entries()].sort((a, b) => b[1] - a[1]);
      const total = sorted.reduce((acc, [, n]) => acc + n, 0);
      return { region, semver: sorted[0][0], count: total, extra: byVer.size - 1 };
    })
    .sort((a, b) => a.region.localeCompare(b.region));
}
