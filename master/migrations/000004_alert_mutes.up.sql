-- alert_mutes — mute/подавление алертов на уровне master+панели
-- (docs/specs/master.md §6). v0-семантика: mute — это АННОТАЦИЯ master'а, а НЕ
-- настоящий silence vmalert/Discord (для того нужен alertmanager silence API,
-- см. ops.md §1 TODO). master хранит правила и помечает совпадающие алерты
-- muted:true в /v1/alerts/{active,history}, чтобы панель их приглушала/скрывала
-- и вёлся аудит; Discord/лог-синк продолжают их получать (ограничение v0).
create table alert_mutes (
  id         uuid primary key default gen_random_uuid(),
  alertname  text not null,                 -- сопоставление по labels.alertname
  region     text,                          -- null = все регионы; иначе точное совпадение labels.region
  note       text not null default '',
  created_at timestamptz not null default now(),
  expires_at timestamptz,                   -- null = бессрочно; истёкшие считаются неактивными
  created_by text not null default ''       -- name ключа из сессии
);
-- И матчинг muted-флага, и список активных мьютов выбирают по alertname —
-- индекс ускоряет обе горячие выборки на стороне /v1/alerts/*.
create index alert_mutes_alertname_idx on alert_mutes (alertname);
