-- Откат проектного измерения статистики (симметрично 000013 по env).
--
-- Проектные строки СХЛОПЫВАЮТСЯ обратно в одну на (day, region, semver, env):
-- просто снять колонку нельзя — PK стал бы дублирующимся, как только у одного
-- дня есть строки двух проектов. Аддитивные агрегаты складываются (matches,
-- players_peak_sum, dur_sum_seconds, dur_count, slot_seconds — все суммы), так
-- что схлопывание точное, а не приблизительное.
create temporary table match_stats_daily_collapsed on commit drop as
select day, region, semver, env,
       sum(matches)::int as matches,
       sum(players_peak_sum)::bigint as players_peak_sum,
       sum(dur_sum_seconds)::double precision as dur_sum_seconds,
       sum(dur_count)::int as dur_count,
       sum(slot_seconds)::double precision as slot_seconds
from match_stats_daily
group by day, region, semver, env;

delete from match_stats_daily;
alter table match_stats_daily drop constraint match_stats_daily_pkey;
alter table match_stats_daily drop column project;
alter table match_stats_daily add primary key (day, region, semver, env);
insert into match_stats_daily (day, region, semver, env, matches, players_peak_sum,
                               dur_sum_seconds, dur_count, slot_seconds)
select day, region, semver, env, matches, players_peak_sum,
       dur_sum_seconds, dur_count, slot_seconds
from match_stats_daily_collapsed;

-- CCU складывать НЕЛЬЗЯ — пик не аддитивен: сумма проектных пиков больше
-- реального одновременного пика платформы. Оставляем ровно платформенную
-- строку (project = ''), которую писали и до, и после этой миграции.
delete from match_ccu_daily where project <> '';
alter table match_ccu_daily drop constraint match_ccu_daily_pkey;
alter table match_ccu_daily drop column project;
alter table match_ccu_daily add primary key (day);
