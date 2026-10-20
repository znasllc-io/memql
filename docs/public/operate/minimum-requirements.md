---
title: Minimum Requirements
audience: public
status: stable
area: operate
sinceVersion: 0.9.88
owner: znas
---

# Minimum Requirements

This page is the short answer to *"what do I actually need to run MemQL?"*
There is one answer per run mode:

| Mode | What it is | How |
|------|-----------|-----|
| **One machine** | The entire cluster on your own Mac or Linux PC: every engine node, Postgres, Argo CD and Ask voice in k3d, with local models answering chat, embeddings, speech recognition and speech. | The install wizard, or `make up` from a checkout. See [One machine](#one-machine-the-entire-cluster) below, [Before you install a local cluster](install-prerequisites.md) and [Reproduce the cloud locally](reproduce-the-cloud-locally.md). |
| **Beyond one machine** (any installation) | A Kubernetes mesh against **self-hosted CloudNativePG** (PostgreSQL + TimescaleDB Community + pgvector), in-cluster. | [Beyond one machine](#beyond-one-machine), below. |

Everything below is **target-agnostic by design**: the architecture is
identical from a local k3d cluster to a cloud one; only the *config values*
(DSNs, replica counts, sizes) differ. If a requirement is "do X differently
over there," that's a bug, not a feature.

MemQL ships **one installation shape** (epic memql#3943) — there is no
staging-versus-production dimension inside the product. An operator who wants a
second environment installs a second instance, with its own domain and its own
database. What varies is the deploy TARGET (`provider`: `docker-local` |
`azure`).

---

## One machine: the entire cluster

Today MemQL runs as a whole on the machine you install it on. A local cluster
cannot reach a model vendor (its OIDC issuer is not publicly reachable, so no
vendor can verify what it mints), so **every model call is answered on that
machine too**: chat and embeddings, and for Ask voice, speech recognition and
speech. The minimum is what holds the cluster, those models and the operating
system at the same time.

### Hardware

| | Apple silicon Mac | Linux PC (x86_64) | Models held at once |
|---|---|---|---|
| **Minimum** | 32 GB unified memory, 8 CPU cores, 100 GB free on an SSD | 32 GB RAM and an NVIDIA or AMD GPU with 16 GB VRAM, 8 CPU cores, 100 GB free on an SSD | `qwen3.5:9b`, `qwen3-embedding:0.6b`, whisper-large-v3-turbo and Kokoro |
| **Recommended** | 48 GB or more on an M-series Pro, Max or Ultra chip, 200 GB free | 64 GB RAM and a GPU with 32 GB VRAM, 200 GB free | `qwen3.8:27b` in place of `qwen3.5:9b` |

### Why these numbers

On Apple silicon the GPU and the cluster share one pool of memory, so all of
this has to fit in it at once:

| What | Memory | Where the figure comes from |
|---|---|---|
| The cluster in Docker | 8 GB | Docker Desktop's allocation. 4.8 GiB was in use across 37 pods at idle, measured |
| macOS, Cockpit and a browser | 8 GB | Allowance |
| Chat and embeddings, minimum set | 11.4 GB | Catalog estimate at 32K chat and 8K embedding context ([by machine class](local-models.md#what-to-pull-by-machine-class)) |
| Chat and embeddings, recommended set | 22.7 GB | The same estimate for `qwen3.8:27b` |
| Speech recognition and speech | about 3 GB | Allowance. whisper-large-v3-turbo is a 1.5 GiB whisper.cpp file and Kokoro is under 0.4 GB; the catalog has not measured either |

The minimum set comes to about 30 GB and the recommended set to about 42 GB,
which is why the floor is 32 GB and the recommendation 48 GB.

Measured on 2026-10-19 on a 36 GB MacBook Pro (M4 Max) with the cluster
running: loading the recommended set (`qwen3.8:27b` at 32K context and
`qwen3-embedding:0.6b` at 8K) took free memory from 82% to 23% and swap in use
from 2.4 GB to 5.3 GB, before any speech model, browser tab or image build was
added. Replies still arrived at 22.6 tokens per second, but the machine was
swapping. Below 48 GB, use the minimum set.

**Fleet's automatic model choice does not count the cluster yet**
([memql#6254](https://github.com/znasllc-io/memql/issues/6254)). It sizes a
Mac from three quarters of its memory, so a 32 GB or 36 GB Mac that also runs
the cluster is offered `qwen3.8:27b`. On such a machine, choose `qwen3.5:9b`
on the machine's Models tab instead of Automatic.

On Linux the cluster uses system memory and the models use the GPU's, so the
two budgets are separate. The minimum set and the speech models take about
14 GB of a 16 GB card. The recommended set with them takes about 26 GB, which
fits a 32 GB card but not a 24 GB one. The cluster takes about 5 GB of RAM at
idle; image builds take more, and the desktop, browser and model runtime need
the rest.

Eight cores is a working floor, not a measured limit: the cluster idles at
about a quarter of a core, and image builds use two thirds of Docker's CPUs by
default. For disk, the measured machine held 13.9 GB of Docker images and
27.9 GB of volumes, and building the images left a 25 GB build cache. The
minimum model set downloads 7.2 GB and the recommended one 18.6 GB. The
database, object storage and pipeline cache grow with use.

### What qualifies

| Machine | Runs the whole cluster |
|---|---|
| MacBook Pro, Mac mini, iMac, Mac Studio or Mac Pro with Apple silicon | Yes, from 32 GB; 48 GB or more recommended |
| MacBook Air | Not recommended. Until the M4 it stopped at 24 GB. The 32 GB configurations meet the memory minimum, but the Air has no fan, so sustained inference beside the cluster slows it down |
| Linux laptop, desktop, workstation or server on x86_64 with an NVIDIA or AMD GPU of 16 GB or more | Yes |
| Linux PC with no discrete GPU, or less than 16 GB VRAM | No. CPU-only inference is not supported |
| Linux on arm64: NVIDIA DGX Spark, the Grace-based DGX Station, Jetson | Not yet. The installer supports Linux only on x86_64, and Cockpit offers models only on x86_64 Linux |
| Intel Mac | No. The installer supports Apple silicon only, and Cockpit does not run models on an Intel Mac |
| Windows PC | No. Neither the installer nor Cockpit runs on Windows |

### Software

- **macOS 13 or newer** for Cockpit, on a release Docker Desktop still
  supports: Docker covers the current and two previous major macOS releases.
- **Docker Desktop** with at least 8 GB of memory allocated (12 GB
  recommended).
- **Linux**: Debian/Ubuntu or Fedora/RHEL with Docker Engine, set up as
  [Before you install a local cluster](install-prerequisites.md) describes,
  and the GPU's driver: NVIDIA's (with `nvidia-smi`) or AMD's `amdgpu`.

Ask voice is the only realtime voice surface in the cluster: two LiveKit
replicas and a Redis. There is no telephony (SIP) component.

---

## Beyond one machine

The rest of this page is what an installation outside one machine needs: the
minimums, with a link to the deep runbook for each piece. The biggest
non-obvious requirement is the **database**, so that section is first and most
detailed.

---

## 1. Database — **self-hosted CloudNativePG**

MemQL stores everything (the time-series memory graph + the observability
hypertables) in **PostgreSQL 16 + TimescaleDB Community + pgvector**, run
**inside the cluster** by the CloudNativePG operator (epic memql#3842). The
same operator, the same operand image and the same four resource kinds run in
local k3d and in a cloud cluster; only the numbers and endpoints differ.

> **This replaced "Tiger Cloud is the only supported provider today."** It was
> true, and it was the source of the constraint the rest of this page used to
> be organized around: a per-tier `max_connections` ceiling of ~59 usable
> slots, a control-plane pool that could not be terminated, and $0.883/GB-month
> storage against a schema that never evicts. Self-hosting removed all three.
>
> Azure's managed PostgreSQL is still **not** an option, and for an unrelated
> reason: the schema requires TimescaleDB **Community** features — continuous
> aggregates, compression policies, retention policies — and hyperscalers ship
> the Apache build. That is why this is self-hosted rather than moved to
> another managed provider.

### Minimum DB checklist

- [ ] The **CNPG operator stack** reconciled by ArgoCD: CloudNativePG + the
      Barman Cloud plugin + cert-manager (`deploy/cnpg`, `deploy/cert-manager`).
- [ ] The **operand image** built on the build server and pinned by digest in
      the overlay (native `tests.database-image` plus reviewed release publication). The tag must begin
      with the PostgreSQL major — CNPG parses it.
- [ ] A `Cluster` composed from the `cnpg-db` component with a tier preset
      (`entry` / `mid` / `top`), sized so the **connection budget** fits the
      mesh (see below).
- [ ] An **object store for backups**: its own resource group, ZRS + Cool,
      versioning and soft delete on, and the cluster identity granted
      **write but not purge**.
- [ ] **Two** connection strings wired into `memql-secrets`:
      `MEMQL_DATABASE_DSN` and `MEMORY_NODES_DATABASE_DIRECT_DSN`. Both point
      at the `-rw` Service today — see below for why they are still two.

Provisioning detail, alerts, and the failover/restore drills:
[Database platform](database-platform.md).

### Why two endpoints? — the connection model

The whole mesh (7 node-types × replicas) shares **one** database.

`max_connections` is now **ours to set** — 400 for a local cluster and for the
mid and top cloud presets, 200 for the entry preset — so the ceiling is a sizing
decision rather than a tier limit. It
is still a ceiling: every Postgres backend is a process with its own memory, so
raising it trades RAM for headroom, which is why the budget below is still
enforced by a deploy gate.

**There is no pooler in the path today**, and the two DSNs both resolve to
`memql-db-rw`. The split is kept because it is about *transaction-mode pooling
breaking session state*, not about Tiger:

- **Bulk traffic** (the bun pool — all queries and mutations, every mesh pod)
  rides `MEMQL_DATABASE_DSN`.
- **Session-stateful work** — session-scoped advisory locks (cron leader,
  topology reconciler, planner admission) **and migrations** — rides
  `MEMORY_NODES_DATABASE_DIRECT_DSN`. A transaction-mode pooler recycles a
  server backend *between statements*, which would silently drop a held
  session lock.

So when a pooler is eventually enabled (`cnpg-db/optional/pooler`, ready but
not composed), only the first DSN moves. (This paragraph used to add that
`MEMQL_DB_SEARCH_PATH` "is the staging/production boundary" and must never ride
a transaction-pooled connection. There is no such variable — it belonged to
epic memql#3748's two-environments-in-one-cluster design, which epic memql#3943
reversed in favour of one installation shape. Separate installations have
separate databases, not separate schema search paths.)

In code: `Database.DirectBunDB()` returns the direct pool when `DIRECT_DSN` is
set, else falls back to the main pool — so a single-pool deployment is
unaffected. bun's `pgdriver` speaks the simple query protocol (no server-side
prepared statements), so transaction pooling is safe when it arrives.

### Sizing the budget

```
peak_connections ≈ Σ_over_node_types(replicas × MAX_OPEN_CONNS) + rollSurge
REQUIRE:  peak_connections ≤ max_connections − reserved
```

- **Reserved is now small and knowable**: Postgres's own
  `superuser_reserved_connections` plus CNPG's instance-manager and monitoring
  connections. There is no un-terminable control-plane pool to budget around —
  that was Tiger's (#1822), and it went with the provider.
- `max_connections` is set on the `Cluster` in
  `deploy/k8s/components/cnpg-db`, not in a vendor console.

Full detail, the budget formula, the pre-deploy gate, and the monitor:
[DB connection budget & graceful deploy](db-connection-budget.md).

---

## 2. Compute — Kubernetes

- A **Kubernetes** cluster. We run and test on **Azure Kubernetes Service**;
  any conformant cluster should work, but AKS is the exercised path. See
  [Infrastructure](infrastructure.md).
- **The database runs here too** — a CNPG `Cluster`, not an external service.
  It wants a dedicated node pool: Postgres is memory- and IO-sensitive, so a
  noisy neighbour costs latency on every query.
- The engine mesh node-types (each a `Deployment`, 2 replicas for HA):
  `identity`, `agent`, `planner`, `workbench`, `mcp`, `edge`. Manifests:
  `deploy/k8s/base`. A product stack adds a `bff` -- a plain,
  product-agnostic engine node that fronts the product's DSL bundle -- plus
  the product client (SPA), both layered in from the product's own overlay --
  see [Downstream product stacks](downstream-stacks.md).
- Rough small-installation footprint: ~4 × 2-vCPU nodes. Right-size per workload.
- **Argo CD** for GitOps (+ **Argo Rollouts** when a product stack runs a
  blue-green `bff`; the controller install lives in `deploy/rollouts/install`).

---

## 3. Secrets & auth

Every DB-connecting pod mounts the `memql-secrets` Secret via `envFrom`. The
**four** keys (see [`deploy/k8s/base/secret.example.yaml`](https://github.com/znasllc-io/memql/blob/main/deploy/k8s/base/secret.example.yaml)):

| Key | What |
|-----|------|
| `MEMQL_MASTER_KEY` | 32-byte key that decrypts stored secrets at rest (`v1:platform:globalSecret`). It DECRYPTS and never authenticates -- the operator bearer is `MEMQL_OPERATOR_KEY` (memql#3519). |
| `MEMQL_OPERATOR_KEY` | 32-byte credential that AUTHENTICATES `Authorization: Operator <key>` as a synthetic cluster owner. A **different** value from the master key (memql#3519). |
| `MEMQL_IDENTITY_SIGNING_KEY_B64` | base64-std 32-byte Ed25519 seed. Required for any multi-replica identity: without it each pod mints its own key, JWKS diverges, and ~50% of token verifications fail (memql#3400). |
| `MEMQL_DATABASE_DSN` | DB — the bulk-traffic endpoint (bun pool; resolves to `memql-db-rw` today, will move to a pooler if `cnpg-db/optional/pooler` is ever composed). |
| `MEMORY_NODES_DATABASE_DIRECT_DSN` | DB — the **direct** endpoint. |

- **Identity** is the in-house auth service; for multi-replica HA it needs a
  shared `MEMQL_IDENTITY_SIGNING_KEY_B64` (Ed25519 seed) in the envelope — every
  replica derives the same key/JWKS. See [Identity Service](auth/identity-service.md)
  and the [Access Model](auth/access-model.md).
- **AI providers: NO key is required, because no key exists.** Both vendors
  are reached by workload identity federation, so the only per-cluster values
  are the federation ids, they are optional, and a cluster with none boots and
  serves everything that needs no model. Streaming transcription and Whisper
  are OpenAI calls and therefore need OpenAI federation specifically -- they
  are unavailable on a cluster that has not done that cutover, and on every
  local cluster, whose OIDC issuer no vendor can reach. See
  [OpenAI federation](auth/openai-federation.md),
  [Anthropic federation](auth/anthropic-federation.md), and
  [Environment Variables](env-vars.md) for the full env surface and the
  bootstrap-envelope vs concept-stored split.

---

## 4. Images — built on the build server, never a laptop

Deployable images are built on the **MemQL build server** and published to the
configured registries with workload identity, never
hand-built locally (local Docker is dev-only):

- `memql` (this repo) → **every** node-type image — `identity`, `bff`,
  `agent`, `planner`, `workbench`, `mcp`, `edge` — as a
  **product-agnostic engine image**
  (`memql-package.yaml`, native release image stage). There are no per-product node
  images: a plain engine node runs a product's DSL at runtime, so it does
  **not** need product code compiled in and does **not** fail on a product
  prompt template — the DSL bundle supplies the prompts (next bullet).
- The product → a tiny **data-only DSL bundle image** (just the `.memql` tree),
  mounted at `MEMQL_DSL_PATH` by the `deploy/k8s/components/dsl-bundle`
  init-container so every engine node loads the product's domains at boot.
- The product frontend repo → the client (SPA) image.

So a product ships just **two** artifacts — a DSL bundle and a client — and
rides the same product-agnostic engine images as everyone else. A release is
`{engine version, bundle digest, client digest}` pinned in one per-env overlay
(see **Deploy**, below) — there is no separate carrier build workflow or
release lockfile. See [Downstream product stacks](downstream-stacks.md) for the
contract.

---

## 5. Deploy — GitOps, digest-pinned, connection-gated

- The per-env overlay (`deploy/k8s/overlays/<env>`) is the **single image
  authority** — every image pinned by `@sha256:` digest (a CI gate enforces
  it). Argo CD reconciles the overlay; rollback = `git revert`.
- **Migrations** run once (a gated pre-deploy `migrate` Job + identity on
  boot), on the **direct** endpoint.
- **Connection safety is a deploy requirement, not an afterthought** (#1958):
  - Pre-deploy gate `scripts/deploy/conn-headroom-check.sh --live` — blocks a
    deploy whose projected peak + **live** foreign backends would exceed the
    budget (catches "deploy into an already-full instance").
  - `*/5` monitor (`conn-monitor` CronJob) — logs total-vs-budget + a
    per-`application_name` leak detector, so pressure is **seen** before it
    storms.
- **Cutover ordering gotcha:** an Argo CD image-sync re-applies a Deployment's
  `replicas` *past* `ignoreDifferences`, so it un-drains a scaled-to-0 fleet.
  When changing a connection-config secret, cut the secret over **before** the
  sync scales pods up; bring **identity up first**. See
  [DB connection budget](db-connection-budget.md) and
  [Scaling and upgrading an instance](instance-scale-and-upgrade.md).

---

## 6. Versions

| Component | Minimum |
|-----------|---------|
| Go | 1.26.1+ (to build) |
| PostgreSQL | 16 + TimescaleDB Community + pgvector (self-hosted, CloudNativePG) |
| Kubernetes | a recent conformant cluster (AKS is the exercised target) |
| Argo CD / Rollouts | required for the GitOps + blue-green path |

---

## See also

- [DB connection budget & graceful deploy](db-connection-budget.md) — the budget formula, pooler split, monitor + gate.
- [Database platform](database-platform.md) — the CNPG operator stack, what an
  operator must provision, alerts, and the failover / restore drills.
- [Scaling and upgrading an instance](instance-scale-and-upgrade.md) — version rolls, upgrade checks and verification.
- [Infrastructure](infrastructure.md) — the AKS cluster.
- [Environment Variables](env-vars.md) — the full env surface.
- [Reproduce the cloud locally](reproduce-the-cloud-locally.md) — the local dev cluster.
