# Design decisions

Why birdman is shaped the way it is — and what each choice costs you.

Every entry is a decision we would make again, stated with its price and with the
case where you should pick something else instead. If you are evaluating birdman,
read the "wrong choice when" lines first: they will tell you faster than any
feature list whether this fits.

---

## No Kubernetes

birdman runs its own thing: a **master** that matchmakes and reconciles the
fleet, and an **agent** on each game machine that runs dedicated servers as
containerd containers. No cluster, no operator, no CRDs, no `kubectl`.

A session-based game fleet is not a general workload. It runs one kind of
process, with a lifecycle Kubernetes has no opinion about — a server is born
warm, waits, serves exactly one match, and dies. What matters is that a match
gets a `host:port` in milliseconds and that a rollout never interrupts a game in
progress. Kubernetes gives you neither directly; you add a game-server layer on
top of it, and then you own two systems instead of one — the cluster and the
layer — plus the people who keep both alive. For a fleet of dozens of machines
running a single workload, that trade is upside down.

**What it costs.** No ecosystem: no Helm charts, no HPA, no service mesh, no
`kubectl` for debugging, no cloud autoscaler. Machines are provisioned with
Ansible and joined by a script, not requested from an API. Our operational
tooling is ours alone, which means it is thinner than what a mature cluster
gives you, and it is on us to keep it good.

**Wrong choice when** you already run Kubernetes and have people who know it —
then the marginal cost of adding a game-server layer is much lower than the cost
of a second, unfamiliar runtime. Also wrong when you need to schedule workloads
other than dedicated servers, or want a managed control plane you do not operate
yourself.

## The agent dials out; game machines have no inbound admin port

Nodes open an mTLS gRPC connection *to* the master. The master never connects to
a node.

This means a game machine exposes only what players need — the game ports and a
QoS probe — and nothing an operator would use. There is exactly one managed
public address in the system.

**What it costs.** The master becomes that single address, and everything
administrative flows through it. You cannot reach into a node from the control
plane; if an agent stops dialing, you go in over SSH like any other host.

**Wrong choice when** your network policy forbids outbound connections from game
machines, or when you need push-style control of nodes that are offline.

## Players connect straight to the dedicated server

Game traffic never passes through the master. The matchmaker hands the client a
`host:port` and steps out of the way.

The consequence worth having: restarting or redeploying the master never
interrupts a live match. The control plane and the game are genuinely separate
failure domains.

**What it costs.** There is no central point to inspect, shape or filter game
traffic. DDoS protection is per-node and yours to arrange. Dedicated server IPs
are visible to players by construction.

**Wrong choice when** you need a relay for IP privacy, centralized traffic
scrubbing, or protocol inspection between players and servers.

## A pool of idle servers, kept warm on purpose

Ready dedicated servers wait in a buffer so allocation is a database write, not
a container start.

**What it costs.** You pay for capacity that is idle by design. That is not
waste to be optimized away later — it *is* the feature, and sizing the buffer is
a real operational dial with a real bill attached.

**Wrong choice when** cost per idle slot matters more than time-to-match:
turn-based games, games with a lobby the player expects to wait in, or anything
where ten seconds of matchmaking is fine.

## Two versions live at once, and the old one drains

A rollout does not replace the running version — it joins it. The previous
version keeps serving the matches it already holds and disappears as they end.
Rollback is the same move in reverse.

**What it costs.** Your fleet runs two builds simultaneously during every
rollout, so your game and your backend must tolerate that. Both images stay
pullable. "Draining" is a normal state you will see constantly and must not
treat as an incident.

**Wrong choice when** your backend cannot tolerate two server versions
coexisting, or when your compliance story requires a single deployed version at
any instant.

## birdman authenticates infrastructure, not players

Operators and services present scoped API keys; nodes present mTLS certificates.
A `player_id` in a matchmaking ticket is an opaque string the master trusts and
never persists.

That is why a `matchmaking` key belongs to **your game backend**, which
authenticates the player and files tickets on their behalf — never to the game
client.

**What it costs.** You must run that backend. birdman will not tell you whether
a player is who they claim to be, and shipping a matchmaking key inside a client
hands anyone the ability to file tickets as anybody.

**Wrong choice when** you wanted a turnkey player-facing matchmaking service
rather than a fleet runtime you put behind your own.

## Postgres is the single source of truth

Fleet state, matches, versions, events and secrets all live in one database. The
master holds no authoritative state of its own; it can be restarted freely.

**What it costs.** Master availability is bounded by Postgres availability, and
there is no distributed-write story: one database, one region. Backups are yours
to own — which is why scheduled dumps with an offsite copy are built in rather
than left as an exercise.

**Wrong choice when** you need a globally distributed control plane, or an
availability target that a single Postgres cannot meet.

## Linux, containerd, session-based

Dedicated servers are Linux containers running short-lived matches.

**What it costs.** No Windows dedicated servers. No persistent worlds, no shards
that live for weeks — the whole lifecycle assumes a server exists for one match
and is then replaced.

**Wrong choice when** your server binary is Windows-only, or your game is a
persistent world rather than a sequence of sessions.

## The API contract is generated, not written

The route table in the master is the only description of the API surface: the
router is registered from it, and `openapi.yaml` is generated from it. A route
that is not in the table does not answer, and a contract that disagrees with the
code fails CI.

**What it costs.** Adding an endpoint means adding a table entry with its scope
and response type — you cannot quietly register a handler on the side.

We consider that a feature. A hand-written contract next to the code is a second
copy of the truth, and it drifts silently, because nothing forces anyone to
re-read it.

---

## When birdman is the wrong choice

The short version of everything above. Pick something else if:

- you already run Kubernetes and have a team that knows it;
- your game is a persistent world rather than short sessions;
- your dedicated server is Windows-only;
- you need players to reach servers through a relay rather than directly;
- you want a managed service instead of infrastructure you operate;
- you want player-facing matchmaking without running your own game backend;
- you need a control plane spanning regions with distributed writes.

Nothing here is a criticism of the alternatives. [Agones](https://agones.dev/)
on Kubernetes is a good answer to a genuinely different question: how to run
game servers *inside a cluster you already have*. birdman answers a narrower
one — how to run a fleet of dedicated servers when you would rather not have the
cluster at all.

## Status of this document

These are decisions, not predictions. If one of them changes, this file changes
with it — and if you find a claim here that the code no longer supports, that is
a bug worth reporting.
