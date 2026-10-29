# Pipeline Fleet-First Placement with Retries and Failover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pipeline steps run on the owner's machines first, retry transient failures on the same machine, then fail over to other machines and, for `pool` steps, to the cluster, with every attempt journaled so a replica takeover resumes correctly.

**Architecture:** Carry the Codex fleet-first commits (`placement: pool`, the captured compute placement policy, admission version 2, worker contract 5) onto main. The executor classifies each failed attempt and honours a per-attempt placement; the dispatcher gains pipeline-only pin and exclude; the driver runs a bounded attempt loop recorded as versions of the step's work row, with the history in `binding.attempts`.

**Tech Stack:** Go 1.26 multi-module workspace, MemQL DSL, PostgreSQL + TimescaleDB (db-gated tests), React/TypeScript (MemQL OS), Node (VS Code test runners).

**Spec:** `docs/superpowers/specs/2026-10-28-pipeline-fleet-first-retries-failover-design.md`

## Global Constraints

- Branch `claude/fleet-first-retries` in `.claude/worktrees/fleet-first-retries`, based on `origin/main` 565af0d93. Never switch branches in the shared checkout.
- Verify with `make test` (never `go test ./...`), plus `go test -tags agent -count=1 ./integrations/agent/ ./integrations/agent/worker/ ./integrations/pipelinesteps/ ./component/node/` and the db-gated trees with `MEMQL_REQUIRE_DB=1`.
- Stage files by explicit path; never `git add -A` or `git add .`. End every commit message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- No change to `memql-package.yaml` keys (spec D11). No GitHub Actions fallback.
- Pre-release rule: no compatibility shim on the worker contract; contract 5 is exact (spec D10).
- Engine cap: `pipelines.MaxStepAttempts = 8`.
- Policy bounds: `sameMachine` 0..3; `delaysSeconds` 1..3 integers each 0..900; `otherMachines` boolean; no other field in `retries`.
- Portable default: `{fleetWaitSeconds: 0, cloudFallback: false, retries: {sameMachine: 2, delaysSeconds: [30, 120], otherMachines: true}}`.
- Exit 75 (`pipelines.ExitRetryRequested`) means "transient, retry me".
- Every string written into an attempt record is masked with the run's secrets and cut to the receipt's message bound.
- Harness rule (CLAUDE.md): docs and comments explaining each decision change in the same PR; cross-replica tests are required for cross-node behaviour.

## Review Focus

1. Machine words in an attempt record carry a secret the step echoed: the record must be masked like a receipt (Task 7 test).
2. A cancel or supersede arrives while the driver waits between attempts: the step settles cancelled and no further attempt is sent (Task 7 test).
3. The run's ceiling arrives mid-retries: no attempt starts with less than a second left, and the step keeps its last failure (Task 7 test).
4. The installed policy changes between a run's start and its takeover: recovery refuses the changed definition instead of applying new limits (Task 5 test).
5. Two parallel steps pin the same busy machine: the pinned wait is bounded, then counts as nothing ran there, and the ladder moves on (Task 4 test).

---

### Task 1: Carry the Codex fleet-first commits onto main

**Files:**
- Cherry-pick, in order, with `-x`: `cbf2f39b4`, `eb186e726`, `1d592988b`, `138658c8c`, `8ae6ec3f2`, `19fc82812`. Do not take `599128679` (the 0.31.4 version) or anything else from `codex/local-clean-start-2026-10-28`.
- Expected conflicts: `component/pipelinerun/admission.go`, `component/pipelinerun/admission_test.go`, `component/envregistry/manifest.yaml`, `scripts/secrets/manifest.yaml`, `docs/public/operate/env-vars.md`, `docs/internal/ops/harness-decisions.md`.

**Interfaces:**
- Produces (from the carried code): `pipelines.PlacementPool`, `pipelines.CheckPoolStep(Step) error`, `pipelines.ComputePlacementPolicy{FleetWaitSeconds int; CloudFallback bool}`, `pipelines.StepRequest.ComputePlacement *ComputePlacementPolicy`, `StepRequest.HeadBranch`, `(*runDriver).selectComputePlacement(ctx) (*pipelines.ComputePlacementPolicy, error)`, `parseComputePlacement(any) (*pipelines.ComputePlacementPolicy, error)` in `component/pipelinerun/compute_placement.go`, `(*Executor).runPool` in `integrations/pipelinesteps/executor.go`, the DSL template `pipelineComputePlacementPolicy`, admission version 2 (`allowNewRuns`, `allowCloud`, `allowTrainRuns`), `PipelineStepContract = 5` in `integrations/agent/worker/pipeline_purpose.go`, and Cockpit's `workerHost.pipeline_step: 5`.

- [ ] **Step 1: Cherry-pick the series and resolve conflicts**

