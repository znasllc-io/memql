# Progressive delivery — Argo Rollouts + the in-cluster gate (Phase 3, #701)

Replaces the bespoke public-host shell smoke with **controller-driven**
progressive delivery: the **BFF rolls blue/green** (per #616/#675) and **engine
nodes roll canary**, each gated by an **in-cluster, convergence-safe
`AnalysisTemplate`**. A failed analysis **auto-aborts → auto-rollback** to the
stable ReplicaSet — no `rollout undo`, no script.

## Why this kills the old gate's failure modes

| Old failure | Why it's now impossible |
|---|---|
| #680 — smoke hit the wrong public host (SPA catch-all) | The gate dials **in-cluster service DNS** (`bff-preview:8085`, `cognition-canary:50051`), never a public host. |
| #682 — smoke raced rolling convergence (mixed-version pods) | Analysis runs as a **Rollout step** against the **preview/canary** ReplicaSet only, after its pods are Ready — never against a load-balanced public endpoint mid-converge. |
| #691 — PAT rejected on the BFF | The auth check uses the **`class="service_account"` JWT** (verifies on the BFF via JWKS, no DB). See `docs/auth/service-account-jwt.md`. |
| firewall-coupled, untested shell | The gate is a **declarative, tested k8s artifact** running inside the mesh. |

## Files

| Path | What |
|---|---|
| `install/` | Pinned Argo Rollouts controller (v1.7.2) + `argo-rollouts` ns. Additive. |
| `analysis/deploy-gate.yaml` | The gate: `/readyz` schema assertion (#657) + authenticated `service_account` query (#691) + an optional Prometheus SLO template (error-rate / p95 / active-stream drop via the #616 counter). |
| `bff-rollout.yaml` | BFF **blue/green** Rollout (adopts `bff` Deployment via `workloadRef`) + `bff-preview` Service; `prePromotionAnalysis` = `deploy-gate`. |
| `cognition-canary.yaml` | Engine **canary** Rollout exemplar (25→50→100 with background analysis). Same shape for voice/agent/planner/workbench. |

## Install the controller (additive, safe)

```bash
kubectl apply -k deploy/rollouts/install
kubectl -n argo-rollouts rollout status deploy/argo-rollouts
# Or add an app-of-apps entry so Argo CD manages it (deploy/argocd/apps/).
```

Installing the controller + CRDs does **not** touch the running Deployments.
Converting a Deployment to a Rollout (below) is the supervised step.

## Gate client image

`analysis/deploy-gate.yaml` runs `acrmemql.azurecr.io/deploy-gate:<digest>`, a
small image bundling `curl` + the WS/gRPC client + a `deploy-gate-check`
entrypoint that performs the authenticated query. Build it in CI (digest-pinned
like every artifact) from the useful logic migrated out of
`scripts/deploy/staging-smoke-test.sh` (the `/readyz` probe + `deep_authenticated_query`).
The `/readyz` leg already works with stock `curl`; only the authenticated-query
leg needs this image. (Image build wiring is a small follow-up task on #701.)

## Provisioning the gate JWT (#691)

The gate authenticates with a short-lived `class="service_account"` JWT
delivered as the `deploy-gate-jwt` Secret:

```bash
TOKEN="$(kubectl -n memql exec deploy/identity -- \
          memql service-account-token mint --label deploy-gate-staging)"
kubectl -n memql create secret generic deploy-gate-jwt \
  --from-literal=MEMQL_SVC_JWT="$TOKEN" --dry-run=client -o yaml | kubectl apply -f -
```

Wire an identity-side `CronJob` to re-mint on the 1h TTL cadence. Full design:
`docs/auth/service-account-jwt.md`.

## Convert BFF → blue/green (SUPERVISED cutover)

This is traffic-bearing. Do it in a watched window with a **sustained WS client
held across the cutover** (the zero-dropped-streams proof):

```bash
# 0. Controller installed; deploy-gate-jwt Secret present; gate image pushed.
# 1. Hold a sustained authenticated WS client against app.staging.copresent.ai.
# 2. Apply the Rollout + preview Service (adopts the bff Deployment):
kubectl apply -f deploy/rollouts/bff-rollout.yaml
# 3. The controller brings up the new color as PREVIEW and runs deploy-gate
#    against bff-preview. On green, promote (autoPromotionEnabled=false here):
kubectl argo rollouts promote bff -n memql
# 4. New logins flip to the new color; the old color keeps serving open streams
#    until they drain (scaleDownDelaySeconds=3600 + #615), then scales down.
kubectl argo rollouts get rollout bff -n memql --watch
# Assert: the held WS client never dropped; new logins hit the new color.
# Rollback (if needed): kubectl argo rollouts abort bff -n memql
```

Once proven, set `autoPromotionEnabled: true` to let a green gate promote
automatically (and, under Argo CD, manage these manifests via the app-of-apps).

## Convert an engine node → canary (SUPERVISED)

```bash
kubectl apply -f deploy/rollouts/cognition-canary.yaml
kubectl argo rollouts get rollout cognition -n memql --watch
# 25%→gate→50%→gate→100%; a failed AnalysisRun auto-aborts to stable.
```

## Zero-dropped-streams definition of done (#701 / #697)

A held `MemqlService.Stream` (browser WS) survives a full BFF cutover with **0
dropped streams**: new logins land on the new color, the pre-existing session
stays served by the old color until it closes, `activeStreams` winds to 0, and
the old color scales down only after drain. The blue/green strategy +
`scaleDownDelaySeconds` + the #615 graceful drain provide this; the held-client
assertion above is the proof.
