drop table matchmaking_roster;
drop index matches_backfill_idx;
alter table matches drop constraint matches_reserved_capacity;
alter table matches drop column max_size, drop column reserved_size, drop column game_mode;