Resolve `admission.go` so main's `allowTrainRuns` and the carried version-2 split both hold: legacy `allowNewRuns:false, allowTrainRuns:true` admits a train run; version 2 requires `allowCloud` for a cloud Job, and an explicit cloud hold wins even for a train (this is exactly `19fc82812`'s shape; use `git show d962310d1` as the reference resolution). In the manifests and docs keep both sides' entries.

- [ ] **Step 2: Build every module and the agent tag**

Run: `go build github.com/znasllc-io/memql/... && go build -tags agent github.com/znasllc-io/memql/... && go vet -tags agent github.com/znasllc-io/memql/...`
Expected: no output, exit 0.

- [ ] **Step 3: Run the carried tests**

Run: `go test -count=1 ./component/pipelines/... ./component/pipelinerun/... ./integrations/workbench/... && go test -tags agent -count=1 ./integrations/agent/worker/ ./integrations/pipelinesteps/ && go test -count=1 ./cockpit/... && make env-registry-check && go test -run TestEngineInitLoadsFullDSL ./component/memql/`
Expected: PASS, including `TestSplitAdmissionKeepsCostAndOperatorHoldsSeparate`, `TestTrainAllowancePreservesIndependentCloudHold`, `TestComputePlacementUsesCapturedPolicyAcrossReplicas`.

- [ ] **Step 4: Commit the conflict resolutions** (each cherry-pick commits itself; amend nothing). Confirm `git log --oneline origin/main..` shows the six carried commits plus the design record.

---

### Task 2: "Host commands" off is a refusal before start

**Files:**
- Modify: `component/worker/owner_features.go` (typed refusal), `component/worker/server.go:850` (wrap admission errors)
- Modify: `component/worker/fleetcatalog/machine.go`, `component/worker/fleetcatalog/store.go` (carry the setting)
- Modify: `integrations/agent/worker/dispatch.go` (`translateResult`, `attemptLocal`, the pipeline plan), `integrations/agent/worker/router.go` (`RequireHostCommands`), `integrations/agent/worker/forward_handler.go:318`
- Test: `component/worker/owner_features_test.go`, `integrations/agent/worker/host_commands_hop_test.go` (new, `//go:build agent`)

**Interfaces:**
- Produces: `type RefusedBeforeSendError struct{ Code string; Err error }` with `Error()` and `Unwrap()`, and `func RefusedBeforeSend(err error) (code string, ok bool)` in `component/worker`; `Candidate.HostCommands bool`; `func (p *RoutePlan) RequireHostCommands()`.

- [ ] **Step 1: Write the failing tests**

`TestAdmissionRefusalIsTypedButMidCallWithdrawalIsNot` (component/worker): a dispatch to a worker whose policy has `Headless: false` returns an error for which `RefusedBeforeSend(err)` answers `("worker_feature_disabled", true)` and nothing was sent; an in-flight call cancelled by `ApplyOwnerFeaturePolicy` answers `ok == false`. A policy read failure answers `("worker_feature_unavailable", true)`.

`TestHostCommandsOffIsSkippedByTheRouterForPipelines`: with `siblingThenLocal(t)`, mark `laptop`'s registration `ownerFeatures.headless=false`; `dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))` lands on `desktop`, and `row.Routing["rejected"]["laptop"] == "Host commands are off in Fleet settings"`.

`TestHostCommandsTurnedOffAfterPlanningMovesOnAcrossReplicas`: the row says headless on; the sibling-held `laptop`'s dispatch func returns `&workerservice.RefusedBeforeSendError{Code: "worker_feature_disabled"}`; the result comes from `desktop` with `Routing["attempts"] == 2` and `reroutedFrom == "worker:laptop"`. Repeat with `laptop` held locally.

`TestMidCallFeatureWithdrawalIsNeverRepicked`: the dispatch func returns the plain `featureDisabled` error after "sending"; the result is not `RefusedBeforeStart` and only one machine was tried.

- [ ] **Step 2: Run them to see them fail**

Run: `go test -count=1 -run 'TestAdmissionRefusalIsTyped' ./component/worker/ && go test -tags agent -count=1 -run 'HostCommands|MidCallFeature' ./integrations/agent/worker/`
Expected: FAIL (undefined `RefusedBeforeSend`; dispatch not re-picked).

- [ ] **Step 3: Implement**

`admitOwnerFeature` wraps its refusals in `*RefusedBeforeSendError` (store or version failure: `worker_feature_unavailable`; disabled: `worker_feature_disabled`); `ApplyOwnerFeaturePolicy` keeps cancelling with the plain error. `translateResult` maps a `RefusedBeforeSend` error to its code before the generic `worker_disconnected`. `attemptLocal` returns `ForwardRefusedBeforeStart` for it; `ForwardHandler` ORs `refusedBeforeSend(err)` into `RefusedBeforeStart`. `WorkersForOwner` sets `HostCommands: workerservice.OwnerFeaturesFromRow(row["ownerFeatures"]).Headless`. `Dispatch` calls `plan.RequireHostCommands()` for `PurposePipeline`. Comment each branch with why (the refusal precedes the send; the withdrawal does not).

