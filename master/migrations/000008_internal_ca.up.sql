-- internal_ca — внутренняя CA платформы, живёт в PG мастера
-- (docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §1). Мастер при
-- первом старте генерит её (ECDSA P-256, IsCA, CN «birdman internal CA»,
-- TTL 10 лет) под PG advisory-lock и подписывает ею свой server-лист и
-- клиентские листы нод (Enroll). Ключ в PG переживает потерю бокса вместе с
-- дампом — restore-runbook (ops.md §5) не меняется. Контракт на бандлах:
-- ClientCAs мастера и агентский траст — пул всех active-строк (задел под
-- ротацию CA без ансибл).
create table internal_ca (
  id         uuid primary key default gen_random_uuid(),
  cert_pem   text not null,
  key_pem    text not null,          -- обратимый секрет: класс риска = registries.token (гейт оффсайт-бэкапов уже стоит, ops.md §5); в логи/%v не попадает
  active     bool not null default true,
  created_at timestamptz not null default now(),
  not_after  timestamptz not null
);

-- Аддитивно к nodes: идентичность/срок выданного клиентского серта (всё
-- nullable — заполняется на Enroll). enrolled_at — момент ПЕРВОГО обмена;
-- renewal обновляет cert_serial/cert_not_after, но не enrolled_at.
alter table nodes
  add column cert_serial    text,
  add column cert_not_after timestamptz,
  add column enrolled_at    timestamptz;
