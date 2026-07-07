# Reproduce staging locally (k3d + ArgoCD)

The k3d + ArgoCD cluster is the **blessed local dev topology** (memql#2061,
Epic 0 -- Argo parity): boot it with `make up`. It mirrors staging
(the `aks-memql-staging` AKS cluster, `deploy/k8s/`) end to end -- the
same Kustomize overlays, the same ArgoCD-reconciled manifests, the same
`ignoreDifferences`/selfHeal config -- so the full class of GitOps +
cross-node mesh bugs reproduces locally instead of only on staging.

> **Development principle: multi-node is the default.** Every feature
> runs across the 2-replica mesh in local, staging, and prod -- never
> assume a single process. State/context/events that cross a node
> boundary need explicit plumbing (proxied/forwarded requests don't
> carry another node's session state; cross-node events need a routing
> rule). Implement AND test for the hop: a green single-node unit test
> is a false signal -- exercise the proxied/cross-node path
> (`test/clustere2e/`, `component/grpc/ai_forward_test.go`) and verify
> on this cluster. See the "Multi-node is the DEFAULT" rule in the
> root `CLAUDE.md`. (Bugs this would have caught: memql#1448, #1412,
> #1388.)

## What "parity" means here

| Aspect | Local cluster | Staging | Parity |
|---|---|---|---|
| Orchestrator | ArgoCD (k3d) | ArgoCD (AKS) | identical |
| Manifests | `deploy/k8s/overlays/local/` | `deploy/k8s/overlays/staging/` | same base, env config differs |
| Node-type split | identity / voice / mcp / cognition / agent / planner / workbench / voice-agent | same | identical (the product `bff` head is pack-owned, #2204) |
| Build model | engine (`Dockerfile`) for ALL node types (`memql-<type>:local`); product stacks overlay carrier builds via the carrier hook | carrier builds for product node types (product repo's pipeline) | engine-only here; see [downstream-stacks.md](downstream-stacks.md) |
| Replicas per mesh node (default) | **1** (scale to 2 with `make scale N=2`) | **2** | equivalent |
| Per-replica node id | `fieldRef: metadata.name` (downward API, same as staging) | `fieldRef: metadata.name` | **identical** |
| `MEMQL_NODE_ID` uniqueness | enforced by fieldRef -- unique per pod | enforced by fieldRef | identical |
| ArgoCD `ignoreDifferences` | `/spec/replicas` excluded | same | identical |
| Database | local Postgres + TimescaleDB (postgres pod) | Tiger Cloud | config only |
| Connection pooler | not present locally (single db-pool pod, direct) | Tiger Cloud managed PgBouncer | config only |
| Blob storage | Azurite emulator | Azure Blob | config only |
| Secrets / keys | dev defaults (seeded by `make secrets`) | Key Vault via ESO | config only |
| ExternalSecrets | deleted by `$patch: delete` in local overlay | present | config only |
| LiveKit | **LiveKit Cloud** (outbound; no self-hosted livekit/sip/redis locally) | self-hosted `livekit-server` + `livekit/sip` | divergent -- justified (Epic #2184) |
| Ingress | k3s-bundled traefik front door (`identity.local.znas.io`, mkcert TLS) + port-forwards for the gRPC heads | ingress-nginx | divergent -- traefik vs nginx |
| Digest-pinning gate | skipped for `ENV=local` in drift-check.sh | enforced | divergent -- justified |

## Prerequisites

- **docker** -- Docker Desktop or Colima (must be running).
- **k3d** -- `brew install k3d`.
- **kubectl** -- `brew install kubectl`.
- **git** -- the cluster's ArgoCD Application points at the current git branch;
  you must push your branch before ArgoCD can sync it.

No product sibling repo is required: the engine cluster builds every node
image from this repo's Dockerfile. Product stacks bring their own carrier
builds via the carrier hook (see
[downstream-stacks.md](downstream-stacks.md)).

No genesis env file is required for `make up`; dev secrets are hardcoded
in `scripts/k3d/seed-secrets.sh` (Azurite well-known key, `memql_dev` Postgres
password).

## Bring it up

```bash
# Single-node (fast, default):
make up

# Multi-node (2 servers + 1 agent, for cross-node mesh testing):
make up SERVERS=2 AGENTS=1
make scale N=2

# Clean slate (nuke + repave -- wipes the in-cluster DB by construction):
make up-refresh
```

`make up` is the full fresh bring-up (`scripts/k3d/bringup.sh`); it does the
following in order:

1. Creates a k3d cluster (default name `memql`).
2. Installs ArgoCD v2.13.3 (same version as staging) via
   `kubectl apply -k deploy/argocd/bootstrap`.
3. Seeds k8s Secrets (`memql-secrets`, `memql-local-db-creds`,
   `livekit-secrets`, `telephony-secrets`) via `scripts/k3d/seed-secrets.sh`.
4. Applies the ArgoCD Application `memql-local` pointing at
   `deploy/k8s/overlays/local` on the current git branch.
5. Waits for ArgoCD to sync and pods to become Ready (configurable via
   `MEMQL_K3D_ARGOCD_TIMEOUT`, default 300s).
6. Builds + imports the engine images (same as `make dev`) so no pod sits in
   ImagePullBackOff, then waits for every Deployment to become Available.

`make up-refresh` runs the same bring-up preceded by a purge teardown
(`make down PURGE=1`), so the in-cluster Postgres is wiped and the
environment repaves from scratch. It is idempotent and honors the same
`SERVERS`/`AGENTS`/`REVISION` overrides as `make up`.

## Port-forward reference

After `make up`, these local ports are forwarded from the k3d cluster:

| Port | Service |
|------|---------|
| `8085` | identity service (direct) |
| `7880` | livekit (WebSocket) |
| `5432` | Postgres (direct) |

The product SPA (`:8080`) and the `bff` carrier gRPC head (`:50051`) are NOT
part of the engine repo's local overlay (#2204) -- they are built and pinned
from their own sibling repos. The local engine gRPC head is the `mcp` node,
reached on demand:

```bash
kubectl port-forward -n memql svc/mcp 50051:50051
```

Access identity: `https://identity.local.znas.io` (front-door TLS -- needs the
seeded mkcert cert + a `*.local.znas.io` hosts entry) or `http://localhost:8085`
(direct / fallback when the cert isn't seeded).
Access the engine gRPC head: `localhost:50051` (after the port-forward above)

## Inner-loop dev

The workflow after a code change:

```bash
# Rebuild ALL nodes and restart pods:
make dev

# Rebuild a single node type (faster):
make dev NODE=bff
make dev NODE=identity
make dev NODE=cognition

# Pull and import upstream infra images (postgres/azurite; the local dev
# loop uses LiveKit Cloud, so no local livekit/redis images are needed):
make dev PULL_INFRA=1
```

`make dev` does:

1. `docker build` the node image from this repo's Dockerfile (`BUILD_TAGS=<type>`); a downstream product stack overrides a subset via the carrier hook.
2. `k3d image import` -- loads the image into the cluster's containerd.
3. `kubectl rollout restart deployment/<node>` -- triggers a pod roll so
   the new image is used. ArgoCD's `ignoreDifferences` does not cover
   the `restartedAt` annotation so selfHeal won't revert this.

The inner loop is **pure-Argo**: no manifest files are applied directly. The
pod restart is purely at the pod level; ArgoCD still owns the Deployment spec.

## Multi-node mesh testing

```bash
# Scale to 2 replicas per Deployment:
make scale N=2

# Litmus: verify every pod has a UNIQUE MEMQL_NODE_ID:
make status

# Scale back to single-node:
make scale N=1
```

Because `deploy/k8s/base/` sets `MEMQL_NODE_ID` via `fieldRef: metadata.name`,
each pod automatically gets a unique node id matching its pod name. No overlay
changes are needed to enable multi-node.

`make status` checks that all running pods have distinct `MEMQL_NODE_ID`
values. Shared ids are the root cause of the #1042 class of mesh bugs.

## Re-seed secrets

```bash
make secrets
```

This re-runs `scripts/k3d/seed-secrets.sh` and is idempotent. Use it if you've
torn down and recreated the cluster, or if you've rotated the dev secret values.

## Tear down

```bash
make down           # delete cluster (keeps kubeconfig context)
make down PURGE=1   # also remove the kubeconfig context
```

## Config-vs-topology audit

The governing invariant: **along the mesh-delivery path, only config may differ
from staging -- never topology or build.** Every divergence below is enumerated
with its justification.

### Invariants -- MUST stay identical

- **Service set:** identity / voice / mcp / cognition / agent / planner /
  workbench / voice-agent (the product `bff` head and SPA are pack-owned,
  #2204).
- **Build source per node** (engine-built here; carrier-built within a product stack -- the #1053 rule, revised).
- **`fieldRef: metadata.name`** for `MEMQL_NODE_ID` on every Deployment in
  `deploy/k8s/base/` -- identical to staging.
- **ArgoCD `ignoreDifferences`** on `/spec/replicas` -- identical to staging.
- **Inter-node addressing** (`MEMQL_NODE_ADDRESS` / `MEMQL_PARENT_ADDRESS` /
  `MEMQL_WORKER_PEERS` / `MEMQL_WORKBENCH_REMOTE`) -- via k8s Service DNS
  (same as staging, just cluster-local).

### Divergences -- justified

| # | Divergence | Local | Staging | Why acceptable |
|---|---|---|---|---|
| 1 | **Replicas (default)** | 1 per Deployment | 2 per Deployment | Resource-constrained laptops. Multi-node is opt-in via `make scale N=2`. The fieldRef mechanism is identical to staging so the multi-node path fully reproduces. |
| 2 | **Ingress** | k3s-bundled **traefik** front door for `identity.local.znas.io` (mkcert TLS); gRPC heads via port-forward | ingress-nginx on AKS | Same ingress *topology* as cloud (an HTTPS front door for identity); traefik ships with k3s so there's no extra install. gRPC heads (`mcp:50051`) stay on port-forward -- they're not fronted locally. |
| 3 | **Digest-pinning gate** | skipped for `ENV=local` in `scripts/deploy/drift-check.sh` | enforced | Local images are built by `make dev` with a stable `:local` tag; they have no ACR digest. The gate exemption is tested by `TestDriftCheckRenderedLocalOverlaySkipsDigestGate`. |
| 4 | **ExternalSecrets / Key Vault** | deleted by `$patch: delete` in local overlay | ESO syncs from Key Vault | Dev secrets are seeded directly by `make secrets`. |
| 5 | **Connection pooler** | direct Postgres connection | Tiger Cloud managed PgBouncer | Single-node dev without a pool is safe; the hybrid-endpoint split used in staging can be reproduced by running PgBouncer as a separate pod if needed. |
| 6 | **voice-agent** | opt-in (`deploy/k8s/overlays/local/` includes it) | in base | Needs live OpenAI + a **LiveKit Cloud** project. Export `LIVEKIT_URL` / `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` before `make up` (seed-secrets sources them); see [voice-bringup-verification.md](voice-bringup-verification.md) and, for telephony, [telephony-local-dev.md](telephony-local-dev.md). |

### Config-only -- EXPECTED to differ

- `MEMQL_DATABASE_DSN` (local Postgres vs Tiger Cloud).
- Blob backend (Azurite connection string vs Azure Blob).
- LiveKit plane (local → a **LiveKit Cloud** project, creds from your env via
  `livekit-secrets`; staging/prod → self-hosted `livekit-server`, key/secret
  ESO-synced from Key Vault — Epic #2184).
- Bootstrap/dev escape hatches (`MEMQL_IDENTITY_ALLOW_INSECURE_*`).
- `MEMQL_IDENTITY_BASE_URL` / `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` (local port-forward
  vs AKS ingress hostname).

## Worked example: reproduce a cross-node mesh bug

1. `make up SERVERS=2 AGENTS=1 && make scale N=2`.
2. `make status` -- verify all pods show distinct `MEMQL_NODE_ID` values.
   If any share an id, stop: the mesh cannot reproduce cross-node bugs.
3. Reproduce the scenario (e.g. send a chat message that triggers an assistant
   reply). Watch logs:
   ```bash
   kubectl logs -n memql -l app=cognition --all-containers -f | grep -Ei 'node_id|EventForward|dedup'
   ```
4. Root-cause against the cross-node path (event routing rules, session state
   on the wrong node, missing proxy forward).
5. Fix and `make dev` to rebuild + restart. ArgoCD reconciles; the pod roll
   picks up the new image.

## The previous Compose-based local stack is retired

The k3d + ArgoCD topology is the **only** supported local path as of
memql#2061 (Epic 0). The earlier Compose-based local stack is fully retired
(memql#2068 / #2088) -- the old compose files and their `make` targets are
deleted, replaced by `make up` / `make dev` / `make down`. k3d + ArgoCD wins
because:

- It uses the same manifests and reconciliation path as staging (Kustomize +
  ArgoCD), so GitOps bugs reproduce locally.
- `fieldRef: metadata.name` for `MEMQL_NODE_ID` is identical to staging.
- The ArgoCD `ignoreDifferences` and selfHeal behavior matches staging exactly.

There is no *nginx* front door locally, but the local overlay DOES ship a
k3s-bundled **traefik** front door on 443 for `https://identity.local.znas.io`
(`deploy/k8s/overlays/local/front-door.yaml`), terminating TLS with a
browser-trusted mkcert `*.local.znas.io` wildcard (`local-znas-tls`, seeded by
`make secrets` from `docker/nginx/certs/dev.{crt,key}` -- skip-with-warning if
that pair is absent, in which case identity is reached via the port-mapped
`:8085` instead). This mirrors the cloud ingress topology. The **gRPC** heads
are still reached via kubectl port-forward -- identity is not exposed on gRPC
externally, and the `mcp` engine gRPC head `:50051` is forwarded on demand.
Postgres `:5432` is likewise port-forwarded; the product SPA + product `bff`
carrier are engine-external and absent locally (#2204); the voice/media plane
is LiveKit Cloud, reached outbound -- no local port-forward.
