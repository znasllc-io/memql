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
drives progressive delivery, and a single env overlay pins the release
as `{engine version, bundle digest, client digest}`. This guide is the
console-driven workflow; the underlying mechanics live in the
deployment-v2 docs cross-linked at the bottom and are not duplicated
here.

## The surfaces

| Surface | Where | Use it when |
|---------|-------|-------------|
| **memQL portal -- Deployments** | `https://cockpit.<env>.example.com/portal/views/deployments` | You want the designed operator view: the live release beside the last gate's legs, the image digests in force, and the whole deployment history on one page (memql#3319). |
| **Identity portal -- Deployments** | `https://identity.<env>.example.com/admin/deployments` (your identity host) | You want a full-screen, point-and-click view with confirm dialogs; you are already in the admin portal (users / sessions / audit / JWKS). |
| **Cockpit Topology** | memQL Cockpit, cluster/Topology view | You are already in the terminal-native ops console watching node health + observability overlays and want deployment state and controls inline. |

Every surface calls the same role-gated **deploy-control API**
(memQL `DeployControlService`); none shells out to
`kubectl` / `argocd` / `git` directly. They show the same data and
offer the same actions. Pick whichever you are already in.

A surface may HIDE an action the caller's role cannot take -- the memQL
portal does, so an admin is not offered a rollback that would come back
`PermissionDenied`. That is a courtesy, not a control: the gate is the
service's, and it applies identically however the RPC arrived.

### Two transports, one service (memql#3311)

`DeployControlService` is a **unary** gRPC service mounted on the same
listener as `MemqlService`. A native gRPC client (the Go SDK, the
cockpit) dials it directly. A **browser cannot** -- and neither can
anything else reaching memQL through the `/memql/ws` WebSocket bridge,
which tunnels `MemqlService.Stream` and nothing else.

So every deploy RPC is **also** reachable on the stream, as a
`DeployControlMsg` envelope whose `request` oneof carries the service's
own request messages verbatim (the reply is `DeployControlResult`). The
TS SDK exposes it as `@znasllc-io/memql-sdk-core/deploy`; that is how
the VS Code extension and the memQL portal drive the console.

This is a transport, not a second implementation. The stream handler
calls the identical service methods the unary path calls, so **the role
gate and the audit write are one code path** -- a parity test in
`component/grpc` drives the real service through both transports and
fails if any gated RPC answers differently. Denials arrive on the
stream as an ordinary `QueryError` carrying the gRPC status code
verbatim, so a caller below the role floor sees `PermissionDenied` on
either transport. Actions return their audit event id identically.

Deployment **history** is deliberately not bridged: `v1:cluster:deployment`
rows are ordinary concept rows, read with a normal query.

### Reaching the API from a WebSocket client (memql#3311)

`DeployControlService` is a separate **unary** gRPC service mounted on
the same listener. That is dialable from Go (`sdk/go`'s
`DeployControlClient`) and from `grpcurl`, but **not from a browser and
not from any WebSocket client** -- so the VS Code extension and the
memQL portal, which both speak `/memql/ws`, could not reach the deploy
surface at all. (The identity portal only sidesteps this by being
server-rendered.)

Every RPC is therefore also reachable over `MemqlService.Stream` as a
single bridged envelope pair, `DeployControlMsg` /
`DeployControlResult`, whose inner `oneof` carries the
`DeployControlService` request messages verbatim. `sdk/ts` exposes it as
`DeployControlClient` (`@znasllc-io/memql-sdk-core/deploy`) with the
same nine methods as the Go client.

Two properties an operator should know:

- **The gate is the same gate**, not a second copy of it. The stream
  handler stamps the caller's identity and invokes the *same* service
  methods the unary path serves, so the role matrix below and the audit
  event hold identically on both surfaces -- and a parity test asserts a
  denied role gets `PermissionDenied` from both, for every RPC.
- **Errors ride inside the reply.** A multiplexed stream has no
  per-message status channel, so a bridged refusal comes back as
  `error_code` (the canonical gRPC code, e.g. `7` =
  `PERMISSION_DENIED`) rather than as a stream error. `sdk/ts` turns
  that back into a thrown `DeployControlError` carrying the code.

