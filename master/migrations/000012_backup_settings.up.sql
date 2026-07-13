-- Backups v1 (docs/superpowers/specs/2026-07-13-backups-admin-v1-design.md §1):
-- бекапы исполняет master (планировщик + pg_dump + ротация + S3), админ
-- конфигурирует из панели. backup_settings — singleton (id=true), дефолты
-- повторяют снесённый systemd-таймер (6ч, 14 дампов) — поведение бокса после
-- миграции не меняется. s3_secret_key — AEAD-конверт birdman:v1:<key_id>:…
-- (store/secrets.go, AAD backup_settings.s3_secret_key). Оба ретеншна —
-- В ШТУКАХ дампов, не в днях. backup_runs — история прогонов, ротация
-- в коде (PruneBackupRuns, 200 свежих).
create table backup_settings (
  id boolean primary key default true check (id),
  enabled boolean not null default true,
  interval_hours int not null default 6 check (interval_hours between 1 and 168),
  retention_local int not null default 14 check (retention_local between 1 and 365),
  s3_enabled boolean not null default false,
  s3_endpoint text not null default '',
  s3_region text not null default '',
  s3_bucket text not null default '',
  s3_prefix text not null default '',
  s3_access_key text not null default '',
  s3_secret_key text not null default '',
  retention_s3 int not null default 30 check (retention_s3 between 1 and 3650),
  updated_at timestamptz not null default now()
);
insert into backup_settings (id) values (true);

create table backup_runs (
  id bigserial primary key,
  started_at timestamptz not null default now(),
  finished_at timestamptz,
  kind text not null check (kind in ('scheduled','manual')),
  result text not null check (result in ('running','ok','error')),
  size_bytes bigint,
  s3_uploaded boolean not null default false,
  error text not null default ''
);
create index backup_runs_started_at_idx on backup_runs (started_at desc);
