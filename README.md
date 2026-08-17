# birdman

**Lightweight dedicated-server hosting runtime for session-based multiplayer games — no Kubernetes.**

[![agent](https://github.com/ufna/birdman/actions/workflows/agent.yml/badge.svg)](https://github.com/ufna/birdman/actions/workflows/agent.yml)
[![master](https://github.com/ufna/birdman/actions/workflows/master.yml/badge.svg)](https://github.com/ufna/birdman/actions/workflows/master.yml)
[![panel](https://github.com/ufna/birdman/actions/workflows/panel.yml/badge.svg)](https://github.com/ufna/birdman/actions/workflows/panel.yml)
[![sdk](https://github.com/ufna/birdman/actions/workflows/sdk.yml/badge.svg)](https://github.com/ufna/birdman/actions/workflows/sdk.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

<p align="center">
  <img src="docs/images/panel-overview.png" width="100%" alt="birdman admin panel — Overview: 50 live matches, 583 players online, a ready buffer of 24 dedicated servers across three regions, 12 of 12 nodes active, fleet version 1.14.3, a sparkline of matches this hour and a real-time event feed (dark theme)">
  <br>
  <em>Overview — the live fleet at a glance: matches, players, warm buffer, nodes and a real-time event feed.</em>
  <br>
  <sub><i>Panel screenshots are captured against the built-in demo dataset (<code>npm run dev:demo</code> in <code>panel/</code>), not a production fleet.</i></sub>
</p>

birdman runs your own fleet of dedicated game servers — matchmaking, allocation, deploys and observability — without Kubernetes. It is three cooperating pieces: a **master** (matchmaker + fleet control + REST API + admin panel), a **node agent** on each machine that runs dedicated servers as containerd containers, and an **in-server SDK** the game server links against. Linux-only, session-based (short-lived matches, not persistent worlds). Built for our own games and open-sourced under MIT.

## Features

- **Warm pool, millisecond allocation.** Ready dedicated servers wait in a buffer, so a match gets a `host:port` in milliseconds instead of a cold container start.
- **Soft multi-version deploys.** Roll out a new build with two versions live at once, drain the old one gracefully, roll back instantly — without dropping matches in flight.
- **Region- and RTT-aware matchmaker.** Tickets are placed by region and by real measured round-trip time to the fleet.
- **Many games and environments, one install.** Projects and environments are a first-class dimension: nodes, versions, matches, events, metrics and logs each belong to one `(project, env)` pair, every screen narrows to the selected scope, and an API key bound to a pair can reach nothing outside it.
- **mTLS agent link.** Nodes enroll against a built-in CA and dial the master over mTLS gRPC, so game machines need no inbound admin port.
- **Secrets encrypted at rest.** Registry tokens and the internal CA key are sealed with AES-256-GCM.
- **Central observability.** Per-server logs, fleet metrics and alerts in one place; log history survives server reaping.
- **Postgres backups with an offsite copy.** Scheduled dumps with local retention and an optional S3 target; schedule, history, failures and run-now live in the panel, and a stale backup raises an alert.
- **Isolated control-plane overlay.** An optional WireGuard overlay keeps master↔node traffic off the public internet.
- **Bilingual admin panel.** Real-time fleet, matches, deploys, statistics, cost and alerts — English/Russian, light/dark, embedded in the master binary.
- **Self-host in one command.** `docker compose up` brings up the master and Postgres with the panel baked in.
- **AI-ready by construction.** The master is itself an MCP server, and its OpenAPI 3.1 contract is generated from the router — so an agent can drive the fleet without a separate integration to install or version.

## Two versions live at once

A new build does not replace the running one — it joins it. birdman pre-pulls the image on every node, flips the fleet once the pull lands, and lets the previous version *drain*: its dedicated servers keep serving the matches they already hold and disappear as those matches end. Nobody is dropped mid-game. A rollback is the same move in reverse.

<p align="center">
  <img src="docs/images/panel-deploys.png" width="100%" alt="birdman admin panel — Deploys: the multi-version window with 1.15.0 active on 14 dedicated servers and 1.14.2 deprecated with 6 still draining, active version per region, a pre-pull progress bar for 1.15.1 at 7 of 12 nodes, and the version table with states and live dedic counts (dark theme)">
  <br>
  <em>Deploys — 1.15.0 and 1.14.3 both active, 1.14.2 draining its last six dedicated servers, 1.15.1 pre-pulling across the fleet.</em>
</p>

## What the fleet is doing

Per-server logs, fleet metrics and alerts land in one place. The master proxies VictoriaMetrics and VictoriaLogs, so the panel never talks to your monitoring stack directly and log history outlives the servers that wrote it.

<p align="center">
  <img src="docs/images/panel-stats.png" width="100%" alt="birdman admin panel — Statistics: 24-hour charts of players online, matches running, matchmaker queue depth and slot utilization, plus dedicated servers by state over time with an allocated/ready/draining/creating legend (dark theme)">
  <br>
  <em>Statistics — players, running matches, queue depth and slot utilization over 24 hours, with dedicated servers by state below.</em>
</p>

## Quickstart

Bring up the master (REST API + admin panel + Postgres) with Docker Compose. You only need Docker and Docker Compose v2.

```bash
git clone https://github.com/ufna/birdman.git && cd birdman/deploy
cp .env.example .env                                  # 1. set POSTGRES_PASSWORD (don't leave change-me)
umask 077 && openssl rand -base64 32 > secrets.key    # 2. at-rest secrets encryption key
docker compose up -d --build                          # 3. build & start (postgres + master)
docker compose logs master | grep 'bootstrap admin'   # 4. admin key (bmk_…) — shown ONCE, save it
# then open http://127.0.0.1:8100 in your browser     # 5. panel + REST (host localhost only)
```

Adding game nodes, releasing a build and running your first match: [docs/self-host.md](docs/self-host.md).

Want to see the panel before installing anything? `cd panel && npm ci && npm run dev:demo` opens it against a built-in demo fleet — no master, no database, no nodes.

## Architecture

birdman has three moving parts and one firm rule about traffic.

- **master** (Go + Postgres) is the brain: it runs the matchmaker, reconciles the fleet (warm pool, deploys, drain), serves the REST + SSE API, exposes `/metrics`, and embeds the admin panel (`go:embed`).
- **agent** runs on every game machine over containerd. It starts and supervises dedicated servers as containers, assigns ports from a pool, and ships their logs and metrics.
- The **SDK** — a small C++ library — is linked into the dedicated server. It reports lifecycle, players and tick metrics to the agent over a local unix socket; there is no network or token inside the container.

**Control plane and game traffic are separate paths.** The agent dials *out* to the master over mTLS gRPC, so nodes expose no inbound admin port and the master is the only managed public address; Postgres is the single source of truth. **Players connect straight to the dedicated server's `host:port`** — game traffic never passes through the master, so restarting the master never interrupts a live match.

**birdman authenticates infrastructure, not players.** Operators and services
present scoped API keys and nodes present mTLS certificates, but `player_id` in a
matchmaking ticket is an opaque string the master trusts and never persists — so a
`matchmaking` key belongs to your game backend, which authenticates the player
and files tickets on their behalf, not to the game client. Details: the
[self-host guide](docs/self-host.md), "The `matchmaking` key and `player_id`".

Full component specs: [docs/specs/architecture.md](docs/specs/architecture.md) *(in Russian)*.

## Why no Kubernetes

birdman runs its own runtime — a master plus a per-machine agent over containerd
— instead of a game-server layer on top of a cluster. A session-based fleet runs
one kind of process, with a lifecycle Kubernetes has no opinion about: a server
is born warm, waits, serves exactly one match, and dies. What matters is that a
match gets a `host:port` in milliseconds and that a rollout never interrupts a
game in progress. Running that on Kubernetes means operating two systems — the
cluster and the game-server layer above it — where a fleet of dozens of machines
running a single workload needs neither.

The price is real: no Helm, no HPA, no service mesh, no `kubectl` for debugging,
no cloud autoscaler. Machines are provisioned with Ansible, and the operational
tooling is ours alone.

**birdman is the wrong choice if** you already run Kubernetes with a team that
knows it, your game is a persistent world rather than short sessions, your
dedicated server is Windows-only, or you want a managed service instead of
infrastructure you operate yourself. Every decision behind the runtime, with what
it costs you: [docs/design-decisions.md](docs/design-decisions.md).

## AI agents

**A running master *is* an MCP server.** Point an MCP client at `POST /v1/mcp`
(streamable HTTP) with an ordinary birdman API key, and an agent can look at
matches, nodes, dedicated servers, events, alerts, logs and metrics — and, if you
let it, drain a node or roll a build out.

```bash
claude mcp add --transport http birdman https://your-master.example/v1/mcp \
  --header "Authorization: Bearer bmk_..."
```

Nothing to install and no second version to track: the endpoint ships inside the
master binary you already run.

**Its permissions are the key's permissions.** `tools/list` is assembled per
request from the scopes of the key presented, so a `readonly` key does not see
write tools at all — it cannot spend context on them or try to call them. Write
tools stay hidden until the operator sets `mcp.write_enabled` (default `false`),
because "let an agent change the fleet" is a decision of a different kind than
handing out a scope, and it deserves its own switch. A key bound to a
`(project, environment)` pair reaches nothing outside it here either.

**The tools cannot drift from the API**, because a tool call *is* an API call:
the handler builds a real HTTP request and runs it through the master's own
router, with the caller's key. Authorization, tenant narrowing, rate limits and
error shapes are inherited rather than reimplemented, and a test fails if any
tool names a route the router does not have.

**There is a machine-readable contract.** OpenAPI 3.1, at
[`master/api/openapi.yaml`](master/api/openapi.yaml) and served by a live master
at `GET /v1/openapi.yaml`. It is *generated* from the route table the router is
registered from, so it cannot silently disagree with the code, and CI fails if
the committed file falls behind. It covers every path, method, required scope,
success code and response body; query parameters and request bodies are not
described yet.

**Contributing agents get a map too:** [AGENTS.md](AGENTS.md) has the build and
test commands, the repository layout and the rules that are not visible from the
code, and [llms.txt](llms.txt) points at the docs with their language labelled.


## Status

Built for our own games, then open-sourced. Iterations 0–5 are implemented and accepted on a live multi-node fleet: matchmaking, warm pool, multi-version deploys with rollback and drain, observability, mTLS agent enrollment, at-rest secret encryption, and a second region reached over an isolated overlay. Since then the platform became multi-tenant — projects and environments with API keys bound to a pair — and grew scheduled Postgres backups with an S3 target. It is young software shaped around our own needs — expect rough edges, and APIs that may still change.

## Documentation

| Doc | What's inside |
|---|---|
| [Self-host guide](docs/self-host.md) | From `git clone` to your first match: master (`deploy/`), first node (`infra/add-node.sh`), release a version and run a match (`mmcli`). |
| [Component specs](docs/specs/README.md) | master, agent, SDK, protocols, panel, ops/CI — reference specs *(in Russian)*. |
| [Design decisions](docs/design-decisions.md) | Why the runtime is shaped this way, what each choice costs, and when to pick something else. |
| [Panel screenshots](tools/panelshots/README.md) | How the screenshots above are produced from the panel's demo dataset. |
| [AGENTS.md](AGENTS.md) | Build, test and change this repo — written for coding agents, useful to humans. |
| [OpenAPI contract](master/api/openapi.yaml) | The master's REST API, generated from its route table; a live master serves it at `/v1/openapi.yaml`. |
| [Contributing](CONTRIBUTING.md) | How to propose a change, and what is unlikely to be accepted. |
| [Security policy](SECURITY.md) | How to report a vulnerability, and what is in scope. |
| [LICENSE](LICENSE) | MIT. |

Code comments occasionally reference internal design notes (`docs/superpowers/...`) — those live in a private companion repo; the public specs are under `docs/specs/`.

## License

[MIT](LICENSE) © 2026 ufna.
