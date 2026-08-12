<!-- NB: bilingual pair — правишь один, правь второй (self-host.md ↔ self-host.ru.md). -->
# birdman self-host: from `git clone` to your first match

Русская версия: [self-host.ru.md](self-host.ru.md)

End-to-end guide for a **third-party project**: bring up the master, add your
first game node, roll out a build version and get your first match. Every snippet
is verified against the code on the self-host branches. Quickstarts this document
builds on: `deploy/README.md` (master), `infra/README.md` (nodes/ansible),
`master/README.md` (REST API + `mmcli`).

---

## 0. What it is and what you need

birdman is a lightweight dedicated-server runtime **without Kubernetes**: `master`
(matchmaker + fleet controller + REST + built-in admin panel), `agent` on the game
nodes (on top of containerd) and an SDK inside the dedicated server itself.
Linux-only, session-based (one-shot matches, not persistent worlds).

Self-host topology:

- **master box** — a single host with Docker: master + Postgres (the `deploy/`
  stack). The only thing exposed externally is the nodes' gRPC port (`8444`,
  strict mTLS); REST and the panel listen on `127.0.0.1`.
- **game nodes** — one machine per region/capacity; added from the operator
  machine via `infra/add-node.sh` (ansible). A node needs Docker + containerd.

Requirements:

- **master box**: Docker + Docker Compose v2 (`docker compose version`), external
  access to port `8444`, a public IP.
- **operator machine** (where you add nodes from): `git`, `ansible-core`,
  `python3` + `PyYAML`, SSH access to the master box and to the nodes.
- **node**: Docker + containerd, SSH access, game ports open externally.

> v1 self-host builds the master **from source** (`docker compose … --build`).
> There is no public image yet — the `birdman-master` image compiles the panel in
> via `go:embed`, so self-host gets the whole product (see `master/Dockerfile`).

---

## 1. Master: bring up the `deploy/` stack

Everything runs from the `deploy/` directory of the repo clone:

```bash
git clone <repo-url> birdman && cd birdman/deploy

cp .env.example .env                                  # 1. set POSTGRES_PASSWORD (don't leave change-me)
umask 077 && openssl rand -base64 32 > secrets.key    # 2. at-rest secrets encryption key
docker compose up -d --build                          # 3. build and start (postgres + master)
docker compose logs master | grep 'bootstrap admin'   # 4. admin key (bmk_…) — shown ONCE, save it
curl -s http://127.0.0.1:8100/healthz                 # 5. check: master responds (panel at the same address)
```

- **POSTGRES_PASSWORD** — no URL-special characters (`/ @ # ? :`): the password
  is substituted into the `postgres://` DSN as-is (`deploy/.env.example`).
- **the admin key** is printed once to the startup log (`bootstrap admin API key
  created — store it now, it is shown exactly once`, field `api_key`); prefix
  `bmk_`, scope `admin`. Lose it and the only recovery is via the DB (deactivate the keys in the api_keys table → restart the master, which re-bootstraps a new one).
- **Panel + REST** — `http://127.0.0.1:8100`, panel login with this same
  admin key (or a key with the `readonly` scope). The port is published **only on
  `127.0.0.1`** of the master box.

**Panel/REST externally — via a reverse proxy or SSH tunnel.** compose
publishes `8100` on `127.0.0.1` on purpose; to open the panel to an admin, put a
reverse proxy with TLS and authentication in front of it, or go through a tunnel:

```bash
ssh -L 8100:127.0.0.1:8100 <user>@<MASTER_PUB_IP>   # then http://127.0.0.1:8100 on your machine
```

### Panel over a domain (optional)

If the master box already runs nginx (Debian/Ubuntu `sites-available` layout)
and a certificate for the name already exists, the `birdman_master_dev` role
publishes the panel for you — set two inventory variables and re-run
`playbooks/dev-node.yml`:

```yaml
birdman_panel_domain: panel.example.com
# under a wildcard cert this is the directory of the BASE name, not of the host:
birdman_panel_tls_dir: /etc/letsencrypt/live/example.com
```

An empty `birdman_panel_domain` (the default) means the role never touches nginx
at all. The role installs neither nginx nor certificates: on a box that serves
other sites that would be an intrusion, so it fails with a hint instead. It
writes exactly one file (`sites-available/birdman-panel` plus its symlink), runs
`nginx -t`, and if the test fails it removes the symlink again — a broken vhost
never survives to break somebody else's reload.

What the generated vhost does:

- terminates TLS, redirects `:80` → `:443`, proxies everything to
  `127.0.0.1:8100` and passes `X-Forwarded-Proto` — that is what makes the
  master mark the session cookie `Secure`;
