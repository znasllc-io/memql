# Blue/green BFF cutover (#616) -- DESIGN DRAFT (WIP)

Status: DRAFT for owner review. Pairs with the draft PR for #616. Not wired
into `deploy/k8s/kustomization.yaml` yet; implementation is finalized only
after the open questions below are answered.

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
   max-drain deadline). Residual streams at the deadline take the #615 path.

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

### Required Ingress change (NOT yet made -- see open questions)

`deploy/k8s/public-entry.yaml` currently routes `/memql -> service bff`. For
blue/green it must route `/memql -> service bff-active`. Drafted but not applied
pending the owner's decision on whether to also keep `bff-external`.

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

1. Bring up NEW color (set image to new version, wait Ready). Abort before the
   flip if it isn't Ready.
2. Flip `bff-active` + `bff-external` selector `color: OLD -> NEW`.
3. Poll OLD color pods' `/healthz activeStreams` until 0 or `--max-drain`.
4. Scale OLD color to 0 (unless `--no-teardown`). Residual streams take #615.

## Rollback

- Pre-teardown: re-run with `--to=<OLD>` -- both colors are still up, so the
  flip back is instant; new logins return to OLD and NEW drains instead.
- Post-teardown: bring the prior color back up at its prior image, then flip.

## Open questions for the owner

1. Scope: BFF-only? The issue notes mesh nodes are stateless behind the BFF and
   can roll normally if the BFF drains. Confirm voice/LiveKit rooms are a
   separate drain story (out of scope for #616).
2. Switchover trigger: manual flip (operator runs the script) vs. automated
   after the new color passes deep smoke? Draft is manual.
3. Drain SLA / `--max-drain`: default ceiling is 1h. What is acceptable for
   long sessions before forced teardown?
4. Drain definition: count only `MemqlService.Stream`, or also VoiceAgent /
   Node / Worker streams? Draft counts only `MemqlService.Stream` (the
   user-facing anchor).
5. Topology authoring: two explicit Deployments (drafted) vs. one kustomize
   base + per-color overlay (DRYer, hides color in patches)?
6. `bff-external` LoadBalancer: keep it (color-pinned) or is the public path
   exclusively nginx Ingress -> `bff-active` in prod (making the LB redundant)?
7. Capacity: blue/green needs ~2x BFF during cutover. Confirm #614 surge
   headroom / autoscaler covers it, or scale OLD down progressively as it
   drains.
8. Integration with `aks-deploy.sh`: should the main deploy script call the
   cutover for the bff, or stay a separate operator step? Draft is separate.

## Staging-proof plan

- Apply `bff-bluegreen.yaml` + Ingress change to staging with both colors up.
- Open N authenticated WS sessions against the active color; record pod IPs.
- Run the cutover to the other color at a new image.
- Assert: (a) new logins hit the NEW color, (b) the pre-existing N sessions stay
  served by OLD-color pods (no dropped streams) until closed, (c) `activeStreams`
  on OLD pods winds down to 0, (d) OLD color tears down only after drain.
- Metric: dropped-vs-resumed streams across the cutover (target: 0 dropped for
  sessions that stayed connected).
