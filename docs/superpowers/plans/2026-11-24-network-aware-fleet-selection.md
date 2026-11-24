# Network-aware fleet selection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fleet pipeline steps prefer machines whose connection to the cluster has not dropped recently, wait for a steady machine that is busy running work, and say why on the step and in Fleet.

**Architecture:** The agent replica holding a machine's stream classifies each stream end and writes one `v1:worker:connectionDrop` row per drop. The pipeline branch of the worker dispatcher reads each candidate's recent drops, orders steady machines first, and (for `placement: fleet` steps only) returns a steady busy machine's refusal instead of starting on an unsteady one, so the fleet executor's existing capacity wait applies. The window comes from the installation's captured compute placement policy.

**Tech Stack:** Go (multi-module workspace), MemQL DSL (concept, mutation, query, template), TypeScript/React (MemQL OS Fleet), vitest.

**Spec:** [docs/superpowers/specs/2026-11-24-network-aware-fleet-selection-design.md](../specs/2026-11-24-network-aware-fleet-selection-design.md). Section and decision numbers below (D1 to D11, sections 3 to 7) are the spec's.

## Global Constraints

- Test runs on this Mac stay light until the release session reports 0.31.40's release run done: one focused command at a time, each prefixed `nice -n 19 env GOMAXPROCS=4`, `-p=1`, one `-run` pattern for a db-gated test; no `make test`, package-wide or db-tree runs, `make os-test` or `make dev`.
- Name packages by MODULE PATH in `go test` (`github.com/znasllc-io/memql/...`), never `./...` (CLAUDE.md, Testing).
- Agent-only packages (`integrations/agent/worker`, `integrations/pipelinesteps`) need `-tags agent`.
- db-gated tests: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql?sslmode=disable'` against the `memql-testdb` container (localhost:5432 is the k3d load balancer, not a database).
- Every sentence that can reach a GitHub check run names a machine by `pl.MachineWords(ownerChosenName, id)` only, never the reported name or hostname.
- Window default 120 minutes; bounds 0 to 1440; zero turns the preference off (D4, section 7).
- Per-machine read cap 100 rows; a capped count reads "100 or more" (section 4).
- Drop record retention: `@retention(ttl="7d", keepLatest=false, archive="none")` (D3).
- Work in flight = pending tool calls, app sessions, open app sign-ins, model calls, model pulls; never model probes (section 3).
- No string literal "production", "prod" or "staging" in engine Go code (`environment_branching_test.go`).
- No Cockpit change, no proto change (spec header).
- Stage files by explicit path; never `git add -A`. Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Docs: no emojis; harness decisions documented beside the code and in `docs/internal/ops/harness-decisions.md`.
- This plan is deleted in the change's final merge (docs/CLAUDE.md: an executed plan is spent).

## Review Focus

