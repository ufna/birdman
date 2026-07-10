-- Dropping the column also drops the type-check constraint that depends only
-- on it (Реестры v2). The distribution columns (host/username/token) are
-- untouched, so a down leaves a working registries v1 table.
alter table registries drop column type;
