-- Roll 'down' nodes back into 'quarantine' — the state they were auto-promoted
-- from — so the restored (narrower) constraint holds. NOT into 'dead': that is
-- the manual revocation terminal and would lock the nodes out of agentlink.
update nodes set state = 'quarantine' where state = 'down';
alter table nodes drop constraint nodes_state_check;
alter table nodes add constraint nodes_state_check
  check (state in ('active','draining','quarantine','dead'));
