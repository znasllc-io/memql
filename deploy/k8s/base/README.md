# memQL backend cluster on AKS

Kubernetes manifests for the memQL multi-node mesh, deployed to AKS
(epic [znasllc-io/memql#522](https://github.com/znasllc-io/memql/issues/522)
-- pivot from ACA, which can't host the per-node multi-port mesh).

## No database pod

Staging/prod use the managed **Tiger Cloud** DB (`xahn9ru4v6`, Azure East
US 2). There is **no postgres/pgadmin/nginx/livekit** in this directory --
only the 7 memQL node-types. Every node connects to Tiger via the
`MEMQL_DATABASE_DSN` key in the `memql-secrets` Secret.

## Mesh = cluster DNS

Each node's `Service` is named after its node-type short name (`bff`,
`cognition`, `voice`, `agent`, `planner`, `identity`, `workbench`) in the
`memql` namespace. Same-namespace cluster DNS resolves the mesh
values (`bff:50058`, `agent:50055`, ...), so `MEMQL_NODE_ADDRESS` /
`MEMQL_WORKER_PEERS` are the same in the local k3d cluster and on AKS.

**#1399 exception -- the parent dial target is `bff-active`, not `bff`.**
Under the live Argo Rollouts blue/green cutover the unscoped `bff` Service
selects BOTH colors during the 3600s `scaleDownDelay`, so a leaf's single
parent stream could land on a draining old-color pod (~1h mixed-version mesh).
Leaf nodes therefore dial `bff-active:50058` (the Rollout-managed,
color-pinned active Service, which carries `:50058` for exactly this reason);
the voice-agent dials `bff-active:50051`. The local k3d cluster has
a single bff color and no Rollout, so it keeps `bff:50058` -- same star
topology, only the dial-target value differs per environment.

| Node | Image | NodeService port | NODE_ADDRESS | PARENT | WORKER_PEERS |
|------|-------|------------------|--------------|--------|--------------|
| bff | memql-bff-copresent:0.9.0 | 50058 | bff:50058 | -- | voice=voice:50059,agent=agent:50055,cognition=cognition:50054,planner=planner:50056,workbench=workbench:50060 |
| cognition | memql:0.9.0 | 50054 | cognition:50054 | bff-active:50058 | agent=agent:50055 |
| voice | memql:0.9.0 | 50059 | voice:50059 | bff-active:50058 | -- |
| agent | memql:0.9.0 | 50055 | agent:50055 | bff-active:50058 | workbench=workbench:50060 |
| planner | memql:0.9.0 | 50056 | planner:50056 | bff-active:50058 | -- |
| workbench | memql:0.9.0 | 50060 | workbench:50060 | bff-active:50058 | -- |
| identity | memql:0.9.0 | 50061 | identity:50061 | -- | -- |

Every ClusterIP Service exposes the node's NodeService port (5005x) + `8085`
(http) + `50051` (grpc). `bff` also gets a `LoadBalancer` Service
(`bff-external`) on 8085 -- the external entry point (maps to
`app.copresent.ai` later).

## Multi-replica HA ([#551](https://github.com/znasllc-io/memql/issues/551))

**`copresent` (the frontend) runs 2 replicas + a PodDisruptionBudget.** It's
a stateless SPA/proxy server — no memQL engine, no automation scheduler, no
mesh — so it scales freely, and the PDB keeps ≥1 serving through node drains
/ upgrades / rollouts. (`identity` HA is handled separately via its
env-provided signing key, [#550](https://github.com/znasllc-io/memql/issues/550).)

**The engine node-types `cognition`, `voice`, `agent`, `planner`,
`workbench` run 2 replicas + PDBs (#561).** Multi-replica was unsafe because
automations could double-fire; both paths are now cluster-singleton:

1. **Scheduled (cron) automations** — a `CronLeader`
   (`component/automations/cron_leader.go`) holds a Postgres advisory lock so
   exactly one node cluster-wide runs them; the scheduler gates cron firings
   on `LeaderGate`. Session-scoped → automatic failover. (Also fixed the
   pre-existing once-per-node-type firing.)
2. **Event-triggered automations** — a `ClusterExecutionGuard`
   (`component/automations/cluster_guard.go`) claims each `(automation,
   InitialChainHead)` in `automation_execution_claims` before the event
   executor runs it, so an event reaching several replicas executes exactly
   once. Fail-open (never drops work) and **observable**: every prevented
   duplicate is WARN-logged + counted (`duplicatesPrevented`), and the claim
   rows are an audit trail. **Watch that counter** — a rising
   `duplicatesPrevented` means events do reach multiple replicas (and we're
   correctly collapsing them); `claimErrors > 0` flags any unguarded window.

**`bff` stays single-replica** until the CoPresent **carrier**
(`memql-bff-copresent`, sibling repo) re-pins memQL ≥ the version carrying
these guards — its image doesn't include them yet. **`identity`** HA is
[#550](https://github.com/znasllc-io/memql/issues/550); **`copresent`** (the
stateless SPA) is in [#551](https://github.com/znasllc-io/memql/issues/551).

> **Deploy note:** the `replicas: 2` bump is only safe with an image that
> contains the guards (≥ the #561 version) and after the
> `automation_execution_claims` migration. `make deploy VERSION=…` builds +
> migrates + applies together, so the normal path is consistent; don't
> `kubectl apply` `replicas: 2` against an older image.

Remaining (optional, [#561](https://github.com/znasllc-io/memql/issues/561)):
worker **load distribution** (headless Service + dial-all) so extra replicas
share load instead of standing by — failover HA already works via Service-VIP
reconnect.

## Migrations run once

The shared Tiger DB must not be migrated by 7 racing nodes. Only the
**identity** node sets `MEMORY_NODES_DATABASE_MIGRATE_ON_START=true` +
`MEMORY_NODES_DATABASE_AUTO_MIGRATE=true`; every other node has both
`false`.

### Gated pre-deploy migration ([#553](https://github.com/znasllc-io/memql/issues/553))

So a schema change never races the worker rollout, `make deploy` /
`make deploy-aks` apply `migrate-job.yaml` (a one-shot `memql migrate` Job)
and **wait for it to complete before any Deployment rolls** — a failed
migration aborts the deploy. The Job is idempotent (bun advisory lock +
mark-applied), and identity's boot migration is retained as a no-op
fallback. The Job is **not** in the kustomization (one-shot); a bare
`kubectl apply -k deploy/k8s` does not run it — precede it with
`kubectl apply -f deploy/k8s/migrate-job.yaml`, or just use `make deploy`.

**Author migrations expand/contract** so old- and new-version pods can both
run against the schema during a rollout: additive only (add columns/tables
nullable-or-defaulted; never drop/rename/tighten a column the currently
deployed code reads in the same release). Split a destructive change across
releases: (1) add the new shape + write both, (2) backfill + switch reads,
(3) a later release drops the old shape once nothing reads it.

## Secrets (genesis A2)

Four keys in `memql-secrets`: `MEMQL_MASTER_KEY`, `MEMQL_GENESIS_B64`,
`MEMQL_DATABASE_DSN`, `MEMORY_NODES_DATABASE_DIRECT_DSN`. With
`MEMQL_GENESIS_AUTOLOAD=true`, each pod decrypts the sealed envelope in-process
at boot and applies ~150 vars set-if-absent; the per-pod overrides (node type,
mesh addresses, the DSNs) win. Every DB-connecting node mounts the Secret via
`envFrom: secretRef: memql-secrets`, so a key added here reaches all pods (no
manifest change). See `secret.example.yaml` for the imperative
`kubectl create secret` recipe and the Azure Key Vault CSI alternative.

### Hybrid connection-pool endpoint split (epic [#1925](https://github.com/znasllc-io/memql/issues/1925))

To kill the deploy-time connection storm (SQLSTATE 53300), bulk traffic rides
the Tiger **transaction pooler** (PgBouncer) and only session-stateful work
takes a direct backend:

- `MEMQL_DATABASE_DSN` -> the **transaction pooler** (db
  `tsdb_transaction`, pooler port `39578`). All queries + mutations. The
  pooler decouples client connections from Postgres backends, so a blue-green
  + rolling deploy surge no longer maps 1:1 to direct slots.
- `MEMORY_NODES_DATABASE_DIRECT_DSN` -> the **direct** (non-pooled) endpoint
  (db `tsdb`). Session-scoped advisory locks (cognition dispatch/greet/feedback
  gates, cron leader, topology reconciler, planner admission) + the bun
  migrator resolve their handle here (`Database.DirectBunDB()`), so a
  transaction-mode pooler can't recycle a held session out from under them.
  **Optional** — when unset, `DirectBunDB()` falls back to the main pool
  (single-pool behavior), so dev/local without a pooler is unaffected.

The split is env-agnostic: only the two DSN values differ per environment,
never the code path. The local k3d parity cluster mirrors it with a
PgBouncer pod (#1932). bun's `pgdriver` speaks
the simple query protocol (no server-side prepared statements), so it is
transaction-pool safe. Migrations stay correct because they run on the direct
endpoint; pods reading bulk through the pooler see the same schema (the pooler
fronts the same database).

## Identity HA — env-provided signing key ([#550](https://github.com/znasllc-io/memql/issues/550))

Identity runs **2 replicas** on a RollingUpdate (with a PodDisruptionBudget
keeping ≥1 up), instead of a single pod with `strategy: Recreate`. That
used to be forced by a ReadWriteOnce key PVC — only one pod could mount the
signing key — so every deploy had an auth-down window.

Now the Ed25519 signing key comes from the sealed envelope as
`MEMQL_IDENTITY_SIGNING_KEY_B64` (a base64 32-byte seed). Every replica derives
the **same** key + `kid` + JWKS from it, so there's no single-writer volume.
Generate one with `make identity-signing-key` and seal it into the genesis
envelope. **Rotate** by generating a new seed, re-sealing, and rolling the
deployment (automatic rotation is disabled in this mode). Without
`MEMQL_IDENTITY_SIGNING_KEY_B64`, identity falls back to the on-disk `MEMQL_IDENTITY_KEY_DIR`
(dev) — which is single-replica only.

## Apply order

```bash
# 1. Namespace
kubectl apply -f deploy/k8s/namespace.yaml

# 2. Secret (real values -- created out-of-band, NOT in kustomize)
#    DSN = transaction pooler (tsdb_transaction:39578); DIRECT_DSN = direct (tsdb).
kubectl create secret generic memql-secrets -n memql \
  --from-literal=MEMQL_MASTER_KEY="$MEMQL_MASTER_KEY" \
  --from-literal=MEMQL_GENESIS_B64="$(base64 < ~/.memql/genesis.znas)" \
  --from-literal=MEMQL_DATABASE_DSN="$POOLER_DSN" \
  --from-literal=MEMORY_NODES_DATABASE_DIRECT_DSN="$(tiger db connection-string xahn9ru4v6 --with-password)"

# 3. All node Deployments + Services (digest-pinned overlay, #699)
kubectl apply -k deploy/k8s/overlays/staging
```

> deployment-v2 Phase 1 (#699): apply the per-env **overlay**, not this base.
> The overlay pins every image by `@sha256:` digest (the single image
> authority); the base `:tags` are placeholders. Rollback = `git revert` of the
> overlay (see `scripts/deploy/aks-rollback.sh`), never `kubectl rollout undo`.

Or, from the repo root: `make deploy-aks ENV=staging` (runs the namespace +
kustomize apply; the Secret step is a one-time prerequisite). `identity`
comes up first to run the one-time migration and serve JWKS; the other
nodes' verifiers retry JWKS non-fatally until it is ready.

## Graceful shutdown (zero-downtime rollout)

On a rollout / scale-down / node drain, each pod drains before it exits
([#552](https://github.com/znasllc-io/memql/issues/552)) so in-flight
requests (incl. WebSocket chat + `/memql/audio`, and gRPC mesh streams)
aren't cut:

1. On `SIGTERM` the process flips `/healthz` to **503 `draining`**, so the
   readiness probe / LB stops routing **new** traffic to it.
2. It keeps serving for `MEMQL_SHUTDOWN_DRAIN_DELAY` (default **5s**) while
   the endpoint removal propagates, then runs the dependency Stop sweep —
   the gRPC servers `GracefulStop` (drain in-flight RPCs/streams).
3. `terminationGracePeriodSeconds: 45` on every pod gives that whole
   sequence (5s drain + ≤30s Stop budget + buffer) room to finish before
   the kubelet force-kills.

So a rolling deploy surges a new Ready pod in, then drains the old one out
instead of cutting it. (The distroless runtime has no shell, so this is done
in-process rather than via a `preStop` sleep.)

## Validate

```bash
kubectl kustomize deploy/k8s/overlays/staging | kubeconform -strict -summary -kubernetes-version 1.30.0
```

## Smoke test (live front door)

After a deploy, exercise the real product path through the public HTTPS
entry (not just pod health) with the repeatable smoke test
([#535](https://github.com/znasllc-io/memql/issues/535)):

```bash
make smoke-staging                                # baseline, read-only
make smoke-staging SMOKE_EMAIL=me@example.com     # + issue a real magic link
make smoke-staging MEMQL_SMOKE_TOKEN=mql_pat_xxx  # + run a live authenticated query
make smoke-staging APP_HOST=app.copresent.ai IDENTITY_HOST=identity.copresent.ai  # smoke prod
```

It checks, in order: TLS + DNS for both public hosts (valid Let's Encrypt
cert), identity `/healthz` + JWKS (directly **and** through the app's
same-origin `/.well-known/jwks.json` proxy), the magic-link login page,
the BFF `/memql/ws` upgrade, and the `/memql/audio` voice route. The
baseline is read-only (sends no email, needs no auth); the **deep** checks
(full magic-link round trip, authenticated query, cross-node AI forward)
run only when `SMOKE_EMAIL` / `MEMQL_SMOKE_TOKEN` are supplied, and every
skipped check is reported explicitly. Exit code is non-zero iff a check
**failed**. Impl: `scripts/deploy/staging-smoke-test.sh`.
