# The Fleet -- cockpit machines, a routing policy, and workbenches that can be found

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project G of nine)
**Owner:** `component/worker`, `integrations/agent/worker`, `integrations/workbench`, `component/node` (the forward), `dsl/worker`, `dsl/workbench`, `clients/portal`

Sub-project G of the 2026-08-22 backlog brief. A portal section, named
**Fleet** by the owner, for the places a user's work can run: the machines
running MemQL Cockpit that they own, and the cluster's workbenches -- with a
routing policy that decides which machine gets a piece of work, and the
cross-node plumbing without which a fleet page would list machines the
engine cannot reach. Sub-project H (delegating to locally installed apps
over MCP) builds on this record.

---

## 1. What exists, and where it stops

At `948de3de`:

- **The machine record is richer than its use.** `v1:worker:registration`
  (`dsl/worker/concepts.memql:53-75`) carries `capabilities`, a
  `capabilityDescriptor` (platform, display server, per-action availability),
  `labels` ("free-form key=value tags ... for routing among the user's own
  workers"), `concurrency`, `platformInfo`, `version`, `buildTag`,
  `lastSeenAt`, `lastConnectedFromIP`, and revocation fields. `labels` are
  copied verbatim from the cockpit's `Register` message
  (`component/worker/server.go:227`) and **overwritten on every reconnect**
  (`:257`); `lastSeenAt` is written on a 60-second flush
  (`handleHeartbeat`, `:431-465`; `HeartbeatBatchInterval`, `worker.go:35`)
  and **read by no decision path**.
- **Selection is first-fit, and the label matcher is never fed.**
  `Registry.PickWorker(owner, capability, labels)` (`registry.go:198-216`)
  returns the first online registration in connection order that has the
  capability and matches the labels; `MatchesLabels` (`:237-251`) is an
  exact-match AND; the single caller passes `nil`
  (`integrations/agent/worker/dispatch.go:350-359`). The agent-facing builtins
  (`dsl/worker/builtins.memql:9-38`) have no way to name a machine or a
  requirement. `worker_busy` and `worker_disconnected` are terminal
  (`dispatch.go:166-184`); nothing re-picks.
- **A worker is reachable from one replica.** The registry is in-memory and
  per agent node, never rehydrated from the rows (the query doc at
  `dsl/worker/queries.memql:156-159` claims otherwise; no such code exists).
  `component/node/node.proto` forwards `WorkbenchForward*`, `AiForward*` and
  `DeployControlForward*` -- nothing for workers. With two agent replicas, the
  default topology, a turn served on the replica that does not hold the
  stream finds no worker.
- **The workbench has the mirror-image bug.** `pickWorkbenchPeer`
  (`integrations/workbench/forward_router.go:175-191`) is any-fit, and a
  plan's workspace is a directory on the chosen replica's disk
  (`MEMQL_WORKBENCH_ROOT/{planId}`), so two workbench replicas give one plan
  two workspaces and no one is told.
- **Workbench-versus-machine is decided by prose.** No Go chooses between
  `workbenchHost` and `workerHost`; the `workbench` knowledge domain
  (`integrations/knowledge/seed.go:1521-1541`) tells the agent to prefer the
  workbench and, at `workbench:failureFallback`, forbids switching to the
  user's machine without the consent card
  (`requestComputerUseScope` -> a plan in `awaitingFeedback` -> the user's
  Allow dispatches a fresh turn). That ruling stands.
- **Consent is layered and runs before the pick**: per-task approval, the
  kill switch, standing scope (`agentAuthorization.computerUseScope`), the
  classifier (`dispatch.go:216-347`).
- **There is no portal page** for machines or workbenches; both render in
  the generic concept browser. `CreateWorkerTokenMsg` / `RevokeWorkerTokenMsg`,
  the pairing-code routes, `workersForUser`, `revokeWorker` exist; **no
  rename, no delete**, no routing surface of any kind.
- **The cost model has a precedent worth copying**: `v1:router:*`
  (`dsl/router/concepts.memql`) -- a persisted policy, a virtual catalog, a
  chain with fallbacks, a per-call ledger.

---

## 2. Decisions

### D1 -- The section is called Fleet

Chosen over Compute (accurate, cold for the page that shows your laptop) and
Runtimes (reads as language runtimes). Machines and workbenches are the
places your work runs; a fleet is the set of things that carry work for you.

### D2 -- The fleet is per user; admins see all

Every registration is owned by one user and dispatch admits only that
user's sessions (`concepts.memql:50-54`); the composite tier from
sub-project B keeps that while letting cluster owners list everything. An
org-wide shared pool is a different sharing model and is out of scope.

### D3 -- Operator labels live in their own field

`operatorLabels` is set from the portal and never touched by reconnect;
`labels` stays the cockpit's. Matching uses the merge with operator
precedence. Two fields, one rule, no merge ambiguity.

### D4 -- The agent expresses needs; the policy picks the machine

The dispatch builtins gain `requireLabels` / `preferLabels`; there is no
`workerId` argument. A per-user `routingPolicy` chooses the strategy. A
hallucinated machine id is a failure mode this design cannot have.

### D5 -- Re-pick only before side effects; a mid-call loss is a failure

`worker_busy` and a stream gone before the call starts re-pick the next
candidate under `fallback=nextMatching`. A disconnect during an `exec` is
reported as a failure: the command may have run.

### D6 -- Consent is unchanged, and the card says "any matching machine"

The gates run before the pick as today. The consent card names the
requirements and the current choice, so the user's Allow covers the task on
any of their machines that match, and the card says so.

### D7 -- The workbench stays first; the fleet is where it cannot

A typed `environment_mismatch` from the workbench routes the call to the
fleet only when the plan already holds standing scope for that level of
work; otherwise the consent card is raised. Routing decides where among
consented machines; it never manufactures consent.

### D8 -- Cross-node dispatch is a forward, like the workbench's

`WorkerForwardRequest / Response / Cancel` over `NodeService.Stream`,
mirroring `WorkbenchForward*`, with `connectedNodeId` on the registration
telling the dispatcher where to forward. Chosen over pinning worker streams
to one replica (a new role or session affinity -- a topology change) and
over scoping the feature to single-replica clusters (the multi-node default
is non-negotiable).

### D9 -- A workspace knows its replica

`v1:workbench:workspace.nodeId`, honoured by `pickWorkbenchPeer`, closes the
two-workspaces-per-plan split.

---

## 3. The machine record and the policy

### 3.1 `v1:worker:registration`, extended

| Field | Change |
|---|---|
| `operatorLabels` | new, object; portal-set; survives reconnect; merged over `labels` with precedence |
| `connectedNodeId` | new, string; the agent replica holding the stream; stamped on register and every heartbeat flush; cleared on disconnect |
| `displayName` | new, string; `renameWorker` mutation; `name` stays the cockpit's hostname default |
| `lastSelectedAt` | new, datetime; stamped on every successful pick |
| `activeCount` | new, int; reported on heartbeat (per-capability active calls) for `leastLoaded` |
| tier | `@rowAuthz(owner="ownerUserId", clusterOwner)` (memql#4312) |

`lastSeenAt` flushes every 15 s instead of 60 (one row write per heartbeat
is cheap; staleness was the reason nothing read it). **Online** is derived:
`lastSeenAt` within twice the heartbeat interval and not revoked -- the same
answer on every replica and in the portal.

New mutations: `renameWorker`, `setWorkerOperatorLabels`,
`touchWorkerSelected`. New query: `workersForOwnerWithStatus` (the derived
online flag, connected node, merged labels) for the router and the page.

### 3.2 `v1:worker:routingPolicy`

| Field | Type |
|---|---|
| `ownerUserId` | string! |
| `strategy` | enum(`firstFit`, `roundRobin`, `leastLoaded`, `labelMatch`)! |
| `requireLabels` | object (AND-ed with the agent's) |
| `preferLabels` | object (ordering hint: candidates matching more preferred labels first) |
| `fallback` | enum(`none`, `nextMatching`)! |
| `active` | bool! |

One active policy per user; absent means `firstFit` + `nextMatching`, the
safest default (today's behaviour plus a re-pick). Composite tier. The
portal edits it; agents never do.

### 3.3 `v1:worker:invocation`, extended

`routing` object: `policyId`, `strategy`, `candidatesConsidered`,
`attempts`, `selectedBy` (`policy` | `reroute` | `only_candidate`),
`reroutedFrom` (`workbench` | `worker:<id>`). `outcome` gains `rerouted`.

---

## 4. The router

```
Pick(ctx, owner, capability, req{require, prefer}):
  policy   := active routingPolicy for owner, or the default
  cands    := workersForOwnerWithStatus(owner)
              filtered: online, has capability, merged labels satisfy
              policy.requireLabels ∪ req.require
  order    := by strategy
              firstFit    -> registration order
              roundRobin  -> oldest lastSelectedAt first
              leastLoaded -> lowest activeCount / concurrency[capability]
              labelMatch  -> most prefer-label hits first, then registration order
  for cand in order:
      if cand.connectedNodeId == self: local dispatch
      else:                            WorkerForward to cand.connectedNodeId
      on worker_busy | disconnected-before-start and policy.fallback == nextMatching: continue
      stamp lastSelectedAt; record routing on the invocation; return
  record no_worker_available with the candidates considered
```

`Registry.PickWorker` is deleted; the in-memory registry keeps its role as
the stream table and the concurrency valve (`Acquire` / `Release`), which
are local facts about local streams. The builtins
`agentworkerDispatchHost` / `agentworkerDispatchComputer` gain
`requireLabels` and `preferLabels`; the `Request` struct carries them.

---

## 5. Cross-node dispatch

`component/node/node.proto` gains `WorkerForwardRequest` (the dispatch
request, owner, capability, correlation, timeout), `WorkerForwardResponse`
(result or typed refusal), `WorkerForwardStream` (forwarded `ToolStream`
chunks) and `WorkerForwardCancel`, mirroring the `WorkbenchForward*` trio
and its request-id parking in `forward_router.go:94-170`. The receiving
replica runs the local dispatch exactly as if the turn were its own (the
gates ran on the sender; the receiver verifies the forwarded authority the
way `DeployControlForward` does). Routing rules registered for the four
messages. A cluster e2e test: the machine connected to replica 1, the turn
on replica 2, result and stream chunks back.

---

## 6. Workbench first

### 6.1 The environment hint and the typed mismatch

`workbenchHost` gains an optional `environment { os, needs: [] }` hint
(`needs` from a closed list: `display`, `gpu`, `macos_tooling`,
`user_files`). `integrations/workbench/integration.go:161`
`handleDispatchHost` compares it with what the workbench is (Linux,
headless, no user files) and returns a typed `environment_mismatch`
result naming the unmet needs instead of running and failing obscurely.

### 6.2 Reroute under existing consent, or ask

The tool loop, on `environment_mismatch`: if the plan holds standing scope
at or above the level the needs imply (`observe` for `user_files` reads,
`interact` / `full` per the existing scope table) and the task is approved,
dispatch to the fleet through the router with `requireLabels` derived from
the needs (`os=darwin`, `display=true`, ...), recording
`reroutedFrom: workbench`; otherwise raise the consent card through
`requestComputerUseScope` exactly as the knowledge corpus prescribes
(`seed.go:1540`), with the card text from D6. The corpus text is updated to
describe the automatic path and to keep its prohibition on silent
switching -- quoted in the Go that implements it.

### 6.3 Workbench replicas

`v1:workbench:workspace` gains `nodeId` (set on provision) and
`ownerUserId` (from the plan's `requestedBy`; composite tier);
`pickWorkbenchPeer` prefers the workspace's node when one exists and any
healthy peer otherwise; a plan whose node is gone gets a fresh workspace
with the old one marked `releasedReason: node_lost`. A cluster e2e test
with two workbench replicas.

---

## 7. The portal

A **Fleet** rail group; feature directory `src/fleet/`; tabs as sub-routes
(`/fleet/machines`, `/fleet/workbenches`), the `/admin/*` pattern.

**Machines**: the caller's registrations (cluster owners: all, with an owner
column) -- display name, online dot (derived), OS / arch / display server,
capabilities, merged labels with the operator labels editable through the
`LabelChips` primitive (#4299), concurrency and current load, last seen,
connected replica; rename; revoke (the existing mutation) behind
`ConfirmDialog`; **Add a machine** -- the existing pairing-code flow (the
code, the install one-liner, the wait for the registration to appear, live
through the subscription); the **routing policy** editor (strategy,
require / prefer labels, fallback); per-machine recent invocations with the
routing record.

**Workbenches**: workbench nodes (`v1:cluster:node` where
`nodeType=workbench`: health, last seen, labels, capacity) and workspaces
(plan, status, node, last used, released reason; release for cluster owners).

---

## 8. Security posture

| Concern | Handling |
|---|---|
| A user's machine receiving another user's work | unchanged: dispatch admits only the owner's sessions; the forward carries the verified authority |
| An agent naming a machine | impossible: needs only, no id |
| Operator labels steering work somewhere unexpected | labels narrow, never widen; the card names the match |
| Silent switch to the user's machine | D7: reroute only under standing scope + approval, else the card |
| Re-running a side-effectful command | D5: never after start |
| A revoked machine still selected | `online` requires not-revoked; the forward refuses a revoked registration |

---

## 9. Testing

1. Router: each strategy's order on a fixture fleet; require/prefer merge;
   operator precedence; `nextMatching` on busy and on pre-start disconnect;
   no re-pick after start; `no_worker_available` records candidates.
2. Registration: operator labels survive a reconnect; rename; `online`
   flips after two missed heartbeats; `connectedNodeId` follows the stream.
3. Forward: the cluster e2e of section 5; cancel propagates; a revoked
   registration is refused on the receiver.
4. Workbench: the hint round-trips; each unmet need yields
   `environment_mismatch`; reroute happens with standing scope and raises
   the card without; the card text names the requirements; two workbench
   replicas keep one workspace per plan.
5. Portal: both pages; pairing shows the new machine live; the policy
   editor round-trips; the repo-root guards pass.

---

## 10. Delivery

| PR | Contains | Depends on |
|---|---|---|
| 1 -- the fleet engine | registration fields + mutations + tier, routing policy, the router, the invocation record, the forward | B's composite tier (#4312) |
| 2 -- workbench first | the environment hint and mismatch, the reroute under consent, workspace node affinity + owner | PR 1 (the router) |
| 3 -- the pages | the Fleet group and both pages, docs | PR 1 |

One `Closes #N` line per issue.

---

## 11. Out of scope

- An org-wide shared machine pool (D2).
- Delegating to locally installed apps (Claude Code, Codex) -- sub-project H.
- Rehydrating the in-memory registry from rows (the derived `online` makes
  it unnecessary; the registry is the stream table).
- Per-task or per-agent routing policies (one per user now; the
  `invocation.routing` record is what a finer policy would be tuned on).
- The product SPA's `?panel=workers` (another repository).

---

## 12. References

- Code: `dsl/worker/*.memql`, `dsl/workbench/*.memql`, `component/worker/{server,registry,store,worker}.go`,
  `integrations/agent/worker/{dispatch,integration,scope}.go`,
  `integrations/workbench/{integration,forward_router}.go`,
  `component/node/{node.proto,routing.go,peer.go}`, `component/planner/executor.go`,
  `integrations/knowledge/seed.go:1521-1541`, `dsl/router/concepts.memql`,
  `clients/portal/src/{admin,artifacts}/` (the patterns the pages copy).
- Docs: `docs/public/operate/workers-runbook.md`, `workbench-runbook.md`,
  `docs/internal/ops/workbench-production.md`, CLAUDE.md "Workers" and
  "Workbench".
- Related: epic #4308 (B, the composite tier), epic #4288 (`LabelChips`),
  memql#1448 / #1412 / #1388 (the cross-node bug class this closes for
  workers).
