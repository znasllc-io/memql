# Reproduce staging locally (cluster parity)

The local Docker cluster (`docker/docker-compose.cluster.yml`) is the
**blessed local dev topology** (memql#1260): boot it with
`make dev-cluster-up`. It is built to mirror staging (the
`aks-memql-staging` AKS cluster, `deploy/k8s/base/*.yaml`) along the
**mesh-delivery path** -- the same 2-replica-per-node fan-out that
staging runs -- so cluster-only bugs (replica fan-out, cross-node event
double-delivery, leader/singleton races) reproduce in local dev instead
of only on staging. A handful of nodes off that path (identity replica
count, the voice-agent, lifecycle probes) diverge for concrete local-
host reasons; every one is enumerated and justified in the
[divergence audit](#config-vs-topology-audit-1216--1260) below.
Epics: memql#1212 (children #1213 / #1214 / #1215 / #1216) +
memql#1260 (adopt as first-class + close/justify divergences).

## What "parity" means here

| Aspect | Local cluster | Staging | Parity |
|---|---|---|---|
| Node-type split | bff / cognition / voice / agent / planner / workbench + identity | same | identical |
| Build model | carrier (`memql-bff-copresent/Dockerfile` + `BUILD_TAGS`) for bff/cognition/agent/planner/workbench; engine for voice/identity | same | identical |
| Replicas per **mesh** node (bff/cognition/voice/agent/planner/workbench) | **2** (`deploy.replicas: 2`) | **2** | identical |
| Per-replica node id (mesh nodes) | hostname-derived (`os.Hostname()`) | `fieldRef: metadata.name` | equivalent |
| copresent SPA | present (2 replicas, built image) | present (2 replicas) | identical |
| LiveKit | present (`livekit/livekit-server:v1.8`) | present (`v1.8`) | identical |
| Front door | nginx, single origin, path-routed | ingress-nginx, single origin | equivalent |
| identity replicas | **1** (static id, host port 8081) | **2** (`fieldRef` id) | **divergent -- justified** (off the delivery path; see audit) |
| voice-agent (LiveKit participant) | layered via `docker-compose.polyphon.yml` (opt-in) | in base (`replicas: 1`) | **divergent -- justified** (needs Deepgram/LiveKit creds) |
| Health probes / graceful drain | bff+identity healthcheck only; no `/livez` split, no preStop | startup+readiness `/healthz` + liveness `/livez` + preStop on every node | **divergent -- deferred to Phase 3** (#1268/#1269) |
| Database | local Postgres + TimescaleDB | Tiger Cloud | **config only** |
| Blob storage | Azurite emulator | Azure Blob | **config only** |
| Secrets / keys | dev defaults | Key Vault via ESO | **config only** |

For the **mesh-delivery path** (the 2-replica bff + worker fan-out this
cluster exists to exercise) topology and build model are identical, by
design -- see the header comment in `docker/docker-compose.cluster.yml`.
The off-path divergences (identity, voice-agent, probes) are each
enumerated, with their justification and any follow-up owner, in the
[divergence audit](#config-vs-topology-audit-1216--1260).

## The per-replica node-id mechanism (the core of #1213)

Docker Compose `deploy.replicas: N` gives every replica the **same
environment**. A static `MEMQL_NODE_ID=<type>-local` would therefore
collide across replicas -- `PeerManager` keys peers by node id, so two
replicas sharing an id is the same failure shape that caused a staging
chat outage (memql#1042, shared `MEMQL_NODE_ID=bff-local`).

The fix mirrors staging's `fieldRef: metadata.name`:

- The replicated services in the compose set **no** `MEMQL_NODE_ID` (and
  no `container_name` / `hostname`, which would force a single
  container). Compose then assigns each replica a unique container
  hostname `<project>-<service>-<n>` (e.g.
  `memql-cluster-multinode-bff-1`, `...-bff-2`).
- `node.NewIdentity` (`component/node/identity.go`) falls back to
  `os.Hostname()` when `MEMQL_NODE_ID` is empty, so each replica gets a
  stable, unique id derived from its container hostname -- the Compose
  equivalent of the k8s downward-API field ref.

Verify it after bring-up with `make dev-cluster-status`: the two
replicas of each service must show **distinct** `node_id` values.

## Prerequisites

- Docker (with BuildKit / `docker compose` v2).
- `MEMQL_PACKAGES_TOKEN` exported -- a GitHub token with `read:packages`
  for both the `@visionarys-io` and `@znasllc-io` scopes. The copresent
  SPA build installs private SDKs from GitHub Packages via a BuildKit
  secret (the token never lands in the image).
- The `../copresent` sibling repo checked out next to `memql` (the SPA
  build context), plus the `memql-bff-copresent` carrier sibling (the
  carrier build context), per the standard workspace layout.
- A genesis env file. The compose reads `${GENESIS_ENV_FILE:-../.env.local}`;
  the repo-root `.env.local` produced by the genesis flow is the default.

## Bring it up

```bash
export MEMQL_PACKAGES_TOKEN=ghp_...      # read:packages, both scopes

# Blessed path -- boot the staging-parity cluster in the background:
make dev-cluster-up                      # build + up -d (the default verb)
#   ...and to stop it again (volumes preserved):
make dev-cluster-down

# When you've edited Go / MemQL / prompt source and need fresh binaries,
# force a --no-cache rebuild instead:
make dev-cluster-restart                 # rebuild, keep the DB
make dev-cluster-restart-purge           # rebuild AND wipe the DB (clean seed)
#   or run it in the foreground:
make dev-cluster

# One-command "fresh testing stack" on the cluster (memql#1283): the
# BLESSED + ONLY supported local refresh (memql#1304). Decrypt genesis
# (needs MEMQL_MASTER_KEY in env) -> wipe DB -> rebuild -> restart ->
# wait healthy -> reseed secrets/variables from genesis, all on this
# 2-replica parity topology. If a raw single-node full.yml stack is
# still up, tear it down first:
export MEMQL_MASTER_KEY=...               # your 64-hex genesis key
docker compose -f docker/docker-compose.full.yml down   # if it's up
make dev-cluster-refresh
```

> **Front-door divergence (memql#1283, resolved option a).** A raw
> single-node `full.yml` stack waits on its TLS
> `https://bff.local.znas.io` front door; `make dev-cluster-refresh`
> uses the cluster's **plain-HTTP single-origin front door at
> <http://localhost:8085>** (HTTP) + `localhost:50050` (gRPC), the
> deliberate divergence adopted in memql#1260. The health-wait probes
> `http://localhost:8085/healthz`, and the secrets seed is routed to
> the cluster gRPC front door via `MEMQL_GRPC_ENDPOINT=localhost:50050`
> (plaintext, since the endpoint has no `:443` suffix) rather than
> full.yml's `bff.local.znas.io:443`. Building a TLS/domain front door
> for the cluster is intentionally out of scope; the routing map itself
> is at parity with staging (only TLS termination + the `:8085` host
> port differ -- see the [divergence
> audit](#config-vs-topology-audit-1216--1260)).

This is heavy -- it builds 6 carrier/engine images plus the copresent
SPA. Allow several minutes on a cold cache.

Then confirm parity:

```bash
make dev-cluster-status
```

Expect two distinct node ids per mesh service, e.g.:

```
  memql-cluster-multinode-bff-1         node_id=memql-cluster-multinode-bff-1
  memql-cluster-multinode-bff-2         node_id=memql-cluster-multinode-bff-2
  memql-cluster-multinode-cognition-1   node_id=memql-cluster-multinode-cognition-1
  memql-cluster-multinode-cognition-2   node_id=memql-cluster-multinode-cognition-2
  ...
```

Front door:

- App / SPA + same-origin auth + WS bridge: <http://localhost:8085>
- gRPC (MemqlService / NodeService): `localhost:50050`
- LiveKit signaling: `ws://localhost:7880` (dev key `devkey` / secret `secret`)
- Identity admin/login (direct): <http://localhost:8081>

## Worked example: reproduce the duplicate-utterance bug (#1217)

memql#1217 is "the assistant utterance is duplicated on staging
(multi-node), not local -- the same text appears twice." It is the
motivating defect for this whole epic: a single-replica local cluster
**structurally cannot** reproduce a cross-replica fan-out, so the bug
was invisible in dev. With parity in place it should now reproduce.

1. Bring up the parity cluster and confirm distinct per-replica node
   ids (`make dev-cluster-status`). If the ids are NOT distinct, stop --
   the static-id collision fix has regressed and you would be testing a
   single-identity mesh, which masks the bug.

2. Open <http://localhost:8085>, sign in (magic-link via the identity
   service), create a space, and add the General Assistant.

3. Send a chat message that triggers an assistant reply. Watch the chat
   transcript and the cognition/bff logs:

   ```bash
   make dev-cluster-logs | grep -Ei 'utterance|AgentGenerateTurn|EventForward|dedup'
   ```

4. The bug presents as the **same** assistant utterance rendered twice
   in the transcript (two `v1:cognition:utterance` rows, or the same row
   delivered to the browser twice). Because cognition and bff each run 2
   replicas now, an event produced on one node can be delivered to a
   browser stream anchored on two different bff replicas, or processed by
   two cognition replicas -- exactly the fan-out staging exhibits.

5. Root-cause against the cross-node event bridge dedup
   (`component/node/dedup.go` + `eventbridge.go`) and the per-replica
   subscription path. The dedup ring buffer is **per-process**, so two
   replicas each forwarding/publishing the same event will not dedup
   across the replica boundary unless the dedup key + TTL window are
   shared at the consumer (browser-subscription) layer. That is the
   surface to fix -- do NOT paper over it by collapsing back to a single
   replica; the multi-replica topology is the parity we want.

> If #1217 does NOT reproduce after several attempts, increase the
> fan-out pressure: `make dev-cluster-scale NODE=cognition N=3` and
> `make dev-cluster-scale NODE=bff N=3`, then retry. More replicas =
> more chances for the double-delivery race to surface.

## Config-vs-topology audit (#1216 / #1260)

The governing invariant: **along the mesh-delivery path, only config may
differ from staging -- never topology or build.** The delivery path is
the 2-replica bff + worker fan-out that the #1217/#1232/#1245 incidents
all lived on; it is exactly the surface a single-replica local cluster
cannot exercise. Everything on that path is held at strict parity. A few
nodes *off* that path diverge for concrete local-host reasons; each is
enumerated below with its justification and follow-up owner, so "local
matches staging" is a checkable claim rather than an aspiration.

This audit was re-run service-by-service against `deploy/k8s/base/*.yaml`
for #1260. It supersedes the earlier note that claimed identity is
single-replica on both (it is not -- staging runs it at 2).

### Invariants -- MUST stay identical (the mesh-delivery path)

These are the parity surface. A change that breaks one of these is a
regression, not a divergence:

- **Service set on the delivery path:** bff, cognition, voice, agent,
  planner, workbench, + copresent + livekit.
- **Build source per node** (carrier vs engine -- the #1053 enforced
  rule; see the table in the repo-root `CLAUDE.md`).
- **`replicas: 2`** on every mesh node (bff/cognition/voice/agent/
  planner/workbench) + copresent.
- **Per-replica unique node identity** on those mesh nodes (hostname-
  derived locally via `os.Hostname()`; `fieldRef: metadata.name` on
  k8s). `make dev-cluster-status` is the litmus.
- **Inter-node addressing** (`MEMQL_NODE_ADDRESS` / `MEMQL_PARENT_ADDRESS`
  / `MEMQL_WORKER_PEERS` / `MEMQL_WORKBENCH_REMOTE`) -- byte-identical to
  the k8s manifests.
- **Front-door routing map** (`/`, `/memql/ws`, `/memql/audio`,
  `/auth/refresh`, `/auth/logout`, `/oauth/token`,
  `/.well-known/jwks.json`).

### Divergences -- justified, with follow-up owner

| # | Divergence | Local | Staging | Why it's acceptable | Owner |
|---|---|---|---|---|---|
| 1 | **identity replica count** | 1 (static `MEMQL_NODE_ID=identity-local`, host port `8081:8081`, pinned `container_name`/`hostname`) | 2 (`fieldRef` id) | On one Docker host the `8081:8081` host-port maps to a single replica, and the static id would collide across replicas (the #1042 chat-outage shape) -- so scaling identity locally means dropping the static id + host port, which is *more* divergence than parity. Identity is **off the chat-reply delivery path** (auth / JWKS / migrations only), so 1 local replica does not mask the cross-replica fan-out this cluster exists to catch. | accepted (revisit if an identity/JWKS multi-replica bug appears) |
| 2 | **voice-agent** (LiveKit room participant, `memql-voice voice-agent` subcommand) | not in the cluster compose; layer it via `docker-compose.polyphon.yml` | in base (`replicas: 1`) | The voice-agent auto-joins LiveKit rooms and needs live Deepgram + LiveKit credentials; baking it into the default cluster bring-up would crash-loop without those keys and hurt the dev path. It is opt-in via the polyphon overlay, and it is not on the text chat-reply delivery path. | accepted (opt-in overlay) |
| 3 | **health probes + graceful drain** | only bff + identity carry a healthcheck (`/app/healthcheck`); no readiness/liveness split, no `/livez`, no `preStop`, no `terminationGracePeriodSeconds` | every node: startup + readiness `/healthz`, liveness `/livez` (#1117), `preStop sleep 5`, `terminationGracePeriodSeconds 45-60` | The readiness-vs-liveness split and graceful SIGTERM drain are the explicit subject of **Phase 3** of the resilient-mesh epic. Adding them here would pre-empt that design. | **deferred to #1268 / #1269** |
| 4 | **bff internal HTTP port** | `8088` (`SERVER_ADDRESS`, nginx upstream `bff:8088`, copresent `VITE_MEMQL_API_URL=http://bff:8088`) | `8085` | Internal port number only; the `/healthz` + WS contract is identical and it sits behind the front door, so it is functionally equivalent. Aligning it would ripple nginx + copresent for zero behavioural gain. | accepted (cosmetic) |
| 5 | **migrations** | no explicit migrate flags (binary default on first boot) | only identity sets `MIGRATE_ON_START`; a dedicated `migrate-job` owns schema | Each environment migrates exactly once at bring-up; the difference is *who* runs it, not *whether*. | accepted |

### Config-only -- EXPECTED to differ

- `MEMORY_NODES_DATABASE_DSN` (local Postgres vs Tiger Cloud).
- Blob backend (Azurite connection string vs Azure Blob via genesis).
- LiveKit keys (dev `devkey`/`secret` vs ESO-synced Key Vault secret).
- Bootstrap/dev escape hatches (`MEMQL_IDENTITY_ALLOW_INSECURE_*`,
  the dev `MEMQL_NODE_BOOTSTRAP_TOKEN`).
- TLS termination (nginx plain-HTTP front door locally vs ingress TLS).
- `pgadmin` (local-only convenience under the `tools` profile).

When you change the local cluster, re-run this audit: anything new that
is not a config-only difference must land in the divergence table with a
justification and an owner, or be closed.

## Teardown

```bash
make dev-cluster-stop          # keep volumes (DB + identity keys persist)
make dev-cluster-restart-purge # next start wipes the DB
```
