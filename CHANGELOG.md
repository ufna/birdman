# Changelog

All notable changes to birdman are documented here, newest first.

birdman follows [Semantic Versioning](https://semver.org/). While the major
version is 0, the REST API and the master↔agent protocol may still change
between minor releases; both surfaces are gated in CI (the generated OpenAPI
contract and buf compatibility), so any break is deliberate and listed here.
Versions before 0.7.0 were internal iterations and were never tagged.

## [0.7.0] - 2026-08-23

First public release. Everything before it was internal iterations on our own
fleet — accepted in production there, but never tagged or published.

The platform, as shipped:

- **Matchmaking and allocation** — region- and RTT-aware tickets, millisecond
  allocation from a warm pool of ready dedicated servers.
- **Soft multi-version deploys** — image pre-pull, atomic fleet flip, instant
  rollback; the previous version drains and keeps serving its live matches.
- **Environments** — versions, fleets, nodes and the active version are scoped
  per `(project, env)`; `auto_deploy` dev streams, guarded prod, one-call
  promote dev → prod.
- **Multi-tenancy** — projects with API keys bound to a `(project, env)` pair;
  the binding narrows reads too, including the log/metrics proxies.
- **Node agent** — dedicated servers as containerd containers, port pools,
  self-upgrade via `POST /v1/agent-upgrade`, enrollment over strict mTLS
  (Enroll-by-token against the built-in CA).
- **Observability** — Prometheus metrics, an SSE event feed, per-server logs
  with history, alert rules surfaced in the panel; VictoriaMetrics/VictoriaLogs
  proxies with tenant narrowing.
- **Backups** — scheduled `pg_dump` with local retention and an optional S3
  offsite; a stale backup raises an alert.
- **Admin panel** — bilingual (EN/RU), light/dark, embedded in the master
  binary; a demo mode for a look without a fleet.
- **Contracts** — OpenAPI 3.1 generated from the route table and served at
  `GET /v1/openapi.yaml`; the master is an MCP server at `POST /v1/mcp`.
- **Self-host** — the `deploy/` compose stack for the master,
  `infra/add-node.sh` for nodes; see `docs/self-host.md`.

Release assets (linux/amd64): `birdman-master`, `mmcli`, `birdman-agent`,
`SHA256SUMS`. The agent binary is stamped `0.7.0` — pass exactly that string
as `version` to `POST /v1/agent-upgrade`, with the asset URL and its sha256
from `SHA256SUMS`.

Known rough edges, honestly:

- The OpenAPI contract does not describe query parameters and request bodies
  yet — paths, methods, scopes and response shapes only.
- `add-node.sh` wires metric/log shippers on the node unconditionally; on a
  bare `deploy/` master without VictoriaMetrics/VictoriaLogs they buffer into
  the void (`birdman_node_vmagent: false` in the host block turns them off).
