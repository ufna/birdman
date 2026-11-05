# Recovering a node at a changed address

`POST /v1/nodes/{id}/reconcile-address` is an administrator-only operation for a
verified host replacement. It takes `project`, `env`, `region`, `hostname`,
`expected_public_ip` and `public_ip`. All identity preconditions must match the
registered node; the IP is operator intent, not an untrusted Hello or NAT source.

An unchanged desired address is an idempotent read. An actual change requires a
`down`, `quarantine` or `draining` node with no creating, ready, allocated or
draining server. The regional fleet advisory lock prevents racing warm-server
creation, and the node row lock fences identity changes. Busy placement, stale
expected IP, scope mismatch and a dead identity return409. Invalid addresses
return400. One `node_address_changed` audit event records each actual change.

A restored node keeps its UUID, token/certificate history and previous server
records. Its new Hello cannot rewrite the address or undrain it. Explicitly
undrain a drained replacement only after checking its certificate, agent version
and reachable game endpoint. A destroyed world's old matches cannot be recovered
by changing an IP; let normal lease handling terminate the lost assignments.

The contract is generated from the route table into `master/api/openapi.yaml`.
Integration tests cover unchanged/replayed repairs, active/dead/scope/live-server
rejection, concurrent fleet planning, credential continuity and preservation of
historical server state against real PostgreSQL.
