-- Node state 'down' (итерация 5 follow-up, РЕВИЗИЯ Task 2 в
-- docs/superpowers/specs/2026-07-11-iter5-followups-design.md §2): the
-- auto-set third lease step — quarantine silent longer than
-- node_down_after_min → 'down' + event node_down. Distinct from 'dead' on
-- purpose: 'dead' is the MANUAL revocation terminal (agentlink refuses a dead
-- node in every auth mode — service.go), so the lease checker must never set
-- it, or an outage longer than the threshold would permanently lock the node
-- out. 'down' self-heals: a heartbeat of a live agent session lifts it back to
-- active (touchNode), like quarantine.
--
-- The original constraint was declared inline on the column in 000001, so it
-- carries the default name nodes_state_check.
alter table nodes drop constraint nodes_state_check;
alter table nodes add constraint nodes_state_check
  check (state in ('active','draining','quarantine','down','dead'));