Deployment **history** is deliberately not bridged: `v1:cluster:deployment`
rows stay readable as ordinary concept rows through the normal query
surface.

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
  caller -- a non-admin gets `PermissionDenied`. This is one enforcement
  point, not one per surface: the WebSocket bridge (memql#3311) calls the
  same service methods, so it cannot admit anyone the unary service
  refuses.

## Reading the console

Both surfaces show the same per-env state. Scope it with the env
toggle (portal) or the per-env rows (cockpit).

- **Version + digests** -- the deployed release version and the
  per-component image `@sha256:` digests, read from the committed env
  overlay (`deploy/k8s/overlays/<env>` under the deploy repo,
  `MEMQL_DEPLOY_REPO_ROOT`). That one overlay pins the release directly
  as `{engine version, bundle digest, client digest}` -- there is no
  separate per-version lockfile.
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
| **Promote staging to prod** | Digest-copy of the validated `{engine, bundle, client}` digests into the prod overlay (`promote.sh` semantics) -- no rebuild. | **Type-to-confirm** (re-enter the exact version). |
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
direct `kubectl` changes, or DR. Those procedures
are owned by the deployment-v2 runbooks:

- Argo CD break-glass (suspend / resume auto-sync):
  [`deploy/argocd/README.md`](../../../deploy/argocd/README.md)
- Rollouts promote / abort / watch reference: the product carrier repo's
  `deploy/rollouts/README.md` (pack-owned since the product deploy estate
  moved out of this repo)
- Overlay digest-pin + promotion mechanics:
  [`deploy-bundle-runbook.md`](deploy-bundle-runbook.md)
- Disaster recovery:
  [`docs/internal/ops/dr-runbook.md`](../../internal/ops/dr-runbook.md)

## Automation-driven deploy path (#2115)

The gRPC `Deploy` / `RollbackDeployment` actions are **automation-driven
kick-offs**. The synchronous Go apply they once ran (select driver ->
`scripts/release/promote.sh` -> inline `in_progress -> succeeded | failed`
transition) was **retired in #2115 step 6**: it is no longer a code path, and
there is no opt-in/opt-out flag.

How a deploy now flows:

1. The deploy pack (`examples/deploypack`) is **always anchored on the
   identity binary** (`app/anchor_deploypack.go`, a bootstrap phase after
   genesis autoload and before the engine loads its DSL tree), so the
   `driveDeploymentInProgress` (E2.3) / `recordReconciledState` (E2.4)
   automations are loaded where the Deploy Console writes deployment records.
2. `Deploy` validates the record's provider and transitions it to
   `in_progress`, then returns. `RollbackDeployment` creates the new rollback
   record at `in_progress` (carrying the historical digest +
   `previousDeploymentId`), then returns.
3. The `in_progress` CDC edge triggers `driveDeploymentInProgress`, which runs
   promote through the **same Executor effects** (`scripts/release/promote.sh`)
   and owns the terminal transition.

### Async response contract

The action RPCs are **async**: `ok=true` means "accepted and kicked off", NOT
"deploy succeeded". The reply carries `details.async="true"` and
`details.status="in_progress"` (Deploy) plus `details.newDeploymentId`
(rollback). The terminal status is read back from the deployment concept (or
`GetDeploymentStatus`) once the automation resolves it -- the console already
polls deployment status, so it reflects `succeeded` / `failed` when the
rollout settles.

Terminal-status parity: an automation-driven **rollback** lands in
`rolled_back` (the `driveDeploymentInProgress` automation branches on
`previousDeploymentId`, #2168); a forward deploy lands in `succeeded` and a
promote failure in `failed`.

> The `MEMQL_DEPLOY_AUTOMATION_DRIVEN` env flag that gated this during the
> owner-supervised staging cutover (steps 1-5) is **removed** -- the path is
> unconditional. The cutover was verified live on staging on 2026-06-24;
> step 6 (this retirement) lands the cleanup. The next real staging release
> exercises the automation path end-to-end as the standing validation.

## References

- Automation-driven deploy cutover: znasllc-io/memql#2115 (Epic 2
  follow-up; meta #2060).
- Deployment Console epic: znasllc-io/memql#724 (children #725-#729 +
  cockpit#144/#145).
- Deployment-v2 design + epic: [`docs/internal/design/deployment-v2.md`](../../internal/design/deployment-v2.md), #697.
- Supervised live cutovers: #712.
- Owner/admin role model: [`docs/public/operate/auth/access-model.md`](auth/access-model.md).
- Machine identity the gate uses: [`docs/public/operate/auth/service-account-jwt.md`](auth/service-account-jwt.md), #691.
