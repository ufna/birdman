-- Проектное измерение статистики (мультипроект W3). До этой миграции роллапы
-- знали только (day, region, semver, env) — 000006 завела таблицы, 000013
-- добавила env (решение I5), проект не добавлял никто. Панель с селектором
-- проекта (W1/W2) обязана сужать Stats/Cost так же, как остальные экраны, а
-- иммутабельная часть окна читается ИЗ роллапов — клиентским фильтром проект
-- оттуда не достать.

alter table match_stats_daily drop constraint match_stats_daily_pkey;
alter table match_stats_daily add column project text not null default '';
alter table match_stats_daily add primary key (day, region, semver, env, project);

-- match_ccu_daily тоже получает измерение, в ОТЛИЧИЕ от решения I5 по env.
-- Причина: env делят одну ёмкость флота, а проекты — непересекающиеся
-- тенанты, и «пик CCU» под выбранным проектом обязан быть пиком ЭТОГО
-- проекта, иначе цифра врёт. Строка project='' СОХРАНЯЕТСЯ и означает ровно
-- то же, что раньше: платформенный пик за день И маркер «день посчитан»
-- (на наличии ключа держится различение «пустой день» и «день не считался» —
-- store.RollupPeakCCU). Поэтому инвариант не тронут, измерение добавлено
-- рядом: всегда ''-строка + по строке на каждый проект с матчами в этот день.
alter table match_ccu_daily drop constraint match_ccu_daily_pkey;
alter table match_ccu_daily add column project text not null default '';
alter table match_ccu_daily add primary key (day, project);

-- Бэкфилл исторических строк. Атрибутируем ТОЛЬКО те комбинации
-- (day, region, semver, env), которые отображаются в РОВНО ОДИН проект: для
-- них агрегаты целиком принадлежат этому проекту — это точно, а не примерно.
-- Комбинация, разделённая двумя проектами (один semver в двух проектах в один
-- день), сплиту средствами SQL не поддаётся: агрегаты уже просуммированы, а
-- slot_seconds размазаны по дням перекрытия и пересчитываются в Go
-- (stats.AggregateDaily), а не запросом. Такие строки остаются с '' —
-- явным маркером «не атрибутировано», а не молчаливым дефолтом на первый
-- проект, который врал бы в отчётности.
--
-- День считается в UTC ЯВНО (`at time zone 'utc'` даёт timestamp без зоны), а
-- не голым date(m.started_at): started_at — timestamptz, и date() привёл бы
-- его к таймзоне СЕССИИ, тогда как роллап-строки живут в UTC-днях
-- (utctime.StartOfDay по всему коду статистики). На сервере с
-- TimeZone = Europe/Moscow голый date() промахивался бы мимо строки ЛИБО
-- попадал в соседнюю — и та получала бы чужой проект. Штатный деплой — docker
-- postgres:16 с UTC, но self-host на чужом инстансе с локальной TZ рядовой,
-- поэтому зона фиксируется в запросе, а не предполагается у сервера.
--
-- Проект считается «касающимся» дня, если матч этот день ПЕРЕКРЫВАЕТ, а не
-- только в нём начался. Так устроена сама роллап-строка (docs/specs/master.md
-- §6, stats.AggregateDaily): matches/players_peak/duration идут в день СТАРТА,
-- а slot_seconds размазываются по всем перекрытым дням. Значит строка дня D+1
-- может целиком состоять из кросс-полуночного «смира» матча, стартовавшего в
-- D: по дню старта её владелец не виден вовсе, и при коллизии semver она
-- молча досталась бы ЧУЖОМУ проекту (у которого в D+1 свои матчи есть).
-- Перекрытие закрывает обе стороны: чистый смир достаётся своему проекту, а
-- смешанная строка (смир одного проекта + матчи другого) честно остаётся с ''.
--
-- Самолечение: statsrollup.Backfill при каждом старте master безусловно
-- пересчитывает из сырых матчей окно [today-29, today-2] (matches никогда не
-- удаляются), поэтому последние 30 дней приедут корректными проектными
-- строками при первом же рестарте. Неатрибутированными останутся только дни
-- СТАРШЕ окна и только при реальной коллизии semver между проектами.
with spans as (
    select p.slug,
           m.region,
           v.semver,
           m.env,
           (m.started_at at time zone 'utc')::date as first_day,
           -- Полуинтервал [start, end), как в stats.overlapSeconds: матч,
           -- закончившийся ровно в полночь, в следующий день slot_seconds не
           -- вносит и касающимся его считать нельзя — отсюда вычет
           -- микросекунды (разрешение timestamptz). Незавершённый матч
           -- ограничивается «сейчас» — так же, как stats.matchEnd. greatest
           -- страхует от ended_at < started_at: спан не бывает пустым.
           (greatest(m.started_at,
                     coalesce(m.ended_at, now()) - interval '1 microsecond')
            at time zone 'utc')::date as last_day
    from matches m
    join versions v on v.id = m.version_id
    join projects p on p.id = m.project_id
    where m.started_at is not null
),
attribution as (
    select g::date as day,
           s.region,
           s.semver,
           s.env,
           min(s.slug) as slug,
           count(distinct s.slug) as projects
    from spans s
    cross join lateral generate_series(s.first_day::timestamp,
                                       s.last_day::timestamp,
                                       interval '1 day') as g
    group by 1, 2, 3, 4
)
update match_stats_daily d
   set project = a.slug
  from attribution a
 where d.project = ''
   and a.projects = 1
   and d.day = a.day
   and d.region = a.region
   and d.semver = a.semver
   and d.env = a.env;
