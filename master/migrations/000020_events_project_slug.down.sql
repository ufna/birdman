-- Откат снимка слага владельца события (tracker #1083).
-- Различение «платформенное по рождению» / «осиротевшее» теряется безвозвратно:
-- у осиротевших строк project_id уже null, и восстановить имя владельца
-- обратным шагом неоткуда.
alter table events drop constraint if exists events_project_named;
alter table events drop column if exists project_slug;
