-- match_stats_daily / match_ccu_daily — персистентные роллапы чистой
-- дневной агрегации (stats.AggregateDaily, «Статистика v1» T7): по одной
-- строке на day×region×version аддитивных агрегатов, плюс по одной строке в
-- день с пиковым CCU. Роллап-джоба (statsrollup, T9) их поддерживает, чтобы
-- длинные окна статистики не пересчитывали сырые matches заново; day везде —
-- календарная дата в UTC.
create table match_stats_daily (
  day date not null,
  region text not null,
  semver text not null,
  matches int not null default 0,
  players_peak_sum bigint not null default 0,
  dur_sum_seconds double precision not null default 0,
  dur_count int not null default 0,
  slot_seconds double precision not null default 0,
  primary key (day, region, semver)
);

-- Одна строка на UTC-день, ВСЕГДА присутствует после того, как день посчитан
-- (даже с peak_ccu = 0) — само наличие строки помечает день обработанным
-- (store.RolledUpDays / скан «каких дней не хватает» в бэкфилле).
create table match_ccu_daily (
  day date not null primary key,
  peak_ccu int not null default 0
);