- returns **403 on `/metrics`** — belt and braces only. Since tracker #1003 the
  master does not serve the Prometheus registry on the API port at all: it lives
  on its own listener (`listen_metrics`, default `127.0.0.1:9102`), and the API
  port answers 404 there. Point your vmagent at `127.0.0.1:9102`;
- rate-limits **`POST /v1/session`** (login) to 10/min per IP plus a 30 r/s
  overall cap, answering `429`; `GET`/`DELETE` on the same path are not counted,
  so the panel's session check is never throttled;
- turns proxy buffering off and gives SSE/log-tail endpoints a 1h read timeout,
  so realtime in the panel keeps flowing.

Weigh what a public name means: the login endpoint becomes reachable by anyone
and the admin key is the only thing in front of the panel. Keep that key long,
or put an extra layer (VPN, IP allowlist, an identity proxy) before it. The SSH
tunnel above keeps working as a fallback.

**Two self-host secrets, both git-ignored** (`deploy/.env`, `deploy/secrets.key`):

- `secrets.key` — the key that encrypts secrets in the DB at-rest. Loss = the DB
  secrets can't be decrypted. **Keep an escrow copy in a password manager**
  (`deploy/master.yaml`, recovery runbook: `docs/specs/ops.md §5`).
- `.env` with the Postgres password.

The master config is `deploy/master.yaml` (no secrets; the DSN arrives via the
`BIRDMAN_DSN` env variable from compose). `agentlink_auth: mtls` is on from day one.

---

## 2. First node: `infra/add-node.sh`

On the **operator machine** (the same clone):

```bash
cd birdman/infra
cp inventories/dev/hosts.example.yml inventories/dev/hosts.local.yml
```

`hosts.local.yml` is git-ignored — real IPs/users/keys live only there (the
self-host convention: access details outside git). Open the file and, in the
`birdman_dev` group, declare the host `birdman-dev` = your master box
(`ansible_host`, `ansible_user`, `ansible_ssh_private_key_file`).

**Node registration goes through the master box** (`delegate_to`): the agent role
reads the admin key from the file `/etc/birdman/master-admin.key` on the master
box and calls `POST /v1/nodes` on its own `127.0.0.1:8100`. The admin key never
leaves the master box. So on the master box you need to **put the admin key into a
file** (the `deploy/` stack doesn't put it there — it's in the compose logs):

```bash
# on the master box: pull bmk_… out of the logs and save it 0600
cd birdman/deploy && docker compose logs master | grep 'bootstrap admin'
umask 077 && printf '%s' 'bmk_…' | sudo tee /etc/birdman/master-admin.key >/dev/null
sudo chmod 600 /etc/birdman/master-admin.key
```

Then there are two ways to add a node.

### Path A — direct link (recommended for the `deploy/` stack)

