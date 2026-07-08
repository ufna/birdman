-- deprecated_at — момент, когда deploy-менеджер увёл версию active →
-- deprecated (итерация 3, docs/specs/master.md §5): от него отсчитывается
-- reap_ttl_min окна мультиверсий (добой deprecated-дедиков и авто-disable).
-- NULL у версий, никогда не бывших deprecated (и снимается при rollback).
alter table versions add column deprecated_at timestamptz;
