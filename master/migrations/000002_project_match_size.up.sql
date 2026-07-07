-- match_size — сколько игроков собирает матчмейкер в один матч
-- (docs/specs/master.md §4: «конфиг per project»). Колонка на projects, а не
-- на fleet_configs: размер матча — геймдизайн проекта и не зависит от региона;
-- очереди матчмейкера расширяются между регионами (widen), поэтому
-- региональный match_size был бы неоднозначен (уточнено в v0).
alter table projects add column match_size int not null default 2
  check (match_size >= 1);
