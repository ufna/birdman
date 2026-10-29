# Party matchmaking (local implementation)

Status on 2026-10-29: the API and in-process scheduler are implemented and
tested. **Do not deploy this branch for recoverable party traffic yet.** The
queue, request fingerprints and assignments are still in memory. Durable
request/assignment storage and an allocation command outbox are required before
deployment; the CSO tracker owns that remaining work (`cso-ops-orj.4.2`).

## API and ownership

The existing `POST /v1/matchmaking/tickets` accepts either the original
`player_id` plus `regions`, or a `members` array:

```json
{
  "project": "game",
  "env": "dev",
  "client_version": "1.0.0",
  "client_request_id": "00000000-0000-0000-0000-000000000001",
  "party_id": "00000000-0000-0000-0000-000000000002",
  "party_revision": 1,
  "members": [
    {"player_id": "player-a", "regions": [{"region": "eu", "rtt_ms": 30}]},
    {"player_id": "player-b", "regions": [{"region": "eu", "rtt_ms": 50}]}
  ],
  "allocation_payload": "application-owned opaque payload"
}
```

Party requests require canonical UUIDs for both IDs, a positive revision,
unique player IDs and at least one common measured region. Party size cannot
exceed the project's `match_size` or the API limit of 64 members. Mixed legacy
and party fields are invalid. Members share the request's client build.

Only an authenticated application backend should submit verified rosters.
Birdman preserves API-key project/env binding and does not verify player
identities, invitations or ownership within that scope. The CSO lobby owns
those checks and keeps its API key away from game clients.

An identical normalized request ID within the same project/env returns its
original ticket, including a cancelled or matched result. Changed content or
a stale party revision returns `409 request_conflict`. An overlap with a queued
player cancels the entire previous party. A higher party revision cancels the
previous queued ticket even if the new roster is disjoint. These guarantees
currently last only for the lifetime of the master and retained ticket (at
least five minutes after terminal status); they do not provide crash recovery.

## Selection and policies

The scheduler counts players, keeps each ticket intact and looks for the oldest
party that can participate in a complete group. A bounded subset-sum search
handles `4`, `3+1`, `2+2`, `2+1+1` and `1+1+1+1` without a greedy packing trap.
For example, `2,3,1` can match the older fitting `3+1` instead of blocking on `2`.

Party candidate regions are the intersection of every member's measurements.
Their score is the worst member RTT. FIFO between fitting parties precedes
RTT selection; queues containing only legacy requests retain their median RTT
scoring. The usual region widening still applies to the ranked common regions.

Without a policy, matching remains full-only. Set the project's `match_size`
through the existing project API and opt in to partial starts per project/env:

```yaml
matchmaking:
  policies:
    - project: game
      env: dev
      minimum_size: 1
      partial_wait_s: 30
      max_rtt_ms: 120
      expanded_max_rtt_ms: 150
```

These numbers are an example, not an approved CSO timing policy. Full groups
are preferred; a partial group must include the oldest eligible ticket after
its wait and meet the minimum size. RTT expansion takes effect after
`widen_after_s`; it never exceeds `expanded_max_rtt_ms`. Zero RTT limits disable
that bound. A zero partial wait disables partial starts. Policies are validated
at master startup; duplicate project/env entries are rejected. The ordinary
ticket TTL still expires queued tickets.

## Allocation and concurrency

The `allocated` SDK message receives the actual player count and a metadata
entry `ticket_<ticket UUID>` for every nonempty allocation payload (at most
512 bytes each). Existing metadata limits still apply. Payloads are omitted
from ticket snapshots, long-poll results and match-formed logs. Results include
server/version IDs and all ticket references, without other parties' rosters.
Legacy optional join tokens remain for single-player requests; the application
issues personal admission and reconnect tickets for party members.

A queue mutex fences validation, server claim and assignment publication.
Cancel/revision wins before this fence or receives the completed assignment
after it; concurrent ticks cannot allocate the same ticket twice. Store calls
in the background tick have its ten-second context deadline. This deliberately
serializes queue mutations during allocation; it is not multi-master fencing.

The upstream allocator commits a server claim before notifying its in-memory
command hub. A process crash or an ambiguous database response in that window
still needs durable recovery. Reconnect, admission verification and game-server
callbacks belong to the application and are not implemented by this patch.
