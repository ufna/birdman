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
# 1. register a build version (the game image)
curl -s -X POST http://127.0.0.1:8100/v1/versions -H "Authorization: Bearer $KEY" \
  -d '{"project":"game","semver":"1.0.0","image_ref":"ghcr.io/org/game:1.0.0","channel":"staging"}'

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

---

## 4. Observability (optional)

The panel shows charts/logs if you point it at the URLs of **your**
VictoriaMetrics / VictoriaLogs. Uncomment in `deploy/master.yaml`:

```yaml
metrics:
  victoriametrics_url: "http://127.0.0.1:8428"
  victorialogs_url: "http://127.0.0.1:9428"
```

Empty → the metrics/logs tabs answer `503`, everything else works.

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
keep its escrow copy separately (see §1).

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

---

**Next:** the full REST API — `master/README.md`; node/ansible internals and the
overlay — `infra/README.md`; operations/backups/restore — `docs/specs/ops.md`.
