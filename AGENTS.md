# Working on birdman

Orientation for a coding agent (and for a human who has just cloned this). Read
this before touching anything; it is short on purpose.

birdman is a dedicated-server hosting runtime for session-based multiplayer
games — matchmaking, warm pool, deploys, observability — deliberately **without
Kubernetes**. Linux-only. MIT.

## Verify your change

```bash
./check.sh              # everything this machine can run
./check.sh master       # one component: master | agent | panel | sdk
```

`check.sh` skips loudly what it cannot run (no Go, no npm, no `node_modules`) —
a skip is never silent, because a silent skip reads as "checked".

It is a subset of CI, not a replacement: CI additionally runs the Postgres
integration tests, `buf` contract checks, ansible role tests, a TSAN build of the
SDK and a secret scan. Per-component commands, if you want them directly:

| Component | Where | Build / test |
|---|---|---|
| master | `master/` (Go) | `go build ./...`, `go vet ./...`, `go test ./...` (integration tests need Postgres) |
| agent | `agent/` (Go) | `go vet ./...`, `go vet -tags integration ./...`, `go test -race ./...` |
| panel | `panel/` (TS/React/Vite) | `npm ci`, then `npm run check`, `npm run lint`, `npm test`, `npm run build` |
| sdk | `sdk/` (C++) | `cmake -S sdk -B sdk/build -DCMAKE_BUILD_TYPE=Release && cmake --build sdk/build -j && ctest --test-dir sdk/build` |
| proto | `proto/` | regenerated in Docker; CI fails if generated files differ from source |

`master`, `agent`, `proto` and `sdk/mockagent` are **separate Go modules** — run
`go` inside each one, not from the repo root.

## Layout

```
master/     Go. Matchmaker, fleet reconciler, REST+SSE API, embeds the panel.
  internal/httpapi/routes.go     ← the route table: the API's single source of truth
  internal/httpapi/mcp.go        ← the MCP endpoint (/v1/mcp)
  api/openapi.yaml               ← GENERATED contract, do not hand-edit
agent/      Go. Runs on every game machine; supervises dedicated servers over containerd.
sdk/        C++ library linked into the game server; talks to the agent over a unix socket.
proto/      gRPC contract for the master↔agent link (buf).
panel/      TS/React admin panel; built into the master binary via go:embed.
infra/      Ansible roles, inventories, CI helper scripts.
deploy/     docker compose stack (master + Postgres) for self-hosting.
docs/specs/ Component specs — IN RUSSIAN, and large (master.md is ~200 KB).
```

## Rules that are not obvious from the code

These are the ones that get broken by well-meaning changes:

1. **The panel is 100% bilingual, EN + RU.** Every user-visible string goes
   through `panel/src/lib/i18n.tsx`; a hardcoded string is a bug, and
   `panel/src/test/i18n.test.tsx` is the guard that fails on one. No exceptions,
   including error toasts and empty states.
2. **`README.md` and `README.ru.md` are a pair**, as are `docs/self-host.md` and
   `docs/self-host.ru.md`. Edit one, edit the other in the same commit.
3. **Never hand-edit `master/api/openapi.yaml`.** It is generated from the route
   table; run `go generate ./...` inside `master/`. A hand-written contract is a
   second copy of the truth and drifts silently — the whole design exists to
   prevent that.
4. **A new endpoint means one new entry in `master/internal/httpapi/routes.go`**,
   with its scope, summary and a response type from `dto.go`. Do not call
   `s.mux.HandleFunc` anywhere: the router is registered from the table, so a
   route outside it simply does not answer.
5. **The panel is a plain client of the public API.** If a screen needs data,
   add the endpoint first. There are no private side doors (ADR-9).
6. **No Kubernetes, ever.** Running without it is the product, not a limitation
   waiting to be fixed.
7. **Draining is not an outage.** A draining node or version keeps serving the
   matches it already holds. Code and copy must not treat it as a failure state.
8. Comments explain **why**, not what, and they are dense in this codebase —
   many carry a tracker number. Match the surrounding style; several files mix
   English and Russian, and that is deliberate rather than an oversight to fix.

## Commits

Conventional-commit prefixes with a **Russian** subject line — match the
existing history:

```
feat(panel): демо-ряды метрик — сутки с формой в любом окне
fix(panel): дренящаяся нода больше не подписывается «все активны» на Обзоре
test(panel): гвард i18n разбирает код парсером
```

Scopes in use: `master`, `agent`, `panel`, `sdk`, `proto`, `infra`, `docs`,
`tools`, `oss`, `chore`.

## If you are an AI agent operating a running fleet

You do not need to read this repository for that — point your MCP client at a
master's `/v1/mcp` and use the tools it exposes. See the "AI agents" section of
[README.md](README.md).
