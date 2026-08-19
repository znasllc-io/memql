---
title: Minimum Requirements (running beyond local)
audience: public
status: stable
area: operate
sinceVersion: 0.9.88
owner: znas
---

# Minimum Requirements — running memQL beyond local

This page is the short answer to *"what do I actually need to stand memQL up
somewhere real?"* — i.e. anything past the local dev cluster. It states the
minimums and links the deep runbooks for each piece. The biggest non-obvious
requirement is the **database**, so that section is first and most detailed.

There are two run modes:

| Mode | What it is | How |
|------|-----------|-----|
| **Local dev** | k3d + ArgoCD cluster on one machine — Postgres, the mesh, LiveKit. Throwaway. | `make up` (primary local path, memql#2061). See [Reproduce the cloud locally](reproduce-the-cloud-locally.md). |
| **Real deployment** (any installation) | A Kubernetes mesh against **self-hosted CloudNativePG** (PostgreSQL + TimescaleDB Community + pgvector), in-cluster. | The rest of this page. |

Everything below is **target-agnostic by design**: the architecture is
identical from a local k3d cluster to a cloud one; only the *config values*
(DSNs, replica counts, sizes) differ. If a requirement is "do X differently
over there," that's a bug, not a feature.

memQL ships **one installation shape** (epic memql#3943) — there is no
staging-versus-production dimension inside the product. An operator who wants a
second environment installs a second instance, with its own domain and its own
database. What varies is the deploy TARGET (`provider`: `docker-local` |
`azure`).

---

## 1. Database — **self-hosted CloudNativePG**

memQL stores everything (the time-series memory graph + the observability
hypertables) in **PostgreSQL 16 + TimescaleDB Community + pgvector**, run
**inside the cluster** by the CloudNativePG operator (epic memql#3842). The
same operator, the same operand image and the same four resource kinds run in
local k3d and in a cloud cluster; only the numbers and endpoints differ.

> **This replaced "Tiger Cloud is the only supported provider today."** It was
> true, and it was the source of the constraint the rest of this page used to
> be organised around: a per-tier `max_connections` ceiling of ~59 usable
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
      the overlay (`.github/workflows/build-db-image.yml`). The tag must begin
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

The whole mesh (≈10 node-types × replicas) shares **one** database.

`max_connections` is now **ours to set** — 200 for a local cluster, 400 for a
cloud one — so the ceiling is a sizing decision rather than a tier limit. It
is still a ceiling: every Postgres backend is a process with its own memory, so
raising it trades RAM for headroom, which is why the budget below is still
enforced by a deploy gate.

**There is no pooler in the path today**, and the two DSNs both resolve to
`memql-db-rw`. The split is kept because it is about *transaction-mode pooling
breaking session state*, not about Tiger:

- **Bulk traffic** (the bun pool — all queries and mutations, every mesh pod)
  rides `MEMQL_DATABASE_DSN`.
- **Session-stateful work** — session-scoped advisory locks (cognition
  dispatch/greet/feedback gates, cron leader, topology reconciler, planner
  admission) **and migrations** — rides `MEMORY_NODES_DATABASE_DIRECT_DSN`. A
  transaction-mode pooler recycles a server backend *between statements*, which
  would silently drop a held session lock.

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
  `identity`, `cognition`, `voice`, `agent`, `planner`, `workbench`, `mcp`,
  plus `livekit` and the `voice-agent`. Manifests: `deploy/k8s/base`. A
  product stack adds a `bff` -- a plain, product-agnostic engine node that
  fronts the product's DSL bundle -- plus the product client (SPA), both
  layered in from the product's own overlay -- see
  [Downstream product stacks](downstream-stacks.md).
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
- **AI providers:** an `MEMQL_OPENAI_API_KEY` is required (cascade voice = OpenAI
  ASR/TTS); Anthropic optional. See [Environment Variables](env-vars.md) for the
  full env surface and the bootstrap-envelope vs concept-stored split.

---

## 4. Images — built on the build server, never a laptop

Deployable images are built on **GitHub Actions → OIDC → ACR `acrmemql`**, never
hand-built locally (local Docker is dev-only):

- `memql` (this repo) → **every** node-type image — `identity`, `bff`,
  `cognition`, `agent`, `planner`, `voice`, `workbench`, `mcp` — as a
  **product-agnostic engine image**
  (`.github/workflows/build-engine-images.yml`). There are no per-product node
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
  Deployment Strategy (see the product pack repo's docs/operate/deployment-strategy.md).

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
- Deployment Strategy (see the product pack repo's docs/operate/deployment-strategy.md) — release/lockfile/promote/GitOps.
- [Infrastructure](infrastructure.md) — the AKS cluster.
- [Environment Variables](env-vars.md) — the full env surface.
- [Reproduce the cloud locally](reproduce-the-cloud-locally.md) — the local dev cluster.
