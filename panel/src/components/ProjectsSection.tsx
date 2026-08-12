// «Админка»: секция «Проекты» — CRUD проектов (слайс B спеки
// panel-scope-projects-nodes). До неё проект нельзя было ни завести осознанно,
// ни убрать: он появлялся молча побочным эффектом регистрации первой ноды или
// версии (ensureProject) и оставался в селекторе навсегда.
//
// Устроено как соседняя EnvironmentsSection, уровнем выше: таблица + форма
// add/edit + удаление каскадом с подтверждением вводом слага и составом из
// GET .../usage. Удаление блокируется, пока в проекте есть ЖИВЫЕ ноды —
// выведенные (dead) уезжают вместе с проектом.
//
// Слаг не переименовывается: на него завязаны API-ключи, матчмейкинг и CI —
// переименование тихо сломало бы внешних клиентов.

import { useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import * as AlertDialog from '@radix-ui/react-alert-dialog';
import * as Dialog from '@radix-ui/react-dialog';
import { api } from '../lib/api';
import { apiErrorMessage } from '../lib/apiError';
import type { ProjectInfo, ProjectUsage } from '../lib/api';
import { useProject } from '../lib/project';
import { useT } from '../lib/i18n';
import { useToast } from './Toast';
import { Card, CardHeader, ErrorNote, LoadingRow } from './ui';

const inputClass =
  'rounded-lg border border-line bg-paper px-2.5 py-1.5 text-sm text-ink focus:border-accent focus:outline-none';

export function ProjectsSection() {
  const { t } = useT();
  const { projects, selected, loading, error, reload } = useProject();

  return (
    <Card>
      <CardHeader
        title={t('access.projects')}
        aside={
          <div className="flex items-center gap-3">
            <span className="tabular font-mono text-xs text-muted">{projects.length}</span>
            <ProjectFormDialog
              mode="add"
              onSaved={reload}
              trigger={
                <button
                  type="button"
                  className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90"
                >
                  {t('access.projects.add')}
                </button>
              }
            />
          </div>
        }
      />
      <p className="border-b border-line px-4 py-2.5 text-xs text-muted">{t('access.projects.hint')}</p>
      {error !== undefined && projects.length === 0 ? (
        <div className="p-4">
          <ErrorNote error={error} retry={reload} />
        </div>
      ) : loading && projects.length === 0 ? (
        <LoadingRow />
      ) : (
        <ul className="divide-y divide-line">
          {projects.map((p) => (
            <li key={p.slug} className="flex flex-wrap items-center gap-3 px-4 py-2.5">
              <span className="font-mono text-sm font-medium">{p.slug}</span>
              {p.slug === selected && (
                <span className="rounded border border-accent px-1.5 py-0.5 text-[11px] text-accent-ink">
                  {t('access.projects.current')}
                </span>
              )}
              <span className="text-xs text-muted">{t('access.projects.matchSize', { size: p.match_size })}</span>
              <span className="ml-auto flex items-center gap-2">
                <ProjectFormDialog
                  mode="edit"
                  project={p}
                  onSaved={reload}
                  trigger={
                    <button
                      type="button"
                      className="rounded-lg border border-line px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:text-ink"
                    >
                      {t('common.edit')}
                    </button>
                  }
                />
                <DeleteProjectAction project={p} onDone={reload} />
              </span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

/**
 * Форма проекта. add — слаг + размер матча (POST, 409 на занятом слаге);
 * edit — только размер матча (PUT): слаг неизменяем by design.
 */
function ProjectFormDialog({
  mode,
  project,
  onSaved,
  trigger,
}: {
  mode: 'add' | 'edit';
  project?: ProjectInfo;
  onSaved: () => void;
  trigger: ReactNode;
}) {
  const { t } = useT();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [slug, setSlug] = useState('');
  const [matchSize, setMatchSize] = useState(String(project?.match_size ?? 2));
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = () => {
    const size = Number(matchSize);
    if (!Number.isInteger(size) || size < 1) {
      setError(t('access.projects.err.matchSize'));
      return;
    }
    setPending(true);
    setError(null);
    const done = () => {
      setPending(false);
      setOpen(false);
      onSaved();
    };
    const fail = (e: unknown) => {
      setPending(false);
      setError(apiErrorMessage(e, t, { forbidden: 'confirm.err.forbidden', generic: 'access.projects.err.save', byStatus: { 400: 'ui.err.badRequest', 409: 'access.projects.err.slugTaken' } }));
    };
    if (mode === 'add') {
      api.createProject(slug.trim(), size).then((p) => {
        toast.success(t('access.projects.toast.created', { slug: p.slug }));
        done();
      }, fail);
    } else if (project !== undefined) {
      api.upsertProject(project.slug, size).then(() => {
        toast.success(t('access.projects.toast.saved', { slug: project.slug }));
        done();
      }, fail);
    }
  };

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return;
        setOpen(o);
        setError(null);
        setSlug('');
        setMatchSize(String(project?.match_size ?? 2));
      }}
    >
      <Dialog.Trigger asChild>{trigger}</Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed top-1/2 left-1/2 z-50 w-[min(26rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl"
        >
          <Dialog.Title className="text-base font-semibold">
            {mode === 'add' ? t('access.projects.add') : t('access.projects.edit', { slug: project?.slug ?? '' })}
          </Dialog.Title>

          <div className="mt-4 flex flex-col gap-3">
            {mode === 'add' && (
              <label className="flex flex-col gap-1.5 text-xs font-medium">
                {t('access.projects.slug')}
                <input
                  autoFocus
                  autoComplete="off"
                  value={slug}
                  onChange={(e) => {
                    setSlug(e.target.value);
                  }}
                  placeholder="my-game"
                  className={`${inputClass} font-mono`}
                />
                <span className="font-normal text-muted">{t('access.projects.slug.hint')}</span>
              </label>
            )}
            <label className="flex flex-col gap-1.5 text-xs font-medium">
              {t('access.projects.matchSize.label')}
              <input
                type="number"
                min={1}
                value={matchSize}
                onChange={(e) => {
                  setMatchSize(e.target.value);
                }}
                className={`${inputClass} w-28 font-mono`}
              />
            </label>
          </div>

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
                className="rounded-lg border border-line px-3 py-1.5 text-sm text-muted transition-colors hover:text-ink disabled:opacity-40"
              >
                {t('common.cancel')}
              </button>
            </Dialog.Close>
            <button
              type="button"
              disabled={pending || (mode === 'add' && slug.trim() === '')}
              onClick={submit}
              className="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-40"
            >
              {pending ? t('common.running') : t('common.save')}
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/**
 * Удаление проекта вместе с содержимым. Состав приезжает из usage ДО
 * подтверждения: оператор видит, что именно исчезнет, а не подтверждает вслепую.
 * Живые ноды блокируют удаление — их сначала выводят или переводят («Флот»).
 */
function DeleteProjectAction({ project, onDone }: { project: ProjectInfo; onDone: () => void }) {
  const { t } = useT();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [usage, setUsage] = useState<ProjectUsage | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [typed, setTyped] = useState('');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    setUsage(null);
    setLoadError(null);
    api.projectUsage(project.slug).then(
      (u) => {
        setUsage(u);
      },
      (e: unknown) => {
        setLoadError(apiErrorMessage(e, t, { forbidden: 'confirm.err.forbidden', generic: 'access.projects.delete.err', byStatus: { 400: 'ui.err.badRequest' } }));
      },
    );
  }, [open, project.slug, t]);

  const blocked = usage !== null && usage.nodes > 0;
  const empty =
    usage !== null &&
    usage.versions === 0 &&
    usage.fleets === 0 &&
    usage.nodes === 0 &&
    usage.retired_nodes === 0 &&
    usage.matches === 0 &&
    usage.servers === 0 &&
    usage.api_keys === 0;
  const canDelete = usage !== null && !blocked && (empty || typed === project.slug);

  const submit = () => {
    if (!canDelete) return;
    setPending(true);
    setError(null);
    api.deleteProject(project.slug, typed).then(
      (deleted) => {
        setPending(false);
        setOpen(false);
        toast.success(
          deleted === undefined
            ? t('access.projects.toast.deleted', { slug: project.slug })
            : t('access.projects.toast.deletedCascade', {
                slug: deleted.slug,
                environments: deleted.environments,
                versions: deleted.versions,
                keys: deleted.api_keys_revoked,
              }),
        );
        onDone();
      },
      (e: unknown) => {
        setPending(false);
        setError(apiErrorMessage(e, t, { forbidden: 'confirm.err.forbidden', generic: 'access.projects.delete.err', byStatus: { 400: 'ui.err.badRequest' } }));
      },
    );
  };

  return (
    <AlertDialog.Root
      open={open}
      onOpenChange={(o) => {
        if (pending) return;
        setOpen(o);
        setTyped('');
        setError(null);
      }}
    >
      <AlertDialog.Trigger asChild>
        <button
          type="button"
          className="rounded-lg border border-line px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:text-dead"
        >
          {t('common.delete')}
        </button>
      </AlertDialog.Trigger>
      <AlertDialog.Portal>
        <AlertDialog.Overlay className="fixed inset-0 z-40 bg-black/40 backdrop-blur-[1px]" />
        <AlertDialog.Content className="fixed top-1/2 left-1/2 z-50 w-[min(30rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-line bg-card p-5 shadow-xl">
          <AlertDialog.Title className="text-base font-semibold">
            {t('access.projects.delete.title', { slug: project.slug })}
          </AlertDialog.Title>
          <AlertDialog.Description asChild>
            <div className="mt-2 text-sm text-muted">{t('access.projects.delete.desc')}</div>
          </AlertDialog.Description>

          {loadError !== null && (
            <p role="alert" className="mt-3 rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
              {loadError}
            </p>
          )}
          {usage === null && loadError === null && <LoadingRow />}

          {usage !== null && (
            <>
              <p className="mt-4 text-xs font-medium">{t('access.projects.delete.usage')}</p>
              <ul className="mt-1.5 grid grid-cols-2 gap-x-6 gap-y-1 rounded-lg border border-line bg-paper px-3 py-2 text-xs">
                <UsageRow label={t('access.projects.delete.usage.environments')} value={usage.environments} />
                <UsageRow label={t('access.projects.delete.usage.versions')} value={usage.versions} />
                <UsageRow label={t('access.projects.delete.usage.matches')} value={usage.matches} />
                <UsageRow label={t('access.projects.delete.usage.keys')} value={usage.api_keys} />
                {usage.retired_nodes > 0 && (
                  <UsageRow label={t('access.projects.delete.usage.retiredNodes')} value={usage.retired_nodes} />
                )}
              </ul>

              {blocked ? (
                <p role="alert" className="mt-3 rounded-lg bg-warn-bg px-3 py-2 text-xs text-warn">
                  {t('access.projects.delete.nodesBlock', { count: usage.nodes })}
                </p>
              ) : (
                !empty && (
                  <div className="mt-4 flex flex-col gap-1.5">
                    <label htmlFor="project-delete-confirm" className="text-xs font-medium">
                      {t('access.projects.delete.confirmLabel')}
                    </label>
                    <code className="w-fit rounded border border-line bg-paper px-2 py-1 font-mono text-xs">
                      {project.slug}
                    </code>
                    <input
                      id="project-delete-confirm"
                      autoFocus
                      autoComplete="off"
                      value={typed}
                      onChange={(e) => {
                        setTyped(e.target.value);
                      }}
                      placeholder={project.slug}
                      className={`${inputClass} font-mono`}
                    />
                  </div>
                )
              )}
            </>
          )}

          {error !== null && (
            <p role="alert" className="mt-3 rounded-lg bg-dead-bg px-3 py-2 text-xs text-dead">
              {error}
            </p>
          )}

          <div className="mt-5 flex justify-end gap-2">
            <AlertDialog.Cancel asChild>
              <button
                type="button"
                disabled={pending}
                className="rounded-lg border border-line px-3 py-1.5 text-sm text-muted transition-colors hover:text-ink disabled:opacity-40"
              >
                {t('common.cancel')}
              </button>
            </AlertDialog.Cancel>
            <button
              type="button"
              disabled={!canDelete || pending}
              onClick={submit}
              className="rounded-lg bg-dead px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-40"
            >
              {pending ? t('common.running') : t('common.delete')}
            </button>
          </div>
        </AlertDialog.Content>
      </AlertDialog.Portal>
    </AlertDialog.Root>
  );
}

function UsageRow({ label, value }: { label: string; value: number }) {
  return (
    <>
      <span className="text-muted">{label}</span>
      <span className="tabular text-right font-mono">{value}</span>
    </>
  );
}
