-- Capacity counts every admitted seat for the match lifetime, including
-- disconnected reservations. Legacy/manual allocations remain ineligible.
alter table matches add column game_mode text not null default '';
alter table matches add column reserved_size integer not null default 0;
alter table matches add column max_size integer not null default 0;
alter table matches add constraint matches_reserved_capacity check
  (reserved_size >= 0 and max_size >= reserved_size);
create index matches_backfill_idx on matches(project_id,env,game_mode,version_id,created_at)
  where state in ('pending','running') and max_size > reserved_size;
-- Hashes of scoped opaque player IDs prevent a new queue request from
-- replacing that player's existing/reserved/left seat in the same match.
create table matchmaking_roster (
  match_id uuid not null references matches(id) on delete cascade,
  player_key text not null,
  primary key(match_id,player_key)
);