1. A drop stamped by a sibling replica whose clock runs a few seconds ahead (droppedAt after the planner's now) still counts as inside the window. Test: Task 4, `TestConnectionHealthCountsADropStampedAhead`.
2. A drop recorded under the canonical registration id matches a candidate carrying the bare id, and the reverse. Test: Task 4, `TestConnectionHealthMatchesBothSpellingsOfTheId`.
3. A machine with no display name is called "machine <id prefix>" in every notice and failure sentence, never by its reported name. Test: Task 5, `TestUnsteadyNoticesNameAnUnnamedMachineByItsId`.
4. A machine at the read cap (100 drops) reads "100 or more" and is unsteady, and its count does not hide another machine's drops. Test: Task 4, `TestConnectionHealthAtTheCapIsUnsteadyAndSaysSo`.
5. A drop exactly at the window's start counts; one a nanosecond older does not. Test: Task 4, `TestConnectionHealthWindowStartIsInclusive`.
6. The production stores actually carry the new methods: the dispatcher finds its reader and the stream server its recorder only by type assertion, so a store that lacks one would turn the feature off silently. Compile-time assertions in Task 2 (`component/worker.EngineStore`) and Task 4 (`integrations/agent/worker.EngineStore`).

---

### Task 1: The policy setting

**Files:**
- Modify: `component/pipelines/contract.go` (`ComputePlacementPolicy`, near `DefaultRetryPolicy`)
- Modify: `component/pipelinerun/compute_placement.go` (`selectComputePlacement`, `parseComputePlacement`, `computePolicyShape`)
- Modify: `dsl/pipelines/automations.memql` (`pipelineComputePlacementPolicy` return and doc comment)
- Test: `component/pipelinerun/compute_placement_test.go`

**Interfaces:**
- Produces: `type ConnectionPolicy struct { UnsteadyMinutes int \`json:"unsteadyMinutes"\` }`; `const MaxUnsteadyMinutes = 1440`; `func DefaultConnectionPolicy() ConnectionPolicy` (120); `func (p ConnectionPolicy) Window() time.Duration` (minutes as a duration, zero when off); `ComputePlacementPolicy.Connection ConnectionPolicy \`json:"connection"\``.

- [ ] **Step 1: Write the failing tests** in `compute_placement_test.go`:
  - `TestConnectionPolicyParsesStrictly`: table over `parseComputePlacement(map[string]any{...})` with `fleetWaitSeconds: 0, cloudFallback: false` plus: no `connection` -> `Connection.UnsteadyMinutes == 120`; `{unsteadyMinutes: 0}` -> 0; `{unsteadyMinutes: 1440}` -> 1440; and refusals whose error contains the field name `connection.unsteadyMinutes` for `-1`, `1441`, `"120"`, `nil`, and `connection.window` for an extra key `{unsteadyMinutes: 120, window: 1}`; a `connection: nil` (JSON null) refuses naming `connection`.
  - `TestARunWithoutAPolicyPhaseGetsTheDefaultConnectionWindow`: the no-phase path of `selectComputePlacement` (follow `TestEveryRunGetsAPolicyEvenWithoutPoolSteps`) returns `Connection.UnsteadyMinutes == 120`.
  - Extend the existing shipped-template assertion (the test that parses the template's answer, or add `TestTheShippedPlacementTemplateCarriesTheConnectionWindow`) to expect 120.
- [ ] **Step 2: Run them and see them fail**
  Run: `nice -n 19 env GOMAXPROCS=4 go test -p=1 -count=1 github.com/znasllc-io/memql/component/pipelinerun -run 'TestConnectionPolicyParsesStrictly|TestARunWithoutAPolicyPhaseGetsTheDefaultConnectionWindow|TestTheShippedPlacementTemplateCarriesTheConnectionWindow'`
  Expected: FAIL (undefined `Connection` / unknown field `connection`).
- [ ] **Step 3: Implement.** Add the type and default to `contract.go` with a comment naming D4 and section 7. In `parseComputePlacement` add `Connection json.RawMessage \`json:"connection"\`` beside `Retries` and a `parseConnectionPolicy(raw json.RawMessage) (pipelines.ConnectionPolicy, error)` that decodes members one by one exactly as `parseRetryPolicy` does (unknown key refused naming it, null refused, integer 0..1440). Extend `computePolicyShape` to mention `connection`. Set `Connection: pipelines.DefaultConnectionPolicy()` in `selectComputePlacement`'s early return. Add `connection: {unsteadyMinutes: 120}` to the template's return and one sentence to its `///` comment (what the window does, zero turns it off).
- [ ] **Step 4: Run the tests again**: same command. Expected: PASS. Then the package's existing placement tests: `nice -n 19 env GOMAXPROCS=4 go test -p=1 -count=1 github.com/znasllc-io/memql/component/pipelinerun -run 'ComputePlacement|Retries|RetryPolicy'` Expected: PASS.
- [ ] **Step 5: Commit** `component/pipelines/contract.go component/pipelinerun/compute_placement.go component/pipelinerun/compute_placement_test.go dsl/pipelines/automations.memql` with "pipelines: a placement policy window for unsteady fleet machines".

### Task 2: The drop record (DSL, write, read)

**Files:**
- Modify: `dsl/worker/concepts.memql` (new `concept connectionDrop` after `registration`)
- Modify: `dsl/worker/mutations.memql` (new `@actor mutation connectionDrop recordWorkerConnectionDrop`)
- Modify: `dsl/worker/queries.memql` (new `@cache(0) @actor query connectionDrop myWorkerConnectionDrops`)
- Create: `component/worker/connection_drop.go` (types and `RecordConnectionDrop` contract)
- Modify: `component/worker/store.go` (`EngineStore.RecordConnectionDrop`)
- Modify: `component/worker/fleetcatalog/store.go` (`EngineStore.ConnectionDrops`)
- Test: `component/worker/fleetcatalog/connection_drops_db_test.go`
- Regenerate: `component/conceptfields/concept-fields.snapshot.json` (`make concept-snapshot`), `sdk/go/client`, `sdk/ts/src/client` (`make sdk-gen`)

**Interfaces:**
- Produces (component/worker):
  ```go
  const (
      DropCauseReplaced     = "replaced"
      DropCauseWorkInFlight = "workInFlight"
  )
  type ConnectionDrop struct {
      RegistrationId, OwnerUserId string
      DroppedAt, ConnectedAt      time.Time
      Cause                       string
      WorkInFlight                int
      NodeId, Code, Reason        string
  }
  type ConnectionDropRecorder interface {
      RecordConnectionDrop(ctx context.Context, drop ConnectionDrop) error
  }
  func (s *EngineStore) RecordConnectionDrop(ctx context.Context, drop ConnectionDrop) error
  ```
- Produces (fleetcatalog): `func (s *EngineStore) ConnectionDrops(ctx context.Context, ownerUserId, registrationId string, since time.Time) ([]workerservice.ConnectionDrop, error)`, newest first, at most 100.
- DSL: concept fields per spec section 4 (`ownerUserId string! @serverSet`, `registrationId string!`, `droppedAt datetime!`, `connectedAt datetime`, `cause enum("replaced", "workInFlight")!`, `workInFlight int`, `nodeId string`, `code string`, `reason string`), each with `@description`; annotations `@rowAuthz(owner="ownerUserId", clusterOwner)`, `@retention(ttl="7d", keepLatest=false, archive="none")`, `@displayCard(primary="cause", secondary="registrationId", tertiary="droppedAt")`, `@belongsTo(cluster)`; relationships `parent` on `ownerUserId` -> user and `registrationId` -> registration, as `invocation` declares. Mutation args `dropId string!`, `registrationId string!`, `droppedAt string!`, `connectedAt string`, `cause string! @enum("replaced", "workInFlight")`, `workInFlight int`, `nodeId string`, `code string`, `reason string`; `insert` stamps `ownerUserId: actor.userId`. Query args `registrationId string!`, `since string!`; `filter row => row.ownerUserId == actor.userId && row.registrationId == args.registrationId && row.droppedAt >= args.since`; `sort "droppedAt", "desc"`; `paginate 100`.

- [ ] **Step 1: Write the failing db test** `TestConnectionDropsRoundTripAcrossReplicasAndOwners` in `fleetcatalog/connection_drops_db_test.go`, following `component/worker/sharing_activity_db_test.go`'s two-engine setup (and fleetcatalog's own `main_dbschema_test.go`): engine A's `workerservice.EngineStore` records three drops for machine M of owner O (one 3 h old, two within the hour, one of them under the canonical id `v1:worker:registration:<M>`) and one for machine N; engine B's `fleetcatalog.EngineStore.ConnectionDrops(ctx, O, M, now-2h)` returns exactly the two recent drops of M, newest first, with `Cause`, `NodeId` and `DroppedAt` intact; the same call for another owner returns none; a call with the bare id and one with the canonical id return the same rows. Clean up the inserted rows by concept and id.
- [ ] **Step 2: Run it and see it fail**
  Run: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql?sslmode=disable' nice -n 19 env GOMAXPROCS=4 go test -p=1 -count=1 github.com/znasllc-io/memql/component/worker/fleetcatalog -run TestConnectionDropsRoundTripAcrossReplicasAndOwners`
  Expected: FAIL to compile (undefined `RecordConnectionDrop`, `ConnectionDrops`).
- [ ] **Step 3: Implement** the concept, mutation and query (doc comments say: what a drop is, who writes it and why it is not `@serverOnly` -- copy `clearWorkerConnectedNode`'s reason -- and why `@cache(0)`). `RecordConnectionDrop` runs under `ownerActor(ctx, drop.OwnerUserId)`, renders `recordWorkerConnectionDrop` with a fresh `dropId` (`randomHex(12)`), times as RFC3339Nano UTC, omitting `connectedAt` when zero; add `var _ ConnectionDropRecorder = (*EngineStore)(nil)`. `ConnectionDrops` follows `WorkersForOwner`'s actor handling and row helpers (`outputPayloadRows`, `rowString`, `rowTime`, `rowInt`), reading under `memql.ContextWithFreshRead`.
- [ ] **Step 4: Regenerate and run**: `nice -n 19 make concept-snapshot`, `nice -n 19 make sdk-gen`, then the Step 2 command. Expected: PASS. Then the DSL gates for a new concept: `nice -n 19 env GOMAXPROCS=4 go test -p=1 -count=1 github.com/znasllc-io/memql/test/dslconformance -run 'TestEveryShippedConceptDeclaresBelongsTo|Conformance'` and the owner-stamp gate (`grep -rn "func TestDeclaredOwnerFieldsAreServerStamped"` to find its package, then run it alone). Expected: PASS.
- [ ] **Step 5: Commit** the three DSL files, `component/worker/connection_drop.go`, `component/worker/store.go`, `component/worker/fleetcatalog/store.go`, the db test, the snapshot and the regenerated SDK files (by explicit path, from `git status --short`), with "worker: one record per dropped machine connection".

### Task 3: Recording drops at stream close

**Files:**
- Modify: `component/worker/connection_drop.go` (classification)
- Modify: `component/worker/server.go` (`streamSession.close`)
- Test: `component/worker/connection_drop_test.go`

**Interfaces:**
- Consumes: Task 2's `ConnectionDrop`, `ConnectionDropRecorder`, cause constants.
- Produces: `type WorkInFlight struct { ToolCalls, AppSessions, AppSignIns, ModelCalls, ModelPulls int }`; `func (w WorkInFlight) Total() int`; `func classifyStreamEnd(drainReason string, inFlight WorkInFlight) (cause string, drop bool)`.

- [ ] **Step 1: Write the failing tests** in `connection_drop_test.go`:
  - `TestClassifyStreamEnd`: table: `("superseded", {})` -> `replaced, true`; `("superseded", {ToolCalls: 1})` -> `replaced, true`; `("", {ToolCalls: 1})`, `("", {AppSignIns: 1})`, `("", {ModelPulls: 1})` -> `workInFlight, true`; `("", {})` -> `"", false`; `("server_drain"|"revoked"|"token_expired", {ToolCalls: 1})` -> `"", false`.
  - `TestAReplacedStreamRecordsOneDrop`: `terminationServer`-style harness (copy `stream_termination_test.go`) with a store embedding `terminationStore` that also implements `ConnectionDropRecorder` and records under a mutex; register, register again (same replica), `assertTerminatedStream(first)`; then exactly one recorded drop with `Cause == DropCauseReplaced`, `RegistrationId == "reg-1"`, `OwnerUserId == "owner"`, `NodeId == "agent-a"`, non-zero `ConnectedAt`.
  - `TestAStreamReplacedOnAnotherReplicaRecordsOneDrop`: as `TestRemoteRegistrationEventEndsAnIdleGRPCStream`; the drop is recorded by agent-a's store with `NodeId == "agent-a"`, none by agent-b's.
  - `TestAStreamLostWithWorkInFlightRecordsOneDrop`: a pending `Dispatch` on the registered worker, then the client's stream context is cancelled; one drop, `Cause == DropCauseWorkInFlight`, `WorkInFlight == 1`.
  - `TestAnIdleCloseAndADrainRecordNoDrop`: an idle `CloseSend` records none; `registry.Terminate("reg-1", DisconnectReasonRevoked)` with a pending dispatch records none.
- [ ] **Step 2: Run them and see them fail**
  Run: `nice -n 19 env GOMAXPROCS=4 go test -p=1 -count=1 github.com/znasllc-io/memql/component/worker -run 'TestClassifyStreamEnd|TestAReplacedStreamRecordsOneDrop|TestAStreamReplacedOnAnotherReplicaRecordsOneDrop|TestAStreamLostWithWorkInFlightRecordsOneDrop|TestAnIdleCloseAndADrainRecordNoDrop'`
  Expected: FAIL (undefined `classifyStreamEnd`; no drop recorded).
- [ ] **Step 3: Implement.** `classifyStreamEnd` per the spec's section 3 table, with the comment tying `WorkInFlight` to Cockpit's `Runner.busy()` (and saying model probes are left out and why). In `close`, take the five counts under the existing `s.mu` block (count `len(s.appSignIns)` there, before `failOpenAppSignIns` clears it), compute `code, reason` once, add `"drop", cause` to the "worker disconnected" fields when it is a drop, and record it on `context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)` through `s.server.store.(ConnectionDropRecorder)`, logging a failed write at Warn. `ConnectedAt` is `s.worker.ConnectedAt`, `NodeId` is `s.server.nodeId`, `DroppedAt` is `s.server.clock()`.
- [ ] **Step 4: Run the tests again**: same command. Expected: PASS. Then the neighbours: `nice -n 19 env GOMAXPROCS=4 go test -p=1 -count=1 github.com/znasllc-io/memql/component/worker -run 'Termination|Reconnect|Disconnect|Heartbeat'` Expected: PASS.
- [ ] **Step 5: Commit** `component/worker/connection_drop.go component/worker/server.go component/worker/connection_drop_test.go` with "worker: record a stream end that was not Cockpit's choice as a drop".

### Task 4: Placement in the dispatcher

**Files:**
- Create: `integrations/agent/worker/connection.go`
- Modify: `integrations/agent/worker/dispatch.go` (`Request`, `Result`, `NewDispatcher`, `Dispatch` pipeline branch and candidate loop)
- Modify: `integrations/agent/worker/router.go` (`RoutePlan.Connection`, `RoutingRecord`, `AsMap`)
- Modify: `integrations/agent/worker/pipeline_purpose.go` (`machineNarrowingBinding`)
- Modify: `integrations/agent/worker/store.go` (`EngineStore.ConnectionDrops`, delegating to `fleetcatalog.EngineStore` as `WorkersForOwner` does, and `var _ ConnectionDropReader = (*EngineStore)(nil)`: production wires the dispatcher with this type, `app/integrations_worker_agent.go`)
- Modify: `dsl/worker/concepts.memql` (the `invocation.routing` description: add `connection`, `passedOver`)
- Modify (tests' fake): `integrations/agent/worker/router_test.go` (`fakeFleet.drops`, `fakeFleet.dropsErr`, `fakeFleet.ConnectionDrops`)
- Test: `integrations/agent/worker/connection_test.go`, `integrations/agent/worker/pipeline_connection_hop_test.go`

**Interfaces:**
- Consumes: `workerservice.ConnectionDrop`; Task 2's reader signature.
- Produces:
  ```go
  type ConnectionPreference struct { Window time.Duration; WaitForSteady bool }
  type ConnectionHealth struct {
      Drops int; LastDropAt time.Time; Window time.Duration
      Capped bool       // the read hit its cap of 100
      Unavailable bool  // the read failed; counts as steady
  }
  func (h ConnectionHealth) Steady() bool            // Unavailable || Drops == 0
  func (h ConnectionHealth) RecordValue() string     // "steady" | "2 drops, last <RFC3339>" | "100 or more drops, last <RFC3339>" | "unavailable"
  type PassedOver struct { WorkerId, MachineName string; Health ConnectionHealth }
  type ConnectionDropReader interface {
      ConnectionDrops(ctx context.Context, ownerUserId, registrationId string, since time.Time) ([]workerservice.ConnectionDrop, error)
  }
  func healthFrom(drops []workerservice.ConnectionDrop, now time.Time, window time.Duration, capped bool) ConnectionHealth
  func (p *RoutePlan) ApplyConnection(health map[string]ConnectionHealth) // stable: steady first, then D5
  Request.Connection ConnectionPreference
  Result.Connection  *ConnectionHealth
  Result.PassedOver  []PassedOver
  RoutePlan.Connection map[string]ConnectionHealth   // keyed by registration id
  RoutingRecord.Connection map[string]string; RoutingRecord.PassedOver []string  // AsMap keys "connection", "passedOver"
  ```

- [ ] **Step 1: Write the failing unit tests** in `connection_test.go`:
  - `TestConnectionHealthCountsDropsInTheWindow` (two in, one out), `TestConnectionHealthWindowStartIsInclusive`, `TestConnectionHealthCountsADropStampedAhead` (droppedAt = now+5s counts), `TestConnectionHealthMatchesBothSpellingsOfTheId` (through `ApplyConnection` with canonical-id drops and bare-id candidates), `TestConnectionHealthAtTheCapIsUnsteadyAndSaysSo` (`RecordValue()` starts "100 or more drops").
  - `TestApplyConnectionPutsSteadyMachinesFirstInStrategyOrder` (three candidates, the first unsteady) and `TestApplyConnectionOrdersUnsteadyByFewestThenOldest` (D5 ties), `TestApplyConnectionLeavesAPlanWithNoDropsUnchanged`.
- [ ] **Step 2: Write the failing dispatch tests** in `connection_test.go` (single replica, `newPipelineFleet`) and `pipeline_connection_hop_test.go` (`siblingThenLocal`, `h.link.reachable = true`, drops put in the shared `h.store` as "written by the other replica"):
  - `TestPipelineTriesASteadyMachineBeforeAnUnsteadyOne`: unsteady first in registration order; the steady one runs; `Result.Connection.Steady()`; routing record `connection` names both.
  - `TestPipelineWaitsForASteadyMachineBusyWithWork` (hop, both orientations like `TestPipelineCapacityCandidateSurvivesOtherPreStartRefusalsAcrossReplicas`): steady machine answers `pipeline_capacity_busy` with its candidate row `ActiveCount = 1`, unsteady machine free; `Result.ErrorCode == "pipeline_capacity_busy"`, `RefusedBeforeStart`, `WorkerId` is the steady machine, the unsteady machine's dispatch count is 0, `PassedOver[0].WorkerId` is the unsteady one with its owner-chosen name.
  - `TestPipelineDoesNotWaitForASteadyMachineThatCannotFreeUp`: steady busy with `ActiveCount = 0`; steady `pipeline_capacity_fenced`; steady `pipeline_runtime_unavailable`: each time the unsteady machine runs the step.
  - `TestAPoolStepDoesNotWaitForASteadyMachine`: `WaitForSteady: false`, steady busy with work: the unsteady machine runs the step.
  - `TestAllUnsteadyMachinesRunFewestDropsFirst`.
  - `TestAPinnedRetryIsNotReorderedButCarriesItsHealth`.
  - `TestAFailedDropReadLeavesTheOwnersOrder`: `fakeFleet.dropsErr` set: first machine runs, `RecordValue` "unavailable".
  - `TestNoDropsAndNoWindowRouteExactlyAsBefore`: identical `Result` and routing record to a request with no `Connection`.
  - `TestConnectionPreferenceIsRefusedOutsideThePipelinePurpose`: an agent request with `Connection.Window` set is refused with `codeDeniedPipelinePurpose`.
- [ ] **Step 3: Run them and see them fail**
  Run: `nice -n 19 env GOMAXPROCS=4 go test -tags agent -p=1 -count=1 github.com/znasllc-io/memql/integrations/agent/worker -run 'ConnectionHealth|ApplyConnection|TestPipelineTriesASteady|TestPipelineWaitsForASteady|TestPipelineDoesNotWait|TestAPoolStepDoesNotWait|TestAllUnsteady|TestAPinnedRetryIsNotReordered|TestAFailedDropRead|TestNoDropsAndNoWindow|TestConnectionPreferenceIsRefused'`
  Expected: FAIL to compile.
- [ ] **Step 4: Implement.** `NewDispatcher` keeps `opts.Store.(ConnectionDropReader)` like `destinationChecker`. In the pipeline branch, after `ExcludeRegistrations`, when `req.Connection.Window > 0` and the reader is present: read each candidate's drops (one call per candidate, `since = now - Window`, `Capped` when 100 rows come back; a failed call marks that machine `Unavailable` and is logged), then `plan.ApplyConnection(health)` unless `req.PinRegistration != ""` (D8: health only). `plan.Record()` carries `Connection`. In the candidate loop, when `req.Connection.WaitForSteady`: remember the first steady candidate whose refusal is `pipeline_capacity_busy` and that has work in flight (`cand.ActiveCount > 0`, or the local registry's `WorkerById(id).ActiveCount() > 0`); on reaching the first unsteady candidate with one remembered, return that refusal through the existing capacity return path, with `PassedOver` built from the remaining unsteady candidates and the record's `PassedOver` ids. Stamp `Result.Connection` in `attempt` from `plan.Connection` (pass the plan's health map or the candidate's entry). `machineNarrowingBinding` also refuses `Connection.Window != 0 || Connection.WaitForSteady` outside the pipeline purpose. Comment each new branch with its decision number. Update the `routing` description in `dsl/worker/concepts.memql`.
- [ ] **Step 5: Run the tests again**: same command. Expected: PASS. Then every existing routing test: `nice -n 19 env GOMAXPROCS=4 go test -tags agent -p=1 -count=1 github.com/znasllc-io/memql/integrations/agent/worker -run 'Pipeline|Router|Dispatch|Routing'` Expected: PASS.
- [ ] **Step 6: Commit** the files above with "worker: fleet pipeline steps prefer machines whose connection holds".

### Task 5: The fleet executor

**Files:**
- Modify: `integrations/pipelinesteps/fleet.go` (`dispatchStep` gains a `pref worker.ConnectionPreference` parameter; `runStep` notice; `classify` node-lost sentence)
- Modify: `integrations/pipelinesteps/fleet_capacity.go` (preference per poll, passed-over notice, D11 run-out)
- Create: `integrations/pipelinesteps/fleet_connection.go` (preference from the request, sentences)
- Test: `integrations/pipelinesteps/fleet_connection_test.go`

**Interfaces:**
- Consumes: Task 1's `ConnectionPolicy.Window()`; Task 4's `ConnectionPreference`, `ConnectionHealth`, `PassedOver`, `Result.Connection`, `Result.PassedOver`.
- Produces: `func connectionPreference(req pl.StepRequest) worker.ConnectionPreference` (window from `req.ComputePlacement`, nil policy -> zero; `WaitForSteady` only when the window is positive and `req.Step.Placement == pl.PlacementFleet`); `func droppedWords(h worker.ConnectionHealth) string` -> `"dropped 2 times in the last 2 hours (last at 19:44 UTC)"` ("1 time", "100 or more times", window as "2 hours" / "1 hour" / "90 minutes"); `func machineWords(id, name string) string` = `pl.MachineWords(name, id)`.

- [ ] **Step 1: Write the failing tests** in `fleet_connection_test.go` (`fakeDispatcher`, `newTestFleet`, `fleetReq`, `poolRequest`, `fleetRun`):
  - `TestTheConnectionWindowReachesTheRequest`: a fleet step with a policy of 120 min dispatches `Connection{Window: 2h, WaitForSteady: true}`; a pool step `WaitForSteady: false`; a policy of 0, and no policy, dispatch the zero preference.
  - `TestTheWaitNamesThePassedOverMachineOnce`: three busy refusals carrying the same `PassedOver`, then success: the sink holds "memql: waiting for Box A, whose connection is steady; Box B is free, but its connection dropped 2 times in the last 2 hours (last at 19:44 UTC)" exactly once.
  - `TestABoundedWaitEndsOnTheFreeMachine`: `run.MaxWaitSeconds = 30`, `f.now` advancing 10 s per call; busy-with-`PassedOver` while `req.Connection.WaitForSteady`, success when it is false: the result succeeds, the last request had `WaitForSteady == false`, and no `waitRanOut` refusal is returned.
  - `TestAStepOnAnUnsteadyMachineSaysSo`: success with `Result.Connection` unsteady: sink holds "memql: the step ran on Box B, whose connection dropped 2 times in the last 2 hours (last at 19:44 UTC); no machine with a steady connection could take it".
  - `TestANodeLossOnAnUnsteadyMachineSaysItsConnectionHadDropped`: `worker_disconnected` (not refused before start) with unsteady `Connection`: failure message ends "Its connection had dropped 2 times in the 2 hours before the step started."; with a steady or nil `Connection` the message is exactly today's.
  - `TestUnsteadyNoticesNameAnUnnamedMachineByItsId`: `MachineName` empty: every sentence says "machine " plus the id prefix `pl.MachineWords` gives.
- [ ] **Step 2: Run them and see them fail**
  Run: `nice -n 19 env GOMAXPROCS=4 go test -tags agent -p=1 -count=1 github.com/znasllc-io/memql/integrations/pipelinesteps -run 'TestTheConnectionWindowReachesTheRequest|TestTheWaitNamesThePassedOverMachineOnce|TestABoundedWaitEndsOnTheFreeMachine|TestAStepOnAnUnsteadyMachineSaysSo|TestANodeLossOnAnUnsteadyMachine|TestUnsteadyNoticesName'`
  Expected: FAIL to compile.
- [ ] **Step 3: Implement.** `dispatchWhenCapacityAvailable` computes `pref := connectionPreference(req)` once and passes it on every poll. A refusal with `PassedOver` writes the waiting notice through `capture.Notice` once per distinct passed-over set. In the `waitEnds` run-out branch, before `waitRanOut`: when `pref.WaitForSteady` and the last refusal had `PassedOver`, set `pref.WaitForSteady = false`, note "memql: the wait for a steady machine ran out; trying the free machine" and `continue` without sleeping, once (D11). `runStep` adds the unsteady notice beside its existing "the step ran on machine" note. `classify`'s `worker_disconnected` case appends the sentence when `r.Connection != nil && !r.Connection.Steady()`.
- [ ] **Step 4: Run the tests again**: same command. Expected: PASS. Then the executor's existing suites: `nice -n 19 env GOMAXPROCS=4 go test -tags agent -p=1 -count=1 github.com/znasllc-io/memql/integrations/pipelinesteps -run 'Fleet|Pool|Attempt|Placement'` Expected: PASS.
- [ ] **Step 5: Commit** the files above with "pipelinesteps: say why a fleet step waited or ran on an unsteady machine".

### Task 6: Fleet shows a machine's drops

**Files:**
- Create: `clients/os/src/apps/fleet/machines/connectionDrops.ts` (summary type, words, hook)
- Modify: `clients/os/src/apps/fleet/machines/MachineDetailsInfo.tsx` (prop and fact, Connection help)
- Modify: `clients/os/src/apps/fleet/machines/MachineDetail.tsx` (hook, pass prop, AutoRefresh reread)
- Test: `clients/os/test/fleet/connectionDrops.test.tsx`

**Interfaces:**
- Consumes: Task 2's generated `connection.query.myWorkerConnectionDrops({ registrationId, since })`.
- Produces: `export type ConnectionDrops = { state: "reading" } | { state: "unavailable" } | { state: "read"; count: number; capped: boolean; lastAt: string }`; `export function connectionDropsFromRows(rows: readonly Row[]): ConnectionDrops`; `export function connectionDropsWords(d: ConnectionDrops): { value: string; title?: string }`; `export function useConnectionDrops(workerId: string, enabled: boolean): ConnectionDrops & { reread: () => void }` (reads the last 24 hours with `priority: "background"`, as `useMachineRecommendation` does); `MachineDetailsInfo` prop `drops?: ConnectionDrops`.

- [ ] **Step 0: Read the UI rules for this surface** (`.agents/skills/memql-ui-design/SKILL.md`, `clients/os/DESIGN.md` "Details and settings content" and "Quiet copying") and state the three meaningful states (reading, read, unavailable).
- [ ] **Step 1: Write the failing tests** in `connectionDrops.test.tsx` (render `MachineDetailsInfo` as `machineFactsCopy.test.tsx` does):
  - words: no rows -> "None in the last day"; one row -> "1 in the last day, last at <formatMoment of it>" with `title` the exact timestamp; three -> "3 in the last day, ..."; 100 rows -> "100 or more in the last day, ..."; unavailable -> "Could not be read"; reading -> "Checking".
  - the "Connection drops" fact renders in the Connection section and offers no copy button (a status, DESIGN.md "Quiet copying").
- [ ] **Step 2: Run and see it fail**
  Run: `cd clients/os && nice -n 19 npx vitest run test/fleet/connectionDrops.test.tsx`
  Expected: FAIL (module not found).
- [ ] **Step 3: Implement** with the kit's `Fact`; the Connection `PageHelp` gains: "A connection drop is a connection that ended while the machine was running work, or that a new connection replaced before the cluster saw the old one end. Cockpit's own reconnects are not drops. Pipeline steps wait for a machine with no recent drop when one can take them." `MachineDetail` calls the hook only for the Details view and adds its `reread` to the existing `AutoRefresh`.
- [ ] **Step 4: Run the test again** (same command; expected PASS), then `cd clients/os && nice -n 19 npx vitest run test/fleet/machineFactsCopy.test.tsx` (PASS) and `nice -n 19 make os-typecheck` once the release limits allow it, else `cd clients/os && nice -n 19 npx tsc --noEmit -p .` (PASS).
- [ ] **Step 5: Commit** the three source files and the test with "os(fleet): a machine's Connection details show its recent drops".

### Task 7: Documentation

**Files:**
- Modify: `docs/internal/ops/harness-decisions.md` (new section inserted immediately before "## A pipeline's work run is not beaten (2026-11-08)")
- Modify: `docs/public/operate/pipelines.md` ("Retries and failover")
- Modify: `docs/public/operate/workers-runbook.md` (after the Ping paragraph)
- Modify: `cockpit/docs/pipelines.md`
- Modify: `docs/internal/design/feature-notes.md` (workers section, one bullet)

- [ ] **Step 1: Write the decision-map section** "Fleet steps prefer machines whose connection holds (2026-11-24)" with the bold lead-ins the retries section uses: Trigger, Evidence, Chosen path, Alternatives rejected, Stop conditions, Invariants, What survives a replica change, Known limitations, Not verified, Regression tests (every test name from Tasks 1 to 6). Link the spec.
- [ ] **Step 2: Write the operator text**: pipelines.md says how a fleet step and a pool step choose among machines by connection, the `connection: {unsteadyMinutes}` setting with its default and bounds, the log lines, and the rollback note (section 12); workers-runbook says what a connection drop is, where it is recorded and for how long, and that round trip still decides nothing; cockpit/docs/pipelines.md says a step needs an unbroken connection, a drop fails it, and a machine with recent drops is used last; feature-notes gets one bullet pointing at the decision map.
- [ ] **Step 3: Run the docs gates**: `nice -n 19 env GOMAXPROCS=4 go test -p=1 -count=1 github.com/znasllc-io/memql -run 'Docs|Positioning|TestDocumentedTestCommandCoversTheEngine'` Expected: PASS.
- [ ] **Step 4: Commit** the five files with "docs: fleet steps prefer machines whose connection holds".

### Task 8: Whole-change verification and landing

- [ ] **Step 1:** While the release limits hold, re-run each task's focused commands once more, one at a time.
- [ ] **Step 2:** Once the release session reports 0.31.40's release run done: `MEMQL_REQUIRE_DB=1 make test` (against `memql-testdb`), the db-gated trees touched (`scripts/ci/db-gated-packages.sh --trees` to confirm which include `component/worker` and `component/pipelinerun`, run each with `-p=1`), the agent-tagged packages, and every go-checks command (sdk-gen-check, concept-snapshot check, gitleaks over the range, `scripts/ci/module-integrity.sh`, the per-tag lanes), as the memory note "Run go-checks before pushing" lists. Expected: all PASS; any failure is fixed or reported with its output.
- [ ] **Step 3:** Delete this plan file in the final commit (docs/CLAUDE.md).
- [ ] **Step 4:** Ask the owner whether to push. On their word: pull `main`, merge with `--no-ff`, run the focused tests once more, push (never force), and watch `MemQL / memql / push`.
