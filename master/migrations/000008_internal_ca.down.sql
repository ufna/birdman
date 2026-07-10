alter table nodes
  drop column if exists cert_serial,
  drop column if exists cert_not_after,
  drop column if exists enrolled_at;

drop table if exists internal_ca;