The `deploy/` stack doesn't bring up our WireGuard overlay, so the agent talks to
the master box's public `8444` directly; the public link is held by **strict
mTLS** (a non-loopback `master_addr` ⇒ the agent's config gate requires mTLS).
Turn the overlay off across the whole fleet — add the line
`birdman_use_overlay: false` under `all: vars:` in `hosts.local.yml` (then the
hub play becomes a no-op) and remove the `birdman_overlay_ip: 10.77.0.1` line from
`birdman-dev`.

Add the node:

```bash
./add-node.sh node-eu-1 203.0.113.7 \
  --no-overlay --master-addr <MASTER_PUB_IP>:8444 \
  --region eu --user root --key ~/.ssh/id_ed25519
```

What the script does: validates the input, appends a host block to
`hosts.local.yml` (`birdman_use_overlay: false` + a direct `birdman_master_addr`),
shows a diff, and after confirmation runs `ansible-playbook playbooks/add-node.yml`.
The agent comes up as a daemon, registers (`POST /v1/nodes` → a one-time
`node_token`, 0600, on the node only), pulls the master's CA (`GET /v1/ca`) and
does Enroll-by-token: exchanges the `node_token` for a client mTLS cert.

Useful flags (`./add-node.sh -h`): `--port` (SSH port), `--slots`
(dedicated servers per node, default 2), `--dry-run` (just print the block, change
nothing), `-y/--yes` (no write confirmation).

```bash
./add-node.sh node-eu-1 203.0.113.7 --no-overlay --master-addr 203.0.113.1:8444 --dry-run
```

### Path B — through our overlay (birdman's internal default)

The `add-node.sh` default (without `--no-overlay`) assigns the node an overlay IP
`10.77.0.X` (X≥2) and points the control plane at the hub `10.77.0.1:8444`. This
path requires the **WireGuard hub to be up** on the master box (birdman's isolated
overlay, `10.77.0.0/24`, UDP `51827`) — it's installed by the `birdman_overlay`
ansible role, not the `deploy/` stack. This is the configuration birdman runs
itself (several nodes behind the overlay). Hub details/teardown —
`infra/README.md` (the "Second+ node" section). For a fresh self-host on the
`deploy/` stack, take Path A.

Check after adding (in the "Fleet" panel or via REST on the master box):

```bash
KEY=bmk_…   # admin key
curl -s http://127.0.0.1:8100/v1/nodes -H "Authorization: Bearer $KEY"   # node visible immediately (state=active)
```

---

## 3. Build version, fleet and first match

All REST calls use the admin key on the master box (`127.0.0.1:8100`) or through
the tunnel from §1. `KEY=bmk_…`.

```bash
# 1. register a build version (the game image). `env` (dev|prod|…) is required —
#    it replaced the old `channel`; see "Environments" below.
curl -s -X POST http://127.0.0.1:8100/v1/versions -H "Authorization: Bearer $KEY" \
  -d '{"project":"game","semver":"1.0.0","image_ref":"ghcr.io/org/game:1.0.0","env":"dev"}'

# 2. enable the region warm pool (matches the node's --region from §2)
curl -s -X PUT http://127.0.0.1:8100/v1/fleets/eu -H "Authorization: Bearer $KEY" \
  -d '{"project":"game","buffer_ready":2}'

# 3. soft-deploy the version: prepull the image to all nodes → atomic flip to active
curl -s -X POST http://127.0.0.1:8100/v1/deploy -H "Authorization: Bearer $KEY" \
  -d '{"version_id":"<version_id from step 1>"}'
```

> The node pulls the game image using registry credentials that the master
> distributes to nodes (`SetRegistries` over the mTLS link; configured in the
> panel, the "Registries" section). `add-node.sh` writes `birdman_registry_legacy:
> false` to the node — there's no registry token on the node.

After the flip, reconcile creates `buffer_ready` dedicated servers on the node;
wait until the servers become `ready` (the "Fleet" panel or `GET /v1/servers`).

**First match — `mmcli`** (a second binary, built by `./master/build.sh` →
`master/dist/mmcli`; a key with the `matchmaking` or `admin` scope). `mmcli`
creates a ticket, long-polls it and prints `host:port` on a match.

⚠️ **Flag order**: `--master`/`--key` are global and go **before** the `request`
subcommand; `--player`/`--version`/`--region`/`--measure`/`--project`/`--timeout`
go **after** `request`.

```bash
# region rtt is set by hand (NAME:RTT_MS, repeatable):
mmcli --master http://127.0.0.1:8100 --key $KEY request \
  --player p1 --version 1.0.0 --region eu:5
# → result JSON + the line "203.0.113.7:20001" on a match
```

Or let `mmcli` **measure** rtt itself — UDP probes via `GET /v1/qos` (an
acceptance measurement; in production the game client measures rtt). With
`--measure` the region can be omitted:

```bash
mmcli --master http://127.0.0.1:8100 --key $KEY request \
  --player p1 --version 1.0.0 --measure
# optional: --probes 5 --probe-timeout 700ms
```

`mmcli` exit codes: `0` — matched, `1` — a different terminal status/error, `2` —
usage. Two `mmcli request` calls with different `--player` in the same region get
the same `host:port` and `match_id` — that's your first match.

Direct allocation without the matchmaker (for integrating your own backend) —
`POST /v1/allocate {project,region,match_id}` (idempotent by `match_id`,
`409 no_capacity` when there's no capacity). Full REST — `master/README.md`.

### Environments: dev, prod, promote

Every project has **environments** — at least `dev` (the CI build stream) and
`prod` (what real players match into). They are a full dimension, not a label:
versions, fleets, nodes, servers and matches each belong to one `(project, env)`,
and the active version is scoped per env — a dev build can never flip what prod
players play. `POST /v1/versions` (step 1 above) therefore takes `env` (dev|prod),
not the old `channel`.

Behaviour is driven by two per-env flags, not by the name:

- **`auto_deploy`** (dev default): registering a version immediately prepulls and
  activates it — every CI push goes live in dev with no extra call. On a burst only
  the newest is deployed ("forward-only"); a failed build is not retried, the next
  push supersedes it. Forbidden together with `production` (DB + API guardrail).
- **`production`** (prod default): auto-deploy is refused, retention keeps versions
  forever, alerts are critical.

**Promote dev → prod** is one call — it registers the *same image_ref* in the
target env (provenance `promoted_from`) and deploys it:

```bash
curl -s -X POST http://127.0.0.1:8100/v1/promote -H "Authorization: Bearer $KEY" \
  -d '{"version_id":"<the dev version>","to_env":"prod"}'
```

Idempotent (re-promoting the same ref reuses the registered row). If a prod deploy
is already in flight the promote returns `409` — the version is already registered
in the target env; retry the promote once the running deploy finishes (it reuses
the registered row and starts the deploy).

**Nodes belong to one env.** New nodes join as `dev` (never prod implicitly); move
an empty node explicitly — any state but `dead`, no live servers on it:

```bash
curl -s -X PATCH http://127.0.0.1:8100/v1/nodes/<node_id> -H "Authorization: Bearer $KEY" \
  -d '{"env":"prod"}'
```

**Keys can be bound** to a `(project, env)` pair (`POST /v1/apikeys {project, env}`)
— a bound deploy key may only touch its own env (a dev key on a prod deploy → 403).
Leave both fields unset for a global key (the default; existing keys keep working).
CI uses two: a dev-bound key on every push (auto-deploy) and a prod-bound key gated
behind a GitHub environment approval for the promote.

**Binding is a tenant boundary, not just a write gate.** It narrows READS too,
but NOT everywhere at the same granularity, and the difference matters:

- **by the (project, env) pair** — listings and aggregates (an explicit foreign
  `?project=` → `403`, no parameter → narrowed to your own pair) and the raw
  observability proxies;
- **by PROJECT only** — the event feed and its live SSE stream, active alerts
  and mutes. A key bound to `game/dev` sees `game/prod` events and alerts too:
  there is no per-environment split on those surfaces.

**The two filters differ in one more way, and it is the one that surprises
operators.** The second one is **non-hiding**: a row with no project — a
platform event, a `ScrapeTargetDown`/`DiskHigh` alert — stays visible to every
bound key, because otherwise a tenant would never learn that the platform
itself is down. (A silent master arrives as `ScrapeTargetDown`: the scrape has
a `birdman-master` job. There is no `MasterDown` rule at all — it would need an
EXTERNAL probe, see `ops.md` §1 — and `NodeDown` stopped being an example in
#1064, when its series gained a `project` label.) The first one — the raw observability proxies — **hides**: a log line or
a metric series that does not carry the pair is not served to a bound key at
all. Measured on VictoriaLogs v1.51.0 / VictoriaMetrics v1.102.1: an unlabelled
legacy log line and the label-less `birdman_players_online` series are returned
to a global key and NOT returned to a bound one. That is the same trap as
`log_scope_dirs` below — see "Who sees what" in section 4.

What binding does not close at all: `GET /metrics` is unauthenticated and
carries `{project, env, server_id}` (project slugs, environment names and the
shape of the fleet, with no key at all), and `GET /v1/qos` is public by design
and returns the addresses of active nodes. The registry is no longer reachable
from the API port, though: since tracker #1003 it is served on a separate
listener, `listen_metrics`, defaulting to `127.0.0.1:9102` — so whoever reaches
the API port does not get it, and the guarantee no longer depends on your proxy
configuration. Keep that listener on loopback (or set `listen_metrics: ""` to
turn the endpoint off entirely if you do not scrape it), and do not proxy
`/metrics` outward.

Two things are worth knowing BEFORE you hand bound keys out:

- a client that used to read the whole platform with a bound key is broken
  deliberately;
- **observability has its own price, and it is not merely "you see less"** — a
  bound key will not see some of its OWN logs at all. The details are in
  section 4, "Who sees what"; read it before you issue an operator a bound key.

`GET /v1/alerts/rules` is deliberately left open: it is a configuration catalog,
not alert instances, and without it a tenant cannot explain its own alerts to
itself. The price is that the `state` and `expr` of other tenants' rules are
visible; do not hardcode a project slug into rule expressions.

**Retention** (`retention_keep` on the env, dev default 20, prod 0 = unlimited):
versions past the newest N are moved to `disabled` and their images are dropped from
the env's nodes. Removal happens in two beats: a `RemoveImage` goes out immediately on
the `disabled` transition, and — because the version's containers are usually still
draining at that exact moment (the agent then skips a busy image) — a **converging
sweep** in the same ~60s tick re-sends it once the version has no live servers left.
The agent reports the RESULT of every removal back (`removed`, `absent`, `busy`,
`error`), and the master stamps `versions.image_cleanup_at` — «done, never re-send» —
only once EVERY target node of the env has confirmed the image is gone; a `busy` or
`error` leaves the version unmarked, so the next sweep simply tries again. The sweep
sends nothing at all to a node that is currently offline (it would only pile duplicates
into an in-memory queue). Watch `birdman_image_removals_total{status}` — a fleet stuck
on busy/error is leaking disk. The disk-watermark GC stays as a backstop.
`active`/`prepulling`/`deprecated` are never touched — the rollback window stays warm.

Manage environments under **Admin → Environments** in the panel, or via
`GET/POST /v1/environments` and `PATCH /v1/environments/{project}/{name}`.
Deleting an environment removes everything inside it (versions, fleets, matches,
servers; bound API keys are revoked), so it asks you to type the environment name
to confirm — and it refuses while any node still lives there: move the nodes to
another environment first (Fleet → *Move to env…*, or `PATCH /v1/nodes/{id}`).

---

## 4. Observability (optional)

The panel shows charts/logs if you point it at the URLs of **your**
VictoriaMetrics / VictoriaLogs. Uncomment in `deploy/master.yaml`:

```yaml
metrics:
  victoriametrics_url: "http://127.0.0.1:8428"
  victorialogs_url: "http://127.0.0.1:9428"
```

**Leaving the block commented out is NOT "empty".** Master's own defaults for
these two keys are the very URLs printed above — `http://127.0.0.1:8428` and
`:9428` — and they are applied before the YAML is parsed, so a missing `metrics:`
section does not clear them. Consequences, measured by feeding `deploy/master.yaml`
verbatim to a master with nothing listening on those ports:

- the reference stack happens to run on the same box → the tabs simply work,
  configured or not;
- nothing is listening there → all three handles answer **`502 upstream`**, not
  `503`. The panel tells the two apart (`503` renders as "not configured", `502`
  as "unavailable"), so this is the difference between being sent to fix a
  config and being sent to fix a stack you never deployed. The master also logs
  `ERROR upstream narrowing probe failed` for both upstreams at startup;
- you actually want "not configured" → uncomment the block and set an
  **explicit empty string**: `victoriametrics_url: ""`. Only that yields
  `503 metrics_unconfigured` / `logs_unconfigured`.

`503 *_unconfigured` is one of THREE upstream-side answers, and they mean
different things: not configured (`503 *_unconfigured`), the upstream cannot
be asked to narrow (`503 *_narrowing_unsupported`, bound keys only,
tracker #1007), the upstream is not answering (`502 upstream`). On top of that
a bound key gets `200` with a narrowed — sometimes empty — result, a bad `limit`
on the logs proxy gets `400`, and `403` comes either from the key's scope or
from a pair that cannot be narrowed at all — binding by itself does NOT close
these handles. The exact order master checks them in is at the end of this
section, "In which order master answers".

**Who sees what (tracker #994).** A global (unbound) key uses both raw proxies —
`GET /v1/logs/query` and `GET /v1/metrics/query`·`/query_range` — exactly as
before: nothing is narrowed by a pair. "Verbatim", however, is true of the two
metrics handles only; on the logs proxy master sets and clamps `limit` for
everybody (see the end of this section). A key **bound to a
(project, env) pair** gets `200`, but master NARROWS its query by that pair
(`extra_stream_filters` for VictoriaLogs, `extra_label` for VictoriaMetrics), so
it never receives another project's lines or series, whatever it asks for.
Consequences worth knowing up front:

- **Lines without the pair are not served to a bound key.** The pair reaches the
  `project`/`env` stream labels from the log file PATH
  (`{log_dir}/servers/{project}/{env}/{id}.log`, `docs/specs/agent.md` §5). So
  the following are NOT labelled: everything written to VictoriaLogs before the
  upgrade; logs of dediks that were already running at upgrade time (they keep
  appending to the old flat path for the rest of their life); run-once logs.
  A global key still sees those lines, a bound key does not — they age out with
  the VictoriaLogs retention (14 days in the reference stack). This is a
  deliberate trade: treating an unlabelled line as "platform-wide" would have
  kept another project's game output readable for the whole retention window.
- **The node turns the labelling on, and it will NOT turn itself on.** The agent
  writes into a path carrying the pair only with `log_scope_dirs: true` in its
  config (`agent.yaml`), and **the agent binary's own default is `false`**. The
  ordering is not incidental: the agent binary upgrades itself (`POST
  /v1/agent-upgrade`), the shipper config does not, and an agent that started
  writing into subdirectories ahead of its vector would stop shipping logs at
  all (the old `servers/*.log` glob does not match the new path). So ONE and the
  same ansible role `birdman_agent_dev` lays down both the flag and the new
  vector config (variable `birdman_log_scope_dirs`, default `true` — that is the
  ROLE's default, not the binary's), and a bare binary upgrade keeps the old,
  safe layout.

  **The trap: while the flag is off, a bound key gets `200` with an EMPTY
  result** — not a `403`, not a `502`, not an error of any kind. On screen this
  is indistinguishable from an honest "no logs in this window", and nothing will
  signal it: neither master nor the panel knows about a flag left off on a node.
  Upgrading master does not carry it either — roll your config management over
  the GAME NODES (ours: `cd infra && ansible-playbook playbooks/add-node.yml`,
  which puts the `birdman_agent_dev` role on the `birdman_nodes` group;
  `dev-node.yml` is the master box and does not touch the nodes).

  The check that answers directly is to look at the flag on the node itself:
  `grep log_scope_dirs /etc/birdman/agent.yaml`. The indirect one, if you cannot
  get onto the node: compare the output for ONE dedik whose project AND
  ENVIRONMENT you know to be exactly your own pair — call `GET /v1/logs/query`
  for that `server_id` with a GLOBAL key first, then with the bound one. Global
  sees lines, bound does not → it is the labelling. "The bound key gets nothing"
  on its own proves nothing: an honest "no logs in this window" looks the same,
  and so does a correctly enforced boundary on a dedik from someone else's pair
  (including another environment of your own project). The opposite outcome —
  "both see the same thing" — is no reason to relax either: that is exactly what
  the upstream fail-open of the next bullet looks like. So compare not "is it
  non-empty" but whether the bound output is strictly CONTAINED in the global
  one.
- **Upstream versions are not cosmetic — and master now checks that the
  upstream actually PARSES the narrowing arg (tracker #1007).** The narrowing relies on the stock `extra_stream_filters`
  (VictoriaLogs) and `extra_label` (VictoriaMetrics) query args, and the
  narrowing itself is executed by the upstream, not by master. An upstream that
  does NOT know such an arg does not answer with an error — HTTP ignores an
  unknown query arg silently, so it would answer `200` with the whole fleet.
  Master therefore probes the upstream before letting a bound key's query
  through: a canary request with a deliberately MALFORMED value of the arg must
  be rejected (`4xx`), and a control request without the arg must succeed
  (`2xx`). Both hold → the arg is parsed, and master lets the narrowed query
  through. They do not → **master refuses instead of answering with everyone's
  data**:

  - `503` + `logs_narrowing_unsupported` / `metrics_narrowing_unsupported` —
    the upstream swallowed a value it cannot possibly parse, i.e. it is not
    parsing the arg at all. The message names the arg, the config option and
    the minimum version. Fix the upstream, not the key.
  - `502` + `upstream` — the probe could not reach a verdict (upstream down,
    `5xx`, a gateway in front that rejects everything, **or an upstream that
    answers but takes longer than the probe's 5-second budget** — the real query
    path would have given it 15s, so a very slow-but-healthy upstream can serve
    a global key while refusing a bound one). Refusing here too is deliberate:
    "could not verify" is not "verified".

  Neither message carries the upstream's own response body — only its status
  code and our explanation. The probe request is master's own and is NOT
  narrowed by the key's pair, so echoing what it got back to a bound key would
  turn the refusal itself into the channel we are closing. The body goes to the
  master log, where you actually want it.

  The probe travels the same URL and the same paths as the real query, so it
  tests your whole chain — an old version, a Loki-compatible stand-in in
  `victorialogs_url`, or a reverse proxy that strips query args all fail it
  identically. It runs once at master start (you get the ERROR in the log at
  boot, not when the first bound key trips over it — but a failing probe never
  blocks master from starting) and is then cached for 5 minutes, so a healthy
  deployment pays at most 2 requests to VictoriaLogs and 4 to VictoriaMetrics
  per 5 minutes, and nothing at all while no bound key is asking. There is no
  background refresh, so once every 5 minutes ONE user request pays for the
  whole probe synchronously and may take a few seconds longer than usual.
  A verdict older than 5 minutes is never used — read that literally: if you
  swap the upstream under a running master for one that does not parse the arg,
  then until the verdict expires (up to 5 minutes) master keeps narrowing into
  the void and that upstream answers with the whole fleet. This is NOT
  monitoring — the probe is lazy, it re-runs on the first bound request AFTER
  the verdict expires, not on a timer.
  Selective interference is its blind spot too: a middlebox that strips the arg
  only for some requests (by query text, by header) passes the probe — it proves
  the route, not the intentions of whoever sits on it. **A global (unbound)
  key is not probed and not refused** — narrowing does not apply to it, so it
  stays your diagnostic tool on a broken deployment.

  The minimum this was verified on, and what our ansible roles pin:
  **VictoriaLogs v1.51.0, VictoriaMetrics v1.102.1**. `victorialogs_url` must
  point at VictoriaLogs; a Loki-compatible stand-in will not understand the arg
  — and will now be refused rather than silently trusted. Note what the probe
  does and does not look at: it never reads the upstream's VERSION, only whether
  the arg is parsed on the paths we narrow. A newer or older build that parses
  it passes; the versions above are simply what this was verified on.

  **What the probe does NOT prove.** It proves the arg is PARSED, not that it is
  ENFORCED. An upstream that parses and validates the arg and then ignores its
  meaning would pass. There is no store-independent probe for enforcement: on an
  empty index "the filter returned nothing foreign" is indistinguishable from
  "the filter was ignored", and an access boundary must not depend on whether
  there happens to be data right now. If you run something other than
  VictoriaLogs/VictoriaMetrics, verify enforcement yourself with the
  global-vs-bound comparison described two bullets above.
- **Rolling the agent back.** An agent older than #994 cannot find logs in
  subdirectories: it will not rotate, finalize, retain or live-tail files that
  are already labelled. If you roll the agent back, move or remove
  `{log_dir}/servers/*/*/` by hand.
- **Custom shipper — put the pair in STREAM fields.** If you ship logs with
  something other than our vector, `project` and `env` must be VictoriaLogs
  stream fields. Wrong labelling creates no leak (the filter simply matches
  nothing), but an operator with a bound key will be left without their own
  logs. Never derive the pair from what the dedik prints to stdout: the access
  boundary would then be controlled by the dedik itself.
- **Metrics: only series carrying the pair are visible.** Master emits it on
  `birdman_servers`, `birdman_versions`, `birdman_server_info`. The agent's
  per-server series (`birdman_server_players`, `birdman_server_tick_ms`,
  `birdman_container_*`) carry it too **since tracker #1008** — but only for
  dediks STARTED after that upgrade: the pair is stamped into container labels
  at create and is never backfilled, so a dedik that was already running keeps
  emitting `server_id` alone and stays invisible to a bound key until it is
  recycled (those series age out with the VictoriaMetrics retention, 30 days in
  the reference stack — the same trade as the unlabelled log lines above). The
  pair is always stamped as a PAIR, never half of it. Platform aggregates
  (`birdman_players_online`, `birdman_matches_running`,
  `birdman_node_capacity_slots`) are fleet-wide data by nature and carry no
  pair at all. For a bound session everything without the pair comes back empty
  (`200` with an empty result, not `403`) — and note that a single pairless
  operand is enough to empty a whole expression: the panel's utilization
  percentage dies on its `sum(birdman_node_capacity_slots)` denominator even
  though its numerator is built on `birdman_servers` (measured: global `0.14`,
  bound `result:[]`).

**In which order master answers.** Measured against a running master, not read
off the source: an earlier version of this document had the relative order of
`400` and `502` backwards, and the only way to know is to run it. First match
wins, top to bottom:

1. no key → `401 unauthorized`;
2. the key lacks the `readonly`/`admin` scope → `403 scope readonly required`.
   That is about the SCOPE, not the binding, and it wins over everything below
   — including an unset URL and a dead upstream, so it never reveals whether
   the proxy is configured;
3. the key's pair cannot be narrowed at all (it does not pass the slug
   alphabet) → `403`, fail-closed. The only `403` binding itself produces, and
   unreachable through the public API: the pair is validated when the key is
   created;
4. the upstream URL is an explicit empty string → `503 metrics_unconfigured` /
   `logs_unconfigured`;
5. a bad `limit` on the logs proxy → `400 bad_request`. After the `503` above,
   but before ANYTHING involving the upstream — such a request never leaves
   master. The metrics proxies have no `limit` of their own; for them it is
   just another forwarded query arg;
6. bound keys only — the narrowing probe (previous bullet):
   `503 *_narrowing_unsupported` when the arg is swallowed, `502 upstream` when
   there is no verdict;
7. the real request fails → `502 upstream`;
8. otherwise `200` — narrowed by the pair for a bound key.

"Verbatim" holds for TWO of the three handles: the metrics proxies forward the
query string exactly as it arrived, unknown args included. The logs proxy is
never verbatim for anyone — master sets `limit` itself (default 1000) and clamps
it at 10000, for a global key just as much as for a bound one.

Reference monitoring stack (VM + vmagent + vmalert + Grafana + Postgres backups)
— the ansible role `infra/roles/birdman_monitoring_dev` (`infra/README.md`, the
"Observability + ops" section). A node can push its agent's metrics to a central
VM via a vmagent sidecar (`birdman_node_vmagent: true` in the host block); for
your own VM, set `birdman_node_vm_remote_write_url: "http://<VM_HOST>:8428/api/v1/write"`
there as well (the role default points at our overlay hub).

⚠️ **Third v1 seam (honestly):** `add-node.sh` writes `birdman_node_vmagent:
true` and a vector sidecar to the node unconditionally — if you do NOT have
VictoriaMetrics/VictoriaLogs (a bare deploy/ master), those shippers will send
into the void (they buffer, they don't get in the game's way). To disable:
`birdman_node_vmagent: false` in the host block; for the vector block — there's
nowhere to point `birdman_vl_sink_url` (the shipper still comes up, but messages
stay in the disk buffer) — cutting vector out entirely = production polish.

---

## 5. Draining a node and teardown

**Draining a node** (stop placing new matches, let the current ones finish):

```bash
curl -s -X POST http://127.0.0.1:8100/v1/nodes/<node_id>/drain   -H "Authorization: Bearer $KEY"
curl -s -X POST http://127.0.0.1:8100/v1/nodes/<node_id>/undrain -H "Authorization: Bearer $KEY"   # put it back in service
```

**Overlay teardown** (if Path B — on EVERY overlay box):

```bash
docker compose -f /opt/birdman/overlay/compose.yml down
sudo rm -rf /etc/birdman/overlay /opt/birdman/overlay
docker rmi birdman-overlay:local
```

The overlay's UFW rules (on the hub only): 4 rules with a fleet comment
(`51827/udp` and `in on birdman-wg0`). A deliberate removal of a node from the
overlay — run add-node.yml with `-e birdman_overlay_allow_peer_removal=true`
(otherwise the `-l` gate protects a live peer).

**Master-stack teardown** (deletes the DB; the .env/secrets.key bind files stay on disk — delete them by hand):

```bash
cd birdman/deploy && docker compose down -v    # -v wipes the Postgres volume and secrets
```

---

## 6. Backups

Backups are on by default: the master dumps its Postgres (`pg_dump -Fc`) every
6 hours into `/var/lib/birdman/backups` (the `backups` volume of the
`deploy/` stack) and keeps the latest 14 dumps. Schedule, retention and an
optional S3-compatible offsite (Backblaze B2 / Wasabi / AWS / MinIO) are
configured in the panel: **Backups** tab — set endpoint/bucket/keys, press
*Test connection*, then *Run now* to verify end-to-end. Both retentions count
dumps, not days.

Restore: stop the master, `pg_restore -d birdman --clean --if-exists <dump>`
against your Postgres, start the master. Dumps carry reversible secrets only
as AEAD ciphertext; the encryption key `secrets.key` never leaves the host —
keep its escrow copy separately (see §1). Step-by-step recovery runbook:
`docs/specs/ops.md §5`.

---

## 7. Security: what's exposed externally

| Port | Where | External | Protection |
|---|---|---|---|
| `8444/tcp` | master box | **yes** | strict mTLS (a token-only Hello is rejected; Enroll-by-token → client cert) |
| `8100` | master box | **no** — `127.0.0.1` only | external only via a reverse proxy/tunnel |
| `20000–20050` tcp+udp | each node | **yes** | dedicated-server game traffic (UFW additive) |
| `19999/udp` | each node | **yes** | QoS echo (the only externally-open service port of a node) |
| `51827/udp` | overlay hub | yes, if Path B | WireGuard of the isolated overlay `10.77.0.0/24` |

Secrets and where they live:

- **`secrets.key`** (master box, `deploy/`, git-ignored, `0600`) — the key that
  encrypts secrets in the DB at-rest. **Keep an escrow copy in a password
  manager**; loss = the DB secrets can't be decrypted.
- **the admin key** (`bmk_…`, scope `admin`) — printed once at startup. Store it
  as a secret; on the master box it lives in `/etc/birdman/master-admin.key`
  (`0600`, for node registration).
- **`node_token`** — one-time, issued by `POST /v1/nodes`, lives `0600` on the
  node only; the agent exchanges it for a client mTLS cert on first connect.
- **`POSTGRES_PASSWORD`** (`deploy/.env`, git-ignored).

Everything else (overlay addresses `10.77.0.0/24`, the WG port `51827`, the game
`20000–20050`/`19999`) — not secrets: RFC1918 or public by design.

### The `matchmaking` key and `player_id`

birdman authenticates operators, services and nodes — **never the end player**,
and by design it never will. `player_id` in a matchmaking ticket is an opaque
string the caller supplies and the master trusts; it is never persisted (no table
has a `player_id` column — players reach the database only as a count).

A `matchmaking`-scoped key therefore reaches further than it looks: it may create
a ticket for **any** `player_id`, and — given a `ticket_id` — read someone else's
ticket (once matched, that includes `host`, `port`, `match_id` and the
`join_token`) and cancel it; a ticket filed under a foreign `player_id` also
evicts that player's own ticket while it is still queued. Binding the key to one
`(project, env)` contains this **across** projects and environments: the binding
is enforced on all three ticket endpoints, so a ticket of another pair answers
`404` — the same body an unknown `ticket_id` gets, since a `403` would confirm
that someone else's ticket exists. It contains nothing **inside** that pair —
there is no ownership check at all — and a global (unbound) key still reaches
every project, because the binding is optional by design. **Give the key
to your game backend, not to the game client**: the backend authenticates the
player its own way, keeps the key, and files the ticket with a `player_id` it has
already verified. A key baked into the client is a public secret, and `player_id`
then guarantees nothing — the 5 rps per-`player_id` limit is abuse damping, not a
security boundary. `join_token` does not close the gap either: it authorizes
joining one dedicated server, it does not authenticate a player, and today
nothing verifies it (the dedicated-server side is still a TODO).

The full model — including what the backend owns (reconnect, player leave,
player-level state) — is in `docs/specs/architecture.md`, section
«Модель доверия (trust boundaries)» *(in Russian)*.

---

**Next:** the full REST API — `master/README.md`; node/ansible internals and the
overlay — `infra/README.md`; operations/backups/restore — `docs/specs/ops.md`.
