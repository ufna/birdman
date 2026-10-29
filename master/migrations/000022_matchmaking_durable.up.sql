-- Ticket bodies and allocation payloads contain application admission keys.
-- Encrypt the entire body with the existing master at-rest key.
create table matchmaking_tickets (
  id uuid primary key,
  project_id uuid not null references projects(id) on delete cascade,
  env text not null,
  request_id uuid,
  party_id uuid,
  party_revision bigint not null default 0,
  status text not null check (status in ('queued','matched','update_required','cancelled','expired')),
  body text not null check (body like 'birdman:v1:%'),
  created_at timestamptz not null,
  updated_at timestamptz not null default now(),
  foreign key (project_id,env) references environments(project_id,name) on delete cascade,
  unique(project_id,env,request_id)
);
create index matchmaking_queued_idx on matchmaking_tickets(status,updated_at);
create index matchmaking_party_idx on matchmaking_tickets(project_id,env,party_id,party_revision desc);

create table allocation_outbox (
  cmd_id uuid primary key,
  node_id uuid not null references nodes(id) on delete cascade,
  server_id uuid not null unique references servers(id) on delete cascade,
  match_id uuid not null,
  players_expected integer not null,
  metadata text not null check(metadata like 'birdman:v1:%'),
  last_sent_at timestamptz,
  acked_at timestamptz,
  created_at timestamptz not null default now()
);
create index allocation_outbox_pending_idx on allocation_outbox(last_sent_at) where acked_at is null;