- [ ] **Step 4: Run the tests to see them pass**, plus `go test -tags agent -count=1 ./integrations/agent/worker/` and `go test -count=1 ./component/worker/...`. Expected: PASS.

- [ ] **Step 5: Commit** the five source files and two test files by path: `fix(fleet): a machine with Host commands off refuses before start and is routed around`.

---

### Task 3: Pipeline-only pin and exclude in the dispatcher

**Files:**
- Modify: `integrations/agent/worker/dispatch.go` (`Request`), `integrations/agent/worker/router.go` (plan narrowing), `integrations/agent/worker/pipeline_purpose.go` (`purposeRefusal`)
- Test: `integrations/agent/worker/pipeline_pin_hop_test.go` (new, `//go:build agent`)

**Interfaces:**
- Consumes: Task 2's `RequireHostCommands`.
- Produces: `Request.PinRegistration string`, `Request.ExcludeRegistrations []string`; `func (p *RoutePlan) PinRegistration(id string)` (keeps only `id`, reason for others "not the machine this retry is pinned to"); `func (p *RoutePlan) ExcludeRegistrations(ids []string)` (reason "already tried in this step"). Both apply to `Candidates` and `OfflineCandidates` through `filterCandidates`.

- [ ] **Step 1: Write the failing tests**

