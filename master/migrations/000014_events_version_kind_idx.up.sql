-- Окружения v1 / гигиена (W3, спека §6): индекс под частый запрос «последнее
-- событие данного kind у версии» — NewestAttemptedMarker (auto-deploy Resume,
-- exists по events где version_id=$ и kind='deploy_started') и панельные
-- per-version выборки событий. Без него — seq scan по events, растущему без
-- ретеншна при потоке dev-билдов.
create index events_version_kind_idx on events (version_id, kind);
