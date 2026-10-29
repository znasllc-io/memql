---
title: Pipeline AI local first -- implementation plan
audience: internal
status: draft
area: operate
sinceVersion: 0.31.8
owner: znas
---

# Pipeline AI Local First Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pipeline AI (independent review, failed-run diagnosis, repair change) runs on local fleet models first, then a signed-in app, then a paid vendor, and each local failure says why.

**Architecture:** Five independent fixes (D1-D5 in the spec): a Cockpit fallback when Ollama cannot compile a schema, a typed machine failure code, per-machine fleet eligibility with a strict embedder rule, an inference-only app descriptor plus the app-gate exemption it unlocks, and a locked `pipelineLocalFirst` rule keyed on a `pipeline` call tag.

**Tech Stack:** Go 1.26 (engine and Cockpit modules), protobuf (worker.proto), MemQL DSL (rules, policies).

**Spec:** `docs/superpowers/specs/2026-10-29-pipeline-ai-local-first-design.md`

## Global Constraints

- Work in the worktree `.claude/worktrees/pipeline-ai-local-first`, branch `claude/pipeline-ai-local-first`, PR against `main`, not merged (release trains land it).
- Stage files by explicit path; never `git add -A`. Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Do not touch `component/pipelinerun/` (another session's release-train work) or pipeline step placement.
- The repo is public: no production run ids, user ids, registration ids or machine names in code, tests or docs.
- Comments explain WHY a branch exists (trigger, evidence, chosen path, fallback); no emojis.
- Iterate with package tests; final verification is `make test` plus the db-gated and agent-tagged lanes listed in Task 7.
- Agent-tagged packages (`integrations/agent/worker` app files) test with `-tags agent`.
- Proto regeneration: `scripts/dev/proto-gen.sh --only=component/grpc` with the pinned toolchain copied from the main checkout's `bin/tools/` (no download).

## Review Focus

1. A Cockpit that does not know the new descriptor field (older 0.20.x) must keep today's app refusal for Auto goals: absent `inferenceOnly` reads as false. Pinned in Task 5.
2. A schema refusal 400 whose text does not mention a grammar (for example "does not support chat") must not trigger the prompt fallback. Pinned in Task 1.
3. A forwarded app call must be judged on the holder by the holder's live descriptor, not the sender's row. Pinned in Task 5 (hop test).
4. A pipeline run whose goal row cannot be read must still execute, untagged, with a logged reason. Pinned in Task 6.
5. A model whose only chat-capable claim comes from an offline machine must not be a candidate. Pinned in Task 3.

---

### Task 1: Cockpit falls back to the prompt when the runtime cannot compile the schema (D1)

**Files:**
- Modify: `cockpit/internal/worker/modelcall/ollama_schema.go` (`ollamaFormatUnsupported`)
- Modify: `cockpit/internal/worker/modelcall/ollama.go:57-62` (the call site)
- Modify: `cockpit/internal/worker/models/ollama.go` (comment beside `ollamaStructuredOutput` naming the 501-only fallback)
- Modify: `cockpit/docs/local-models.md` (structured output section)
- Test: `cockpit/internal/worker/modelcall/ollama_schema_test.go`

**Interfaces:**
- Produces: `ollamaSchemaRefused(err error) bool` replacing `ollamaFormatUnsupported` -- true for status 501, or status 400 whose detail contains "grammar" (case-insensitive).

- [ ] **Step 1: Write the failing tests** beside `TestStructuredCallOnARunnerWithoutFormatAsksInThePrompt`, using the same fake server:
  - `TestStructuredCallTheRuntimeCannotCompileAsksInThePrompt`: first `/api/chat` (with `format`) answers 400 `{"error":"{\"error\":{\"code\":400,\"message\":\"Failed to initialize samplers: failed to parse grammar\",\"type\":\"invalid_request_error\"}}"}`; the second request has no `format`, its system message contains `schemaInstruction`, and answers schema-valid JSON; the call emits exactly that JSON.
  - `TestStructuredCallKeepsOtherBadRequests`: 400 `{"error":"\"m\" does not support chat"}` returns that error after ONE request.
- [ ] **Step 2: Run** `go test ./cockpit/internal/worker/modelcall/ -run 'TestStructuredCall' -count=1` -- expect the first new test to FAIL (400 returned).
- [ ] **Step 3: Implement** `ollamaSchemaRefused`; the comment records the 2026-10-28 evidence (Ollama 0.33.3 400 at sampler init, 0.40.6 compiles the same schemas) and that Ollama answers it before generating anything, which is what makes one retry safe.
- [ ] **Step 4: Run** the same command -- PASS, including the existing `TestStructuredCallKeepsOtherRefusals`.
- [ ] **Step 5: Update** the two comments and `cockpit/docs/local-models.md`; commit `cockpit: ask for the JSON in the prompt when the runtime cannot compile the schema`.

### Task 2: A machine's failure keeps its code (D2)

**Files:**
- Create: `component/worker/modelcall_failed.go`
- Modify: `component/worker/server.go:2025-2028` (the handle's error)
- Modify: `integrations/agent/worker/model_forward.go:358-366` (holder's response code)
- Modify: `integrations/agent/worker/fleet_inference.go` (`attempt` and `attemptLocal`, the `!out.Ok()` returns)
- Modify: `component/router/failure_reason.go` (`failureWords`)
- Test: `component/worker/modelcall_failed_test.go`, `integrations/agent/worker/fleet_inference_test.go` (or the existing forward test file), `component/router/failure_reason_test.go` (create if absent)

**Interfaces:**
- Produces in `component/worker`:
  ```go
  type ModelCallFailed struct{ Machine, ErrorCode, Message string }
  func (e *ModelCallFailed) Error() string // Machine != "": "<Machine>: <ErrorCode> <Message>"; else "worker: model call failed: <Message>" -- today's two texts
  func (e *ModelCallFailed) Code() string  // e.ErrorCode
  ```
- `failureWords` gains `"runtime_error": "the machine's model runtime failed the call"`, `"model_call_failed": "the machine's model call failed"`, `"worker_stopped": "the machine's worker stopped before the call finished"`.

- [ ] **Step 1: Failing tests**
  - `TestAFinishedCallWithAnErrorKeepsTheMachinesCode` (component/worker): a model call whose End carries `ErrorCode: "runtime_error"` makes `handle.Wait` return an error with `errors.As(*ModelCallFailed)` and `Code() == "runtime_error"`, text unchanged.
  - `TestTheHolderForwardsTheMachinesCode` (integrations/agent/worker): the forward response's `ErrorCode` is `runtime_error`, not `model_call_failed`; a deadline error still answers `model_call_failed`.
  - `TestAMachineFailureIsNamedOnTheDecisionLine` (component/router): `FailureReason(&worker.ModelCallFailed{ErrorCode: "runtime_error"})` -- use a local coded fake if the router module cannot import component/worker -- equals `failed when called: runtime_error: the machine's model runtime failed the call`.
- [ ] **Step 2: Run** `go test ./component/worker/ ./component/router/ -run 'Machine|Holder|FinishedCall' -count=1` and `go test -tags agent ./integrations/agent/worker/ -run TestTheHolderForwardsTheMachinesCode -count=1` -- FAIL.
- [ ] **Step 3: Implement**; the sender wraps a completed failure as `&workerservice.ModelCallFailed{Machine: cand.Label(), ErrorCode: out.ErrorCode, Message: out.ErrorMessage}`; transport errors and refusals before start stay as they are. The comment says why: the decision record printed `other` for a runtime refusal on 2026-10-28.
- [ ] **Step 4: Run** the Step 2 commands -- PASS; `grep -rn model_call_failed --include=*_test.go .` shows no test that depended on the old code for a machine failure.
- [ ] **Step 5: Commit** `fleet: a machine's failure keeps its code on the decision record`.

### Task 3: Fleet eligibility per online machine; an embedder never answers a chat (D3)

**Files:**
- Modify: `component/memql/fleet_provider.go` (`FleetMachine`, `FleetModel.eligibleFor`)
- Modify: `component/worker/fleetcatalog/catalog.go` (`Project` fills the per-machine window)
- Modify: `component/worker/fleetcatalog/model.go` (`ModelAttributes.Satisfies` embedder rule)
- Modify test fixtures: `component/router/selectors_test.go` (`sizedModel` stops claiming embeddings and fills machine attributes; add `sizedEmbedder` for embedding tests), and the FleetMachine literals in `component/memql/{fleet_provider,fleet_model_setup,fleet_capacity,work_fleet_capabilities,fleet_measured,fleet_catalog_read,fleet_ordering}_test.go`, `component/router/fleet_park_test.go`, `integrations/agent/work_fleet_context_test.go`, `app/materializer_composer_test.go` where a test relies on a capability or a window
- Test: `component/memql/fleet_ordering_test.go`, `component/worker/fleetcatalog/model_test.go`, `component/worker/fleetcatalog/catalog_test.go`

**Interfaces:**
- `FleetMachine` gains `ContextWindow int` (this machine's advertised window for the model; zero when it advertised none).
- `eligibleFor` answers true only when ONE online machine passes, together: `ModelSetup.Allows(model, purpose, floor)`, every need against that machine's `Capabilities`, the floor against that machine's `ContextWindow`, and (call not an embedding) `!Capabilities.Embeddings`. The reason when none passes is the first online machine's miss, worded as today (`"an embedding model answers embedding calls only"`, `"does not advertise structured output"`, ...). A model with no machines keeps the model-level checks.
- `Satisfies`: `!n.Embeddings && a.Embeddings` refuses, without the tools/structured/vision allowance.

- [ ] **Step 1: Failing tests**
  - `TestAnEmbedderIsNeverAChatCandidateEvenWhenAMachineClaimsTools` (memql): online machine A advertises `embeddings,tools,structured`, online machine B `embeddings` -> structured call refused with `"an embedding model answers embedding calls only"`.
  - `TestACapabilityOnlyAnOfflineMachineClaimsServesNothing` (memql): offline machine advertises structured, online one does not -> refused.
  - `TestSetupAndCapabilityMustHoldOnTheSameMachine` (memql): A advertises structured but its setup forbids the purpose; B allows it but does not advertise structured -> refused.
  - `TestTheContextFloorIsTheServingMachinesWindow` (memql): model-level window 262144 from an offline machine, online machine 32768, floor 40000 -> refused.
  - `TestSatisfiesRefusesAnEmbedderThatClaimsTools` (fleetcatalog): replaces the memql#6238 "still answers" case.
  - `TestProjectKeepsEachMachinesWindow` (fleetcatalog).
- [ ] **Step 2: Run** `go test ./component/memql/ -run 'Embedder|OfflineMachine|SameMachine|ServingMachines' -count=1` and `go test ./component/worker/fleetcatalog/ -count=1` -- FAIL.
- [ ] **Step 3: Implement**; the comment on `eligibleFor` states the 2026-10-28 evidence (union over offline and pre-#551 registrations), and the comment on both embedder checks states why "never" is safe: the current Cockpit gates tools and structured on `completion`, so a pairing comes only from an older Cockpit.
- [ ] **Step 4: Update the fixtures**, then run `go test ./component/memql/ ./component/router/ ./component/worker/... -count=1`, `go test ./integrations/agent/ -count=1` and `go test -tags agent ./app/ -run Materializer -count=1` -- PASS.
- [ ] **Step 5: Commit** `fleet: decide eligibility on the online machine that would serve the call`.

### Task 4: Machines declare app turns they run without tools (D4, wire)

**Files:**
- Modify: `component/grpc/worker.proto` (`AppDescriptor`: `bool inference_only = 5;` documented), regenerate `component/grpc/gen/worker.pb.go`
- Modify: `cockpit/internal/worker/apps/apps.go` (`Spec.InferenceOnly`, `Info.InferenceOnly`; true for `claude-headless` and the `codex-app-server` upgrade, false for `codex-mcp`), `cockpit/internal/worker/apps/detect.go` (copy into Info), `cockpit/internal/worker/appinventory.go` (`appDescriptorsToProto`)
- Modify: `component/worker/apps.go` (`AppDescriptor.InferenceOnly`, json `inferenceOnly`; `AppDescriptorsFromProto`), `component/worker/store.go` (`descriptorsAsMaps` and the row reader), `dsl/worker/concepts.memql` (`appDescriptors` description)
- Test: `cockpit/internal/worker/apps/apps_test.go`, `cockpit/internal/worker/appinventory_test.go`, `component/worker/apps_test.go`, the store round-trip test beside `descriptorsAsMaps`

**Interfaces:**
- Produces: `workerservice.AppDescriptor.InferenceOnly bool` (absent reads false), consumed by Task 5.

- [ ] **Step 1: Failing tests**: `TestInferenceOnlyIsDeclaredOnlyByHarnessesThatHonourIt` (cockpit apps), `TestDescriptorsCarryInferenceOnly` (appinventory), `TestInferenceOnlySurvivesTheRegistrationRow` (component/worker: proto -> descriptor -> row map -> descriptor).
- [ ] **Step 2: Run** `go test ./cockpit/internal/worker/apps/ ./cockpit/internal/worker/ -run InferenceOnly -count=1` and `go test ./component/worker/ -run InferenceOnly -count=1` -- FAIL (compile).
- [ ] **Step 3: Implement** and regenerate (`cp -R ../../../bin/tools bin/` if absent, then `scripts/dev/proto-gen.sh --only=component/grpc`; only `worker.pb.go` changes).
- [ ] **Step 4: Run** the Step 2 commands plus `go test ./cockpit/... -count=1` -- PASS.
- [ ] **Step 5: Commit** `worker: machines declare app turns they run without tools`.

### Task 5: A tool-free app turn in an Auto goal is a model call (D4, gate)

**Files:**
- Modify: `integrations/agent/worker/app_gate.go` (`admitAppSession`)
- Modify: `integrations/agent/worker/app_inference.go` (`Call`, `ServeForwardedAppCall`, `selectMachine`, `callOnWorker`)
- Modify: `integrations/agent/worker/app_forward.go` (`appCandidateRefusal`)
- Modify: `integrations/agent/worker/cockpitapp.go:185` (session door passes `false`)
- Modify: `docs/public/operate/local-apps.md`, `cockpit/docs/local-apps.md` (inference-only turns and the destination)
- Test: `integrations/agent/worker/app_gate_test.go`, `integrations/agent/worker/app_forward_hop_test.go`

**Interfaces:**
- Consumes: `AppDescriptor.InferenceOnly` (Task 4); `memqlengine.ExecutionDestinationRequired{From, To}`.
- Produces:
  ```go
  // appTurnToolFree is the one predicate: it sets AppSessionStart.inference_only and admits the turn below.
  func appTurnToolFree(req memqlengine.AppCallRequest) bool // len(req.Images) == 0 && len(req.Inputs) == 0
  func admitAppSession(ctx, prefs, logger, owner string, pin memqlengine.AppDoorPin, runID string, toolFree bool) (needsToolFreeMachine bool, refusal *appGateRefusal)
  func appToolFreeRefusal(d workerservice.AppDescriptor, known bool) string // "" when d.InferenceOnly
  ```
- Rule: the destination error is waived ONLY when `toolFree` and `errors.As(err, *ExecutionDestinationRequired)` with `From == "auto"`; then `needsToolFreeMachine` is true and every machine check (local select, remote row, holder) refuses a machine whose descriptor is absent or not `InferenceOnly`, reason `"its Cockpit cannot run <app> without tools, and this goal has not been given your computer"`.

- [ ] **Step 1: Failing tests** (fixture `newAppGateFixture`, `chatTurn`, `handedOverStep`):
  - `TestAToolFreeTurnInAnAutoGoalRunsOnAMachineThatDeclaresIt`: `destinationErr = &ExecutionDestinationRequired{From: "auto", To: "computer"}`, descriptor `InferenceOnly: true` -> one session started, `InferenceOnly` set.
  - `TestAToolFreeTurnSkipsAMachineThatCannotRunWithoutTools`: same, descriptor without the flag -> `AppUnavailable` naming the reason, no session.
  - `TestAnAutoGoalStillRefusesATurnWithInputs`, `TestAWorkbenchGoalStillRefusesAToolFreeTurn`, `TestTheSessionDoorStillNeedsDestinationConsent` -> `AppGateDestination`, no session.
  - `TestAToolFreeTurnIsJudgedOnTheHoldersMachine` (hop file): the sender's row claims the flag, the holder's live descriptor does not -> refused at the holder before start.
- [ ] **Step 2: Run** `go test -tags agent ./integrations/agent/worker/ -run 'ToolFree|AutoGoal|WorkbenchGoal|SessionDoorStill|DestinationConsent' -count=1` -- FAIL.
- [ ] **Step 3: Implement**; the comment beside the waiver records why (an inference-only turn runs with no tools, like a local model, and a pipeline goal has nobody to ask) and its limits (Auto only, tool-free only, the machine must declare it).
- [ ] **Step 4: Run** Step 2 plus `go test -tags agent ./integrations/agent/worker/ -count=1` -- PASS, including `TestDestinationConsentClosesBothAppDoorsBeforeStarting`.
- [ ] **Step 5: Update** both local-apps docs; commit `apps: a tool-free turn in an Auto goal is a model call`.

### Task 6: The pipeline tag, rule and policy (D5)

**Files:**
- Modify: `core/airoute/request.go` (`TagPipeline = "pipeline"`)
- Modify: `core/common/modelcall.go` (`RunContext.Tags []string`)
- Modify: `component/memql/ai_attribution.go` (`AICallAttribution.Tags`; zero check counts it; `applyCallAttribution` appends attribution tags then run tags to `req.Tags`, de-duplicated, caller's own first)
- Modify: `app/work_execution_context.go` (`workExecutionContext(..., tags []string)` copies into `RunContext.Tags`), `app/integrations_work_dispatch.go` (reads `workGoalForOwner(goalId)` under the restored owner and passes `pipelineCallTags(goal)`; a failed read logs and passes nil)
- Modify: `app/pipeline_independent_review.go` (`invokePipelineIndependentReview` stamps `Tags: [TagPipeline]`, keeping any attribution already on the context)
- Modify: `dsl/policies/policies.memql` (`localFirstStrongest`: `@primary("fleet:strongest") @fallback("app:*") @fallback("federation:strongest")`), `dsl/rules/rules.memql` (`pipelineLocalFirst`: `@when(level="reasoning", tag="pipeline") @policy("localFirstStrongest") @precedence(105) @onUnavailable("park") @locked`)
- Modify: `test/dslconformance/local_first_policies_test.go` (`shippedPolicies`, `shippedRules`)
- Modify: `docs/public/operate/ai-routing.md` (rules table row and a short "Pipeline work" paragraph with the 1 MiB floor)
- Test: `component/memql/ai_attribution_test.go`, `component/router/rules_test.go`, `component/router/auto_rank_test.go`, `app/work_execution_context_test.go`, `app/pipeline_independent_review_test.go`, `component/router/selectors_test.go`

**Interfaces:**
- Produces: `airoute.TagPipeline`; `func pipelineCallTags(goal map[string]any) []string` (`[TagPipeline]` when `requestedVia == "pipeline"`, else nil).

- [ ] **Step 1: Failing tests**
  - `TestCallTagsFromTheCallerAndTheRunReachTheRequest` (memql): attribution `{Tags: [pipeline]}` and run `{Tags: [pipeline, x]}` on a request already tagged `background` -> `[background pipeline x]`.
  - `TestAPipelineReasoningCallMatchesThePipelineRule` (router): rules `default`, `reasoningParks` (100), `pipelineLocalFirst` (105); reasoning+pipeline -> `pipelineLocalFirst`; reasoning alone -> `reasoningParks`; fast+pipeline -> not `pipelineLocalFirst`.
  - `TestAutoLeavesThePipelineChainInItsWrittenOrder` (router): `autoRankChain` on `localFirstStrongest` returns the chain unchanged with no note.
  - `TestAFullSizeReviewSkipsTheLocalDoor` (router): floor `airoute.EstimateMinContextTokens(strings.Repeat("x", 1<<20), 0)` is 266240 and a `fleet:strongest` model with window 262144 is not a candidate.
  - `TestExecutionHopCarriesTheRunsCallTags` (app): `workExecutionContext(ctx, j, nil, nil, []string{"pipeline"})` -> `RunContext.Tags == [pipeline]` with no opener context; `pipelineCallTags` for `requestedVia` pipeline / ask / absent.
  - `TestTheIndependentReviewCallIsTaggedPipeline` (app): the review context's attribution carries the tag.
- [ ] **Step 2: Run** `go test ./component/memql/ -run CallTags -count=1`, `go test ./component/router/ -run 'PipelineRule|WrittenOrder|FullSizeReview' -count=1`, `go test -tags agent ./app/ -run 'CallTags|TaggedPipeline' -count=1` -- FAIL.
- [ ] **Step 3: Implement**; the rule's `///` comment says what the rule is for, why it outranks `reasoningParks` (an owner rule cannot: locked rules evaluate first), why its policy is outside Auto, and why it parks.
- [ ] **Step 4: Run** Step 2 plus `go test ./test/dslconformance/ -count=1` and `go test ./component/router/ -count=1` -- PASS.
- [ ] **Step 5: Update** `docs/public/operate/ai-routing.md`; commit `routing: work a pipeline started runs local first, in order`.

### Task 7: Decision map, full verification, PR

**Files:**
- Modify: `docs/internal/ops/harness-decisions.md` (section "Pipeline AI runs local first (2026-10-29)": the five decisions, triggers, fallbacks, regression tests, known limits from the spec)
- Delete: `docs/superpowers/plans/2026-10-29-pipeline-ai-local-first.md` (a spent plan is deleted in its own merge)

- [ ] **Step 1: Write** the decision-map section; link the spec and each regression test.
- [ ] **Step 2: Run** `make test` -- expect `ok` for every package; environment failures (Tailwind signing, PyYAML) are reported as such, not fixed here.
- [ ] **Step 3: Run** the db-gated and agent lanes against the local test database: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<memql-testdb> go test -tags agent -count=1 -timeout=900s ./app/` and `... ./integrations/agent/ ./integrations/agent/worker/ ./component/node/`, plus `go test -count=1 ./component/memql/...` with the same DSN.
- [ ] **Step 4: Fresh review** of the whole branch; fix confirmed findings.
- [ ] **Step 5: Delete the plan, commit, push, open the PR** against `main` (not merged), body listing D1-D5, owner actions and unverified behavior.
