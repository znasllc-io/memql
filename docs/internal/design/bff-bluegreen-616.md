---
title: Blue/green BFF cutover (#616) -- DESIGN
audience: internal
status: historical
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Blue/green BFF cutover (#616) -- DESIGN

Status: READY FOR REVIEW. Pairs with PR for #616. All 8 design questions have
proposed answers (see "Resolved decisions" below) -- each is the
implementation's current behavior and is marked "proposed; owner may override."
Intentionally NOT wired into `deploy/k8s/kustomization.yaml`: the new-color
manifest ships alongside `bff.yaml` and the cutover is a separate operator step,
so the owner approves the production-behavior change before it goes live.

## Problem

Users hold a long-lived bidirectional `MemqlService.Stream` (WS -> gRPC) to a
specific BFF pod (other nodes reached via the BFF peer mesh). On a deploy we
want: connected users finish on their CURRENT version (no disruption), and NEW
logins land on the NEW version.

## What #615 (graceful-drain) already provides -- and what it does NOT

Shipped in #615 / #552 (verified in `app/run.go`, `component/server/health.go`,
`deploy/k8s/bff.yaml`):

- On SIGTERM the pod flips `server.SetDraining(true)` so `/healthz` returns 503
  (`buildHealthResponse`), then keeps serving for `DrainDelay` (default 5s) so
  k8s removes the pod from Service endpoints before teardown.
- `bff.yaml`: `preStop: sleep 5`, `terminationGracePeriodSeconds: 60`,
  `maxUnavailable: 0` + `maxSurge: 1`.

The gap: #615 governs how a SINGLE pool's pods die cleanly DURING a rolling
update. But a rolling update still REPLACES pods on the same pool -- once the
grace period elapses, the old pod (and its still-open streams) is killed. There
is no notion of "keep the old version alive, in full, until its users
voluntarily disconnect." That is #616.

## What blue/green adds (the #616 delta)

1. TWO BFF colors run simultaneously (`bff-blue` + `bff-green`), each a full
   Deployment at full replica count.
2. NEW logins land ONLY on the ACTIVE color. Enforced by a color-pinned,
   user-facing entry Service whose selector carries `memql/color=<active>`. The
   nginx `/memql` Ingress points at this Service.
3. EXISTING streams keep hitting the OLD color until they close. A Service
   selector change does NOT tear down already-established TCP/WS connections;
   it only steers NEW connection establishment. So old-color pods keep serving
   their open streams.
4. The OLD color is removed only AFTER it drains to 0 active streams (or a
   max-drain deadline). It shrinks progressively as its pods empty (peak-bounded
   capacity, decision 7). Residual streams at the deadline take the #615 path.

## Topology (`deploy/k8s/bff-bluegreen.yaml`)

| Object | Selector | Purpose |
|---|---|---|
| `Deployment/bff-blue` | `name=bff,color=blue` | Blue color pool |
| `Deployment/bff-green` | `name=bff,color=green` | Green color pool |
| `Service/bff` (ClusterIP) | `name=bff` (COLOR-AGNOSTIC) | In-mesh `bff:50058`/`:50051`/`:8085`. Cross-node forwards must reach whichever color holds the user's stream, so it selects BOTH colors. |
| `Service/bff-active` (ClusterIP) | `name=bff,color=<active>` | USER entry behind the nginx `/memql` Ingress. The cutover anchor. |
| `Service/bff-external` (LoadBalancer) | `name=bff,color=<active>` | External entry, color-pinned. |

Key invariant: the in-mesh `bff` Service is color-agnostic (mesh forwards reach
any color); only the user-facing entry is color-pinned (new login steering).

### Required Ingress change (applied at cutover time, not in this PR)

`deploy/k8s/public-entry.yaml` currently routes `/memql -> service bff`. For
blue/green it must route `/memql -> service bff-active`. This PR deliberately
does NOT change `public-entry.yaml`: `bff-active` only exists once
`bff-bluegreen.yaml` is applied, and that manifest is out of the default
kustomization path. The owner makes the Ingress edit (one backend service name)
as part of approving the production cutover, together with applying
`bff-bluegreen.yaml`. Decision 6 keeps `bff-external` (color-pinned) as well.

## "New login -> new color" enforcement

A browser establishes a WS to `app.staging.copresent.ai/memql`, which nginx
proxies to `Service/bff-active`. Because `bff-active` selects only the active
color, the NEW WS connection is balanced only onto active-color pods. Existing
browsers already hold an open WS to a specific old-color pod IP; that connection
is unaffected by the selector flip and continues until the browser disconnects
(logout, tab close, network drop -> the SPA's #615 auto-reconnect then lands on
the new color).

## Drain detection (the new primitive in this PR)

#616 needs to know when an old-color pod has 0 open user streams. This PR adds:

- `component/server/health.go`: `StreamOpened()` / `StreamClosed()` /
  `ActiveStreams()` on an `atomic.Int64`.
- `component/grpc/server.go`: the `MemqlService.Stream` handler increments on
  open and decrements in its deferred close.
- `/healthz` now always serializes `activeStreams` (incl. 0) so a watcher reads
  an explicit count.

The cutover script polls each old-color pod's `/healthz` (via `kubectl exec`,
localhost) and sums `activeStreams` until 0 or the deadline.