`TestPinnedRetryReachesTheSiblingHoldingTheMachine`: request with `PinRegistration: "laptop"` (held by node B) is forwarded to B and runs there; `Routing["candidatesConsidered"] == ["laptop"]`.
`TestExcludedMachinesAreSkippedAcrossReplicas`: `ExcludeRegistrations: ["laptop"]` lands on `desktop`; `Routing["rejected"]["laptop"] == "already tried in this step"`.
`TestPinAndExcludeAreRefusedOutsideThePipelinePurpose`: an agent request (`Purpose: ""`) with either field set is denied `denied_pipeline_purpose` with `RefusedByGate`.
`TestPinnedOfflineMachineReportsWaitForConnection`: the pinned machine has no stream; the result is `no_worker_available` with `WaitForConnection == true` (the bound is the fleet's, Task 4).

- [ ] **Step 2: Run them to see them fail**: `go test -tags agent -count=1 -run 'Pinned|Excluded|PinAndExclude' ./integrations/agent/worker/`. Expected: FAIL (unknown fields).

- [ ] **Step 3: Implement** the fields, the two `RoutePlan` methods, their call in `Dispatch` for `PurposePipeline` (after the repository, contract and platform requirements), and the guard in `purposeRefusal`. Comment that only the engine's pipeline executor sets them, so fleet design D4 (no model names a machine) holds.

- [ ] **Step 4: Run** `go test -tags agent -count=1 ./integrations/agent/worker/`. Expected: PASS.

- [ ] **Step 5: Commit** by path: `feat(fleet): pipeline steps may pin or exclude machines for retries`.

---

### Task 4: The executor classifies attempts and honours a placement

**Files:**
- Modify: `component/pipelines/contract.go` (types below)
- Create: `integrations/pipelinesteps/attempt_class.go` (cluster and shared rules), `integrations/pipelinesteps/attempt_class_fleet.go` (`//go:build agent`)
- Modify: `integrations/pipelinesteps/executor.go` (`Execute`, `runPool`), `integrations/pipelinesteps/adapt.go` (`stepRunFor`), `integrations/pipelinesteps/protocol.go` (`StepRun` fields), `integrations/pipelinesteps/fleet.go` (`dispatchStep`, `classify`), `integrations/pipelinesteps/fleet_capacity.go` (bounded wait)
- Test: `integrations/pipelinesteps/attempt_class_test.go`, `integrations/pipelinesteps/fleet_attempt_test.go` (`//go:build agent`)

**Interfaces:**
- Produces in `component/pipelines`:
  - `type AttemptClass string` with `AttemptNothingRan = "nothing_ran"`, `AttemptRetryRequested = "retry_requested"`, `AttemptOutcomeUnknown = "outcome_unknown"`, `AttemptFinal = "final"`; `const ExitRetryRequested = 75`; `const MaxStepAttempts = 8`.
  - `StepResult.AttemptClass AttemptClass` (`json:"attemptClass,omitempty"`; empty reads as final) and `StepResult.NotRetriedHere bool` (`json:"notRetriedHere,omitempty"`).
  - `type AttemptPlacement struct { Surface string; PinWorker string; ExcludeWorkers []string; MaxWaitSeconds int }` (json `surface`, `pinWorker`, `excludeWorkers`, `maxWaitSeconds`); `Surface` is `""` (the step's own placement), `"fleet"` or `"cluster"`.
  - `StepRequest.Placement *AttemptPlacement` (`json:"placement,omitempty"`).
- Produces in `pipelinesteps`: `func clusterAttemptClass(res pl.StepResult) pl.AttemptClass`; `func fleetAttemptClass(r worker.Result, res pl.StepResult, dispatched bool) (pl.AttemptClass, bool)` (the bool is `NotRetriedHere`). `StepRun.PinWorker`, `StepRun.ExcludeWorkers`, `StepRun.MaxWaitSeconds`.

- [ ] **Step 1: Write the failing table test** `TestAttemptClassTable` in `attempt_class_test.go`, one row per line of spec section 4, for example:

```go
{surface: "cluster", res: failedResult(pl.CodeImagePullFailed, "x"), want: pl.AttemptNothingRan},
{surface: "cluster", res: pl.StepResult{Status: pl.OutcomeFailed, ExitCode: 75}, want: pl.AttemptRetryRequested},
{surface: "cluster", res: failedResult(pl.CodeNodeLost, "x"), want: pl.AttemptOutcomeUnknown},
{surface: "cluster", res: pl.StepResult{Status: pl.OutcomeFailed, ExitCode: 1}, want: pl.AttemptFinal},
{surface: "cluster", res: failedResult(pl.CodeStepTimeout, "x"), want: pl.AttemptFinal},
```

and in `fleet_attempt_test.go` the machine rows: raw `pipeline_clone_failed`, `pipeline_cache_unavailable`, `pipeline_runtime_unavailable` give nothing ran; `pipeline_recovery_required`, `worker_feature_disabled`, `pipelines_not_allowed` give nothing ran with `NotRetriedHere`; `worker_disconnected` after dispatch gives outcome unknown; exit 0 with `pipeline_artifact_unavailable` gives outcome unknown; a recover-only fleet attempt gives outcome unknown.

- [ ] **Step 2: Write the failing placement tests** in `fleet_attempt_test.go`: `TestPlacementPinsAndExcludesThroughTheDispatcher` (the fake dispatcher receives `PinRegistration`/`ExcludeRegistrations` from `req.Placement`); `TestClusterPlacementRefusedForAFleetOnlyStep` (a native step with `Placement.Surface == "cluster"` is refused `pipeline_step_invalid`); `TestPinnedBusyMachineWaitIsBounded` (the fake answers `pipeline_capacity_busy` forever; with `MaxWaitSeconds: 2` the step returns `OutcomeRefused`, `pipeline_no_machine_for_need`, `AttemptNothingRan`, `NotRetriedHere` within 3 s of the test clock).

- [ ] **Step 3: Run them to see them fail**: `go test -count=1 -run TestAttemptClassTable ./integrations/pipelinesteps/ && go test -tags agent -count=1 -run 'Placement|PinnedBusy|AttemptClass' ./integrations/pipelinesteps/`. Expected: FAIL (undefined types).

- [ ] **Step 4: Implement**

`Execute` sets the class on every non-success result before returning (cluster, pool and fleet paths). With `req.Placement != nil`: `"cluster"` runs `runCluster` for a `pool` or `cluster` step and refuses otherwise; `"fleet"` calls `fleet.RunStep` with the pin, exclusions and `MaxWaitSeconds` copied into `StepRun` by `stepRunFor`. When the placement excludes machines, `dispatchStep` sets `worker.Request.ReroutedFrom` to `"worker:" + <the last excluded machine>`, so the attempt's invocation record says why it moved. `dispatchWhenCapacityAvailable` stops waiting after `run.MaxWaitSeconds` when it is positive and answers the refused, nothing-ran result. The exit-75 rule: failed, `ExitCode == 75`, no `Failure` means retry requested. Comment the class of each code where it is decided.

- [ ] **Step 5: Run** `go test -count=1 ./integrations/pipelinesteps/ ./component/pipelines/... && go test -tags agent -count=1 ./integrations/pipelinesteps/`. Expected: PASS.

- [ ] **Step 6: Commit** by path: `feat(pipelines): classify step attempts and route an attempt to a chosen place`.

---

### Task 5: Retry policy, the `repeatable` key, and recovery binding

**Files:**
- Modify: `component/pipelines/contract.go` (`RetryPolicy`), `component/pipelines/spec.go` (`Spec.Repeatable`, `StepSpec.Repeatable`), `component/pipelines/compile.go` (set `Step.Repeatable`), `component/pipelines/validate.go` (contradiction)
- Modify: `component/pipelinerun/compute_placement.go` (parse `retries`; default), `component/pipelinerun/tree.go` (compute the policy for every run with a command step), `component/pipelinerun/definition.go` (bind it for every command step; keep it out of `verificationOf`), `component/pipelinerun/driver.go` (`request` copies the policy to every command step)
- Modify: `dsl/pipelines/automations.memql` (`pipelineComputePlacementPolicy` default)
- Test: `component/pipelines/repeatable_test.go`, `component/pipelinerun/compute_placement_test.go`

**Interfaces:**
- Produces: `type RetryPolicy struct { SameMachine int; DelaysSeconds []int; OtherMachines bool }` (json `sameMachine`, `delaysSeconds`, `otherMachines`); `ComputePlacementPolicy.Retries RetryPolicy` (`json:"retries"`); `func DefaultRetryPolicy() RetryPolicy` returning `{2, [30, 120], true}`; `Step.Repeatable bool` (`json:"repeatable,omitempty"`); `Spec.Repeatable bool` and `StepSpec.Repeatable *bool` (yaml `repeatable`).

- [ ] **Step 1: Write the failing tests**

`TestRetriesParseStrictly` (table over `parseComputePlacement`): absent `retries` yields `DefaultRetryPolicy()`; `sameMachine: 4`, `delaysSeconds: []`, `delaysSeconds: [1,2,3,4]`, a delay of 901, `otherMachines: "yes"` and an extra key `jitter` are each refused naming the field.
`TestEveryRunGetsAPolicyEvenWithoutPoolSteps`: a plan with only `cluster` steps and a snapshot lacking the phase gets `CloudFallback == false`, `FleetWaitSeconds == 0`, `Retries == DefaultRetryPolicy()`.
`TestRetryPolicyIsPartOfTheRecoveryDefinition`: two drivers differing only in `Retries.SameMachine` give different `definitionOf` for a `cluster` step and the same `verificationOf`.
`TestRepeatableManifestKey` (compile): pipeline `repeatable: true` makes steps repeatable; a step's `repeatable: false` overrides it; `verification: true` makes a step repeatable; `verification: true` with `repeatable: false` is refused `pipeline_step_invalid` with "A verification step is repeatable; remove repeatable: false."

- [ ] **Step 2: Run them to see them fail**: `go test -count=1 -run 'Retries|EveryRunGetsAPolicy|RetryPolicyIsPart|RepeatableManifest' ./component/pipelines/ ./component/pipelinerun/`. Expected: FAIL.

- [ ] **Step 3: Implement**, then update the template's default return to the portable default in Global Constraints and confirm the strict boot gate still loads it: `go test -run TestEngineInitLoadsFullDSL ./component/memql/`.

- [ ] **Step 4: Run** `go test -count=1 ./component/pipelines/... ./component/pipelinerun/...`. Expected: PASS.

- [ ] **Step 5: Commit** by path: `feat(pipelines): installed retry limits and the repeatable step key`.

---

### Task 6: Journal attempts as versions of the step row

**Files:**
- Modify: `component/workjournal/journal.go`
- Test: `component/workjournal/attempt_test.go` (new)

**Interfaces:**
- Produces: `func (r *Run) StepAttempt(ctx context.Context, key string, attempt int, binding map[string]any) (*Step, error)` (intent: `status: running`, `attempt`, `idempotencyKey: <runId>:<key>:<attempt>`, `binding`); `Run.Step(ctx, key)` becomes `StepAttempt(ctx, key, 1, nil)`; `func (s *Step) Retrying(ctx context.Context, binding map[string]any) error` (`updateWorkStep` with `status: running` and `binding` only).

- [ ] **Step 1: Write the failing test** `TestStepAttemptsAreVersionsOfOneRow`: against the package's existing fake engine, `StepAttempt(ctx, "tests.unit", 2, b)` writes `createWorkStep` with `stepId == deriveID("step", runID, "tests.unit")`, `attempt == 2`, `idempotencyKey == runID+":tests.unit:2"`, `binding == b`; `Retrying` writes `updateWorkStep` with `status == "running"` and no `finishedAt`; attempt 0 is refused.

- [ ] **Step 2: Run it to see it fail**: `go test -count=1 -run TestStepAttempts ./component/workjournal/`. Expected: FAIL.

- [ ] **Step 3: Implement**, keeping `Step`'s existing behaviour for attempt 1 byte-for-byte (existing tests prove it).

- [ ] **Step 4: Run** `go test -count=1 ./component/workjournal/`. Expected: PASS.

- [ ] **Step 5: Commit** by path: `feat(work): journal pipeline step attempts as versions of one row`.

---

### Task 7: The driver's attempt loop

**Files:**
- Create: `component/pipelinerun/attempts.go` (records and the next-place choice)
- Modify: `component/pipelinerun/driver.go` (`stepTrack`, `runStep`, `request`, `receiptFor`, `bindingOf`), `component/pipelinerun/types.go` (`StepState.Attempts`)
- Test: `component/pipelinerun/attempts_test.go`, `component/pipelinerun/driver_retry_test.go`

**Interfaces:**
- Consumes: Task 4 (`AttemptClass`, `NotRetriedHere`, `AttemptPlacement`), Task 5 (`RetryPolicy`, `Step.Repeatable`, `MaxStepAttempts`), Task 6 (`StepAttempt`, `Retrying`).
- Produces:
  - `type attemptRecord struct { Attempt int; Class pipelines.AttemptClass; Code, Message, Surface, WorkerID, NodeID, JobName, StartedAt, FinishedAt, LogFileID string; NotRetriedHere bool }` with `func (a attemptRecord) asMap() map[string]any` and `func attemptsFromBinding(b map[string]any) (records []attemptRecord, next time.Time)`.
  - `func chooseNext(step pipelines.Step, policy pipelines.ComputePlacementPolicy, history []attemptRecord) (placement *pipelines.AttemptPlacement, delay time.Duration, ok bool)` implementing spec section 5 exactly.
  - Binding keys: `attempts` (list of `asMap()`), `nextAttemptAt` (RFC3339, only on a between-attempts version). This replaces the spec's mention of `resumeAt`; Task 12 corrects the spec text.
  - `StepState.Attempts []attemptRecord`.

- [ ] **Step 1: Write the failing table test** `TestChooseNextFollowsTheRetryOrder` with the default policy:
  - machine M nothing ran once: pin M, delay 30 s; twice: pin M, 120 s; three times: exclude [M], 0 s.
  - M with `NotRetriedHere`: exclude [M] at once.
  - `fleet` step, every eligible machine tried: `ok == false`.
  - `pool` step, machines exhausted, `CloudFallback: true`: `Surface == "cluster"`; `CloudFallback: false`: `ok == false`; cluster already a place: `ok == false`.
  - `cluster` step, nothing ran: `Surface == "cluster"` up to `sameMachine`, then `ok == false`.
  - outcome unknown, `Repeatable: false`: `ok == false`; `Repeatable: true`: exclude [M]; after the others: pin M once; then cluster for `pool`.
  - retry requested with `Repeatable: false`: retried like nothing ran.
  - 8 records: `ok == false`.
  - pinned placements carry `MaxWaitSeconds == 120` (the largest delay); a `pool` step's fleet failover carries `fleetWaitSeconds`.

- [ ] **Step 2: Write the failing driver tests** in `driver_retry_test.go` with `newHarness` and `fakeExecutor.answer`:
  - `TestARetriedStepPassesOnItsSecondAttempt`: attempt 1 answers nothing ran on `MSI Raider`; attempt 2 (pinned there) passes. Journal: `createWorkStep` attempt 1, `updateWorkStep` running with `binding.attempts[0].code == "pipeline_clone_failed"` and a `nextAttemptAt`, `createWorkStep` attempt 2, final `done` whose binding has both records and no `nextAttemptAt`. The executor acknowledged attempt 1 before attempt 2 was sent.
  - `TestAFailingStepStopsAtTheEngineCap`: always retry requested, policy `sameMachine: 3`, many machines: exactly 8 requests, final `failed`.
  - `TestCancelDuringARetryWaitSettlesCancelled` (Review Focus 2): cancel while waiting; no attempt 2 is sent; the receipt is `cancelled`.
  - `TestRunCeilingStopsTheRetries` (Review Focus 3): the ceiling falls inside the wait; no further attempt; the step keeps attempt 1's failure.
  - `TestAttemptRecordsAreMasked` (Review Focus 1): attempt 1's message echoes a resolved secret; the binding's record shows the mask, never the value.
  - `TestHeldCloudGateMakesTheCloudFailoverWait`: a `pool` step with `CloudFallback: true` fails nothing ran on every machine; the next request carries `Placement.Surface == "cluster"`; the fake holds it, as the workbench does under a held cloud gate, until the run's ceiling and answers `pipeline_run_ceiling`; the step ends with that failure and no further request is sent.

- [ ] **Step 3: Run them to see them fail**: `go test -count=1 -run 'ChooseNext|Retried|EngineCap|RetryWait|RunCeilingStops|AttemptRecordsAreMasked|HeldCloudGate' ./component/pipelinerun/`. Expected: FAIL.

- [ ] **Step 4: Implement**

`runStep` becomes the loop: intent through `StepAttempt`, `execStep`, then on a failed result with a non-final class, `chooseNext`; when it answers, write `Retrying` with the history and `nextAttemptAt`, acknowledge that attempt's resources, wait on a timer (stopping on the lease's stop channel, a cancel or the run's ceiling), raise `t.attempt`, and send the next request with `Placement`. Otherwise settle the receipt with the whole history. Waits never start when less than a second of the ceiling would remain. Comment the loop with the spec's stop conditions and why a step never reads finished between attempts.

- [ ] **Step 5: Run** `go test -count=1 ./component/pipelinerun/`. Expected: PASS.

- [ ] **Step 6: Commit** by path: `feat(pipelines): retry a step on the same machine, then other machines, then the cluster`.

---

### Task 8: A replica that takes over resumes the retries

**Files:**
- Modify: `component/pipelinerun/types.go` (`WorkStep.Binding map[string]any`), `component/pipelinerun/store_dsl.go` (`workStepFromRow` reads `binding`), `component/pipelinerun/driver.go` (`resume`, `adoptRows`, `stepTrack` fields `attempts`, `nextAttemptAt`)
- Test: `component/pipelinerun/driver_db_test.go` (extend), `component/pipelinerun/driver_test.go` (resume unit cases)

**Interfaces:**
- Consumes: Task 7's `attemptsFromBinding`, `chooseNext`.
- Produces: `stepTrack.sent` is true only when the row is running AND `binding.attempts` has no record for `row.Attempt`.

- [ ] **Step 1: Write the failing unit tests** in `driver_test.go`: `TestResumeBetweenAttemptsContinuesAtTheNextAttempt` (row running, attempt 1, a record for 1, `nextAttemptAt` in the past: not sent; the next request is attempt 2 with the chosen placement); `TestResumeWaitsForNextAttemptAt` (future time: attempt 2 is sent only after it); `TestResumeOfAnInFlightAttemptIsRecoverOnly` (no record for attempt 2: re-sent recover-only as attempt 2).

- [ ] **Step 2: Write the failing database tests** in `driver_db_test.go` (Postgres-gated, real journal and DSL store, two drivers over one database):
  - `TestATakeoverMidRetryResumesOverRealRows`: driver A records attempt 1 nothing ran and stops in the wait; driver B claims the run and drives attempt 2 to a pass; rows show attempts 1 and 2 and one final receipt.
  - `TestATakeoverOfAnInFlightFleetAttempt`: driver A stops with attempt 1 in flight on a machine. A repeatable step is answered outcome unknown by the recover-only fleet path and passes on another machine under B. A non-repeatable step fails `pipeline_execution_uncertain`.
  - `TestATakeoverAdoptsTheClusterJobOfTheCurrentAttempt`: attempt 2 in flight on the cluster; B's recover-only request carries `Attempt == 2`.
  - `TestBetweenAttemptsTheStepNeverReadsFinished`: after `Retrying`, the row read back through the work spine's server-only read has status `running`.

- [ ] **Step 3: Run them to see them fail**: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<dsn> go test -count=1 -run 'Takeover|BetweenAttempts|Resume' ./component/pipelinerun/`. Expected: FAIL.

- [ ] **Step 4: Implement**, commenting in `resume` the three row states and what survives a replica change.

- [ ] **Step 5: Run** the package with `MEMQL_REQUIRE_DB=1`. Expected: PASS. Use the local `memql-testdb` container (127.0.0.1:55432; password from `docker inspect memql-testdb`).

- [ ] **Step 6: Commit** by path: `feat(pipelines): a replica that takes a run over resumes its step retries`.

---

### Task 9: The check run and the OS run page show attempts

**Files:**
- Modify: `component/pipelines/summary.go` (`StepReport.Attempts`, `writeRetriedSteps`, `writeFailedSteps`), `component/pipelinerun/types.go` (`StepState.Report`)
- Modify: `clients/os/src/apps/deployables/pipelines/rows.ts` (`StepRow.attempts`), `clients/os/src/apps/deployables/pipelines/RunPage.tsx` (step detail), `clients/os/src/apps/deployables/pipelines/words.ts` (attempt sentence)
- Test: `component/pipelines/summary_test.go`, `clients/os/src/apps/deployables/pipelines/rows.test.ts`

**Interfaces:**
- Produces: `type AttemptReport struct { Attempt int; Code, Where string }` and `StepReport.Attempts []AttemptReport`; TS `interface StepAttempt { attempt: number; code: string; surface: "cluster" | "fleet" | ""; workerId: string; logFileId: string }` and `StepRow.attempts: StepAttempt[]`.

- [ ] **Step 0: Load the `memql-ui-design` skill** (`.agents/skills/memql-ui-design/SKILL.md`) before touching the OS files.

- [ ] **Step 1: Write the failing tests**: `TestCheckOutputNamesARetriedStep` (a passed step with two attempts renders "Passed on attempt 2 of 2. Attempt 1: pipeline_clone_failed on MSI Raider."); `TestCheckOutputListsEveryAttemptOfAFailedStep`; in `rows.test.ts`, `stepFromRow` reads `binding.attempts` into `attempts` and ignores malformed entries.

- [ ] **Step 2: Run them to see them fail**: `go test -count=1 -run CheckOutput ./component/pipelines/` and `npm --prefix clients/os test -- rows.test.ts`. Expected: FAIL.

- [ ] **Step 3: Implement**: the summary sentence; the step detail lists each attempt with its machine name (the page's `useMachineNames`), its code and its log link, with no new visual devices beyond the page's existing step detail pattern.

- [ ] **Step 4: Run** `go test -count=1 ./component/pipelines/...`, `npm --prefix clients/os test`, `npm --prefix clients/os run typecheck`. Expected: PASS.

- [ ] **Step 5: Commit** by path: `feat(pipelines): the check run and run page show a step's attempts`.

---

### Task 10: The VS Code download exits 75 when it stalls

**Files:**
- Modify: `scripts/vscode/download.cjs` (export `exitCodeFor(error): number`), `editors/productivity/test/runner.cjs:55`, `editors/vscode/test-host/runner.js` (exit with `exitCodeFor(error)`)
- Test: `scripts/vscode/download.test.cjs`

**Interfaces:**
- Produces: `exitCodeFor(error)` answers 75 for a final download error that is a stall, the total time limit, or a transient transport failure (`error.retryable` or the time-limit message), and 1 otherwise.

- [ ] **Step 1: Write the failing test**: `exitCodeFor` of a stalled-download error is 75, of the total-time-limit error is 75, of a missing-executable-receipt error is 1.
- [ ] **Step 2: Run it to see it fail**: `node --test scripts/vscode/download.test.cjs`. Expected: FAIL.
- [ ] **Step 3: Implement**; `scripts/ci/editor-host.sh` already passes the status through (`preserve_result`), so confirm with `bash -n scripts/ci/editor-host.sh` and no edit.
- [ ] **Step 4: Run** `node --test scripts/vscode/download.test.cjs`. Expected: PASS.
- [ ] **Step 5: Commit** by path: `fix(editors): a stalled VS Code download asks the pipeline to retry (exit 75)`.

---

### Task 11: A retried step across real replicas

**Files:**
- Modify: `test/pipelinehop/fakes_test.go` (`fakeExecutor.answer` hook), `test/pipelinehop/hop_db_test.go` (new case)

- [ ] **Step 1: Write the failing test** `TestAFleetStepRetriedOnTheAgentReplica`: a GitHub push delivered on replica A; replica B drives; the fake answers the first `Execute` of one step nothing ran (`AttemptNothingRan`, `Where.WorkerID: "laptop"`) and the second (asserting `req.Attempt == 2` and `req.Placement.PinWorker == "laptop"`) passes; the run concludes success; its check run text says "Passed on attempt 2"; the step row's binding carries both records.
- [ ] **Step 2: Run it to see it fail**: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<dsn> go test -count=1 -run TestAFleetStepRetried ./test/pipelinehop/`. Expected: FAIL before the hook, PASS after.
- [ ] **Step 3: Commit** by path: `test(pipelines): a step retried on the driving replica, across the mesh`.

---

### Task 12: Documentation and the decision map

**Files:**
- Modify: `docs/public/operate/pipelines.md` (the `repeatable` key, exit 75, retries and failover, the policy's `retries`, attempt classes), `docs/public/operate/pipelines-substrate.md` (where a step runs, the fleet, losing a replica or machine, when each side gives up, codes), `docs/public/operate/workers-runbook.md` (pin and exclude for pipeline steps; Host commands off), `docs/internal/ops/harness-decisions.md` (new section "Pipeline step attempts, retries and failover"), `docs/superpowers/specs/2026-10-28-pipeline-fleet-first-retries-failover-design.md` (replace `resumeAt` with `binding.nextAttemptAt`, with the reason)

- [ ] **Step 1: Write the decision-map section**: trigger (a failed attempt and its class), evidence (the executor's class, the row's history), chosen path (section 5 order), alternatives (DSL loop, executor loop, version shim), fallback and stop conditions (cap 8, run ceiling, policy, repeatable), invariants (never a second copy of an unknown outcome unless repeatable; never finished between attempts), what survives a takeover (row history and `nextAttemptAt`), the regression tests by name from Tasks 2-11, and what is unverified (live fleet-to-cloud failover, a production maintenance window).
- [ ] **Step 2: Update the public pages**; every new code and key appears in the codes and manifest tables, and the fleet section of `pipelines-substrate.md` says that worker contract 5 switches in lockstep and how the operator hold drains runs for the switch (spec D10, section 12).
- [ ] **Step 3: Run the docs gates**: `go test -count=1 -run 'Docs|Frontmatter|PublicBoundary' github.com/znasllc-io/memql` and `make frontdoor-paths-check` (unchanged paths). Expected: PASS.
- [ ] **Step 4: Commit** by path: `docs(pipelines): step attempts, retries and failover`.

---

### Task 13: Full verification and the PR

- [ ] **Step 1: Run everything**

`make test` (expect every package `ok`; the five PyYAML-backed tests need PyYAML on `python3`), `go test -tags agent -count=1 ./integrations/agent/ ./integrations/agent/worker/ ./integrations/pipelinesteps/ ./component/node/`, `go test -count=1 ./cockpit/...`, and for each tree in `scripts/ci/db-gated-packages.sh --trees`: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<dsn> go test -count=1 <tree>/...`. Record the counts and any failure verbatim.

- [ ] **Step 2: Rebase on `origin/main`** if memql#6339, memql#6369 or the per-step checks branch landed, resolving `driver.go` and `summary.go` conflicts, then rerun Step 1.

- [ ] **Step 3: Push and open the PR against `main`**, not merged, body ending with the Claude Code attribution line; state the rollout order (spec section 12), that the manifest and `memql-znas` changes follow the deploy, and that live verification is pending. Bind it with the ccd_pr tools and report CI. memql#6294 is closed only after this merges.
