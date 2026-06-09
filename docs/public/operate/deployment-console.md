---
title: memQL Deployment Console -- Operator Guide
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# memQL Deployment Console -- Operator Guide

The Deployment Console is the admin/owner-only UI for driving the
[deployment-v2](../../internal/design/deployment-v2.md) machinery -- "what is
deployed, is it healthy, and how do I deploy / promote / roll back" --
from a UI instead of a terminal, for both **staging** and
**production**.

It is a **read + action surface** over the existing machinery. It does
not reimplement any of it: Git stays the single source of truth, Argo
CD reconciles each env to the digest-pinned overlay, Argo Rollouts
drives progressive delivery, and the release lockfile pins every
component by digest. This guide is the console-driven workflow; the
underlying mechanics live in the deployment-v2 docs cross-linked at the
bottom and are not duplicated here.

## The two surfaces

| Surface | Where | Use it when |
|---------|-------|-------------|
| **Identity portal -- Deployments** | `https://identity.<env>.copresent.ai/admin/deployments` | You want a full-screen, point-and-click view with confirm dialogs; you are already in the admin portal (users / sessions / audit / JWKS). |
| **Cockpit Topology** | memQL Cockpit, cluster/Topology view | You are already in the terminal-native ops console watching node health + observability overlays and want deployment state and controls inline. |

Both surfaces call the same owner/admin-gated **deploy-control API**
(memQL `DeployControlService`); neither shells out to
`kubectl` / `argocd` / `git` directly. Both show the same data and
offer the same four actions. Pick whichever you are already in.

## Owner/admin gating

Every read and every action requires the cluster role **owner** or
**admin** (the same role model as the rest of the identity admin app;
see [access-model.md](auth/access-model.md)). `writer` and `reader`
roles get nothing:

- **Portal:** `/admin/deployments` sits behind the same `requireAdmin`
  middleware as the rest of `/admin/*`. A non-admin is rejected with a
  403 and an `admin_auth_forbidden` audit event, and never sees the
  Deployments nav entry.
- **Cockpit:** the Topology view resolves your cluster role; non-admins
  see a single `Deployments: owner/admin only` line, the deploy-control
  read is never issued, and the action menu does not open.
- **API:** the deploy-control read and write RPCs independently enforce
  owner/admin server-side, so the gate holds even for a direct API
  caller -- a non-admin gets `PermissionDenied`.

## Reading the console

Both surfaces show the same per-env state. Scope it with the env
toggle (portal) or the per-env rows (cockpit).

- **Version + digests** -- the deployed release version and the
  per-component image `@sha256:` digests for the 8 components, read
  from the committed env overlay (`deploy/k8s/overlays/<env>`) and the
  matching release lockfile (`releases/<version>.yaml`:
  `engineVersion` / `validatedAt` / `gate`).
- **Argo CD** -- the `memql` Application's sync status (Synced /
  OutOfSync), health (Healthy / Progressing / Degraded), last sync, and
  drift (live-vs-desired). In the cockpit these are color-coded like
  node health (green / amber / red) with a `[drift]` indicator.
- **Rollouts** -- per Rollout: BFF blue/green active vs preview color;
  engine canary current step / set-weight; and the latest `AnalysisRun`
  result (pass / fail).
- **Gate** -- the most recent deploy-gate `AnalysisRun` legs (the
  `/readyz` schema probe, the `service_account`-JWT authenticated
  query, SLO metrics, and the headless-browser tier) with pass/fail and
  a timestamp.

Reads are not audited per call.

## Performing actions

Four actions, all owner/admin-gated, all audited, none of which bypass
Git or the reconciler:

| Action | What it does | Confirmation |
|--------|--------------|--------------|
| **Deploy to staging** | Runs the `promote.sh` digest-bump into the staging overlay for the chosen version; Argo CD then reconciles. | Version required. |
| **Promote staging to prod** | Digest-copy of a validated lockfile into the prod overlay (`promote.sh` semantics) -- no rebuild. | **Type-to-confirm** (re-enter the exact version). |
| **Roll back** | `git revert` of the env overlay commit; Argo CD reconciles back to the prior digest set. | **Type-to-confirm** (re-enter the commit SHA, or `rollback`). |
| **Rollout promote / abort** | `kubectl argo rollouts promote|abort` for a BFF/engine Rollout in the chosen env. | `abort` is **type-to-confirm**; `promote` is immediate. |

Notes that hold on both surfaces:

- **Confirmation.** Production promotion, rollback, and Rollout abort
  require an explicit type-to-confirm step. A mismatched confirmation
  is rejected and the action is never invoked.
- **Audit.** Every action (and every denied attempt) writes a
  `v1:identity:auditEvent` (category `admin`, action
  `deployment_console_<verb>`, with the actor, env, target version /
  digest / rollout, and outcome). The console surfaces the audit-event
  id back to you on success (`SUCCESS: <action> (audit <id>)`); failures
  show `ERROR: <message>`. No emojis.
- **Actions do not auto-push.** Deploy / promote / rollback operate on
  the overlay (via `promote.sh` / `git revert`) and surface the result
  for review; landing the overlay change to `main` follows the normal
  review path. Rollout promote/abort act on the live Rollout directly.

### Portal

`/admin/deployments` -> select the env -> use the action forms in the
Overview panel (deploy / promote), next to the version (rollback), and
in the Rollouts table (per-rollout promote / abort). Forms are
CSRF-protected; destructive actions render an inline confirm field.

### Cockpit

In the cluster/Topology view, press **`D`** (capital D; lowercase `d`
stays the pan key) to open the deploy-control menu. The menu walks you
through the action, env, and any required inputs / confirmation; the
result line shows `SUCCESS:` / `ERROR:` (and `ERROR: requires
owner/admin` if your role is insufficient). On success the deployment
overlay refreshes immediately so Argo / Rollouts state reflects the new
reality.

## Where audit events land

All console writes and denials append to the identity audit log
(`v1:identity:auditEvent`), visible in the portal's `/admin/audit`
view. Promotion-to-prod and rollback in particular are auditable after
the fact: actor, env, target version / digest, and outcome.

## When to drop to the terminal (break-glass)

The console is the day-to-day path. Drop to the terminal for anything
the console does not cover -- suspending Argo auto-sync, emergency
direct `kubectl` changes, lockfile assembly, or DR. Those procedures
are owned by the deployment-v2 runbooks:

- Argo CD break-glass (suspend / resume auto-sync):
  [`deploy/argocd/README.md`](../../../deploy/argocd/README.md)
- Rollouts promote / abort / watch reference:
  [`deploy/rollouts/README.md`](../../../deploy/rollouts/README.md)
- Release lockfile + promotion mechanics:
  [`releases/README.md`](../../../releases/README.md)
- Disaster recovery:
  [`docs/internal/ops/dr-runbook.md`](../../internal/ops/dr-runbook.md)

## References

- Deployment Console epic: znasllc-io/memql#724 (children #725-#729 +
  cockpit#144/#145).
- Deployment-v2 design + epic: [`docs/internal/design/deployment-v2.md`](../../internal/design/deployment-v2.md), #697.
- Supervised live cutovers: #712.
- Owner/admin role model: [`docs/public/operate/auth/access-model.md`](auth/access-model.md).
- Machine identity the gate uses: [`docs/public/operate/auth/service-account-jwt.md`](auth/service-account-jwt.md), #691.