## Cutover sequence (`scripts/deploy/bff-bluegreen-cutover.sh`)

Invocation (operator step, decision 2/8 -- NOT folded into `aks-deploy.sh`):

```
scripts/deploy/bff-bluegreen-cutover.sh --to=<blue|green> [--version=X.Y.Z] \
    [--max-drain=SECS] [--no-progressive-teardown] [--no-teardown] [--dry-run]
```

1. Bring up NEW color (set image to new version, wait Ready). Abort before the
   flip if it isn't Ready.
2. Flip `bff-active` + `bff-external` selector `color: OLD -> NEW`. From here new
   logins land on NEW; existing streams stay on OLD-color pods.
3. Drain (progressive, default): poll each OLD pod's `/healthz activeStreams`
   until 0 or `--max-drain` (default 1h). As individual OLD pods reach 0, scale
   the OLD Deployment down to the count of pods that still hold streams. This
   bounds peak capacity -- the OLD color shrinks as users disconnect rather than
   sitting at a sustained 2x for the whole window (decision 7; relies on the
   #614 autoscaler for the brief surge). `--no-progressive-teardown` holds OLD
   at full replicas for instant rollback during a watched first cutover.
4. Scale OLD color to 0 (unless `--no-teardown`). Residual streams take #615.

## Rollback

- Pre-teardown: re-run with `--to=<OLD>` -- both colors are still up, so the
  flip back is instant; new logins return to OLD and NEW drains instead. With
  progressive teardown, OLD may have shrunk; a late rollback scales it back up
  first. For a fully-watched first cutover, use `--no-progressive-teardown` so
  OLD stays at full replicas and rollback is always instant.
- Post-teardown: bring the prior color back up at its prior image, then flip.

## Resolved decisions

All proposed; the owner may override any. Each is the implementation's current
behavior.

1. **Scope -- BFF-only.** Mesh nodes (cognition/agent/planner) are stateless
   behind the BFF and roll normally once it drains; voice/LiveKit room drain is
   a separate story. The BFF is the connection anchor, so it is the only color
   that needs blue/green.
2. **Switchover trigger -- operator-driven manual flip.** An operator runs the
   cutover script. A clear hook is left to later automate after a green deep
   smoke; automation is NOT wired into `aks-deploy.sh` yet.
3. **Drain SLA -- `--max-drain` default 1h, configurable.** Past the ceiling the
   OLD color is torn down and residual streams take the #615 graceful path.
4. **Drain definition -- user-facing `MemqlService.Stream` (browser) only.**
   Node (mesh) and Worker streams are infra (NodeService / WorkerService), not
   user logins; VoiceAgent lives on voice nodes. Only the browser login anchor
   is counted for the BFF color drain.
5. **Topology -- two explicit color Deployments.** A kustomize base + per-color
   overlay is DRYer but hides the color in patches; two explicit Deployments are
   chosen for review clarity. An overlay refactor can come later.
6. **Public entry -- nginx Ingress -> `bff-active`; keep `bff-external`.** The
   external LB stays color-pinned to the active color and flips with
   `bff-active` in the cutover.
7. **Capacity -- the #614 autoscaler (LIVE, min 2 / max 5 on B2s) covers the
   surge; the cutover scales the new color up and tears the old color down
   progressively** so peak is bounded (no sustained 2x).
8. **Cutover packaging -- separate operator step**
   (`scripts/deploy/bff-bluegreen-cutover.sh`); NOT folded into `aks-deploy.sh`
   yet. Invocation documented above.

## Staging-proof plan

- Apply `bff-bluegreen.yaml` + Ingress change to staging with both colors up.
- Open N authenticated WS sessions against the active color; record pod IPs.
- Run the cutover to the other color at a new image.
- Assert: (a) new logins hit the NEW color, (b) the pre-existing N sessions stay
  served by OLD-color pods (no dropped streams) until closed, (c) `activeStreams`
  on OLD pods winds down to 0, (d) OLD color shrinks progressively as pods empty
  and tears down only after drain, (e) peak combined replica count stays bounded
  (no sustained 2x) and the #614 autoscaler absorbs the brief surge.
- Metric: dropped-vs-resumed streams across the cutover (target: 0 dropped for
  sessions that stayed connected); peak node count during the window.
