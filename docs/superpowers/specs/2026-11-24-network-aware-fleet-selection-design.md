# Fleet pipeline steps prefer machines whose connection holds

- **Date:** 2026-11-24
- **Status:** approved by the owner on 2026-11-24. Four forks were put to the
  owner and answered in the 2026-11-24 brainstorm (D1 to D3, and D10 after a
  builder's read of the first draft). The rest are the builder's calls, marked
  as such, and were approved with this record. Corrected during the build, after
  the final review, on two points, each with its reason below: D6's evidence of
  work in flight (section 2) and the template's `connection` (section 7). The
  maintained account is "Fleet steps prefer machines whose connection holds" in
  `docs/internal/ops/harness-decisions.md`.
- **What it is:** a fleet pipeline step goes to a machine whose connection to
  the cluster has not dropped recently, and waits for one that is busy running
  work. A machine whose connection dropped is used only when no steady machine
  could take the step. A pool step only prefers a steady machine when one is
  free. The step's log and its failure say why, and Fleet shows each machine's
  recent drops.
- **Why now:** on 2026-11-24 the three macOS lanes of a push run on main failed
  twice with `pipeline_node_lost`. The one Mac that allows pipeline steps was
  on weak 2.4 GHz Wi-Fi (about -80 dBm), and its Cockpit logged a keepalive
  timeout eight times in an hour and a half. The cluster chose that Mac with no
  regard for its link. The owner asked for the pipelines to tell a bad
  connection from a good one and choose accordingly.
- **Builds on:** the fleet retries and failover design
  ([2026-10-28-pipeline-fleet-first-retries-failover-design.md](2026-10-28-pipeline-fleet-first-retries-failover-design.md)),
  whose maintained account is "Pipeline step attempts, retries and failover" in
  `docs/internal/ops/harness-decisions.md`.
- **Repositories:** `memql` only. No Cockpit change, no wire change.

## 1. Where it stands (read 2026-11-24)

- **One drop fails a lane outright.** When a machine's stream ends, Cockpit
  stops every tool call in flight (`cockpit/internal/worker/loop.go`, the
  stream-loss branch of `runStream`), so a running step dies. The fleet
  executor maps the dispatcher's `worker_disconnected` to `pipeline_node_lost`
  (`integrations/pipelinesteps/fleet.go` `classify`), an unknown outcome,
  retried only for a `repeatable` step (`component/pipelinerun/attempts.go`
  `decideNext`). `memql-package.yaml` has 15 fleet steps and no `repeatable`
  key, so a drop ends the step.
- **Nothing keeps a drop history.** The cluster keeps one round-trip sample per
  stream (`rttMs` / `rttAt`, observation fields on `v1:worker:registration`),
  documented as "Not a routing input". A Ping that goes unanswered is
  superseded, not counted (`component/worker/server.go` `sendPing`). The
  `worker_disconnected` audit event carries no code or reason. Cockpit's
  `worker_reconnects_total` counts failed dials only, and its `Register`
  carries nothing about how the previous stream ended.
- **Round trip would choose the wrong Mac.** At 20:18Z the Mac that kept
  dropping read 299 ms and the other Mac 360 ms (the Linux machines 160 and
  173 ms). The difference between them was lost connections, not latency.
- **The cluster's own reasons do not separate a lost link from a deliberate
  reconnect.** Cockpit closes its own stream on purpose to re-advertise
  models, a pipeline repository policy or a workbench change, and does so only
  when idle (`Runner.busy`, `loop.go`). Both kinds of end log on the cluster as
  `context canceled` or `superseded`. Of the Mac's eight lost links on
  2026-11-24, the cluster logged one as `context canceled` with a call in
  flight (18:06Z, the one that failed the lanes), six as `superseded`, and one
  as `context canceled` on an idle stream (19:19Z). Its five deliberate
  reconnects logged as `context canceled` or `client_eof`, and one end that
  neither side asked for (both saw EOF, 19:52Z) as `client_eof`.
- **How often machines drop.** Agent logs from 2026-11-14 to 2026-11-24, both
  replicas: the Linux machines end a stream about once or twice a day; the Mac
  two to six times on most days and fifteen on 2026-11-24; another Mac on the
  cluster about 36 times a day, nearly always back within a minute, cause
  unknowable from the cluster's side.
- **Placement.** `integrations/agent/worker/router.go` `Plan` filters the
  owner's machines and orders them by the owner's strategy (`firstFit`, the
  registration order, by default). `dispatch.go` `Dispatch` narrows the plan
  for a pipeline step (repository, action contract, native platform, Host
  commands, then the retry driver's pin and exclusions) and walks the
  candidates, moving on only after a refusal before start. When every
  candidate refused, an ordinarily busy machine's refusal
  (`pipeline_capacity_busy`) is what comes back, and the fleet executor waits
  and asks again (`integrations/pipelinesteps/fleet_capacity.go`, polling every
  5 to 30 s, a first attempt bounded only by the run's ceiling).
- **Why a machine was chosen** is only on `v1:worker:invocation.routing`. The
  step's `binding.attempts[]` names the machine and not why. The run page lists
  attempts only when a step had two or more; a single failed attempt is shown
  as the step's failure notice.

## 2. Decisions

### D1 -- A steady machine that is busy is waited for (owner)

An unsteady machine takes a step only when no steady machine could take it at
all. A steady machine that is connected and refused only because it is busy is
waited for, the way steps already wait for capacity. With one eligible
machine, the step still runs there and says why. This is the answer for
`placement: fleet` steps; D6 says which refusals count as busy, D10 is the
answer for pool steps, and D11 says how a bounded retry wait ends.

Alternatives put to the owner: start on the free unsteady machine as soon as
the steady ones are busy (fastest when the link holds, fails as today when it
does not); avoid unsteady machines entirely, holding the step even when the
only eligible machine is unsteady (strongest protection, but it stalls runs
while that machine's Wi-Fi stays weak, which on 2026-11-24 lasted over 90
minutes). Reason for the choice: a drop fails the step outright, so waiting
for a busy steady machine usually finishes sooner than a re-run.

### D2 -- The evidence is the cluster's own record (owner)

A drop is counted from what the replica holding the stream sees when it ends
(section 3): a connection replaced by a new one before the cluster saw it end,
or one that ended with work in flight. Neither can be a deliberate Cockpit
reconnect, which happens only on an idle stream after the old connection is
closed. On 2026-11-24 this counts seven of the Mac's eight lost links,
including the 18:06Z one that failed the lanes, and none of its deliberate
reconnects. It misses a lost link on an idle machine that the cluster noticed
before the machine reconnected.

Alternatives put to the owner: Cockpit reports on each reconnect how its last
connection ended (exact, all eight, but it needs a Cockpit release and every
machine updated); Cockpit also reports Wi-Fi band and signal strength (sees a
weak link before it drops; the most to build, and a new kind of data leaving
the machine). Reason for the choice: it ships with the next engine release,
measures every machine at once, and catches the drops that matter.

### D3 -- One record per drop (owner)

The replica that held the stream writes one small, append-only record per
drop: a new concept, `v1:worker:connectionDrop` (section 4). Placement and
Fleet read the same records. Records expire after seven days.

Alternatives put to the owner: a short list of recent drops on the machine's
own registration row (one read, no new concept, but the drop is written at the
moment the replacing connection registers and writes the same row, so a
read-merge can lose it unless both writes take a lock); counting the existing
`worker_disconnected` audit events after adding the cause to them (no new
storage, but it ties placement to the security audit trail). Reason for the
choice: one writer per drop, so no drop is ever overwritten, and Fleet's
history is exactly what placement used.

### D4 -- Unsteady means a drop within the last two hours (builder's call)

A machine is unsteady while it has at least one drop in the window, two hours
by default. The window is a setting of the installation's compute placement
policy (section 7), so changing it needs no release; zero turns the
preference off. Reason: on 2026-11-24 the Mac's drops came 1 to 56 minutes
apart for an hour and a half, and a lane needs about 40 unbroken minutes; a
two-hour window covers the whole burst. A Linux machine that drops once a day
is passed over for about two hours of it, which costs a fleet step a wait only
when the other machine is busy, and costs a pool step nothing (D10).

### D5 -- Among unsteady machines, fewest drops first, then the oldest last drop (builder's call)

When no steady machine can take the step, the unsteady candidates are tried in
order of fewest drops in the window, then of the oldest most recent drop, then
in the owner's strategy order. Steady candidates keep the owner's strategy
order exactly.

### D6 -- Only a steady machine busy with running work is waited for (builder's call)

The wait in D1 is for a steady machine that refused with
`pipeline_capacity_busy` while it has work the cluster admitted in flight: its
registration's `activeCount`, this replica's live count when it holds the
stream, or a RUNNING pipeline-step `v1:worker:activity` row. *Corrected during
the build:* the first draft named only the first two, and the final review
found that the row's count is flushed only every heartbeat batch (up to about
30 s), so a run's fleet lanes, refused busy seconds after the first took the
steady machine's slot on the other replica, were not held. The replica sending
a call writes the activity row before the call leaves, so it does not lag. Cockpit answers `pipeline_capacity_busy` for five different
things (`cockpit/internal/worker/tools/pipeline_step.go`): all slots taken, a
reconciliation under way, free disk below `min_free_disk_mib` (an invalid
setting included), and memory or CPU headroom, before and after the clone.
Only the first two clear by themselves when work finishes, and work in flight
is what tells them apart without a Cockpit change: a machine short of disk or
memory with nothing of ours running will not free up by waiting, so it is
passed over at once, as today. Waiting on every busy refusal instead would let
one steady machine with a full disk hold every step a free machine could have
run, until the run's ceiling.

A steady machine that refuses for any other reason (its runtime is missing, an
operator's fence holds it with `pipeline_capacity_fenced`, its policy refuses
the repository) could not take the step as things stand, so the unsteady
candidates are tried after it. A steady machine that is disconnected is not
waited for while a connected one can start the step, which is what the
dispatcher already does with offline machines (`WaitForConnection` is set only
when no connected candidate exists).

### D7 -- Pipeline steps only; round trip decides nothing (builder's call)

Only the pipeline purpose reads drops. An agent's computer call keeps the
owner's routing strategy unchanged: it is short, it moves on after a refusal,
and waiting for a steadier machine is not what an agent's turn wants. Round
trip stays on display and stays out of placement (section 1).

### D8 -- A pin is not changed, but its record notes the connection (builder's call)

A same-machine retry the driver pinned has one candidate, and its machine's
connection does not change where it runs. The drops are still read and
recorded on the result, so a node loss on that retry says the machine was
unsteady. Exclusions (a failover to a machine not yet tried) are applied
first, as today, and the connection order is applied to what is left.

### D9 -- No cluster-side fault is filed against a machine (builder's call)

A stream ended by the cluster's own decision is never a drop: a drain
(`server_drain`), a revocation, an expired token. A replica that dies does not
run its close path and writes nothing. One cluster-side cause remains
indistinguishable (section 12): the front door closing every stream at once.

### D10 -- A pool step only orders; it never waits or goes to the cloud for a drop (owner)

A `placement: pool` step (about 40 Linux container steps in
`memql-package.yaml`, which the installation runs fleet-first with a 60-second
fleet wait and the cloud as the fallback) takes a steady machine first when one
is free. When the steady machines are busy, a free unsteady machine takes it at
once, exactly as today. A drop never makes a pool step wait or go to the cloud.

Alternative put to the owner: pool steps wait for the steady machine within the
60-second fleet wait and then go to the cloud, so a machine that dropped
recently is never used while the cloud can take the step (fewer lost steps,
more cloud spend for about two hours after each drop). Reason for the choice:
the Linux machines drop about once a day each, and the cloud is already their
fallback when the fleet is full.

### D11 -- A bounded wait ends on the free machine, not on "nothing ran" (builder's call)

An attempt the retry driver placed on the fleet waits at most its own bound
(`run.MaxWaitSeconds`, for example the longest retry delay). When that bound
runs out while a steady machine was being waited for and an unsteady one was
passed over, the fleet executor dispatches once more in the same attempt with
the preference off, so the unsteady machine is tried before the attempt
answers "nothing ran, not tried here again" (`waitRanOut`). Without this, the
wait would mark the busy machine not retried, the driver would count the fleet
exhausted (`exhausting`, `attempts.go`), and a failover that today runs on the
free machine would end the step. `waitRanOut` and `exhausting` are unchanged.
A fleet step's first attempt has no bound but the run's ceiling, so D1 holds
there as stated.

## 3. What counts as a drop

The replica holding a machine's stream decides, in `streamSession.close`
(`component/worker/server.go`), from what it alone knows at that moment:

| The stream ended | Drop? | Recorded cause |
|---|---|---|
| Replaced by a new connection for the same registration, on this replica or another (`drainReason == superseded`: `Registry.Add` or the registration watcher) | yes | `replaced` |
| With work in flight and no cluster decision behind it (`drainReason` empty, and a tool call, app session, app sign-in, model call or model pull was live) | yes | `workInFlight` |
| By the cluster's decision (`server_drain`, `revoked`, `token_expired`) | no | |
| Idle, with no cluster decision behind it | no | |

**Why "work in flight" proves it was not deliberate.** Cockpit re-registers on
purpose only when `Runner.busy()` is false, and `busy()` covers exactly tool
calls, app sessions, app sign-ins, model calls and model pulls. The set counted
here is the same five as the session tracks them: pending tool calls,
`sessions`, the open app sign-ins, `modelCalls` and `modelPulls`. Model probes are left out on
purpose: Cockpit's `busy()` does not cover them, so a deliberate reconnect can
happen during one. If Cockpit's guard ever changes, this set must change with
it; a comment beside each names the other.

**Why "replaced" proves it was not deliberate.** A deliberate reconnect closes
the old connection first and dials again after at least the one-second
backoff, so on a working link the cluster sees the old stream end before the
new one registers. A replacement means the old connection's end never reached
the cluster: the link failed, or the machine slept for less than the keepalive
window. Both are fair evidence against starting a 40-minute step there.

The decision is a pure function (`classifyStreamEnd(drainReason, inFlight)`,
`component/worker/connection_drop.go`) so it is tested without a stream, and
the "worker disconnected" log line gains a `drop` field naming the cause, so a
reader can match the log to the record.

## 4. The record

A new concept in `dsl/worker/concepts.memql`:

```memql
@rowAuthz(owner="ownerUserId", clusterOwner)
@retention(ttl="7d", keepLatest=false, archive="none")
@displayCard(primary="cause", secondary="registrationId", tertiary="droppedAt")
@belongsTo(cluster)
concept connectionDrop {
  ownerUserId     string!   @serverSet
  registrationId  string!
  droppedAt       datetime!
  connectedAt     datetime
  cause           enum("replaced", "workInFlight")!
  workInFlight    int
  nodeId          string
  code            string
  reason          string
}
```

Each field gets a `@description` in the file's style. `registrationId` is the
bare id, `droppedAt` the replica's clock when the stream ended, `connectedAt`
when that stream was admitted (`Worker.ConnectedAt`), `workInFlight` the count
of live work, `nodeId` the replica, and `code` / `reason` the values the
"worker disconnected" log line carries.

- **Writer:** `component/worker`'s store, new method `RecordConnectionDrop`,
  rendering a new mutation `recordWorkerConnectionDrop` (an `insert`). It runs
  under `auth.ContextWithUserActor` for the registration's owner, as every
  registration write in that store does, on a detached context with a
  five-second timeout (the session's context is already cancelled; see the
  disconnect audit in `close`). The mutation is not `@serverOnly`, for the
  reason `clearWorkerConnectedNode` gives: component/worker is not on the
  internal-origin allowlist and should not be, and the owner tier is the gate.
  The residue is that a signed-in person could add drop records for their own
  machine, which only changes where their own steps run.
- **Optional interface.** The method lives on a small interface
  (`ConnectionDropRecorder`) the session finds by type assertion on its store,
  so the many test fakes of the worker store keep compiling. `EngineStore`
  asserts at compile time that it implements it. A store without it records
  nothing, which leaves placement as it is today.
- **Reader:** a new query `myWorkerConnectionDrops(registrationId, since)` in
  `dsl/worker/queries.memql`: one of the caller's machines' drops at or after
  `since`, newest first, at most 100 (a machine past the cap is unsteady
  either way, and its count reads "100 or more"), `@cache(0)` so a drop another
  replica wrote is seen at once, as `myWorkersWithStatus` is. One read per
  machine rather than one owner-wide read, so a machine that drops in a loop
  cannot push another machine's drops past a shared cap and make it read
  steady. It is the owned tier, so `test/dslconformance` classifies it.
  `fleetcatalog.EngineStore` gains `ConnectionDrops(ctx, owner, registrationId,
  since)`, reading it under the owner's actor exactly as `WorkersForOwner`
  does. Placement reads it once per candidate, which is a handful of reads per
  dispatch.
- **No routing rule.** Nothing subscribes to these rows' graph events: Fleet
  reads them on load and on its existing refresh, placement reads them per
  dispatch. So no broadcast rule is added.
- **Volume.** At most one row per stream end that counts. A machine on very
  bad Wi-Fi writes tens a day; retention removes them after a week.
- **A new concept** needs `make concept-snapshot`, and its `@belongsTo` is
  `cluster`, like every concept in the worker namespace.

## 5. Placement

All in the pipeline branch of `Dispatcher.Dispatch`, after every existing
narrowing:

0. **The request.** `worker.Request` gains `Connection ConnectionPreference`:
   `Window` (how long a drop makes a machine unsteady) and `WaitForSteady`
   (D1). The fleet executor sets it from the run's policy: `Window` for every
   pipeline step's attempt, `WaitForSteady` only for a `placement: fleet` step
   (D10), and neither when the policy's window is zero.
1. **Read.** When `Window` is set and the store implements the reader
   (optional interface, as above), read each candidate's drops since
   `now - Window`. A read that fails is logged, recorded on the routing record
   as `connection: unavailable` for that machine, and that machine counts as
   steady: the preference is never a reason to fail or hold a step.
2. **Order.** `RoutePlan.ApplyConnection(drops, now, window)` gives each
   candidate its health (drops in the window, most recent drop) and orders the
   candidates: steady first in the existing order, then unsteady by D5. It
   stores the health per registration on the plan, so the record and the
   result can say it. A plan whose machines have no drops is left exactly as
   it was, which is what keeps every existing routing test valid.
3. **Walk.** With `WaitForSteady`, the candidate loop gains one rule: on
   reaching the first unsteady candidate, if a steady candidate earlier in the
   walk refused with `pipeline_capacity_busy` while it had work in flight
   (D6), stop and return that refusal (it is the existing `capacity` result:
   refused before start, naming the busy machine). The fleet executor's
   capacity loop then waits and asks again, as it does for any busy refusal,
   and the next plan reads the drops again, so a steady machine that drops
   while the step waits stops being waited for. Otherwise, and always without
   `WaitForSteady`, the walk goes on into the unsteady candidates, as today.
4. **The bound.** When a placed attempt's `MaxWaitSeconds` runs out after a
   refusal that passed an unsteady machine over, the executor dispatches once
   more with `WaitForSteady` off before it answers `waitRanOut` (D11).
5. **Pins and exclusions** behave as D8 says.
6. **Purpose.** `purposeRefusal` refuses `Connection` on every purpose but
   the pipeline's, as it does a pin, so nothing an agent's request carries can
   set it. The builtins never read it.

Nothing crosses the mesh that did not before. The drops are rows, so the
replica planning the step reads what the replica that held the dropped stream
wrote; a forward to a sibling carries the same request as today, and the
sibling still re-checks the machine's consent and contract.

## 6. What the step, the record and Fleet say

- **Result.** `worker.Result` gains `Connection` (the attempted machine's
  health when drops were read: count, most recent drop, window) and
  `PassedOver` (the unsteady machines not tried because a steady one was busy,
  each with its owner-chosen name and health). Both are stamped by the
  dispatching replica from the plan, never read off a forward's reply, as
  `MachineName` already is.
- **Routing record.** `RoutingRecord` gains `connection` (per registration:
  `steady`, `2 drops, last 2026-11-24T19:44:56Z`, or `unavailable`) and
  `passedOver` (registration ids). They land on `v1:worker:invocation.routing`,
  which Fleet's machine activity already shows.
- **Step log.** The fleet executor writes, with `capture.Notice`:
  - while waiting, once per change in who is passed over: "memql: waiting for
    Box A, whose connection is steady; Box B is free, but its connection
    dropped 2 times in the last 2 hours (last at 19:44 UTC)";
  - when the step starts on an unsteady machine: "memql: Box B's connection
    dropped 2 times in the last 2 hours (last at 19:44 UTC); no machine with a
    steady connection could take this step now".
- **Failure.** When an attempt on an unsteady machine ends
  `pipeline_node_lost`, `classify` appends: "Its connection had dropped 2
  times in the 2 hours before the step started." That sentence is the step's
  failure, so the run page shows it in the failure notice of a single-attempt
  step and in the attempt line of a retried one, with no change to the OS run
  page.
- **Names.** Every sentence above names a machine only by the display name its
  owner chose, or `pipelines.MachineWords` when it has none: these sentences
  can reach a GitHub check run, public for a public repository
  (`checkRunName`, `machine_words.go`).
- **Fleet.** The machine's Details > Connection section gains one fact,
  "Connection drops": "None in the last day", or "3 in the last day, last at
  12:44" (local time, exact timestamp in the title). It reads
  `myWorkerConnectionDrops` for the last 24 hours on the Details page's
  existing refresh. Fleet shows facts, not the placement verdict, because the
  window belongs to each run's policy. The section's help gains two sentences:
  what a drop is (the definition in section 3, in plain words) and that
  pipeline steps wait for a machine without a recent drop when one can take
  them. No new advisory, badge or list. This follows `clients/os/DESIGN.md`
  (Details and settings content) and is checked against the memql-ui-design
  skill when built.

## 7. The policy setting

`pipelineComputePlacementPolicy` (`dsl/pipelines/automations.memql`) gains an
optional `connection` block, `connection: {unsteadyMinutes: 120}`. *Corrected
during the build:* the portable template does NOT write it, though the first
draft did. A run captures the template's source and a re-run reuses the capture,
and an engine older than the setting refuses an unknown key, so a template that
wrote it would make every run opened on this release unrunnable again after a
rollback. Absence means 120, so the behaviour is the same; the template's comment
names the setting.

- `pipelines.ComputePlacementPolicy` gains `Connection ConnectionPolicy`
  (`json:"connection"`), with `DefaultConnectionPolicy()` returning 120.
- `parseComputePlacement` reads `connection` as strictly as `retries`: absent
  means the default (so every policy installed before it keeps working and
  gets the preference); present must be an object whose only member is
  `unsteadyMinutes`, an integer 0 to 1440, and a null or another key is a
  refusal naming the field. Zero turns the preference off.
- A run whose snapshot has no `pipelineComputePlacementPolicy` phase (an
  installation that never wrote one) gets the default too:
  `selectComputePlacement`'s early return sets `Connection` beside the
  default retries.
- The policy is captured per run and bound into each command step's recovery
  definition, as today. That binding already includes the engine revision, so
  a run in flight across the upgrade is affected no more than by any upgrade.
- The fleet executor turns `req.ComputePlacement.Connection.UnsteadyMinutes`
  into `Request.Connection.Window`, and sets `WaitForSteady` for a
  `placement: fleet` step only (section 5). A request with no policy (a test,
  or a caller that built a step by hand) carries no window and reads no drops.

## 8. Which node holds what

| State | Where | Who needs it |
|---|---|---|
| The stream, its pending calls, its `drainReason` | the agent replica holding it | that replica, at close, to decide and write the drop |
| Drop records | `v1:worker:connectionDrop` rows | any replica planning a step; Fleet |
| The window, and whether to wait for a steady machine | the run's captured policy and the step's placement | the agent replica executing the step, which copies them into the request |
| Health per candidate, passed-over machines | the dispatching replica's plan | the result and the routing record, written there |

A drop written by replica B while the machine reconnects to replica A is read
by whichever replica plans the next step. No in-memory state is shared or
forwarded.

## 9. Testing

Each test fails against today's code.

- `component/worker`: a table test of `classifyStreamEnd` with the
  2026-11-24 cases (replaced; work in flight; idle deliberate close; drain,
  revocation and expiry with and without work in flight; a probe alone is not
  work in flight). A session test that a superseded close and a close with a
  pending call each write one record under the owner's actor with the right
  cause and replica, and that an idle close and a drained one write none.
- `dsl/worker` + `component/worker` db test (`MEMQL_REQUIRE_DB=1`): the
  mutation and query round-trip under the owner; another owner reads zero
  rows; the `since` and `registrationId` arguments filter.
- `integrations/agent/worker` (agent tag), using the existing hop harness
  (`siblingThenLocal`, `recordingHop`):
  - a steady machine is tried before an unsteady one that comes first in
    registration order;
  - a steady machine refusing busy and an unsteady free one: the result is
    the busy refusal, refused before start, naming the busy machine; the
    unsteady machine is never dispatched to; `PassedOver` names it; with the
    busy steady machine held by the sibling replica and the drops written by
    "the other replica" through the shared store;
  - a steady machine refusing for a reason other than busy (runtime missing,
    fenced), or refusing busy with no work in flight (disk or memory): the
    unsteady one runs the step;
  - without `WaitForSteady` (a pool step), a steady machine refusing busy
    with work in flight: the unsteady one runs the step;
  - all unsteady: fewest drops first, then the oldest last drop;
  - a pin is not reordered and its result carries the health;
  - a failed drop read leaves the owner's order and records `unavailable`;
  - no drops, and a window of zero: identical plans and results to today's;
  - `Connection` on a non-pipeline purpose is refused.
- `component/pipelinerun`: `parseComputePlacement` accepts an absent block
  (default), a valid one and zero, and refuses null, a negative, 1441, a
  string and an extra key, each naming the field; the shipped template's
  answer parses.
- `integrations/pipelinesteps` (agent tag): the window reaches the request,
  with `WaitForSteady` for a fleet step and not for a pool step; the waiting
  notice names the passed-over machine once; a placed attempt whose bound runs
  out while an unsteady machine was passed over runs on that machine instead
  of answering `waitRanOut`; the start notice on an unsteady machine; the
  node-lost failure carries the sentence only when the machine was unsteady.
- `component/pipelinerun`: a run with no policy phase gets the default
  connection window.
- `clients/os`: the Fleet fact's words for none, one and several drops, and
  that the page reads drops for that machine only.

Then `make test`, `MEMQL_REQUIRE_DB=1` on the db-gated trees touched
(`component/worker`, `component/pipelinerun`), the agent-tagged packages, and
the go-checks commands before pushing.

## 10. Documentation

- `docs/internal/ops/harness-decisions.md`: a new section, inserted directly
  after "Pipeline step attempts, retries and failover" (not appended at the
  end), in that section's shape: trigger, evidence, chosen path, alternatives,
  stop conditions, invariants, what survives a replica change, known
  limitations, not verified, regression tests.
- `docs/public/operate/pipelines.md`, "Retries and failover": how a step picks
  among machines by connection, the policy setting, and what the log says.
- `docs/public/operate/workers-runbook.md`: what a connection drop is, where
  it is recorded, and that round trip is still not a routing input.
- `cockpit/docs/pipelines.md`: one paragraph that a step needs an unbroken
  connection for its whole run, that a dropped connection fails it, and that
  a machine with recent drops is used last.
- `docs/internal/design/feature-notes.md`, workers: one bullet.
- The `rttMs` description in `dsl/worker/concepts.memql` stays true and is not
  changed.

## 11. Rollout

The engine release carries everything. Drops start being recorded when the new
engine runs on the agent replicas; placement starts reading them at the same
time, so for the first two hours after the upgrade every machine reads steady,
which is today's behaviour. No migration: a new concept needs none. No
installation change: an installed policy without `connection` gets the
default. The owner can turn it off by installing a policy with
`connection: {unsteadyMinutes: 0}`.

## 12. Risks and accepted limits

- **A lost link on an idle machine the cluster noticed first is not counted**
  (one of eight on 2026-11-24). Drops come in bursts, so the burst is still
  seen. Cockpit's own report (the alternative to D2) closes this.
- **The front door closing every stream at once** (an ingress restart) would
  record a drop for every machine that had work in flight at that moment. It
  is rare and expires with the window. Not addressed.
- **A machine that slept for less than the keepalive window** reads as
  replaced. Accepted: it was not reachable either.
- **With one eligible machine** nothing is avoided; the step only says why.
  The owner's separate fix (a second Mac allowing pipeline steps) is what gives
  the preference a choice.
- **A busy steady machine is waited for until the run's ceiling** on a first
  attempt (D1), the bound every capacity wait already has, but only while it
  has work in flight (D6). A refusal for low disk or memory that happens to
  come while an agent's short command runs on that machine is waited out for
  one more poll, then passed over.
- **Rolling the engine back below this release** is safe while no installed
  policy writes `connection`: an older engine's parser refuses an unknown key,
  so an installation should add `connection` to its installed policy only once
  no such rollback is planned. The portable template leaves the key out for the
  same reason (section 7).
- **Not verified until deployed:** the classification on real streams in the
  cluster, and the seven-of-eight figure beyond one afternoon of one Mac.

## 13. Follow-ups (not in this change)

- Let a running step survive a brief reconnect, which removes the damage a
  drop does instead of avoiding the machine.
- Cockpit reports how each connection ended (exact drops, the D2 alternative),
  and optionally Wi-Fi band and signal.
- Mark the fleet steps that are harmless to repeat as `repeatable`, so a node
  loss fails over to another machine (D11 of the 2026-10-28 design).
