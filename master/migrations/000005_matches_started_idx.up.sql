create index matches_started_idx on matches (started_at) where started_at is not null;
