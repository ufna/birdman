# Party matchmaking (local implementation)

Status on 2026-10-29: the party API, scheduler, encrypted PostgreSQL tickets,
atomic assignments and allocation command outbox are implemented and tested.
The CSO tracker owns deployment and end-to-end acceptance. Identity and game
admission remain application responsibilities.

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
survive a master restart and the in-memory cache's terminal retention. Original
request IDs remain in SQL, so an old retry returns its original terminal result
instead of allocating another server. Queued state also survives a restart.

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

A queue mutex and PostgreSQL transaction advisory lock fence validation, server
claim and assignment publication across process instances.
Cancel/revision wins before this fence or receives the completed assignment
after it; concurrent ticks cannot allocate the same ticket twice. Store calls
in the background tick have its ten-second context deadline. This deliberately
serializes queue mutations during allocation. Each ticket body and allocation
metadata envelope uses the existing master at-rest encryption key, bound to
its row ID with authenticated encryption. Preserve that key with backups.

The matchmaking transaction commits the server claim, match row, all selected
ticket results and private command metadata together. Failed assignment writes
roll back the claim. After an ambiguous commit response, retries reload SQL.
The command outbox retries a stable `cmd_id` until an authenticated agent ACKs.
Every new authenticated agent Hello re-arms active allocations as well: an ACK
can precede delivery to the game, and an agent restart loses its cached Unix
socket frame. Replaying the same match and payload repairs that window without
claiming another slot. The older external `POST /v1/allocate` path is unchanged.

`GET /v1/servers/{id}` exposes authoritative state without the historical list's
1000-row limit. Readonly project/env bindings return the same 404 for foreign
and missing IDs. Applications can distinguish a draining live server from a
failed or reaped one without invalidating a valid long-running match.

## Portable game configuration

Nonsecret application settings are supplied at runtime, separately from the
OCI image. These values are scoped to the named project/environment and passed
through the existing agent command; `BIRDMAN_*` variables are reserved:

```yaml
game_runtime:
  - project: game
    env: dev
    variables:
      CSO_LOBBY_URL: https://lobby.example
      CSO_ENVIRONMENT: dev
```

Admission secrets belong in encrypted allocation metadata, not in this public
configuration. Reconnect, admission verification and game-server callbacks are
implemented by the application.
