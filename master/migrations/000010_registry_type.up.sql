-- registry type (Реестры v2,
-- docs/superpowers/specs/2026-07-10-registries-v2-design.md §1). type ∈
-- {ghcr,gar,generic} drives the panel's credential form, the per-type
-- validation on write, and the server-side normalization (gar →
-- username='_json_key', secret = the whole service-account JSON). Additive:
-- default 'generic' so existing rows are valid without a token re-entry, and
-- the raw (host,username,token) columns stay docker-basic-auth — the agentlink
-- distribution (SetRegistries) is type-agnostic and unchanged.
alter table registries
  add column type text not null default 'generic'
  check (type in ('ghcr','gar','generic'));

-- Backfill the box's existing ghcr.io row to its real type — functionally
-- identical (ghcr is user+PAT, exactly what a generic row already held), it
-- just labels the row so the panel shows the right form/hint.
update registries set type = 'ghcr' where host = 'ghcr.io';
