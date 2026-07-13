-- Окружения v1 (docs/superpowers/specs/2026-07-13-environments-v1-design.md §1).
-- environments — полноценное измерение платформы per-project: колонка env у
-- versions/fleets/nodes/servers/matches, active-версия и deprecated-окно
-- скоупятся (project, env). channel умирает (staging→dev, prod→prod). Ключи
-- получают опциональную привязку (project, env). Блок скопирован VERBATIM из
-- секции §1 спеки (включая C3-констрейнты и seed dev/prod).

create table environments (
  id uuid primary key default gen_random_uuid(),
  project_id uuid not null references projects(id) on delete cascade,
  name text not null check (name ~ '^[a-z0-9][a-z0-9-]{0,31}$'),
  production boolean not null default false,
  auto_deploy boolean not null default false,
  retention_keep int not null default 0 check (retention_keep >= 0),
  created_at timestamptz not null default now(),
  unique (project_id, name),
  check (not (production and auto_deploy)),          -- guardrail и на уровне БД
  check (name not in ('all', 'global'))              -- РЕВИЗИЯ M7: зарезервировано под UI/API-конвенции
);
-- seed обоих окружений каждому существующему проекту:
insert into environments (project_id, name, production, auto_deploy, retention_keep)
  select id, 'dev', false, true, 20 from projects;
insert into environments (project_id, name, production, auto_deploy, retention_keep)
  select id, 'prod', true, false, 0 from projects;

-- versions: env-бэкфилл (ERRATUM №3, 13.07, whole-wave ревью W1 + данные стенда):
-- channel был ЗРЕЛОСТЬЮ билда, не размещением — стенд регистрировал всё через
-- channel='prod' при dev-флоте; маппинг staging→dev/prod→prod дал бы
-- fleet-active версию в env='prod' при флоте 'dev' → составной FK
-- fleet_active_version_env_fk рвётся, миграция падает dirty. По §9 вся
-- существующая история — dev: бэкфилл env='dev' ДЛЯ ВСЕХ версий + guard на
-- дубли (project, semver) между channel'ами (тогда оба легли бы в dev и
-- нарушили unique — оператор разруливает вручную ДО миграции; на стенде дублей нет).
alter table versions add column env text;
do $$
begin
  if exists (select 1 from versions group by project_id, semver having count(*) > 1) then
    raise exception 'migration 000013: duplicate (project, semver) across channels — resolve manually before migrating (see spec erratum #3)';
  end if;
end $$;
update versions set env = 'dev';
alter table versions alter column env set not null;
alter table versions add column promoted_from uuid references versions(id);  -- provenance (Promote → env)
alter table versions drop constraint versions_project_id_semver_channel_key;
alter table versions add constraint versions_project_env_semver_key unique (project_id, env, semver);
alter table versions drop column channel;
alter table versions add constraint versions_env_fk
  foreign key (project_id, env) references environments (project_id, name);
-- РЕВИЗИЯ C3: опора для составного FK флота (изоляция active_version по env на уровне БД)
alter table versions add constraint versions_id_project_env_key unique (id, project_id, env);

-- fleet_configs: PK расширяется env; существующие флоты — dev
alter table fleet_configs add column env text;
update fleet_configs set env = 'dev';
alter table fleet_configs alter column env set not null;
alter table fleet_configs drop constraint fleet_configs_pkey;
alter table fleet_configs add primary key (project_id, env, region);
alter table fleet_configs add constraint fleet_env_fk
  foreign key (project_id, env) references environments (project_id, name);
-- РЕВИЗИЯ C3: active_version обязан принадлежать ТОМУ ЖЕ (project, env) — дыра
-- «PUT /v1/fleets ставит dev-флоту prod-версию» закрывается на уровне БД:
alter table fleet_configs drop constraint fleet_configs_active_version_fkey;
alter table fleet_configs add constraint fleet_active_version_env_fk
  foreign key (active_version, project_id, env) references versions (id, project_id, env);

-- nodes: нода принадлежит ровно одному env; новые — только dev (никогда prod неявно)
alter table nodes add column env text not null default 'dev';
alter table nodes add constraint nodes_env_fk
  foreign key (project_id, env) references environments (project_id, name);

-- servers/matches: денормализованный env для метрик/статистики
-- РЕВИЗИЯ I6: env исполнения ВСЕГДА берётся из этих колонок, НИКОГДА join'ом к
-- nodes — перевод ноды между env не должен переписывать историю.
alter table servers add column env text not null default 'dev';
update servers s set env = v.env from versions v where s.version_id = v.id;
alter table matches add column env text not null default 'dev';
update matches m set env = v.env from versions v where m.version_id = v.id;  -- РЕВИЗИЯ M2: симметрично servers

-- статистика (РЕВИЗИЯ I5, решение): env получает ТОЛЬКО match_stats_daily;
-- match_ccu_daily остаётся глобальной платформенной метрикой (PK (day) —
-- строка-маркер «день посчитан», per-env маркер ломается при изменении набора env).
alter table match_stats_daily drop constraint match_stats_daily_pkey;
alter table match_stats_daily add column env text not null default 'dev';
alter table match_stats_daily add primary key (day, region, semver, env);

-- ключи: опциональная привязка; NULL = глобальный (существующие работают как раньше)
alter table api_keys add column project_id uuid references projects(id);
alter table api_keys add column env text;
-- РЕВИЗИЯ I8: полусвязанных ключей НЕТ — либо оба NULL, либо оба заданы:
alter table api_keys add constraint api_keys_binding_all_or_nothing
  check ((project_id is null) = (env is null));
alter table api_keys add constraint api_keys_env_fk
  foreign key (project_id, env) references environments (project_id, name);
