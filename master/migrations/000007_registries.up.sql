-- registries — приватные container-registry credentials мастера
-- (docs/superpowers/specs/2026-07-09-registries-design.md §1). Раздаются
-- подключённым агентам по agentlink (SetRegistries, proto/agentlink/v1,
-- поле 11) и host-matched на pull приватных образов — закрывает дыру, когда
-- единственный агентский credential из agent.yaml отдавался любому хосту из
-- image_ref. token — plaintext: мастер обязан отдать его агенту для pull;
-- шифрование at-rest — follow-up (риск принят; см. ops.md §5 про бэкапы).
create table registries (
  id         uuid primary key default gen_random_uuid(),
  host       text unique not null,   -- нормализован: lowercase, без схемы/слэшей (store.NormalizeRegistryHost); docker.io/index.docker.io отклоняются на записи
  username   text not null,
  token      text not null,          -- plaintext, write-only через API: GET/события/логи его никогда не отдают
  note       text not null default '',
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);
