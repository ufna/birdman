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
-- Самолечение: statsrollup.Backfill при каждом старте master безусловно
-- пересчитывает из сырых матчей окно [today-29, today-2] (matches никогда не
-- удаляются), поэтому последние 30 дней приедут корректными проектными
-- строками при первом же рестарте. Неатрибутированными останутся только дни
-- СТАРШЕ окна и только при реальной коллизии semver между проектами.
with attribution as (
    select date(m.started_at) as day,
           m.region,
           v.semver,
           m.env,
           min(p.slug) as slug,
           count(distinct p.slug) as projects
    from matches m
    join versions v on v.id = m.version_id
    join projects p on p.id = m.project_id
    where m.started_at is not null
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
