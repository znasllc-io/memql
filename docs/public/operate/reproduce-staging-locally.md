# Reproduce staging locally (cluster parity)

The local Docker cluster (`docker/docker-compose.cluster.yml`) is built
to be **topologically identical** to staging (the `aks-memql-staging`
AKS cluster, `deploy/k8s/base/*.yaml`). The point is to catch
cluster-only bugs -- replica fan-out, cross-node event double-delivery,
leader/singleton races -- in local dev instead of discovering them on
staging. Epic: memql#1212 (children #1213 / #1214 / #1215 / #1216).

## What "parity" means here

| Aspect | Local cluster | Staging | Parity |
|---|---|---|---|
| Node-type split | bff / cognition / voice / agent / planner / workbench + identity | same | identical |
| Build model | carrier (`memql-bff-copresent/Dockerfile` + `BUILD_TAGS`) for bff/cognition/agent/planner/workbench; engine for voice/identity | same | identical |
| Replicas per mesh node | **2** (`deploy.replicas: 2`) | **2** | identical |
| Per-replica node id | hostname-derived (`os.Hostname()`) | `fieldRef: metadata.name` | equivalent |
| copresent SPA | present (2 replicas, built image) | present (2 replicas) | identical |
| LiveKit | present (`livekit/livekit-server:v1.8`) | present (`v1.8`) | identical |
| Front door | nginx, single origin, path-routed | ingress-nginx, single origin | equivalent |
| Database | local Postgres + TimescaleDB | Tiger Cloud | **config only** |
| Blob storage | Azurite emulator | Azure Blob | **config only** |
| Secrets / keys | dev defaults | Key Vault via ESO | **config only** |

The only things that differ are **config** (DB endpoint, blob backend,
secret values, LiveKit keys). Topology and build model are identical, by
design -- see the header comment in `docker/docker-compose.cluster.yml`.

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

# Fresh build of all 6 carrier/engine images + the SPA, then up.
make dev-cluster-restart-purge           # also wipes the DB (clean seed)
#   or, keeping existing data:
make dev-cluster-restart
#   or foreground:
make dev-cluster
```

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

## Config-vs-topology audit (#1216)

When changing the local cluster, the invariant is: **only config may
differ from staging, never topology or build.** Concretely, the
following MUST stay identical between
`docker/docker-compose.cluster.yml` and `deploy/k8s/base/*.yaml`:

- The set of services (node types + copresent + livekit + identity).
- The build source per node (carrier vs engine -- the #1053 enforced
  rule; see the table in the repo-root `CLAUDE.md`).
- `replicas: 2` on every mesh node + copresent (identity + livekit stay
  single-replica in both).
- Per-replica unique node identity (hostname locally, fieldRef on k8s).
- The front-door routing map (`/`, `/memql/ws`, `/memql/audio`,
  `/auth/*`, `/oauth/token`, `/.well-known/jwks.json`).

The following are EXPECTED to differ (config only):

- `MEMORY_NODES_DATABASE_DSN` (local Postgres vs Tiger Cloud).
- Blob backend (Azurite connection string vs Azure Blob via genesis).
- LiveKit keys (dev `devkey`/`secret` vs ESO-synced Key Vault secret).
- Bootstrap/dev escape hatches (`MEMQL_IDENTITY_ALLOW_INSECURE_*`,
  the dev `MEMQL_NODE_BOOTSTRAP_TOKEN`).
- TLS termination (nginx plain-HTTP front door locally vs ingress TLS).

## Teardown

```bash
make dev-cluster-stop          # keep volumes (DB + identity keys persist)
make dev-cluster-restart-purge # next start wipes the DB
```
