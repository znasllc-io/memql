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
| **Local dev** | k3d + ArgoCD cluster on one machine — Postgres, the mesh, LiveKit. Throwaway. | `make up` (primary local path, memql#2061). See [Reproduce staging locally](reproduce-staging-locally.md). |
| **Real deployment** (staging / prod / self-host) | A Kubernetes mesh against a **managed** TimescaleDB. | The rest of this page. |

Everything below is **environment-agnostic by design**: the architecture is
identical local → staging → prod; only the *config values* (DSNs, replica
counts, sizes) differ. If a requirement is "do X differently in prod," that's a
bug, not a feature.

---

## 1. Database — **Tiger Cloud is the only supported provider today**

memQL stores everything (the time-series memory graph + the observability
hypertables) in **PostgreSQL 16 + the TimescaleDB extension**. For a real
deployment that means a **managed Tiger Cloud** service (`TIMESCALEDB` type).
Tiger Cloud is the **only DB provider we support right now** — the engine
assumes TimescaleDB (hypertables + continuous aggregates back `code_invocation`
observability), and the deploy tooling, connection model, and runbooks are all
written against Tiger. A vanilla self-managed Postgres is **not** a supported
target yet.

### Minimum DB checklist

- [ ] A Tiger Cloud service, **PostgreSQL 16 + TimescaleDB**.
- [ ] Sized so the **connection budget** fits the mesh (see below) — start at
      **1 CPU / 4 GB** and **`max_connections` = 500** for a small staging mesh.
- [ ] The **transaction pooler enabled** (PgBouncer, transaction mode).
- [ ] **Two** connection strings wired into `memql-secrets`:
      `MEMQL_DATABASE_DSN` → the **pooler**, and
      `MEMORY_NODES_DATABASE_DIRECT_DSN` → the **direct** endpoint.

### Why two endpoints? — the connection model (epic [#1925](https://github.com/znasllc-io/memql/issues/1925))

The whole mesh (≈10 node-types × replicas) shares **one** database. Tiger caps
direct `max_connections` per tier (25–500 on the 0.5–4 CPU range) and reserves
~17 for superuser/ops, so the *direct* budget is small and fixed — and a deploy
**surge** (blue-green + rolling restart briefly doubles pods) used to blow past
it → `SQLSTATE 53300` ("remaining connection slots…") storms that wedged a
roll. Tiger's intended answer to "many connections" is the **connection
pooler**, not a bigger `max_connections`.

So memQL runs a **hybrid endpoint split**:

- **Bulk traffic** (all queries + mutations, every pod) rides
  `MEMQL_DATABASE_DSN` → the **transaction pooler**. Client connections
  decouple from Postgres backends, so a deploy surge no longer maps 1:1 to
  slots (transaction-pool ceiling ≈ `(max_connections − 17) × 20`).
- **Session-stateful work** — session-scoped advisory locks (cognition
  dispatch/greet/feedback gates, cron leader, topology reconciler, planner
  admission) **and migrations** — rides `MEMORY_NODES_DATABASE_DIRECT_DSN` →
  the **direct** endpoint. A transaction-mode pooler recycles a server backend
  *between statements*, which would silently drop a held session lock; these
  few, bounded connections take a real slot instead.

In code: `Database.DirectBunDB()` returns the direct pool when `DIRECT_DSN` is
set, else falls back to the main pool — so **local/dev without a pooler is
unaffected** (single pool, identical behaviour). bun's `pgdriver` speaks the
simple query protocol (no server-side prepared statements), so transaction
pooling is safe.

### Sizing the budget

```
peak_direct ≈ Σ(session-stateful holders) + migrate(1) + live FOREIGN backends
REQUIRE:  peak_direct ≤ max_connections − reserved(~17)
```

- **Foreign backends are real and must be budgeted.** Tiger's own control-plane
  process (`application_name=deployer`, the `postgres` superuser) holds a pool
  of connections you **cannot** terminate (#1822). Size `max_connections` with
  headroom above it (this is why staging runs 500, not 105).
- Bulk pods do **not** count against the direct budget — they multiplex through
  the pooler.

Full detail, the budget formula, the pre-deploy gate, and the monitor:
[DB connection budget & graceful deploy](db-connection-budget.md). Tiger CLI /
service management: [Database Setup](database-setup.md). `max_connections` is
set in the **Tiger console → Common parameters** (not the CLI/SQL).

---

## 2. Compute — Kubernetes

- A **Kubernetes** cluster. We run and test on **Azure Kubernetes Service**
  (`aks-memql-staging`); any conformant cluster should work, but AKS is the
  exercised path. See [Infrastructure](infrastructure.md).
- **No database pod** — the DB is the managed Tiger service above.
- The mesh node-types (each a `Deployment`, 2 replicas for HA except bff which
  is an Argo **Rollout**): `identity`, `bff`, `cognition`, `voice`, `agent`,
  `planner`, `workbench`, `mcp`, plus the `copresent` SPA, `livekit`, and the
  `voice-agent`. Manifests: `deploy/k8s/base`.
- Rough small-staging footprint: ~4 × 2-vCPU nodes. Right-size per workload.
- **Argo CD + Argo Rollouts** for GitOps + the bff blue-green cutover.

---

## 3. Secrets & auth

Every DB-connecting pod mounts the `memql-secrets` Secret via `envFrom`. The
**four** keys (see [`deploy/k8s/base/secret.example.yaml`](https://github.com/znasllc-io/memql/blob/main/deploy/k8s/base/secret.example.yaml)):

| Key | What |
|-----|------|
| `MEMQL_MASTER_KEY` | 32-byte key that decrypts the genesis envelope. |
| `MEMQL_GENESIS_B64` | base64 of the **sealed env envelope** (~150 config vars, decrypted in-process at boot; `MEMQL_GENESIS_AUTOLOAD=true`). |
| `MEMQL_DATABASE_DSN` | DB — the **transaction pooler** endpoint. |
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
hand-built locally (local Docker is dev-only). A release builds three repos at
one engine ref and assembles a digest-pinned lockfile:

- `memql` → engine images (`identity`, `voice`, `mcp`).
- `memql-bff-copresent` → the **carrier** images (`bff`, `cognition`, `agent`,
  `planner`, `workbench`) — these MUST be carrier-built (they carry the
  CoPresent DSL; a pure-engine image fails with `unknown prompt template`).
- `copresent` → the SPA.

See [Deployment Strategy](deployment-strategy.md) for the full release →
lockfile → promote → reconcile flow.

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
  [Deployment Strategy](deployment-strategy.md).

---

## 6. Versions

| Component | Minimum |
|-----------|---------|
| Go | 1.26.1+ (to build) |
| PostgreSQL | 16 + TimescaleDB (Tiger Cloud) |
| Kubernetes | a recent conformant cluster (AKS is the exercised target) |
| Argo CD / Rollouts | required for the GitOps + blue-green path |

---

## See also

- [DB connection budget & graceful deploy](db-connection-budget.md) — the budget formula, pooler split, monitor + gate.
- [Database Setup](database-setup.md) — Tiger CLI, service management.
- [Deployment Strategy](deployment-strategy.md) — release/lockfile/promote/GitOps.
- [Infrastructure](infrastructure.md) — the AKS cluster.
- [Environment Variables](env-vars.md) — the full env surface.
- [Reproduce staging locally](reproduce-staging-locally.md) — the local dev cluster.
