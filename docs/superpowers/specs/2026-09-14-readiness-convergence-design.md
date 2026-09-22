# Readiness convergence: a failed evaluation is unknown, a lagging row is stale, every node converges -- Design

- **Date:** 2026-09-14
- **Status:** shipped in the PR that closes memql#5259 and memql#5252. It carries
  the rulings D1, D2 and D7 of epic memql#5316 (the owner-approved plan in PR
  #5352) and replaces that epic's D3 with S1 below, for the reason S1 gives.
- **Owner areas:** `component/memql/readiness` and
  `clients/os/src/system/readinessFold.ts` (the fold and its mirror),
  `component/memql` (evaluator, writer, recompute loop), `dsl/platform` (the
  concept), `component/database` (the connector), `app/run.go` (boot),
  `clients/os` (the surfaces that say why).
- **Depends on:** 2026-09-06-configuration-readiness (the rows and the fold),
  2026-09-07-core-gate-and-honest-install D3 and D5 (inference from rows;
  event-driven recompute). Amends D5 of the latter.

---

## 1. Problem

Two reports of one failure, four days apart.

**2026-09-09 (memql#5259).** After a worker was configured and a real Ask
request succeeded, the `ai` verdict stayed `partial`: of fourteen live node
rows, seven recomputed to `configured` (two agents, two planners, two
workbenches, one shared bff) and seven kept their boot answer, `unconfigured`
-- the other shared bff, both product bffs, both edges and both identity nodes,
with `reportedAt` from 09:03-09:14 or 15:09. Those were STALE rows, not fresh
evaluations of the new registration.

**2026-09-13 (memql#5316).** A rolling deploy saturated the database; every
node's boot evaluation of `ai` failed its fleet read, the evaluator wrote the
failure as `unconfigured`, the thirty-second re-write failed too, and the pods
that never hear a mesh event kept the failed verdict until the next deploy. The
desk read "Inference: Partly set up" while inference worked.

## 2. Why, established

Three independent defects.

1. **"Could not ask" and "no door" were one row.** `evaluateModule` mapped a
   failed registration read to `unconfigured` and the writer persisted it; the
   integration arm did the same with `notApplicable` on a failed probe.
2. **A broadcast does not reach every node, and nothing else recomputed.** The
   recompute subscriber listened only to the local event bus, on the premise
   (2026-09-07 D5) that the three registration verbs reach every replica. They
   do not:
   - forwarding goes only along OUTBOUND connections
     (`PeerManager.sendTarget`), plus a relay of at most three hops along each
     receiver's own outbound dials; nothing pushes server->client, so a node
     nobody dials -- the edge, a product bff -- hears nothing;
   - identity is excluded from every broadcast by design
     (`meshEventParticipants`);
   - a peer whose connection is absent or reconnecting at that moment is
     skipped and nothing queues the event.

   The delivery evidence for the five replicas memql#5259 could not explain
   with the identity filter is written down in
   `component/node/readiness_mesh_hop_test.go`: in the cloud's dial topology,
   with every child's parent dial landing on one bff pod, a registration event
   published on the agent reaches the agent, that bff and the workbench, and
   none of the other shared bff, the product bff, the edge, identity, a peer
   behind an absent transport, or one mid-reconnect. The production logs on
   2026-09-13 say the same (the product bff and edge logged 6 `event trigger
   fired` since boot, against 430-1084 on bff, agent and planner).
3. **A lagging row was folded as a vote.** `ai` is evaluated from cluster rows,
   so two nodes that disagree about its fleet doors read the rows at different
   moments -- and "any disagreement is `partial`" turned every such lag into a
   user-facing "Partly set up".

## 3. Decisions

### D1 -- A failed evaluation is `unknown`, and never overwrites a known row

From memql#5316, as the plan in PR #5352 specified it.

- `evaluateModule` answers `readiness.Unknown` with a closed-vocabulary
  `Reason` (`fleetReadFailed`, `integrationProbeFailed`) when a resolver the
  verdict depends on errored. The error text stays in the WARN line; a row is
  broadcast and never carries it. An integration that is not registered stays
  `notApplicable`.
- The fold sets `unknown` aside: no vote, named in `Verdict.Unknown` (sorted,
  never null); a module whose every live row is unknown is `unreported`, which
  opens the core gate rather than holding it on a read that broke.
- `persistReadiness` writes `unknown` only where nothing known stands. "Known
  before" is this process's memory of a known verdict, OR a known standing row,
  OR a standing read that FAILED -- a row we could not see may be the correct
  answer unknown would replace. (The plan used the in-process memory alone; the
  standing read exists now, since write-on-change landed in PR #5315, and it is
  what makes the rule hold across a container restart under the same pod name.)
- A pass with any unknown module returns `*ReadinessUnknownError`; the known
  modules in the same pass are still written. `readinessRecompute` answers the
  unknown modules in its result rather than failing the call.
- The enum gains `unknown` and the concept gains `reason`, additively.

### D2 -- The recompute loop retries, and a safety net bounds every row's lag

From memql#5316, as planned, on the loop that write-on-change already made cheap.

- A failed or unknown pass re-runs on an exponential, jittered backoff
  (`ReadinessRetryBase` 2 s doubling to `ReadinessRetryMax` 60 s, each step
  jittered to [step/2, step]) until one pass lands. A `Notify` during the
  backoff collapses into the retry.
- A pass runs every `ReadinessSafetyNetInterval` (10 min, +-20%) whether or not
  anything arrived. It costs one cluster read per node per period and writes
  only what changed (`readinessRewriteNeeded`). It is the bound on how long any
  node's row can lag: identity and every undialed node converge within one
  period without a restart.
- Boot hands its write to the loop after a 0-5 s jitter; the 30 s re-write goes
  through the same loop. A pass is logged at INFO when it wrote a row or ended a
  failure streak, and at Debug when it found nothing changed.

### S1 -- A row whose cluster-scoped lanes lag the freshest row is stale, not a vote

This REPLACES memql#5316's D3 (one `ai--cluster` row written by whichever node
evaluated last).

- A lane computed from cluster rows alone carries `scope: "cluster"` -- today
  the two fleet doors of the inference module. An unmarked lane is node-scoped.
- The fold takes the freshest report that carries cluster-scoped lanes as the
  reference; a report whose cluster-scoped completeness differs from it read the
  cluster before something changed, and is SET ASIDE and named in
  `Verdict.Stale`. When the freshest moment holds reports that disagree among
  themselves, recency cannot choose and nothing is set aside. A report with no
  cluster-scoped lane is never judged, which is every module but inference and
  every row written before lanes carried a scope.
- Only completeness is compared. A lane's `live` slot is presence -- a laptop
  that slept between two evaluations -- not a fact about the cluster.

**Why not one cluster row.** The inference module is not a pure function of
cluster rows: its federation lane reads the node's in-process provider registry,
and a replica deployed without the federation configuration (a product bff that
does not mount the configmaps; any node that missed a providers reload, which
rides the same mesh) answers "no federated door" while its siblings answer
"configured". With one row written by whoever evaluated last, that replica would
overwrite its siblings' answer on each of its passes -- flipping the verdict,
and in a cluster whose only door is federation, holding the core gate from one
misconfigured replica. With per-node rows and S1, that difference stays what it
is: a real per-node difference, folded to `partial` and named. And nothing is
lost that D3 wanted: rollout skew, a failed evaluation and an island node --
the three causes D3 listed for `ai` disagreement -- are set aside by S1 and D1.

**Why the verdict does not wait for the transport.** The node that wrote a
registration always hears its own event on its local bus, so the freshest row
reflects every change within the debounce; S1 decides the verdict from it, and
D2 bounds how long the other rows lag. memql#5338 (server-to-client push) makes
the lag shorter; nothing here depends on it.

### D5' -- The surfaces say why, quietly

The card-and-gate half of memql#5316's D5, in the shell's existing vocabulary.

- `partial` has two causes and two names: nodes that differ, some set up, read
  **Set up on some nodes**; a lane every node agrees is half-filled keeps
  **Partly set up**. `unreported` has two: nobody answered is **Not reported**;
  nodes answered and none could finish is **Could not check**.
- A stale node changes no word anywhere: the verdict reads as it would without
  it. The Set up group and the Modules list carry the nodes behind a verdict in
  a title, never as raw `nodeId=state` pairs in the row; the Modules list adds
  ONE quiet count chip when the fold set nodes aside.
- The Cluster app's Modules detail opens the fold up: every live node's own
  answer and freshness, then -- in muted ink, listed and not counted -- the nodes
  catching up (with the time they last checked and "it re-checks on its own")
  and the ones that could not check (with the reason).
- The core gate, while it holds, adds one quiet line when some nodes could not
  read the fleet: the answer is not every node's.

### D7 -- The connector retries a dial timeout, inside a budget

From memql#5316, as planned: `isRetryableConnectError` is 53300 OR a dial or
handshake i/o timeout, retried with full jitter inside a 15 s wall-clock budget
that a context deadline always beats.

### Left open, deliberately

- memql#5325's stopped-node deletion and purge migration: a stopped node's
  rows already drop out of the fold through the liveness window, and
  write-on-change (PR #5315) removed the churn. **Closed on 2026-09-21 --
  section 6 below.** The reason above held and still holds; what it did not
  cover is the rows, which nothing had ever removed.
- memql#5338, the transport: S1 and D2 make readiness correct without it.

## 4. Failure modes

- **A node that hears nothing** -- identity, an edge, a product bff, a peer
  mid-reconnect: its row lags until its safety-net pass (at most 12 min); the
  verdict is decided from the freshest row meanwhile, and the Modules detail
  names it as catching up.
- **A saturated database at boot:** the fleet read fails, the node's pass is
  unknown, a fresh pod writes `unknown` with the reason and a pod with a known
  row keeps it, and the loop retries at 2 s, 4 s ... 60 s until one pass lands.
- **Every node fails at once:** stale-known rows stand; with no known row at
  all the verdict is `unreported` ("Could not check") and the gate opens.
- **Clock skew between nodes:** recency compares row times to the second, and
  the debounce separates the evaluations that straddle a change by two seconds;
  skew below that cannot reorder them. A tie is not a reference.
- **A replica genuinely different** (federation on some nodes only): no lane it
  differs on is cluster-scoped, so it is never stale; the verdict is `partial`,
  read as "Set up on some nodes", with the nodes named.

## 5. Testing

- The shared fold fixtures 10-15 (unknown, the memql#5259 staleness, a
  node-scoped lane that still disagrees, a revocation, a tie, all three
  together), run by `fold_test.go` and `readinessFold.test.ts`.
- `readiness_convergence_db_test.go`: two nodes on one database through the
  real evaluator, mutation, standing read and fold -- setup and revocation seen
  by one node, the other converging on its own pass, a failed read never
  overwriting a known row; and the production loop converging a node that hears
  nothing through a refused first write.
- `component/node/readiness_mesh_hop_test.go`: the real bus, peer manager,
  event bridge and recompute loop on nine replicas in the cloud's dial
  topology, with an absent transport, a reconnecting one and a failed write --
  every replica converges; identity never hears the broadcast; the island is
  written down.
- Evaluator, writer and loop unit tests; the OS vocabulary, the Modules detail,
  the gate's line and `reportFromRow`'s `unknown` default.

---

## 6. Addendum, 2026-09-21: D4's second half (memql#5325)

Section 3 left this open with a reason that was true and is still true: "a
stopped node's rows already drop out of the fold through the liveness window,
and write-on-change (PR #5315) removed the churn." Nothing below contradicts
that. What it closes is the half neither of those touched -- the rows
themselves, which no code in the tree had ever removed.

### What was actually wrong

`v1:platform:moduleReadiness` had no retention of any kind. Every pod that has
ever booted kept its seven rows forever, and before write-on-change every
registration heartbeat appended seven more per hearing node -- 772k versions
for a few dozen live ids on a production instance (2026-09-13). The fold could
not see any of it and no query filtered on it, so it was invisible from every
screen and from every read: a table nobody pruned.

### A4 -- The rows go, and the fold's liveness window stays where it is

- **`TopologyReconciler.retire` purges the node it has just recorded stopped**
  (`component/node/readiness_row_purge.go`), under the bare `MEMQL_NODE_ID` a
  readiness row's `nodeId` carries. It is the fast path, it runs on the one
  replica holding the reconcile lease, and it infers nothing -- the node was
  declared gone by the statement above it.
- **A ten-minute sweep takes the rest**: every node whose LATEST
  `v1:cluster:node` version says `stopped`. That is what collects the
  30-minute prune cron's retirements (the backstop this repo already had for
  whatever the reconciler missed) and every row an engine before this one left
  behind. Ten minutes is `readinessRewriteFloor`, the longest a live node's own
  row may go unrestated.
- **A node with NO cluster-node row is left alone.** "No row" is also what a
  pod looks like between its first readiness pass and its registration, and
  deleting on absence would race a booting node.
- **The liveness window in `Fold` is unchanged and is still the verdict's
  answer.** These deletes are hygiene; they are not what makes a stopped node
  stop counting, and nothing about the verdict now depends on a delete having
  run.

### A5 -- No delete mutation, because the DSL has none

D4 asked for a `@serverOnly deleteModuleReadinessForNode(nodeId)`. That cannot
be authored: `component/language/parser/body_clauses.go` accepts `insert` and
`update` and nothing else, and the DSL's word for "gone" is a soft-delete
field. The engine's existing answer for a retention delete is a statement
against the node table -- `component/identity/authactivity/prune.go` and
`component/node/delivery_store_pg.go` -- and this follows it, including its two
stated consequences: every version goes, not only the latest, and nothing is
notified.

### A6 -- The deleted routing rule is NOT taken, and the reason is in code

D4 asked for `graph.node.deleted.v1:platform:moduleReadiness`. Refused, and
recorded as a reasoned entry in `RoutingExclusions()` rather than left as an
absence:

- **It would change no word on any screen.** The liveness read excludes a
  stopped `v1:cluster:node` row outright, so a replica that never hears the
  delete folds exactly the verdict it would fold if it had. Every readiness
  surface reads the fold's verdict; none counts rows.
- **It would be the first production publisher of `events.KindNodeDeleted`**,
  which is constructed only in tests today -- new transport surface bought for
  an observable difference of zero, which is the change `routing.go`'s
  campaigns block already argues against.
- Reversible with one rule and a new reason the moment a surface counts
  readiness ROWS rather than reading the verdict.

### A7 -- The migration collapses history; it does not delete every row

D4 said "delete every row once. The next boot writes the new shape", and the
reason was D3's plan to change that shape. S1 replaced D3 and per-node rows
stayed, so there is no new shape and nothing for a rewrite to correct.
`20260921000000_module_readiness_history_collapse` therefore keeps the newest
version of each row and removes the restatements behind it. Deleting the
current verdicts too would read every module as `unreported` -- "Not reported"
on the desk, an open core gate -- from the moment the migration commits until
each node's next pass: honest, but a flicker on every installation bought for a
reason that no longer holds.

### A7b -- Measured, because the migration runs before the engine serves

772,828 readiness versions seeded onto a real TimescaleDB -- the shape the
2026-09-13 instance was in -- and then:

| | |
|---|---|
| the collapse migration | **985 ms**, 0 ids left holding a second version |
| the sweep, worst case (that volume, 3 of 6 nodes stopped) | **494 ms**, exactly the stopped nodes' rows |

The migration's number is the one that mattered. It runs at boot, ahead of the
engine serving anything, so a statement that degrades badly on a real
installation's volume is every pod blocked -- and a fresh CI database cannot
show it. In steady state the sweep runs against a few hundred rows.

### A8 -- Supervision is a counter, not a screen

`memql_readiness_rows_purged_total{path}`, `retired` against `swept`. The two
paths are labelled apart because their RATIO is the diagnosis: `swept` carrying
the whole rate while `retired` sits at zero says the fast path is not running.
Zero on both is the normal shape for a cluster that is not rolling, so there is
nothing here to alert on -- which is why this is a counter and not a surface.

### What the Modules detail says now

One sentence, because the list's meaning changed: a stopped node is not on it
at all, so it is the cluster now rather than whatever has not aged out. Beside
it, each node row carries its exact `reportedAt` on the row's title -- two
nodes that both read "3h ago" are in an order the reader cannot see, and that
order is what S1's staleness rule turns on. Judged rendered in both modes, at
the pane's own measure and at 380px, populated and with each of the fold's
empty answers; the list needed no other change, and the prose under it now
keeps a sentence's measure (DESIGN.md rule 9) rather than running the width of
a maximised window.
