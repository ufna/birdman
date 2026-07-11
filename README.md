<!-- NB: bilingual pair — правишь один, правь второй (README.md ↔ README.ru.md). -->
# birdman

**Lightweight dedicated-server hosting runtime for session-based multiplayer games — no Kubernetes.**

[![agent](https://github.com/ufna/birdman/actions/workflows/agent.yml/badge.svg)](https://github.com/ufna/birdman/actions/workflows/agent.yml)
[![master](https://github.com/ufna/birdman/actions/workflows/master.yml/badge.svg)](https://github.com/ufna/birdman/actions/workflows/master.yml)
[![panel](https://github.com/ufna/birdman/actions/workflows/panel.yml/badge.svg)](https://github.com/ufna/birdman/actions/workflows/panel.yml)
[![sdk](https://github.com/ufna/birdman/actions/workflows/sdk.yml/badge.svg)](https://github.com/ufna/birdman/actions/workflows/sdk.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Русская версия: [README.ru.md](README.ru.md)

birdman runs your own fleet of dedicated game servers — matchmaking, allocation, deploys and observability — without Kubernetes. It is three cooperating pieces: a **master** (matchmaker + fleet control + REST API + admin panel), a **node agent** on each machine that runs dedicated servers as containerd containers, and an **in-server SDK** the game server links against. Linux-only, session-based (short-lived matches, not persistent worlds). Built for our own games and open-sourced under MIT.

<p align="center">
  <img src="docs/images/panel-overview.png" width="100%" alt="birdman admin panel — Overview: live matches, players online, ready buffer, node count, fleet version and a real-time events feed (dark theme)">
  <br>
  <em>Overview — the live fleet at a glance.</em>
</p>

<p align="center">
  <img src="docs/images/panel-stats.png" width="100%" alt="birdman admin panel — Statistics: players online, matches running, matchmaker queue depth, slot utilization and dedicated servers by state over time (dark theme)">
  <br>
  <em>Statistics — players, matches, queue depth and utilization over time.</em>
</p>

## Features

- **Warm pool, millisecond allocation.** Ready dedicated servers wait in a buffer, so a match gets a `host:port` in milliseconds instead of a cold container start.
- **Soft multi-version deploys.** Roll out a new build with two versions live at once, drain the old one gracefully, roll back instantly — without dropping matches in flight.
- **Region- and RTT-aware matchmaker.** Tickets are placed by region and by real measured round-trip time to the fleet.
- **mTLS agent link.** Nodes enroll against a built-in CA and dial the master over mTLS gRPC, so game machines need no inbound admin port.
- **Secrets encrypted at rest.** Registry tokens and the internal CA key are sealed with AES-256-GCM.
- **Central observability.** Per-server logs, fleet metrics and alerts in one place; log history survives server reaping.
- **Isolated control-plane overlay.** An optional WireGuard overlay keeps master↔node traffic off the public internet.
- **Bilingual admin panel.** Real-time fleet, matches, deploys, statistics, cost and alerts — English/Russian, light/dark, embedded in the master binary.
- **Self-host in one command.** `docker compose up` brings up the master and Postgres with the panel baked in.

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

## Architecture

birdman has three moving parts and one firm rule about traffic.

- **master** (Go + Postgres) is the brain: it runs the matchmaker, reconciles the fleet (warm pool, deploys, drain), serves the REST + SSE API, exposes `/metrics`, and embeds the admin panel (`go:embed`).
- **agent** runs on every game machine over containerd. It starts and supervises dedicated servers as containers, assigns ports from a pool, and ships their logs and metrics.
- The **SDK** — a small C++ library — is linked into the dedicated server. It reports lifecycle, players and tick metrics to the agent over a local unix socket; there is no network or token inside the container.

**Control plane and game traffic are separate paths.** The agent dials *out* to the master over mTLS gRPC, so nodes expose no inbound admin port and the master is the only managed public address; Postgres is the single source of truth. **Players connect straight to the dedicated server's `host:port`** — game traffic never passes through the master, so restarting the master never interrupts a live match.

Full component specs: [docs/specs/architecture.md](docs/specs/architecture.md) *(in Russian)*.

## Status

Built for our own games, then open-sourced. Iterations 0–5 are implemented and accepted on a live multi-node fleet: matchmaking, warm pool, multi-version deploys with rollback and drain, observability, mTLS agent enrollment, at-rest secret encryption, and a second region reached over an isolated overlay. It is young software shaped around our own needs — expect rough edges, and APIs that may still change.

## Documentation

| Doc | What's inside |
|---|---|
| [Self-host guide](docs/self-host.md) | From `git clone` to your first match: master (`deploy/`), first node (`infra/add-node.sh`), release a version and run a match (`mmcli`). |
| [Component specs](docs/specs/README.md) | master, agent, SDK, protocols, panel, ops/CI — reference specs *(in Russian)*. |
| [LICENSE](LICENSE) | MIT. |

Code comments occasionally reference internal design notes (`docs/superpowers/...`) — those live in a private companion repo; the public specs are under `docs/specs/`.

## License

[MIT](LICENSE) © 2026 Vladimir Alyamkin.
