-- silence_id — id зеркального alertmanager-silence для этого mute'а
-- (ops.md §1, tracker #238/#245). Раньше mute был чистой АННОТАЦИЕЙ master'а
-- (muted:true в /v1/alerts/*), теперь master зеркалирует его в настоящий
-- alertmanager silence best-effort — реальное подавление sink/Discord.
-- NULL = ещё не зеркалирован: v0-mute, созданный до апгрейда, или AM был
-- недоступен в момент mute/unmute — reconcile-луп догонит и проставит id.
-- Источник истины — сама строка alert_mutes; silence вторичен и в любой момент
-- перевыпускается из неё, поэтому здесь достаточно nullable-колонки без FK/индекса.
alter table alert_mutes add column silence_id text;
