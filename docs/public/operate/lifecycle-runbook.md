---
title: Node Lifecycle, Graceful Drain & Maintenance Runbook
audience: public
status: stable
area: operate
sinceVersion: 0.9.40
owner: znas
---

# Node Lifecycle, Graceful Drain & Maintenance Runbook

Operational reference for the memQL node lifecycle introduced by the
resilient-mesh epic (znasllc-io/memql#1259, Phase 3). It covers the explicit
node state machine, the graceful SIGTERM drain, the operator on-demand
maintenance trigger, the coordinated/ordered rollout driver, and the
green-before-staging-deploy parity gate that gates every staging roll.

This is the *lifecycle* companion to the cluster/secrets/promotion reference in
[`deployment-strategy.md`](./deployment-strategy.md) (topology, `make deploy`,
genesis envelope, staging→prod promotion). Read that for *what* deploys; read
this for *how a node enters and leaves rotation cleanly* during one.

---

## 1. Lifecycle state machine

Every node advertises an explicit lifecycle state (`component/node/lifecycle.go`,
#1268). The state is the node's **self-asserted operational intent**, surfaced
both in gossip (as `NodeHealthStatus`) and in the readiness/liveness probes.

| State | Serving? | `/livez` | `/healthz` + `/readyz` | Gossip health | Meaning |
|---|---|---|---|---|---|
| `starting` | no | 200 | 503 (not-ready) | `CONNECTING` | Booting; dependencies (DB, engine, mesh registration) wiring up. |
| `ready` | yes | 200 | 200 | `HEALTHY` | Serving normally; in LB + mesh rotation. |
| `draining` | finishing in-flight | **200** | **503 (`status: draining`)** | `DRAINING` | Graceful shutdown begun; **still alive**, de-routed, finishing in-flight work. |
| `stopped` | no | (gone) | 503 | `STOPPED` | Terminal; drain finished, process exiting. |

Legal transitions (forward-only; self-edges are idempotent no-ops):

```
starting ──► ready ──► draining ──► stopped
   │           └───────────────────────▲
   └────────────► draining / stopped ───┘   (a node can drain/stop before ever going ready)
```

**Readiness ≠ liveness is the load-bearing invariant.** `/livez` is a *pure
process-liveness* probe (#1117) — it stays `200` through an entire drain — while
`/healthz` and `/readyz` are *readiness*: they flip to `503` the instant the node
enters `draining`. So:

- The k8s **livenessProbe targets `/livez`** → a transient dependency/mesh blip
  (or an in-progress drain) never liveness-kills an otherwise-alive pod.
- The k8s **readinessProbe targets `/healthz`** (and `/readyz` asserts schema
  presence, #657) → the moment a node drains, the LB and `/memql/ws` front door
  de-route it and peers route around it via the `DRAINING` gossip health.

Coupling liveness to `/healthz` was the #1115/#1117 restart-storm root cause; do
**not** point the livenessProbe at `/healthz` or `/readyz`.

---

## 2. Graceful drain (SIGTERM path)

On `SIGTERM` (the normal k8s rollout / scale-down signal) a node runs the
graceful-drain sequence in `app/run.go` (#1269), bounded by two env vars on the
carrier binary:

| Env var | Default | What it bounds |
|---|---|---|
| `MEMQL_SHUTDOWN_DRAIN_DELAY` | `5s` | How long the node keeps serving **after** flipping readiness to 503, before it stops accepting and starts the in-flight wait. Covers LB/endpoint propagation so no new request lands on a de-routed pod mid-roll. |
| `MEMQL_SHUTDOWN_GRACE_PERIOD` | `25s` | The bound on how long the node waits for **in-flight user streams** to finish after it is de-routed, before forcing `Draining → Stopped`. |

Both accept a Go duration string (`"5s"`, `"25s"`); an unparseable value logs a
warning and falls back to the default. A negative value skips that wait entirely.

The sequence:

1. **`Ready → Draining`** — the lifecycle flips, so the next gossip heartbeat
   advertises `DRAINING` and peers route around this node at once.
2. **Readiness 503, keep serving** — `component/server.SetDraining(true)` makes
   `/healthz` + `/readyz` return `503` (`status: draining`). The node keeps
   serving for `MEMQL_SHUTDOWN_DRAIN_DELAY` so k8s/the LB de-route the pod
   before it stops taking work.
3. **Bounded in-flight drain** — the node waits up to
   `MEMQL_SHUTDOWN_GRACE_PERIOD` for active user streams
   (`MemqlService.Stream` sessions; mesh/worker/voice infra streams don't count)
   to reach zero. So a deploy never drops a turn mid-flight.
4. **`Draining → Stopped` + Stop sweep** — anything still in flight at the
   deadline is cut off; the bounded `GracefulStop` (#1119) forces remaining
   streams closed, then the dependency `Stop` sweep runs and the process exits.

**Set the k8s `terminationGracePeriodSeconds` ≥ `DRAIN_DELAY + GRACE_PERIOD`
plus headroom** (default 5s + 25s = 30s ⇒ use ≥ 45s) so the orchestrator doesn't
`SIGKILL` a pod mid-drain.

Clean startup is the mirror image: a node only flips to `ready` once every
dependency's `Ready()` has fired (bounded by `DefaultStartupReadyTimeout`, 30s) —
it does not advertise readiness merely because `Start` was invoked.

---

## 3. Operator on-demand maintenance trigger

The **same** graceful-drain sequence can be triggered on demand — without a
deploy — via the owner/admin-gated gRPC `NodeMaintenanceMsg` (#1270). One
mechanism, two entry points: deploy `SIGTERM` (§2) and this operator command.

CLI tool: `scripts/cluster/rolling-drain/` (Go). It opens a stream to the target
node, sends `NodeMaintenanceMsg{action: "drain"}`, and the node runs the exact
same Draining→Stopped sequence the SIGTERM path runs.

```bash
# Drain ONE node on demand (owner/admin-gated):
MEMQL_MASTER_KEY=... \
go run ./scripts/cluster/rolling-drain \
    --endpoint bff-2:50051 \
    --reason "manual roll 0.9.40"
```

- **Auth:** `MEMQL_MASTER_KEY` (the cluster operator credential) is required;
  the handler is owner/admin-gated, so a non-operator cannot drain a node.
- `--reason` is recorded in the node's logs/audit.
- The node, once drained, is replaced/restarted by the orchestrator (k8s rolling
  the Deployment, or an operator restarting the process) — the trigger only
  drives **this** node's drain, not the replacement.

---

## 4. Coordinated / ordered rollout

`scripts/cluster/rolling-drain.sh` sequences per-node drains so a rollout is
**deterministic and gap-free**: with N replicas, at most one is ever out of
rotation. For each endpoint, in order, it sends the operator drain trigger (§3),
then **waits for a healthy `Ready` replacement** before moving to the next.

```bash
MEMQL_MASTER_KEY=... \
scripts/cluster/rolling-drain.sh \
    --reason "manual roll 0.9.40" \
    --readiness-url-template 'http://%s:8080/readyz' \
    --ready-timeout 180 \
    bff-1:50051 bff-2:50051
```

- The trailing args are the per-node gRPC endpoints **in drain order** (e.g. one
  replica of a type, wait for its replacement, then the next; or
  hub-after-dependents ordering).
- `--readiness-url-template` is a printf template (host → readiness URL) polled
  until `200` before advancing; omit it to drain + wait a fixed `--settle-seconds`
  instead.
- `--dry-run` prints the plan without draining anything.

The actual pod replacement is the orchestrator's job; this driver only **sequences
the drains and gates on readiness between them**. Under k8s RollingUpdate the
Deployment already does one-at-a-time replacement (identity stays HA via its PDB,
[`deployment-strategy.md` §5](./deployment-strategy.md)); use this driver for the
on-demand / cross-type ordered case (e.g. drain workers before the hub).

### Blue-green / green-before-deploy expectation

A staging roll is **never** the place a multi-replica delivery bug is first seen.
The contract (epic #1259's whole point):

1. The cross-replica parity gate (§5) must be **green** before any staging
   deploy — the multi-replica delivery path is exercised in CI, not staging.
2. The roll itself is gap-free: graceful drain (§2) + ordered rollout (§4) keep
   ≥ N−1 replicas of every type serving throughout, and identity HA keeps auth
   up across the roll.
3. Rollback is `git revert` of the digest overlay
   ([`deployment-strategy.md` §8](./deployment-strategy.md)) — no imperative
   `kubectl set image`.

---

## 5. Parity-CI gate (green-before-staging-deploy)

`.github/workflows/cluster-e2e.yml` boots the 2-replica **staging-parity**
cluster and runs the cross-replica delivery gate (`make cluster-e2e` →
`scripts/test/cluster-e2e.sh` → `test/clustere2e`, build-tagged `clustere2e`,
#1261). The gate asserts the #1259 invariant on two shapes:

- **Single-event delivery** (`TestClusterCrossReplicaDelivery`): an utterance
  produced on one bff replica reaches a subscriber anchored on **every** bff
  replica, exactly once. RED-by-design on the pre-fix mesh; went **green** once
  #1264 migrated the chat-reply path onto the durable backbone.
- **Streamed turn** (`TestClusterStreamedTurn`, the #1266 cross-process
  verification): an ordered token-streamed turn -- including a **mid-stream
  replica switch** (the back half of the turn produced from a different replica)
  -- arrives on every replica complete, exactly once, and **in order** (no lost,
  reordered, or duplicated chunk). Exercises the #1266 ordered/backpressured
  streaming contract over the same substrate.

Both are synthetic-event tests (no SI provider keys): the streamed turn drives a
sequence of utterance rows whose ids encode their order, so ordering is
observable without a live LLM.

**It gates the DEPLOY path, not the PR merge queue.** A full-cluster boot (~16
containers, a cold-cache multi-image build, a 10m health wait) is heavy and
flakier than a unit lane; a required check on `pull_request` would let one slow
boot wedge the merge queue. So the workflow runs on the **staging-deploy trigger**
(a `releases/**` lockfile landing on `main`) and on `workflow_dispatch`, and its
result is the green-before-staging-deploy signal — **not** a branch-protection
required check on PRs (`ci-required` in `ci.yml` stays the only required PR
check).

### Owner prerequisite — secrets

The parity cluster builds the carrier (`memql-bff-copresent`) and the `copresent`
SPA from their **private sibling repos**, so the gate needs two
owner-provisioned secrets. Until they exist, the gate **skips cleanly** (a
visible `secrets present?` → skip) and never spuriously blocks:

| Secret | Scope | Used for |
|---|---|---|
| `SIBLING_CHECKOUT_TOKEN` | read on `copresent` + `memql-bff-copresent` | `actions/checkout` of the two private siblings into the workspace. |
| `MEMQL_PACKAGES_TOKEN` | `read:packages` (both scopes) | the copresent SPA image build (the `node_auth_token` compose build secret). |

---

## 6. Owner-finalization checklist (Phase 4 closeout)

The remaining steps below are **owner actions** — they cannot be done from CI/a
PR (secrets + a live staging deploy):

1. **Provision the two secrets** (`SIBLING_CHECKOUT_TOKEN`, `MEMQL_PACKAGES_TOKEN`)
   as repo/org Actions secrets. The `cluster-e2e` gate's `secrets present?` job
   then flips to running the parity boot.
2. **Verify the gate is green** — run `cluster-e2e` via `workflow_dispatch` (or
   land a release lockfile) and confirm the cross-replica delivery gate passes on
   current `main` (it should, post-#1264).
3. **Flip it to a required deploy gate** — wire `cluster-e2e / gate` as a required
   status on the deploy/promotion path (the release-lockfile/publish step), so a
   red parity gate blocks the staging roll. Keep it **off** the PR-merge-queue
   required set.
4. **Final coherent staging roll** — cut + promote the whole-epic release per
   [`deployment-strategy.md` §2/§6](./deployment-strategy.md) (`make deploy
   VERSION=X`, deep smoke, lockfile, then promote), and verify the resilient-mesh
   stack on staging (no drop / no dup across replicas; clean drains on the roll).
