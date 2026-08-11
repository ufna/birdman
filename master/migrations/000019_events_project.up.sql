-- Проектное измерение ленты событий (эпик #968, шаг 1 из 4).
--
-- До этой миграции events знала только ссылки на сущности (node/server/match/
-- version) и произвольный payload. Из-за отсутствия project_id два алерта
-- эпика #950 — CrashLoop и AgentUpgradeFailed — остались платформенными:
-- падающий в цикле дедик проекта А будил дежурного проекта Б, и по алерту
-- нельзя было понять, чей проект страдает. Тот же корень бил в панели: лента
-- сужалась КЛИЕНТСКИ и не скрывающе, потому что проект несут лишь часть
-- payload'ов.
--
-- NULL — не «неизвестно», а ЗАКОННОЕ значение: события бекапов, внутренней CA,
-- сессий панели проекта не имеют и не должны его выдумывать. Ровно то же
-- различение, что у project='' в match_stats_daily (миграция 000017).
--
-- on delete set null, а не cascade: события это append-only аудит. Удаление
-- проекта уносит его матчи и серверы, но не должно стирать след того, что с
-- ними происходило; строка теряет атрибуцию и становится платформенной.
alter table events add column project_id uuid references projects(id) on delete set null;

-- Читать будут «последние N событий проекта» — (project_id, ts desc).
create index events_project_ts_idx on events (project_id, ts desc);

-- Бэкфилл. Порядок источников по убыванию достоверности:
--
--   1. ссылки на сущности — у них project_id в собственной колонке, это факт;
--   2. слаг в payload — его кладут события, у которых ссылки нет вовсе
--      (project_created, environment_deleted, deploy_*, fleet_updated).
--
-- Строка, которой не хватило ни того, ни другого, ОСТАЁТСЯ NULL: платформенное
-- событие (бекап, CA, сессия) выглядит именно так, и приписать его первому
-- попавшемуся проекту значило бы соврать в аудите.
--
-- Ссылка на удалённую сущность (реап сервера, каскад окружения) не находится —
-- строка остаётся NULL, и это честно: владельца уже не восстановить.
update events e
   set project_id = coalesce(
         (select n.project_id from nodes    n where n.id = e.node_id),
         (select s.project_id from servers  s where s.id = e.server_id),
         (select v.project_id from versions v where v.id = e.version_id),
         (select m.project_id from matches  m where m.id = e.match_id),
         (select p.id from projects p where p.slug = e.payload->>'project')
       )
 where e.project_id is null;
