-- birdman master schema v1 — docs/specs/master.md §1 (DDL v1).
-- v0 clarifications (marked in the spec):
--   * nodes.token_hash — bcrypt of the node bootstrap token (until token→mTLS
--     cert exchange is implemented, see docs/specs/protocol.md §Auth);
--   * servers_match_id_uidx — allocation idempotency by match_id under
--     concurrency.

create table projects (
  id         uuid primary key default gen_random_uuid(),
  slug       text unique not null,          -- 'ourgame'
  created_at timestamptz not null default now()
);

create table nodes (
  id                uuid primary key default gen_random_uuid(),
  project_id        uuid not null references projects(id),
  region            text not null,          -- 'eu', 'us-east'
  hostname          text not null,
  public_ip         inet not null,
  capacity_slots    int  not null,
  agent_version     text not null default '',
  state             text not null default 'active'
                    check (state in ('active','draining','quarantine','dead')),
  last_heartbeat_at timestamptz,
  labels            jsonb not null default '{}',
  token_hash        text not null default '',
  created_at        timestamptz not null default now()
);

create table versions (
  id         uuid primary key default gen_random_uuid(),
  project_id uuid not null references projects(id),
  semver     text not null,                 -- '1.4.2'
  image_ref  text not null,                 -- 'ghcr.io/org/ourgame-server:1.4.2'
  channel    text not null check (channel in ('staging','prod')),
  state      text not null default 'registered'
             check (state in ('registered','prepulling','active','deprecated','disabled')),
  created_at timestamptz not null default now(),
  unique (project_id, semver, channel)
);

create table servers (
  id         uuid primary key default gen_random_uuid(),
  project_id uuid not null references projects(id),
  node_id    uuid not null references nodes(id),
  version_id uuid not null references versions(id),
  state      text not null default 'creating'
             check (state in ('creating','ready','allocated','draining','failed','reaped')),
  port       int  not null,
  players    int  not null default 0,
  tick_ms    real,
  match_id   uuid,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);
create index servers_ready_idx on servers (project_id, version_id, state);
create unique index servers_match_id_uidx on servers (match_id) where match_id is not null;

create table matches (
  id           uuid primary key default gen_random_uuid(),
  project_id   uuid not null references projects(id),
  server_id    uuid not null references servers(id),
  version_id   uuid not null references versions(id),
  region       text not null,
  state        text not null default 'pending'
               check (state in ('pending','running','finished','aborted')),
  players_peak int not null default 0,
  started_at   timestamptz,
  ended_at     timestamptz,
  created_at   timestamptz not null default now()
);

create table fleet_configs (            -- desired state per (project, region)
  project_id      uuid not null references projects(id),
  region          text not null,
  active_version  uuid references versions(id),
  buffer_ready    int  not null default 2,   -- how many ready servers to keep
  max_servers     int  not null default 50,
  reap_ttl_min    int  not null default 180, -- reap deadline for deprecated servers
  primary key (project_id, region)
);

create table events (                    -- audit + feed for panel/alerts
  id      bigint generated always as identity primary key,
  ts      timestamptz not null default now(),
  kind    text not null,                 -- node_quarantine, server_failed, deploy_started...
  node_id uuid, server_id uuid, match_id uuid, version_id uuid,
  payload jsonb not null default '{}'
);
create index events_ts_idx on events (ts desc);

create table api_keys (
  id     uuid primary key default gen_random_uuid(),
  name   text not null,
  hash   text not null,                  -- bcrypt of the key
  scopes text[] not null,                -- {admin} {deploy} {matchmaking} {allocate} {readonly}
  created_at timestamptz not null default now(),
  revoked_at timestamptz
);
