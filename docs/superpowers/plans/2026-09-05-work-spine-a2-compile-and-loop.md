# Work Spine A2 -- Compile and the Loop Implementation Plan

**Goal:** A person gives a goal; the system compiles it against the catalog before it reaches a
model, runs it as journaled steps, verifies each one against a postcondition, and when something
misses, classifies the symptom with rules before a model and repairs from the failed step rather
than from the start. Human gates become rows a person can see and answer.

**Spec:** `docs/superpowers/specs/2026-09-05-work-spine-design.md`, sections B (compile), D
(approval, side effects, waits, modelCall, replay, retention), E (the loop), F (retired), H (entry
points), I (tiers). Read it first. This plan is epic A2; A1 (rows and journal) is a sibling epic
landing in parallel, and A3 (skills, Training) is another.

**Closes:** #4966 (epic), #4967, #4968, #4969.

---

## Global constraints

- **A2 sits on A1's rows and does not re-declare them.** `v1:work:{goal,run,step,modelCall,
  approval,observation}` are A1's. A2 adds constructs over them and never edits the concepts,
  except the three trust-ladder fields on `v1:authoring:construct` that promotion needs (below).
- **`dsl/skills/**` belongs to A1/A3, not here.** A2 consumes the skill catalog; it does not
  create or grow it.
- **`v1:planner:plan`, `task` and `taskState` are NOT retired in this epic.** Spec section F
  retires them and #4969's last checkbox gates that on A3's Training re-key, which has not landed.
  Retiring them here would break the Training app. This is the epic's one deliberate deferral and
  it is recorded in the PR body.
- **Verification is `make test`, never `go test ./...`** -- a relative pattern misses
  `component/memql`, `component/database` and `component/language`. Database-gated work runs with
  `MEMQL_REQUIRE_DB=1` against a real Postgres+TimescaleDB+pgvector.
- **Generated artifacts, in this order after any DSL change:** `go run ./cmd/memqllint dsl/`,
  `make sdk-gen` (gate `make sdk-gen-check`), `make arch-model` after any Go package add/move
  (gate `make arch-model-check`), `make env-registry-sync` + `make env-registry-check` if an env
  var landed.
- **Construct names are unique tree-wide** (one flat registry, first registration wins silently),
  which is why every construct here is prefixed `work`.
- No emojis. Hostnames in docs and comments are `example.com` or `<domain>`.

---

## The shape of the epic

Three layers, and the split between them is what makes the spec's headline claims testable:

| Layer | Where | What it holds |
|---|---|---|
| Decisions | `component/work/` (new leaf module) | Pure functions over values. No engine, no provider, no database. |
| Declarations | `dsl/work/` | The client-reachable reads, the five entry points, the three prompts, the two sweeps. |
| Wiring | `integrations/work/`, `integrations/planner/` | The executors, the compile pass, the heal subscriber. |

The spec's headline claims -- "a goal that fully matches the catalog makes zero provider calls", "a
symptom the rules classified makes zero calls" -- are properties of the DECISION layer, so they are
proved by a test over values rather than by counting calls against a mock. The wiring's only job is
then to obey the decision.

---

## Task 1: the decision layer (`component/work/`) -- DONE

A new leaf module, no internal dependencies, so the safety-gate and engine adapters live with the
wiring instead. `go.work` gains `./component/work`.

| File | Holds |
|---|---|
| `symptom.go` | The rules table (ordered, first match wins), the five symptoms, the five acts, `Evidence`. `ClassifyByRules` returning `ok=false` is the ONLY path that costs a model call. |
| `kind.go` | `DeriveKind`, `ReachesPrompt` (cycle-safe, names the path), `ValidateDeclaredKind` -- the loader rule that refuses a deterministic step reaching a prompt. |
| `postcondition.go` | Derivation for mutations (the row exists with the fields it wrote) and queries (its shape); `RequirePostcondition`, the rule that a deterministic step with none is refused. |
| `footprint.go` | The union over a call graph, sorted and deduplicated so the same step writes the same `expectedFootprint` every run. |
| `compile.go` | `Decide` -- the compile order -- and `GoalSignature`, the normalized-statement + sorted-arg-shape key. |
| `budget.go` | `CheckCeilings`. Dollar ceilings exclude subscription and local spend; loop caps include every call; zero means unset. |
| `approval.go` | The four approval builders, `ArtifactHash` (order-independent), `ResumeAllowed`. |
| `replay.go` | `DecideServe` -- live / replay / fork, and the cross-goal rule that outranks all three. |
| `idempotency.go` | `IdempotencyKey` and `OutboundRequestId`; attempt is part of the key. |

**Order is load-bearing in two places and both are tested:** the stall rule sits above the transient
matchers (a repeated action that also looks transient must escalate, not retry forever), and exact
catalog match sits above near match (a near match that could win would make the free path
unreachable exactly when it is most valuable).

**Why `GoalSignature` is new rather than reused.** The existing `CatalogKey` hashes construct
SOURCE per kind and refuses `automation` outright -- which is the kind a compiled goal is. So a goal
cannot be looked up by it at all.

---

## Task 2: the A2 DSL (`dsl/work/`) -- DONE

- **`queries.memql`** gains seven `@actor` owner-scoped reads (`workGoalForOwner`,
  `workRunsForOwner`, `workRunsForGoal`, `workRunForOwner`, `workStepsForOwnerRun`,
  `workModelCallsForOwnerRun`, `workObservationsForOwnerRun`). They are separate constructs from
  A1's `@serverOnly` twins because the two callers satisfy the owned tier by different arms, and
  one construct cannot spell both without widening the read for somebody.
- **`builtins.memql`** (new) declares the five entry points plus the two sweep handlers. Builtins
  rather than mutations because each is more than a row write and a mutation step cannot hand its
  inserted id to a later one.
- **`prompts.memql` + `prompts/*.tmpl`** (new): `compileGoal` (Sonnet, the expensive path, reached
  last), `classifySymptom` (Nano, reached only on a rules miss), `replanGap` (Sonnet, sees the
  remainder and never the prefix).
- **`automations.memql`** (new): `sweepWaitingWorkRuns` every two minutes,
  `workJournalRetentionSweep` nightly. Both scheduled, so the cron leader gates them.

**Two gates this task must satisfy and the plan for A1 did not anticipate:**

1. Both sweeps must be added to `maintenanceAutomations` in `component/auth/maintenance_actor.go`.
   The work concepts declare the composite owner tier; under the default `RoleReader` system actor
   the owned branch matches nothing, the cluster-owner escape does not apply, and the read answers
   ZERO ROWS AND NO ERROR. A sweep that resumes nothing is indistinguishable from a cluster with
   nothing parked.
2. `shippedAutomationCount` in `component/automations/strict_automation_boot_test.go` goes 48 -> 50.

An empty-argument builtin step (`builtin workSweepWaiting ()`) is refused at load (memql#4927):
it would load clean and fail every time the step RUNS, which for a scheduled automation is on a
timer nobody is watching. The arguments are spelled at the call site.

---

## Task 3: the executors (`integrations/work/`)

Registered via `memql.RegisterPlugin("work", ...)` from `init()`, blank-imported from
`app/plugins_core.go`.

| Handler | Does |
|---|---|
| `createGoal` | Writes the goal under the caller's actor, opens its first run in `compiling` under the owner's borrowed authority, stamps the budget scope, dispatches compile. |
| `cancelGoal` | Closes the goal and sets `cancelRequested` on live runs. REQUESTED, not done: a run notices at its next step boundary, so a step in flight finishes and is journaled rather than abandoned mid-effect. |
| `forkRun` / `replayRun` | Read the source run under the caller's actor, derive a new run with its mode, fork point and replay policy. |
| `decideApproval` | Recomputes the artifact hash, calls `ResumeAllowed`, REFUSES when it changed, then records the decision and returns the run to `running`. |
| `workSweepWaiting` | Resumes due timer waits; marks and re-claims runs whose heartbeat stopped. `lastHeartbeat` falls back `heartbeatAt -> startedAt -> createdAt`, and a row with none is left alone. |
| `workRetentionSweep` | Folds the run summary FIRST, archives, then deletes. No archive means no delete. |

**Three rules, each of which has bitten this repo:**

- Every `@serverOnly` call goes through `auth.ContextWithInternalOrigin`, and the stamped context
  stays a local -- a later frame inheriting it unlocks every other `@serverOnly` construct.
  Stamping is itself gated by `TestOnlyAllowlistedPackagesStampInternalOrigin` in the root package,
  so `integrations/work` needs an honest allowlist entry.
- Owned reads run under `auth.ContextWithUserActor(ctx, ownerUserId)`, and that owner is copied off
  a row the CALLER already read under their own actor -- so it can never name a user the caller
  could not act as.
- Sweeps run under `auth.MaintenanceActor`.

**The safety gate's ask sink is replaced here.** `component/safety`'s `ApprovalSink` currently
writes `v1:safety:approvalRequest` and its own header records the gap: "No Plan-parking integration
(the gate just returns a refusal referencing the row id)." A2 supplies a sink that writes a
`v1:work:approval` of kind `sideEffect` with `artifactHash = safety.ApprovalCorrelationKey(desc)`
and `evidence` straight off the `Classification`, which is why the two field sets match exactly.

**Env:** `MEMQL_WORK_MODELCALL_RETENTION_DAYS` (90), `MEMQL_WORK_OBSERVATION_RETENTION_DAYS` (180),
`MEMQL_WORK_CAPTURE_ENABLED` (on) replacing `MEMQL_ACTION_REPLAY_ENABLED`,
`MEMQL_AUTHORING_CAPTURE_ENABLED` and `MEMQL_AUTHORING_CAPTURE_MODE`. All in the env manifest.

---

## Task 4: compile, in `integrations/planner/`

Compile lives here rather than in `component/work` because the authoring pipeline's entry points are
unexported methods on `*PlannerAgentLoop`, and exporting them to move one caller would widen a
surface for no gain.

The order, obeying `work.Decide`:

1. **Exact.** `cataloguedConstructsForOwner` under the owner's actor, matched on `GoalSignature`.
   A hit reaches no model at all -- not even triage.
2. **Near.** `CatalogNearMatches` at the existing 0.82 threshold, with the gap list.
3. **Triage.** `classifySectionable` -- ONE call answering both complexity and sectionability.
4. **Trivial** -> a one-step run. **Sectionable** -> `maybeGenerateSectionable`, deterministic
   after that one call. **Otherwise** -> `compileGoal`, then Gate 1.

**Gate 1's vacuity trap must be re-asserted.** `SandboxReport.OK` is true when every NON-SKIPPED
construct compiled, and a kind whose compiler hook is not linked reports `Skipped: true` without
failing the bundle. Copy `requireAutomationsActuallyCompiled` from
`agent_loop_authoring_gate1_hook_test.go`, or Gate 1 comes back green having validated nothing.

**`repairBudgetExhausted` reads `v1:planner:plan` and fails OPEN on a load error**, running the
full four-attempt repair loop with no ceiling. For a goal run it must read the run's ceilings
instead. That is a cost bug that looks like nothing.

---

## Task 5: the loop -- verify and miss

- **Postconditions** are derived where they are free and declared otherwise; the loader refuses a
  deterministic step with none, and a failure writes `symptom=contract`.
- **The miss path** runs `ClassifyByRules` first and `classifySymptom` only on a miss, then acts:
  retry inside the budget, heal, repair from the failed step, replan the gap, or ask.
- **Healing is a SUBSCRIBER, not a new loop.** `component/healing` already proposes the four typed
  patches, and `healing.precondition.missed` is already emitted and broadcast across the mesh --
  but **nothing subscribes to it and `NewRepairLoop` has no non-test caller.** A2's heal arm is
  wiring that subscriber and carrying its patches to a person as a `planReview` approval (D5:
  never a silent edit, even to the run's own draft template).
- **Side effects** stage through `stageOutboundRequest` under `OutboundRequestId`. The row id IS
  the idempotency handle, and `@createOnly("status","attempts")` is what keeps a re-stage from
  rewinding a row the drainer already sent.
- **Waits** park the run with `waitingOn`; no process is held open for any of them.

---

## Task 6: promote and govern

- **Promotion** catalogues the construct and offers a repeatedly-successful template back as a
  responsibility. `v1:authoring:construct` gains `reliability`, `reinforceCount` and
  `lastReinforced` -- they do NOT exist there today (the ladder lives on `v1:actions:action` and
  moves to `v1:skills:skill` in A3), so this epic adds them rather than reusing them.
- **Capture runs in-line** at the end of a run rather than as the detached
  `AuthoringCaptureDispatcher` job. Note the default mode is the DETERMINISTIC transcript path, not
  the LLM re-author -- `agent_loop_authoring_transcript.go` is the model to follow.
- **Governance** checks `CheckCeilings` before each model call and parks with a `budget` approval;
  the safety chain runs at every side-effect edge. `ai_guard.go`'s process-wide ceiling and
  identical-request breaker stay exactly as they are.

---

## Task 7: the MemQL OS Work app

`clients/os/src/apps/work/`. Goals, the run timeline, the approval inbox, and the journal on
demand. The design record scopes the Nexus surface to sub-project B, but A2 ships `decideApproval`
with no surface at all, which reproduces exactly the defect the record names about the planner's
canvas cards: in an engine-only cluster those approvals were already invisible. The inbox is the
honest minimum.

`goal`, `run`, `step` and `approval` broadcast, so those lists are live; `modelCall` and
`observation` do not, so the journal is an on-demand read that says when it was last read.

---

## Task 8: docs and the PR

The CLAUDE.md sections on the planner and the coding-agent seam are rewritten. One PR, closing
#4966, #4967, #4968 and #4969, stating the planner-concept deferral explicitly.

---

## Plan self-review

- **Spec coverage.** B: tasks 1, 2, 4. D: tasks 1, 3, 5 (modelCall serving is task 1's `DecideServe`
  plus task 3's journal reads). E: tasks 1, 5, 6. F: task 6, minus the planner concepts, deferred
  with its reason. H: tasks 2, 3. I: task 2. J: every task carries its tests; the two headline
  claims are task 1's.
- **What this plan does NOT cover, and why.** The parts of the loop that hook the automation
  executor's journal writer -- resume-with-approval and the in-process cross-node hop tests -- sit
  on A1's `component/automations/journal.go`, which is landing in a sibling epic. They are written
  against its interface and cannot be verified end to end until it merges.
- **Two facts re-verified at execution rather than trusted from the record:** that
  `v1:authoring:construct` has no reliability fields (it does not), and that the healing repair loop
  has a subscriber (it does not).
