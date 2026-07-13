-- Down окружений v1 — ОБРАТНЫЕ ALTER'ы в обратном порядке up-миграции.
--
-- LOSSY (спека M1 + ERRATUM №3): up сгребает ВСЮ историю в env='dev' (channel
-- был зрелостью билда, не размещением — см. up), поэтому единственный честно
-- обратимый env — dev → blanket channel='staging'. Версии в ЛЮБОМ ином env
-- (prod/qa/…) — это уже реальное пост-миграционное размещение без эквивалента в
-- channel-схеме: их channel станет NULL, и `alter column channel set not null`
-- уронит down с понятной ошибкой (осознанно: откат после заведения окружений
-- требует ручного сноса этих версий). Привязки ключей и денормализованный env
-- серверов/матчей теряются.

-- api_keys: снять привязку
alter table api_keys drop constraint api_keys_env_fk;
alter table api_keys drop constraint api_keys_binding_all_or_nothing;
alter table api_keys drop column env;
alter table api_keys drop column project_id;

-- match_stats_daily: PK назад на (day, region, semver)
alter table match_stats_daily drop constraint match_stats_daily_pkey;
alter table match_stats_daily drop column env;
alter table match_stats_daily add primary key (day, region, semver);

-- servers/matches: снять денормализованный env
alter table matches drop column env;
alter table servers drop column env;

-- nodes: снять env
alter table nodes drop constraint nodes_env_fk;
alter table nodes drop column env;

-- fleet_configs: PK назад на (project_id, region), простой FK active_version
alter table fleet_configs drop constraint fleet_active_version_env_fk;
alter table fleet_configs drop constraint fleet_env_fk;
alter table fleet_configs drop constraint fleet_configs_pkey;
alter table fleet_configs add primary key (project_id, region);
alter table fleet_configs add constraint fleet_configs_active_version_fkey
  foreign key (active_version) references versions (id);
alter table fleet_configs drop column env;

-- versions: env → channel (lossy), снять provenance и env-констрейнты
alter table versions drop constraint versions_id_project_env_key;
alter table versions drop constraint versions_env_fk;
alter table versions add column channel text;
-- ERRATUM №3: up сгребает всё в env='dev', поэтому обратимо только dev → blanket
-- 'staging'. Иные env (prod/qa/…) остаются NULL → NOT NULL ниже роняет down с
-- понятной ошибкой (прежний fail-путь — теперь и для prod: post-migration prod —
-- реальное размещение, а не channel='prod'-зрелость исходных данных).
update versions set channel = 'staging' where env = 'dev';
alter table versions alter column channel set not null;
alter table versions add check (channel in ('staging','prod'));
alter table versions drop constraint versions_project_env_semver_key;
alter table versions add constraint versions_project_id_semver_channel_key unique (project_id, semver, channel);
alter table versions drop column promoted_from;
alter table versions drop column env;

-- environments уходит последней (все FK на неё уже сняты выше)
drop table environments;
