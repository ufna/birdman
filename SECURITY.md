# Security policy

## Reporting a vulnerability

**Do not open a public issue.** Report privately through GitHub:

→ [Report a vulnerability](https://github.com/ufna/birdman/security/advisories/new)

Please include what you need to reproduce it: version or commit, the component
(master, agent, SDK, panel), and what an attacker gains. A proof of concept
helps; a CVSS score is not required.

This is a small project run alongside other work — expect a first reply within a
week, not within an hour. If you get no answer in two weeks, ping the issue
tracker with a message that says only that you are waiting on a security report,
with no details.

## Supported versions

The `main` branch is what gets fixed. There are no maintained release branches
and no backports: birdman is young, and self-hosters are expected to track
`main`. If that changes, this section changes with it.

## What is in scope

- **master** — the REST/SSE API, API key handling and scopes, the (project,
  environment) binding, the panel session cookie, secrets encrypted at rest.
- **agent** — enrollment, the mTLS link to the master, container supervision,
  port assignment.
- **SDK** — the in-server library and its unix-socket protocol to the agent.
- **panel** — the admin UI served from the master binary.

## Worth knowing before you report

These are deliberate design decisions, not oversights:

- **birdman authenticates infrastructure, not players.** `player_id` in a
  matchmaking ticket is an opaque string the master trusts and never persists. A
  `matchmaking` key belongs to your game backend, which authenticates the player
  and files tickets on their behalf — **it is not meant to ship in a game
  client.** A report that a game client holding a matchmaking key can file
  tickets for other player ids describes the documented model.
- **`GET /v1/qos` and `GET /v1/openapi.yaml` are public by design.** The QoS
  endpoint has to work before a client holds any key; the contract describes an
  API whose source is public under MIT.
- **The master is the only managed public address.** Nodes expose no inbound
  admin port; they dial out. A finding that requires already having root on a
  game node is a different class of problem — say so, and we will still read it.
- **An `admin`-scoped key can do administrative things.** Privilege escalation
  matters when it crosses a scope or a (project, environment) binding: a
  `readonly` or bound key reaching outside its boundary is exactly the kind of
  report we want.

## Disclosure

Report privately, we fix, then we credit you in the release notes unless you
would rather stay anonymous. We will not ask you to stay quiet indefinitely: if
a fix is taking too long, say so and set a date.
