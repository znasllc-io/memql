# Work Spine A1 -- Rows and Journal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every automation execution becomes a `v1:work:run` row with one `v1:work:step` row per step, written by the executor at each boundary and read back by resume; the work, skills and memory namespaces exist with their tiers and routing rules; the checkpoint side-record, the harness spine and the action capture library are retired; the portal's Nexus is deleted and its pure scene library moves to the OS.

**Architecture:** The automation executor (`component/automations`) gains a journal writer that renders `@serverOnly` MemQL mutation calls under a synthetic cluster actor and runs them through the engine; the run row is opened before the first step, each step row is written at `running` before its body and again at `done`/`failed`/`skipped` after, and the run closes with its terminal status. Resume loads the run and its step rows through `@serverOnly` queries and rehydrates the evaluator exactly as the checkpoint path did, under the same source-trust rule. The DSL namespaces are ordinary embedded domains; the harness pack, the harness reconciler, the action capture machinery and the portal's Nexus are deleted rather than adapted.

**Tech Stack:** Go 1.26 (multi-module workspace, `make test`), MemQL DSL (`.memql`, `cmd/memqllint`), PostgreSQL + TimescaleDB for db-gated tests (`MEMQL_REQUIRE_DB=1`), TypeScript + vitest for `clients/os` and `clients/portal`.

**Spec:** `docs/superpowers/specs/2026-09-05-work-spine-design.md` (sections A, B, D, F, I, J, K; decisions D4, D7, D8). Read it first. This plan is epic A1 only: A2 (compile and the loop) and A3 (skills grown, Training re-keyed) are later epics.

## Global Constraints

- **Names (spec D4):** `v1:work:goal`, `v1:work:run`, `v1:work:step`, `v1:work:modelCall`, `v1:work:approval`, `v1:work:observation`; `v1:skills:skill`, `v1:skills:skillEdge`; `v1:memory:belief`, `v1:memory:consolidationCursor`. Templates stay `v1:authoring:*`. `responsibility` stays in the planner namespace until epic A2.
- **Tier (spec section I):** every work and memory concept declares `@rowAuthz(owner="ownerUserId", clusterOwner)`. `ownerUserId` is `string @serverSet` WITHOUT `!`: a row written by a synthetic cluster actor gets its owner blanked to present-and-empty (`component/memql/rowauthz_nonprincipal_owner.go`), which is the deployment's row. Do NOT add `unowned=`: the parser requires `rankVisible` beside it, and `rankVisible` would make every person's goals readable by their peers.
- **Skills tier (amendment, Task 1):** `v1:skills:skill` and `v1:skills:skillEdge` declare `@rowAuthz(public, requiresIdentity)`, because the predefined catalog is read by every signed-in person's agents.
- **Pre-release (spec D8):** no shims, no compatibility layers, no migration of existing rows. Rows under retired concepts stay in the hypertable untouched.
- **Every change goes through a branch and a PR; `main` refuses direct pushes.** Stage by explicit path (`git add <file>`), never `git add -A`. Commit messages end with the two trailers below.
- **Verification is `make test`, never `go test ./...`** (root CLAUDE.md, Testing). Database-gated tests run with `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<a real Postgres+TimescaleDB+pgvector>`; an open port 5432 from the k3d load balancer is not a database.
- **Generated artifacts, in this order after any DSL change:** `go run ./cmd/memqllint dsl/`, `make sdk-gen` (gate: `make sdk-gen-check`), `make arch-model` after any Go package add/move/delete (gate: `make arch-model-check`).
- **A new construct name must be unique across the WHOLE tree** (one flat registry; first registration wins silently). Every construct this plan adds is prefixed `work`, `skill` or `memory` for that reason.
- **A new automation is a two-file change:** `shippedAutomationCount` in `component/automations/strict_automation_boot_test.go` must be updated when the count changes (Task 17 moves one automation and the count stays the same).
- **Adding a QUERY over a concept that declares no tier fails the shrink-only ratchet** (`component/memql/rowauthz_undeclared_gate_test.go`). Every concept here declares a tier; entries for retired constructs are DELETED from that map, never left.
- **`@unbounded` cannot be combined with `sort` or `paginate`.** A run-scoped read is bounded by the run and carries `@unbounded("...")`; an owner list carries `sort "row.createdAt", "desc"`.
- **Hostnames in docs and comments are `example.com` or `<domain>`.** `TestNoVendorDomainLiterals` scans the whole tree.
- **No emojis** in docs, comments or user-facing strings.
- **Commit trailers (every commit):**
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01LXdTWPHNiDZUUgyggSAaoT
  ```
- **Branch discipline (the owner's rule: at most two PRs per epic, because PRs are the bottleneck):** two PRs, sequential. PR 1 of 2 is tasks 1-13 on branch `feat/work-spine-a1-rows-and-journal` and closes #4963 and #4964; PR 2 of 2 is tasks 14-21 on branch `feat/work-spine-a1-retire` off the merged `main` and closes #4965. Enqueue with the bare `gh pr merge <n> --repo znasllc-io/memql`; the owner merges their own PRs with `scripts/dev/merge-as-owner.sh --pr=<n>`.

---

## File structure

**PR 1 of 2, first half -- rows (branch `feat/work-spine-a1-rows-and-journal`)**

| Path | Responsibility |
|---|---|
| `docs/superpowers/specs/2026-09-05-work-spine-design.md` | two amendments (Task 1) |
| `dsl/work/concepts.memql` (new) | goal, run, step, modelCall, approval, observation |
| `dsl/work/shapes.memql` (new) | one full projection per concept |
| `dsl/work/queries.memql` (new) | the server-only reads resume needs, plus the two owner-facing lists |
| `dsl/work/mutations.memql` (new) | the server-only writes the journal makes |
| `dsl/embed.go` | `all:work all:skills` in the embed directive |
| `component/database/memory-nodes/concept_ids.go` | the six work constants |
| `component/node/routing.go` | broadcast rules for goal, run, step, approval |
| `component/node/routing_reach_test.go` | the new rules asserted |
| `dsl/skills/concepts.memql`, `shapes.memql`, `queries.memql`, `mutations.memql`, `seeds/*.memql` (new, from `dsl/agents`) | the skill catalog under its own namespace, plus skillEdge |
| `dsl/agents/concepts.memql`, `shapes.memql`, `queries.memql`, `mutations.memql` | the skill constructs removed |
| ten Go files, three TS files (listed in Task 6) | `v1:agents:skill` becomes `v1:skills:skill` |
| `sdk/go/client/generated_*.go`, `sdk/ts/src/generated/*` | regenerated |

**PR 1 of 2, second half -- journal (the same branch)**

| Path | Responsibility |
|---|---|
| `component/automations/journal.go` (new) | the run and step row writer |
| `component/automations/journal_test.go` (new) | the exact calls, without a database |
| `component/automations/executor.go` | the hooks at each boundary; checkpoint call sites removed |
| `component/automations/resume.go` | loads the journal instead of a checkpoint |
| `component/automations/resume_test.go` (new) | validation and the retryable rule |
| `component/automations/scheduler.go` | `ResumeAutomation` re-pointed |
| `component/automations/checkpoint.go` (deleted), `types.go` (checkpoint types removed) | |
| `component/automations/journal_db_test.go` (new) | rows written and resume, against Postgres |
| `dsl/memql/concepts.memql` | the checkpoint concept removed |
| `component/database/memory-nodes/concept_ids.go` | `ConceptMemQLCheckpoint` removed |
| `docs/public/language/memql.md` | the resume paragraph re-pointed |

**PR 2 of 2 -- retire (`feat/work-spine-a1-retire`)**

| Path | Responsibility |
|---|---|
| `component/actions/{pin,bind,fingerprint,surfaceresolver}` (moved from `component/harness/*`) | the authored-action runtime's pure helpers |
| `component/harness/**`, `cmd/harness-eval`, `cmd/action-upgrade`, `app/integrations_harness_*.go`, `integrations/actionsearch`, `integrations/planner/action_substitution.go`, `component/memql/harness_step_validation.go` (deleted) | the harness spine and the capture library |
| `dsl/actions/*.memql` | capture-only constructs removed |
| `dsl/memory/**` (new, from `dsl/harness`), `dsl/harness/**` and `dsl/harness_pack.go` (deleted) | beliefs, the cursor, consolidation, recall |
| `component/memql/memory_consolidation.go` (renamed) | the consolidation helpers |
| `integrations/worktrace` (renamed from `harnesstrace`) | the `workTrace` builtin |
| `integrations/harnessrecall/recall.go` | default concept `v1:work:observation` |
| `component/memql/executor_mutation.go` | the observation embed hook re-pointed; the step guard and the action-intent hook removed |
| `go.work`, `scripts/ci/db-gated-packages.sh`, `.github/workflows/ci.yml` | the harness module and its lane removed |
| `docs/public/overview/why-memql-harness.md`, `docs/public/concepts/modules.md`, `README.md`, `GLOSSARY.md`, `CLAUDE.md` | rewritten paragraphs |
| `clients/os/src/nexus/**`, `clients/os/test/nexus/**` (new) | the pure scene library and its tests |
| `clients/portal/src/scenes/**` (moved from `src/nexus/scene`) | the Views scene registry, minus the goal map |
| `clients/portal/src/nexus/**`, `clients/portal/test/nexus*.test.*`, `goalsRun.test.tsx`, `newGoal.test.tsx`, `mapMaterials.test.ts`, `test/support/nexusHarness.tsx` (deleted) | |
| `portal_view_composition_test.go`, `portal_control_vocabulary_test.go`, `portal_subscription_routing_test.go` | the Nexus entries removed |

---

## Amendments to the spec found while planning

Two things the tree settled differently from the spec's wording. Task 1 writes them into the spec so the record stays true.

1. **The action library boundary.** The DSL's authored `action` construct (`component/actions`, the `action("name@1")` step in `component/automations/steps/action.go`, the `capability` catalog in `dsl/capabilities`, and the `v1:actions:surface` rows with `registerSurface` / `setSurfaceAvailability` / `surfacesForOwner`) is the atomic-action primitive the design keeps, and it is built on four PURE packages inside the harness module: `actionpin`, `parambind`, `actionreplay` (its `Fingerprint`), `surfaceresolver`. Those four MOVE into `component/actions`. What is retired is the CAPTURE library: the `candidate` concept, `mintAction` and its ladder mutations, the fingerprint queries, `searchActions`, `actionplan`, `actiontrace`, `actiontrust`, the planner's substitution seam, and the app wiring behind `MEMQL_ACTION_REPLAY_ENABLED`. The `action` concept's capture-only fields (`inputFingerprint`, `resultFingerprint`, `paramBindings`, `templateFingerprint`, `steps`, `recordedResult`, `reliability`, `reinforceCount`, `lastReinforced`) stay declared and unwritten until epic A3 reshapes the concept.
2. **The skills tier.** The predefined skill catalog is read by every person's agents, so `v1:skills:skill` and `v1:skills:skillEdge` declare `@rowAuthz(public, requiresIdentity)` rather than the composite owner tier. Minted skills carry an owner for provenance; visibility is not narrowed by it.

---

## PR 1 of 2, first half -- rows

### Task 1: Amend the spec

**Files:**
- Modify: `docs/superpowers/specs/2026-09-05-work-spine-design.md`

- [ ] **Step 1: Branch**

```bash
git switch main && git pull --ff-only && git switch -c feat/work-spine-a1-rows-and-journal
```

- [ ] **Step 2: Replace the action-library bullet in section F**

Find the bullet beginning `- **The action library**:` in section F and replace the whole bullet with:

```markdown
- **The action CAPTURE library** (amended by the A1 plan): the `candidate`
  concept, `mintAction` and its ladder mutations, the fingerprint queries,
  `searchActions`, the `actionplan`, `actiontrace` and `actiontrust`
  packages, `integrations/planner/action_substitution.go`, and the app
  wiring behind `MEMQL_ACTION_REPLAY_ENABLED`. The AUTHORED action
  primitive stays: `component/actions`, the `action("name@1")` step, the
  capability catalog, and `v1:actions:surface`, with its four pure helper
  packages (`actionpin`, `parambind`, `actionreplay`'s fingerprint,
  `surfaceresolver`) moved out of the harness module into
  `component/actions`. The idea the design describes survives there, and
  the trust ladder moves onto the construct in epic A3.
```

- [ ] **Step 3: Amend the tier sentence in section I**

Replace the first sentence of section I (`Every work, skills and memory concept declares` ...) with:

```markdown
Every work and memory concept declares
`@rowAuthz(owner="ownerUserId", clusterOwner)`; `v1:skills:skill` and
`v1:skills:skillEdge` declare `@rowAuthz(public, requiresIdentity)`, because
the predefined catalog is read by every signed-in person's agents (amended by
the A1 plan). That settles the old question
```

and keep the rest of the paragraph as it was.

- [ ] **Step 4: Gate and commit**

```bash
go test -count=1 -run 'TestNoVendorDomainLiterals' .
git add docs/superpowers/specs/2026-09-05-work-spine-design.md
git commit -m "docs: amend the work spine record for the action boundary and the skills tier"
```

### Task 2: The work concepts

**Files:**
- Create: `dsl/work/concepts.memql`

**Interfaces:**
- Produces: concepts `goal`, `run`, `step`, `modelCall`, `approval`, `observation` in namespace `work`, imported by later files as `use work.concepts.{ goal, run, step, modelCall, approval, observation }`.

- [ ] **Step 1: Write the file**

```memql
// concepts.memql
//
// The work namespace -- the spine of the execution model (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, sub-project A).
// A goal is the intent, a run is one execution against it, a step is a
// node of a run; modelCall, approval and observation are the journal.
//
// Epic A1 writes run and step rows from the automation executor for
// every automation execution. goal, modelCall and approval gain their
// writers in epic A2. observation gains the executor as its writer in
// A2 as well; in A1 it is the destination the harness observation
// concept moves to (PR 2 of A1).
//
// TIER. Every concept here declares the composite owner tier. A row
// written by a synthetic cluster actor (the journal in A1, the seed
// materializer, a maintenance sweep) has its owner blanked to
// present-and-empty by rowauthz_nonprincipal_owner.go and is the
// deployment's row, readable through the cluster-owner escape. A row
// written under a person's borrowed authority (epic A2) is theirs. That
// is why ownerUserId is @serverSet and NOT required.

use identity.concepts.{ user }

/// The intent, owned by a person. Execution state does not live here: it lives on the run, so a
/// goal that has been re-run, forked or repaired still reads as one thing.
@rowAuthz(owner="ownerUserId", clusterOwner)
@displayCard(primary="statement", secondary="origin", tertiary="requestedVia", status="status")
concept goal {
  ownerUserId       string  @serverSet  @description("v1:identity:user.id of the person this goal belongs to, stamped from the actor; present-and-empty for a cluster-owned goal.")
  statement         string!  @description("The goal in the person's own words.")
  origin            enum("user", "responsibility", "system")!  @description("Who asked: a person, a standing responsibility, or the platform itself (the Library analysis pass).")
  responsibilityId  string  @description("v1:planner:responsibility.id this goal was created for, when origin is responsibility.")
  accountIds        []string  @description("Optional v1:accounts:account tags -- a record of who the work is for, never a visibility scope.")
  input             object  @description("The typed input object, the shape the chosen template's args declare.")
  ceilings          object  @description("{tokenBudget, costCeiling, wallClockMs, maxRetries, maxModelCalls, maxEvents} -- inherited by every run of this goal.")
  status            enum("open", "active", "closed")!  @default("open")  @description("Coarse lifecycle. open = accepted, no run finished; active = a run is in flight; closed = the person or the system closed it.")
  requestedVia      enum("", "api", "ask", "nexus", "responsibility", "library", "materializer")  @default("")  @description("The surface the goal arrived through. Empty when unknown.")
  closedAt          datetime  @description("When status reached closed.")
  closeReason       string  @description("Why it closed, in words a person can read.")
}

/// One execution against a goal, or -- in epic A1 -- one automation execution with no goal.
/// Created the moment the run starts so every later write has a home. The node and heartbeat
/// fields follow the deployment-run pattern (exactly-once per replica, the abandoned sweep in A2).
@rowAuthz(owner="ownerUserId", clusterOwner)
@displayCard(primary="automationName", secondary="status", tertiary="goalId", status="status")
concept run {
  ownerUserId            string  @serverSet  @description("Copied from the goal's owner when there is one; present-and-empty for a system run.")
  goalId                 string  @description("v1:work:goal.id this run executes, or EMPTY for an automation run that no goal asked for.")
  automationName         string!  @description("The automation this run executes -- the template's name in the registry.")
  templateConstructId    string  @description("v1:authoring:construct.id when the template is an authored construct (epic A2).")
  templateVersion        string  @description("The construct version compiled to (epic A2).")
  templateFingerprint    string!  @description("Automation.DefinitionFingerprint at start. Resume refuses a run whose automation changed under it.")
  variables              object  @description("The bound args (epic A2).")
  input                  object  @description("The automation's input envelope, restored on resume.")
  inputFingerprint       string  @description("Hash of input, for verification on resume.")
  triggeredBy            string  @description("How the run was triggered: schedule, event, manual, a sub-automation, or resumed:<runId>.")
  triggerEvent           object  @description("{topic, kind, payload} of the triggering event, when there was one. Restored on resume.")
  callerSuppliedPayload  bool  @default("false")  @description("The trigger payload came from a caller (MCP, a dry-run), so a resume must NOT restore internal origin to it (memql#2888, memql#2890).")
  mode                   enum("live", "replay", "fork")!  @default("live")  @description("live serves nothing from the journal; replay serves every model call from it; fork serves the shared prefix.")
  forkedFromRunId        string  @description("The run this one was forked from, when mode is fork.")
  forkAtStepKey          string  @description("The step key the fork diverged at.")
  replayPolicy           enum("strict", "permissive")  @default("strict")  @description("strict raises a divergence on a journal miss during replay; permissive makes a fresh call and journals it.")
  status                 enum("compiling", "running", "waiting", "succeeded", "failed", "cancelled", "abandoned")!  @description("Where the run is.")
  waitingOn              object  @description("{kind: approval|feedback|timer|external|subrun, subject, since} while status is waiting.")
  spent                  object  @description("{tokens, tokensSubscription, tokensLocal, cost, modelCalls, retries, events, wallClockMs} so far.")
  nodeId                 string  @description("MEMQL_NODE_ID of the replica running this run. Stamped at open, never rewritten.")
  heartbeatAt            datetime  @description("When the running node last wrote a step boundary.")
  cancelRequested        bool  @default("false")  @description("Somebody asked for this run to stop.")
  cancelledBy            string  @description("v1:identity:user.id when a person cancelled, or a system sentinel.")
  expectedFootprint      object  @description("The union of the declared effects of the steps (epic A2).")
  actualFootprint        object  @description("What the run observed itself touching (epic A2).")
  outcome                object  @description("The terminal result, structured.")
  errorCode              string  @description("A catalogued code when the run failed.")
  errorMessage           string  @description("The failure in words.")
  chainHead              string  @description("Chain-tracking head after the last recorded step.")
  initialChainHead       string  @description("Chain-tracking head before the first step.")
  stepOrder              []string  @description("Step keys in execution order.")
  startedAt              datetime!  @description("When the run started.")
  finishedAt             datetime  @description("When status reached a terminal value.")
  summary                object  @description("Folded from the journal before its retention window closes (epic A2).")
  @relationship(type="parent", field="goalId", target=goal, direction="outgoing")
}

/// A node of a run. A retry is a NEW VERSION of the same row with attempt incremented; the intent is
/// the `running` version written before the body executes and the receipt is the `done` or `failed`
/// version written after. Resolved step arguments are deliberately NOT recorded here (they may carry
/// resolved secrets); `call` names what ran, `result` is the trimmed result shape.
@rowAuthz(owner="ownerUserId", clusterOwner)
@displayCard(primary="key", secondary="stepType", tertiary="runId", status="status")
concept step {
  ownerUserId        string  @serverSet  @description("Copied from the run's owner; present-and-empty for a system run.")
  runId              string!  @description("v1:work:run.id this step belongs to.")
  key                string!  @description("The stable step key from the template (the automation step id).")
  seq                int!  @description("Position in the run's step order, 0-based.")
  stepType           string!  @description("The automation step type: query, mutation, shape, webhook, event, function, action, automation, forEach, parallel, switch, detectLeadSignal, emitConceptCard.")
  kind               enum("", "deterministic", "reasoning", "decision", "human", "loop", "subrun")  @default("")  @description("Derived kind (spec section B). Epic A1 derives it for every type except function, which stays empty until the A2 loader rule.")
  call               object  @description("{construct, name} -- what the step invoked, by name only.")
  dependsOn          []string  @description("Step keys this step depends on.")
  input              object  @description("The bound input, when it is safe to record (epic A2 decides redaction). Empty in A1.")
  result             object  @description("The trimmed result: {status, result, error, metadata, contentId} -- the MinimalStepResult shape resume rehydrates from.")
  resultFingerprint  string  @description("StepDeterministicFingerprint of the result.")
  binding            object  @description("{skillIds, providerPolicy, provider, model, surface, machineLabels, workerId, nodeId} recorded at dispatch (epic A3).")
  expectedFootprint  object  @description("Declared effects of what the step calls (epic A2).")
  actualFootprint    object  @description("Observed effects (epic A2).")
  postcondition      object  @description("{kind: spec|schema|check, ref, passed, message} (epic A2).")
  status             enum("pending", "ready", "running", "waiting", "done", "failed", "skipped", "cancelled")!  @description("Where the step is.")
  symptom            enum("", "transient", "environment", "contract", "plan", "human")  @default("")  @description("What the classifier decided on failure (epic A2).")
  attempt            int!  @default("1")  @description("1 on first execution; incremented on each retry of the same key.")
  idempotencyKey     string  @description("runId:key:attempt -- the key a side effect runs under (epic A2 wires the receipts).")
  childRunId         string  @description("The child run a subrun step opened (epic A2).")
  approvalId         string  @description("The approval a human step waits on (epic A2).")
  resumeAt           datetime  @description("When a timer wait resumes (epic A2).")
  externalKey        string  @description("The inbound key an external wait resumes on (epic A2).")
  startedAt          datetime  @description("When the body started.")
  finishedAt         datetime  @description("When the receipt was written.")
  durationMs         int  @description("Body duration in milliseconds.")
  tokens             int  @description("Tokens spent inside this step (epic A2).")
  cost               float  @description("Cost attributed to this step (epic A2).")
  errorCode          string  @description("A catalogued code when the step failed.")
  errorMessage       string  @description("The failure in words.")
  @relationship(type="parent", field="runId", target=run, direction="outgoing")
}

/// One model request, journaled so a replay can serve it without a provider. Owner tier, a
/// hypertable with retention (epic A2), never broadcast.
@rowAuthz(owner="ownerUserId", clusterOwner)
@displayCard(primary="model", secondary="served", tertiary="runId")
concept modelCall {
  ownerUserId   string  @serverSet  @description("Copied from the run's owner.")
  runId         string!  @description("v1:work:run.id the call was made for.")
  stepKey       string  @description("The step that made the call.")
  requestHash   string!  @description("Hash over provider, model, settings, messages, tools and output schema -- the replay key.")
  provider      string!  @description("Provider name as the router resolved it.")
  model         string!  @description("Model id as the router resolved it.")
  settings      object  @description("Temperature, max tokens and the rest of the request settings.")
  promptRef     string  @description("The prompt construct name.")
  promptVersion string  @description("The prompt construct version at call time.")
  inputTokens   int  @description("Tokens in.")
  outputTokens  int  @description("Tokens out.")
  cost          float  @description("Cost attributed to this call.")
  latencyMs     int  @description("Wall-clock latency of the call.")
  served        enum("live", "journal", "local")!  @description("live = a provider answered; journal = served from a recorded response; local = a fleet model answered.")
  response      object  @description("The response, structured output or text.")
  error         string  @description("The provider's error, when there was one.")
  @relationship(type="parent", field="runId", target=run, direction="outgoing")
}

/// One human gate. Every kind of gate is this concept; an approval never carries to a modified
/// artifact, because resume compares artifactHash. It broadcasts.
@rowAuthz(owner="ownerUserId", clusterOwner)
@displayCard(primary="kind", secondary="decision", tertiary="runId", status="decision")
concept approval {
  ownerUserId   string  @serverSet  @description("Copied from the run's owner: the person whose decision this is.")
  runId         string!  @description("v1:work:run.id parked on this approval.")
  stepKey       string  @description("The step that raised it.")
  kind          enum("sideEffect", "scopeElevation", "budget", "skillMint", "feedback", "planReview")!  @description("Which gate.")
  subject       object  @description("What is being approved, structured.")
  artifactHash  string!  @description("Hash of the exact artifact approved: the command, the patch, the message, the draft template.")
  question      string  @description("The question put to the person, for kind feedback.")
  options       []object  @description("[{label, value}] for a choice question.")
  evidence      object  @description("{tier, reason, ruleId, source} from the classifier.")
  requestedAt   datetime!  @description("When the run parked.")
  decidedBy     string  @description("v1:identity:user.id of the decider.")
  decidedAt     datetime  @description("When the decision landed.")
  decision      enum("", "approved", "rejected", "answered")  @default("")  @description("Empty while pending.")
  answer        object  @description("The person's answer, for kind feedback.")
  expiresAt     datetime  @description("When a pending approval lapses.")
  @relationship(type="parent", field="runId", target=run, direction="outgoing")
}

/// What a step observed beyond its result: a tool result, an error, a note, a decision. The
/// embedding source for recall, so the journal is the episodic memory. Never broadcast.
@rowAuthz(owner="ownerUserId", clusterOwner)
@displayCard(primary="kind", secondary="stepKey", tertiary="runId")
concept observation {
  ownerUserId  string  @serverSet  @description("Copied from the run's owner.")
  runId        string!  @description("v1:work:run.id the observation belongs to.")
  stepKey      string  @description("The step that produced it.")
  kind         enum("tool_result", "error", "note", "decision")!  @description("What kind of event this records.")
  data         object  @description("Structured per-kind data. Named data, not payload, because payload is a reserved row intrinsic.")
  content      string!  @description("The text rendering -- the embedding source, so observations are recall-able.")
  embedding    []float  @description("The embedding vector for content, populated lazily by the embed hook.")
  @relationship(type="parent", field="runId", target=run, direction="outgoing")
}
```

- [ ] **Step 2: Add the namespace to the embed directive**

In `dsl/embed.go`, in the `//go:embed all:accounts all:actions ...` directive, insert `all:work` in alphabetical position (after `all:workbench` is wrong: `work` sorts before `workbench`; put `all:work` immediately before `all:workbench`).

- [ ] **Step 3: Lint**

```bash
go run ./cmd/memqllint dsl/
```
Expected: no diagnostics for `dsl/work/`. (If the linter reports the `@relationship` on an optional `goalId`, remove that one annotation from `run` and keep the others; record the reason in the file comment.)

- [ ] **Step 4: Commit**

```bash
git add dsl/work/concepts.memql dsl/embed.go
git commit -m "dsl: the work namespace concepts (goal, run, step, modelCall, approval, observation)"
```

### Task 3: The work shapes and queries

**Files:**
- Create: `dsl/work/shapes.memql`, `dsl/work/queries.memql`

**Interfaces:**
- Produces: queries `workRunById(runId)`, `workStepsForRun(runId)`, `workRunsForAutomation(automationName)`, `workModelCallsForRun(runId)`, `workObservationsForRun(runId)`, `workApprovalById(approvalId)`, `workGoalsForOwner()`, `workApprovalsForOwner()`. The Go journal reads `workRunById` and `workStepsForRun` (Task 10).

- [ ] **Step 1: Write the shapes**

```memql
// shapes.memql
//
// Full projections, one per work concept. Explicit rather than the
// default projection so a later credential-adjacent field cannot leak
// by being added to the concept (the Users app lesson, clients/os/README.md).

use work.concepts.{ goal, run, step, modelCall, approval, observation }

/// Every goal field the OS and the SDK read.
@row
shape goal workGoalFull {
  row.id
  row.createdAt
  ownerUserId
  statement
  origin
  responsibilityId
  accountIds
  input
  ceilings
  status
  requestedVia
  closedAt
  closeReason
}

/// Every run field, including the trust and chain fields resume restores.
@row
shape run workRunFull {
  row.id
  row.createdAt
  ownerUserId
  goalId
  automationName
  templateConstructId
  templateVersion
  templateFingerprint
  variables
  input
  inputFingerprint
  triggeredBy
  triggerEvent
  callerSuppliedPayload
  mode
  forkedFromRunId
  forkAtStepKey
  replayPolicy
  status
  waitingOn
  spent
  nodeId
  heartbeatAt
  cancelRequested
  cancelledBy
  expectedFootprint
  actualFootprint
  outcome
  errorCode
  errorMessage
  chainHead
  initialChainHead
  stepOrder
  startedAt
  finishedAt
  summary
}

/// Every step field.
@row
shape step workStepFull {
  row.id
  row.createdAt
  ownerUserId
  runId
  key
  seq
  stepType
  kind
  call
  dependsOn
  input
  result
  resultFingerprint
  binding
  expectedFootprint
  actualFootprint
  postcondition
  status
  symptom
  attempt
  idempotencyKey
  childRunId
  approvalId
  resumeAt
  externalKey
  startedAt
  finishedAt
  durationMs
  tokens
  cost
  errorCode
  errorMessage
}

/// Every modelCall field.
@row
shape modelCall workModelCallFull {
  row.id
  row.createdAt
  ownerUserId
  runId
  stepKey
  requestHash
  provider
  model
  settings
  promptRef
  promptVersion
  inputTokens
  outputTokens
  cost
  latencyMs
  served
  response
  error
}

/// Every approval field.
@row
shape approval workApprovalFull {
  row.id
  row.createdAt
  ownerUserId
  runId
  stepKey
  kind
  subject
  artifactHash
  question
  options
  evidence
  requestedAt
  decidedBy
  decidedAt
  decision
  answer
  expiresAt
}

/// Every observation field except the embedding vector, which no client reads.
@row
shape observation workObservationFull {
  row.id
  row.createdAt
  ownerUserId
  runId
  stepKey
  kind
  data
  content
}
```

- [ ] **Step 2: Write the queries**

```memql
// queries.memql
//
// Reads over the work namespace. The @serverOnly ones are what resume
// (component/automations/resume.go) and the epic A2 executor read under
// the journal's synthetic cluster actor; they carry no owner conjunct
// because the rows they read are the deployment's and the caller's
// admission is the tier's cluster-owner escape. The two @actor lists are
// the owner-facing reads Nexus (sub-project B) seeds from.

use work.concepts.{ goal, run, step, modelCall, approval, observation }
use work.shapes.{ workGoalFull, workRunFull, workStepFull, workModelCallFull, workApprovalFull, workObservationFull }

/// One run by id, for resume. Server-only: read under the journal's cluster actor.
@serverOnly
@unbounded("one run by id -- bounded to a single row")
query run workRunById {
  args {
    runId  string!
  }
  filter  row.id==args.runId
  shape   workRunFull
}

/// Every step of one run, for resume and for the OS run rail. Bounded by the run.
@serverOnly
@unbounded("every step of ONE run -- bounded by the run, and resume needs all of them")
query step workStepsForRun {
  args {
    runId  string!
  }
  filter  runId==args.runId
  shape   workStepFull
}

/// Runs of one automation, newest first, for the operator's run list.
@serverOnly
query run workRunsForAutomation {
  args {
    automationName  string!
  }
  filter  automationName==args.automationName
  sort    "row.createdAt", "desc"
  shape   workRunFull
}

/// Every model call of one run, for replay. Bounded by the run.
@serverOnly
@unbounded("every model call of ONE run -- bounded by the run, and replay needs all of them")
query modelCall workModelCallsForRun {
  args {
    runId  string!
  }
  filter  runId==args.runId
  shape   workModelCallFull
}

/// Every observation of one run. Bounded by the run.
@serverOnly
@unbounded("every observation of ONE run -- bounded by the run")
query observation workObservationsForRun {
  args {
    runId  string!
  }
  filter  runId==args.runId
  shape   workObservationFull
}

/// One approval by id, for resume's artifact-hash check.
@serverOnly
@unbounded("one approval by id -- bounded to a single row")
query approval workApprovalById {
  args {
    approvalId  string!
  }
  filter  row.id==args.approvalId
  shape   workApprovalFull
}

/// The caller's goals, newest first. Owned: ownerUserId==actor.userId binds server-side.
@actor
query goal workGoalsForOwner {
  filter  ownerUserId==actor.userId
  sort    "row.createdAt", "desc"
  shape   workGoalFull
}

/// The caller's pending approvals, newest first. Owned.
@actor
query approval workApprovalsForOwner {
  filter  ownerUserId==actor.userId && decision==""
  sort    "row.createdAt", "desc"
  shape   workApprovalFull
}
```

- [ ] **Step 3: Lint**

```bash
go run ./cmd/memqllint dsl/
```
Expected: clean. If the linter refuses `decision==""` on an enum with an empty member, change the filter to `ownerUserId==actor.userId && decidedAt==""` is NOT valid either (datetime); instead drop the second conjunct and let the client filter pending rows, and say so in the doc comment.

- [ ] **Step 4: Commit**

```bash
git add dsl/work/shapes.memql dsl/work/queries.memql
git commit -m "dsl: work shapes and the reads resume and the OS need"
```

### Task 4: The work mutations

**Files:**
- Create: `dsl/work/mutations.memql`

**Interfaces:**
- Produces: `createWorkRun`, `updateWorkRun`, `createWorkStep`, `updateWorkStep`, `createWorkGoal`, `createWorkModelCall`, `createWorkObservation`, `createWorkApproval`, `decideWorkApproval`. The Go journal (Task 8) renders `createWorkRun`, `updateWorkRun`, `createWorkStep`, `updateWorkStep` as `name({"arg": value, ...})` calls, the form `mintSkill` already uses in `integrations/planner/mint_skill_handler.go`.

- [ ] **Step 1: Write the file**

```memql
// mutations.memql
//
// Writes over the work namespace. ALL @serverOnly in epic A1: the journal
// writes them from component/automations/journal.go under internal
// origin, and nothing client-reachable writes a run. Epic A2 adds the
// client-reachable createGoal / cancelGoal / decideApproval on top.
//
// The insert blocks use accept/stamp: accept names the caller-provided
// args copied verbatim, stamp names what the engine decides. ownerUserId
// is stamped from actor.userId; under the journal's synthetic cluster
// actor the engine then blanks it to present-and-empty, which is the
// deployment's row.

use work.concepts.{ goal, run, step, modelCall, approval, observation }

/// Open a run. status is an argument rather than a stamp so epic A2 can open a run in `compiling`.
@serverOnly
mutate run createWorkRun {
  args {
    runId                  string!
    goalId                 string
    automationName         string!
    templateFingerprint    string!
    input                  object
    inputFingerprint       string
    triggeredBy            string
    triggerEvent           object
    callerSuppliedPayload  bool
    mode                   string
    forkedFromRunId        string
    forkAtStepKey          string
    status                 string!
    nodeId                 string
    initialChainHead       string
    startedAt              string!
  }
  insert {
    accept { goalId, automationName, templateFingerprint, input, inputFingerprint, triggeredBy, triggerEvent, callerSuppliedPayload, mode, forkedFromRunId, forkAtStepKey, status, nodeId, initialChainHead, startedAt }
    stamp {
      id: args.runId
      ownerUserId: actor.userId
    }
  }
}

/// Advance a run: a heartbeat, the chain head, or the terminal status. Read-merge, so every field
/// not named keeps its prior value.
@serverOnly
mutate run updateWorkRun {
  args {
    runId            string!
    status           string
    heartbeatAt      string
    chainHead        string
    stepOrder        []string
    waitingOn        object
    spent            object
    outcome          object
    errorCode        string
    errorMessage     string
    finishedAt       string
    cancelRequested  bool
    cancelledBy      string
    actualFootprint  object
    summary          object
  }
  update {
    accept { status, heartbeatAt, chainHead, stepOrder, waitingOn, spent, outcome, errorCode, errorMessage, finishedAt, cancelRequested, cancelledBy, actualFootprint, summary }
    stamp {
      id: args.runId
    }
  }
}

/// Write a step's intent: the `running` version before the body executes. A retry writes this
/// again for the same stepId with attempt incremented.
@serverOnly
mutate step createWorkStep {
  args {
    stepId          string!
    runId           string!
    key             string!
    seq             int!
    stepType        string!
    kind            string
    call            object
    dependsOn       []string
    status          string!
    attempt         int!
    idempotencyKey  string
    startedAt       string
  }
  insert {
    accept { runId, key, seq, stepType, kind, call, dependsOn, status, attempt, idempotencyKey, startedAt }
    stamp {
      id: args.stepId
      ownerUserId: actor.userId
    }
  }
}

/// Write a step's receipt: the done / failed / skipped version after the body. Read-merge.
@serverOnly
mutate step updateWorkStep {
  args {
    stepId             string!
    status             string
    result             object
    resultFingerprint  string
    actualFootprint    object
    postcondition      object
    symptom            string
    attempt            int
    errorCode          string
    errorMessage       string
    finishedAt         string
    durationMs         int
    tokens             int
    cost               float
    childRunId         string
    approvalId         string
    resumeAt           string
    externalKey        string
  }
  update {
    accept { status, result, resultFingerprint, actualFootprint, postcondition, symptom, attempt, errorCode, errorMessage, finishedAt, durationMs, tokens, cost, childRunId, approvalId, resumeAt, externalKey }
    stamp {
      id: args.stepId
    }
  }
}

/// Create a goal. Server-only in A1; epic A2 adds the client-reachable createGoal.
@serverOnly
mutate goal createWorkGoal {
  args {
    goalId            string!
    statement         string!
    origin            string!
    responsibilityId  string
    accountIds        []string
    input             object
    ceilings          object
    requestedVia      string
  }
  insert {
    accept { statement, origin, responsibilityId, accountIds, input, ceilings, requestedVia }
    stamp {
      id: args.goalId
      ownerUserId: actor.userId
      status: "open"
    }
  }
}

/// Journal one model call.
@serverOnly
mutate modelCall createWorkModelCall {
  args {
    modelCallId    string!
    runId          string!
    stepKey        string
    requestHash    string!
    provider       string!
    model          string!
    settings       object
    promptRef      string
    promptVersion  string
    inputTokens    int
    outputTokens   int
    cost           float
    latencyMs      int
    served         string!
    response       object
    error          string
  }
  insert {
    accept { runId, stepKey, requestHash, provider, model, settings, promptRef, promptVersion, inputTokens, outputTokens, cost, latencyMs, served, response, error }
    stamp {
      id: args.modelCallId
      ownerUserId: actor.userId
    }
  }
}

/// Journal one observation.
@serverOnly
mutate observation createWorkObservation {
  args {
    observationId  string!
    runId          string!
    stepKey        string
    kind           string!
    content        string!
    data           object
  }
  insert {
    accept { runId, stepKey, kind, content, data }
    stamp {
      id: args.observationId
      ownerUserId: actor.userId
    }
  }
}

/// Raise a human gate.
@serverOnly
mutate approval createWorkApproval {
  args {
    approvalId    string!
    runId         string!
    stepKey       string
    kind          string!
    subject       object
    artifactHash  string!
    question      string
    options       []object
    evidence      object
    requestedAt   string!
    expiresAt     string
  }
  insert {
    accept { runId, stepKey, kind, subject, artifactHash, question, options, evidence, requestedAt, expiresAt }
    stamp {
      id: args.approvalId
      ownerUserId: actor.userId
    }
  }
}

/// Record a decision. Server-only in A1; the client-reachable form arrives in epic A2 with its gate.
@serverOnly
mutate approval decideWorkApproval {
  args {
    approvalId  string!
    decision    string!
    decidedBy   string!
    decidedAt   string!
    answer      object
  }
  update {
    accept { decision, decidedBy, decidedAt, answer }
    stamp {
      id: args.approvalId
    }
  }
}
```

- [ ] **Step 2: Lint, and check every construct name is unique tree-wide**

```bash
go run ./cmd/memqllint dsl/
for n in createWorkRun updateWorkRun createWorkStep updateWorkStep createWorkGoal createWorkModelCall createWorkObservation createWorkApproval decideWorkApproval workRunById workStepsForRun workRunsForAutomation workModelCallsForRun workObservationsForRun workApprovalById workGoalsForOwner workApprovalsForOwner workGoalFull workRunFull workStepFull workModelCallFull workApprovalFull workObservationFull; do c=$(grep -rE "^(shape|query|mutate|builtin|logic|prompt|automation) +[a-zA-Z]+ +$n\b|^(builtin|logic|prompt|automation) +$n\b" dsl --include=*.memql | wc -l); echo "$n $c"; done
```
Expected: memqllint clean; every count is exactly 1.

- [ ] **Step 3: Commit**

```bash
git add dsl/work/mutations.memql
git commit -m "dsl: the server-only work mutations the journal writes"
```

### Task 5: Concept ids, routing rules, generated artifacts, gates

**Files:**
- Modify: `component/database/memory-nodes/concept_ids.go`, `component/node/routing.go`, `component/node/routing_reach_test.go`
- Regenerate: `sdk/go/client/generated_*.go`, `sdk/ts/src/generated/*`

**Interfaces:**
- Produces: constants `ConceptWorkGoal`, `ConceptWorkRun`, `ConceptWorkStep`, `ConceptWorkModelCall`, `ConceptWorkApproval`, `ConceptWorkObservation` (used by Task 8 and PR 3).

- [ ] **Step 1: Write the failing routing test**

In `component/node/routing_reach_test.go`, find the table of `{concept, []string{"created", "updated", "deleted"}, reason}` rows (the row for `"v1:agents:agent"` is at about line 88) and add four rows to the same table:

```go
		{"v1:work:goal", []string{"created", "updated", "deleted"}, "the OS Nexus goal list (sub-project B) is a live feed"},
		{"v1:work:run", []string{"created", "updated", "deleted"}, "a run's status flips on the node that runs it, and the person watching is on the bff"},
		{"v1:work:step", []string{"created", "updated", "deleted"}, "the run rail is one LiveList over step rows"},
		{"v1:work:approval", []string{"created", "updated", "deleted"}, "a person must see an approval arrive without polling"},
```

Run: `go test -count=1 -run 'TestRoutingReach' ./component/node/`
Expected: FAIL naming the four work concepts as not forwarded.

- [ ] **Step 2: Add the routing rules**

In `component/node/routing.go`, inside the default rules slice, immediately after the `{Pattern: "graph.node.created.v1:planner:*", TargetType: ""},` line (about line 188), add:

```go
		// The work spine (design record 2026-09-05-work-spine-design.md,
		// section D "Live feeds"): goal, run, step and approval broadcast so
		// the OS draws them live. modelCall and observation are deliberately
		// ABSENT -- on-demand reads that say when they were read, excluded on
		// volume grounds exactly as v1:worker:invocation is.
		{Pattern: "graph.node.created.v1:work:goal", TargetType: ""},
		{Pattern: "graph.node.updated.v1:work:goal", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:work:goal", TargetType: ""},
		{Pattern: "graph.node.created.v1:work:run", TargetType: ""},
		{Pattern: "graph.node.updated.v1:work:run", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:work:run", TargetType: ""},
		{Pattern: "graph.node.created.v1:work:step", TargetType: ""},
		{Pattern: "graph.node.updated.v1:work:step", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:work:step", TargetType: ""},
		{Pattern: "graph.node.created.v1:work:approval", TargetType: ""},
		{Pattern: "graph.node.updated.v1:work:approval", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:work:approval", TargetType: ""},
```

Run: `go test -count=1 -run 'TestRoutingReach' ./component/node/`
Expected: PASS.

- [ ] **Step 3: Add the concept constants**

In `component/database/memory-nodes/concept_ids.go`, after the action-library block (about line 43), add:

```go
// Work-spine concepts (v1:work:*) -- the execution model's spine
// (docs/superpowers/specs/2026-09-05-work-spine-design.md). Constants
// rather than literals because the journal (component/automations),
// the routing rules and the resume path all have to agree on the
// spelling.
const (
	ConceptWorkGoal        = "v1:work:goal"
	ConceptWorkRun         = "v1:work:run"
	ConceptWorkStep        = "v1:work:step"
	ConceptWorkModelCall   = "v1:work:modelCall"
	ConceptWorkApproval    = "v1:work:approval"
	ConceptWorkObservation = "v1:work:observation"
)
```

Do NOT add them to `AllFilesystemConcepts()`: that list is the filesystem-backed set and these are DSL-declared.

- [ ] **Step 4: Regenerate the SDKs and run the engine gates**

```bash
make sdk-gen && make sdk-gen-check
go test -count=1 -run 'TestUndeclaredRowAuthzPopulationOnlyShrinks|TestEngineLoad|TestSeed' ./component/memql/
go test -count=1 ./test/dslconformance/
```
Expected: sdk-gen-check clean; the ratchet unchanged (every work concept declares a tier, so no entry is added); conformance classifies every new construct (the `@serverOnly` ones as srvOnly, the two `@actor` ones as owned). If conformance reports an unclassified construct, the fix is the classification the message names, never an exemption.

- [ ] **Step 5: Full test, commit**

```bash
make test
git add component/database/memory-nodes/concept_ids.go component/node/routing.go component/node/routing_reach_test.go sdk/go/client sdk/ts/src/generated
git commit -m "work: concept ids, broadcast routing rules, regenerated SDKs"
```

### Task 6: The skills namespace

**Files:**
- Create: `dsl/skills/concepts.memql`, `dsl/skills/shapes.memql`, `dsl/skills/queries.memql`, `dsl/skills/mutations.memql`, `dsl/skills/seeds/` (moved from `dsl/agents/skills/`)
- Modify: `dsl/agents/concepts.memql` (remove `concept skill` and `concept skillChangeEvent` stays), `dsl/agents/shapes.memql` (remove `skillSummary`, `skillFull`), `dsl/agents/queries.memql` (remove `skillBySlug`, `activeSkills`, `activeSkillsFull`, `skillNeedsRefresh`), `dsl/agents/mutations.memql` (remove `mintSkill`, `createSkill`), `dsl/embed.go` (`all:skills`), and the reference sweep below

**Interfaces:**
- Produces: `v1:skills:skill` (same fields as today plus `ownerUserId`), `v1:skills:skillEdge`, queries `skillBySlug`, `activeSkills`, `activeSkillsFull`, `skillNeedsRefresh`, `skillEdgesForSkill`, mutations `mintSkill`, `createSkill`, `createSkillEdge`, `commitSkillEdge`. Every existing construct keeps its NAME, so Go callers that render `activeSkillsFull()` or `mintSkill({...})` keep working; only the concept id and the `use` imports change.

- [ ] **Step 1: Move the DSL blocks**

Create `dsl/skills/concepts.memql` with this header and then the `concept skill { ... }` block copied VERBATIM from `dsl/agents/concepts.memql` (the block starting at the `@displayCard(primary="name", secondary="category", tertiary="tier", status="active")` line above `concept skill {` and ending at its closing brace):

```memql
// concepts.memql
//
// The skills namespace -- the capability graph of the execution model
// (design record docs/superpowers/specs/2026-09-05-work-spine-design.md,
// section C). Moved out of the agents namespace in epic A1 so the
// catalog survives the retirement of that namespace (sub-project F).
// Epic A3 grows skill with instructions, scripts, resources, effects and
// the trust ladder; this file is the move plus the edge concept.
//
// TIER: public, requiresIdentity. The predefined catalog is read by
// every signed-in person's agents; minted skills carry an owner for
// provenance, and visibility is not narrowed by it (A1 plan amendment).

use identity.concepts.{ user }
use knowledge.concepts.{ knowledgeDomain, liveSource }
```

Then apply these edits to the copied `skill` block:
1. Insert the line `@rowAuthz(public, requiresIdentity)` immediately above its `@displayCard(...)` line.
2. Add as the FIRST field: `ownerUserId  string  @serverSet  @description("v1:identity:user.id of the person whose planner minted this skill; present-and-empty for a predefined catalog row.")`
3. Keep every other field, annotation and relationship exactly as copied. If the copied block's `use` imports need a concept this header does not import (check the block's `@relationship(... target=...)` lines), add that import to the header.

Append the edge concept:

```memql
/// A typed relationship between two skills. Compile may propose an edge; a run that succeeds commits
/// it (the propose-then-commit protocol, spec section C). Selection reads committed edges beside
/// vector matches.
@rowAuthz(public, requiresIdentity)
@displayCard(primary="type", secondary="fromSkillId", tertiary="toSkillId", status="status")
concept skillEdge {
  ownerUserId  string  @serverSet  @description("Who proposed it, when a person's run did; present-and-empty for a system-proposed edge.")
  fromSkillId  string!  @description("v1:skills:skill.id the edge starts at.")
  toSkillId    string!  @description("v1:skills:skill.id the edge points to.")
  type         enum("dependsOn", "conflictsWith", "specializes", "duplicates")!  @description("What the edge means.")
  evidence     []object  @description("[{runId, stepKey}] -- the executions that established it.")
  proposedBy   enum("system", "user")!  @default("system")  @description("Who proposed it.")
  status       enum("proposed", "committed")!  @default("proposed")  @description("proposed until a successful run commits it.")
  @relationship(type="references", as="edgeFrom", field="fromSkillId", target=skill, direction="outgoing")
  @relationship(type="references", as="edgeTo", field="toSkillId", target=skill, direction="outgoing")
}
```

Create `dsl/skills/shapes.memql` with `use skills.concepts.{ skill, skillEdge }` and the `skillSummary` and `skillFull` shape blocks copied verbatim from `dsl/agents/shapes.memql`, plus:

```memql
/// Every edge field.
@row
shape skillEdge skillEdgeFull {
  row.id
  row.createdAt
  ownerUserId
  fromSkillId
  toSkillId
  type
  evidence
  proposedBy
  status
}
```

Create `dsl/skills/queries.memql` with `use skills.concepts.{ skill, skillEdge }`, `use skills.shapes.{ skillSummary, skillFull, skillEdgeFull }`, the four skill query blocks copied verbatim from `dsl/agents/queries.memql`, plus:

```memql
/// Every committed edge touching one skill, in either direction.
@unbounded("the edges of ONE skill -- bounded by the skill")
query skillEdge skillEdgesForSkill {
  args {
    skillId  string!
  }
  filter  (fromSkillId==args.skillId || toSkillId==args.skillId) && status=="committed"
  shape   skillEdgeFull
}
```

Create `dsl/skills/mutations.memql` with `use skills.concepts.{ skill, skillEdge }` and the `mintSkill` and `createSkill` blocks copied verbatim, plus:

```memql
/// Propose an edge.
@serverOnly
mutate skillEdge createSkillEdge {
  args {
    edgeId       string!
    fromSkillId  string!
    toSkillId    string!
    type         string!
    evidence     []object
    proposedBy   string!
  }
  insert {
    accept { fromSkillId, toSkillId, type, evidence, proposedBy }
    stamp {
      id: args.edgeId
      ownerUserId: actor.userId
      status: "proposed"
    }
  }
}

/// Commit a proposed edge after a successful run.
@serverOnly
mutate skillEdge commitSkillEdge {
  args {
    edgeId    string!
    evidence  []object
  }
  update {
    accept { evidence }
    stamp {
      id: args.edgeId
      status: "committed"
    }
  }
}
```

Move the seeds: `git mv dsl/agents/skills dsl/skills/seeds`, then in every file under `dsl/skills/seeds/` change the file-top import from `use agents.concepts.{ skill }` to `use skills.concepts.{ skill }` (`grep -n '^use' dsl/skills/seeds/*.memql` shows each).

Delete the moved blocks from the four `dsl/agents/*.memql` files, and drop `skill` from any `use agents.concepts.{ ... }` list that no longer needs it.

- [ ] **Step 2: The reference sweep**

Change every `use agents.concepts.{ ... skill ... }` outside `dsl/agents` to import `skill` from `skills.concepts` instead (`grep -rln 'agents\.concepts\.{[^}]*\bskill\b' dsl --include=*.memql` lists the seven files). Then the concept id, in these ten Go files and three TS files:

```bash
grep -rlE 'v1:agents:skill\b' --include=*.go --include=*.ts --include=*.tsx . | grep -v node_modules | grep -v generated
```
Expected list: `component/node/routing.go`, `component/memql/skill_catalog_reconcile.go`, `component/memql/skill_tier_validation.go`, `component/memql/seed_materializer.go`, `component/memql/skill_resolver.go`, `component/memql/integration_engine.go`, `integrations/cognition/cognition.go`, `integrations/agent/types.go`, `integrations/agent/replier.go`, `integrations/agents/factory.go`, and three files under `clients/portal/src` or `clients/os/src`. In each, replace `v1:agents:skill` with `v1:skills:skill` (a `sed -i 's/v1:agents:skill\b/v1:skills:skill/g'` over exactly those files, then `git diff --stat` to confirm no other file moved). In `component/node/routing.go`, the rule that named `v1:agents:skill` now names `v1:skills:skill`; keep its comment.

Add `all:skills` to the embed directive in `dsl/embed.go` (alphabetical, after `all:shopify`).

- [ ] **Step 3: Lint, regenerate, ratchet**

```bash
go run ./cmd/memqllint dsl/
make sdk-gen && make sdk-gen-check
go test -count=1 -run 'TestUndeclaredRowAuthzPopulationOnlyShrinks' ./component/memql/
```
The ratchet now fails in the SHRINKING direction: the `v1:agents:skill` entries in `undeclaredRowAuthzConstructs` (`component/memql/rowauthz_undeclared_gate_test.go`) name a concept that declares a tier. DELETE those entries (`skillBySlug`, `activeSkills`, `activeSkillsFull`, `skillNeedsRefresh`, and any other `"v1:agents:skill"` row), leaving a one-line comment where the block was: `// v1:agents:skill -- moved to v1:skills:skill with a declared tier (work spine A1).` Re-run; expected PASS.

- [ ] **Step 4: Full test, portal and OS lanes, commit**

```bash
make test
make portal-typecheck && make portal-test
make os-typecheck && make os-test
git add dsl/skills dsl/agents dsl/embed.go component/node/routing.go component/memql sdk/go/client sdk/ts/src/generated clients/portal/src clients/os/src integrations/cognition/cognition.go integrations/agent/types.go integrations/agent/replier.go integrations/agents/factory.go
git status --short
git commit -m "dsl: the skills namespace, moved from agents, with skillEdge"
```
(`git status --short` must show nothing unstaged from another session; if it does, stage only the files this task touched by name.)

### Task 7: Checkpoint before the journal

The rows half is complete; the same branch continues into the journal half, and one PR ships both (the owner's two-PR rule).

- [ ] **Step 1: Regenerate the architecture model and run every gate once more**

```bash
make arch-model && make arch-model-check
make test
go test -count=1 .
```

- [ ] **Step 2: Commit and continue**

```bash
git add component/architecture
git commit -m "arch: regenerate after the work and skills namespaces"
```
Stay on `feat/work-spine-a1-rows-and-journal` and continue with Task 8. Do not open a PR here.

---

## PR 1 of 2, second half -- journal

The same branch, continuing from Task 7.

### Task 8: The journal writer

**Files:**
- Create: `component/automations/journal.go`
- Test: `component/automations/journal_test.go`

**Interfaces:**
- Consumes: `createWorkRun`, `updateWorkRun`, `createWorkStep`, `updateWorkStep` (Task 4); `Automation.DefinitionFingerprint(*id.Engine)`, `StepDeterministicFingerprint`, `ToMinimalStepResults` (existing); `fingerprintEngine` (existing package var).
- Produces: `type journalExecutor interface { Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) }`; `func newWorkJournal(exec journalExecutor, logger *slog.Logger) *workJournal`; methods `openRun`, `stepRunning`, `stepFinished`, `stepSkipped`, `closeRun`; pure helpers `workStepId(runId, stepKey string) string`, `stepKindFor(step *Step) string`, `journalSkipsAutomation(a *Automation) bool`, `journalContext(ctx) context.Context`, `journalArgs(name string, args map[string]any) (string, error)`.

- [ ] **Step 1: Write the failing test**

```go
package automations

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// recordingJournalExecutor captures every call the journal renders so the
// exact MemQL text is asserted without an engine or a database.
type recordingJournalExecutor struct {
	calls []string
	ctxs  []context.Context
}

func (r *recordingJournalExecutor) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	r.calls = append(r.calls, query)
	r.ctxs = append(r.ctxs, ctx)
	return &memql.ExecuteResult{}, nil
}

// argsOf parses the JSON object a rendered `name({...})` call carries.
func argsOf(t *testing.T, call string) (string, map[string]any) {
	t.Helper()
	open := strings.Index(call, "(")
	if open < 0 || !strings.HasSuffix(call, ")") {
		t.Fatalf("call %q is not name({...})", call)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call[open+1:len(call)-1]), &args); err != nil {
		t.Fatalf("call %q carries no JSON object: %v", call, err)
	}
	return call[:open], args
}

func TestWorkStepId_SanitisesTheKey(t *testing.T) {
	if got := workStepId("run-1", "layer0.sales"); got != "run-1-layer0-sales" {
		t.Fatalf("workStepId = %q, want run-1-layer0-sales", got)
	}
	if got := workStepId("run-1", "ok_key"); got != "run-1-ok_key" {
		t.Fatalf("workStepId = %q, want run-1-ok_key", got)
	}
}

func TestStepKindFor_DerivesDeterministicExceptFunction(t *testing.T) {
	for _, tc := range []struct {
		typ  StepType
		want string
	}{
		{StepTypeQuery, "deterministic"},
		{StepTypeMutation, "deterministic"},
		{StepTypeAction, "deterministic"},
		{StepTypeParallel, "deterministic"},
		{StepTypeAutomation, "deterministic"},
		{StepTypeFunction, ""},
	} {
		if got := stepKindFor(&Step{Type: tc.typ}); got != tc.want {
			t.Errorf("stepKindFor(%s) = %q, want %q", tc.typ, got, tc.want)
		}
	}
}

func TestJournalSkipsAutomation_ReactingToItsOwnRows(t *testing.T) {
	loop := &Automation{Name: "onStep", Trigger: &TriggerConfig{Event: "graph.node.created.*.v1:work:step"}}
	if !journalSkipsAutomation(loop) {
		t.Fatal("an automation triggered by a work row must not journal itself: that is a feedback loop")
	}
	plain := &Automation{Name: "sweep", Trigger: &TriggerConfig{Event: "graph.node.created.*.v1:library:file"}}
	if journalSkipsAutomation(plain) {
		t.Fatal("an ordinary automation is journaled")
	}
	if journalSkipsAutomation(&Automation{Name: "cron", Schedule: "0 * * * * *"}) {
		t.Fatal("a scheduled automation is journaled")
	}
}

func TestJournalContext_IsSyntheticInternalClusterActor(t *testing.T) {
	ctx := journalContext(context.Background())
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		t.Fatal("no access context")
	}
	if !ac.Synthetic || !ac.Unranked || ac.Role != auth.RoleOwner {
		t.Fatalf("journal actor = %+v, want Synthetic, Unranked, RoleOwner", ac)
	}
	if !auth.IsInternalOrigin(ctx) {
		t.Fatal("journal writes must carry internal origin: the mutations are @serverOnly")
	}
}

func TestJournal_RunAndStepLifecycle(t *testing.T) {
	rec := &recordingJournalExecutor{}
	j := newWorkJournal(rec, nil)
	j.nodeId = "node-a"
	auto := &Automation{Name: "demo", Steps: []*Step{{ID: "one", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}}}}
	exec := NewExecution("demo", "test")
	exec.ID = "run-1"
	exec.StartedAt = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	j.openRun(context.Background(), auto, exec, nil)
	j.stepRunning(context.Background(), exec, auto.Steps[0], 0, 1)
	res := &StepResult{StepId: "one", Status: "completed", Result: map[string]any{"rows": 3}, StartedAt: exec.StartedAt, CompletedAt: exec.StartedAt.Add(20 * time.Millisecond), Duration: 20 * time.Millisecond}
	j.stepFinished(context.Background(), exec, auto.Steps[0], res)
	exec.Complete()
	j.closeRun(context.Background(), exec)

	if len(rec.calls) != 5 {
		t.Fatalf("calls = %d (%v), want 5: createWorkRun, createWorkStep, updateWorkStep, updateWorkRun (heartbeat), updateWorkRun (close)", len(rec.calls), rec.calls)
	}
	name, args := argsOf(t, rec.calls[0])
	if name != "createWorkRun" || args["runId"] != "run-1" || args["automationName"] != "demo" || args["status"] != "running" || args["nodeId"] != "node-a" {
		t.Errorf("open: %s %v", name, args)
	}
	if args["templateFingerprint"] == "" || args["templateFingerprint"] == nil {
		t.Error("open must record the automation definition fingerprint, or resume cannot refuse a changed automation")
	}
	name, args = argsOf(t, rec.calls[1])
	if name != "createWorkStep" || args["stepId"] != "run-1-one" || args["key"] != "one" || args["status"] != "running" || args["kind"] != "deterministic" || args["stepType"] != "query" {
		t.Errorf("running: %s %v", name, args)
	}
	if _, present := args["input"]; present {
		t.Error("resolved step arguments must not be journaled in A1 (they may carry resolved secrets)")
	}
	name, args = argsOf(t, rec.calls[2])
	if name != "updateWorkStep" || args["stepId"] != "run-1-one" || args["status"] != "done" || args["durationMs"] != float64(20) {
		t.Errorf("finished: %s %v", name, args)
	}
	if args["result"] == nil {
		t.Error("a done step carries its trimmed result for resume to rehydrate")
	}
	name, args = argsOf(t, rec.calls[3])
	if name != "updateWorkRun" || args["runId"] != "run-1" || args["heartbeatAt"] == nil {
		t.Errorf("heartbeat: %s %v", name, args)
	}
	name, args = argsOf(t, rec.calls[4])
	if name != "updateWorkRun" || args["status"] != "succeeded" || args["finishedAt"] == nil {
		t.Errorf("close: %s %v", name, args)
	}
	for i, c := range rec.ctxs {
		if ac, _ := auth.AccessFromContext(c); ac == nil || !ac.Synthetic {
			t.Errorf("call %d was not made under the synthetic journal actor", i)
		}
	}
}

func TestJournal_FailedStepAndFailedRun(t *testing.T) {
	rec := &recordingJournalExecutor{}
	j := newWorkJournal(rec, nil)
	auto := &Automation{Name: "demo", Steps: []*Step{{ID: "one", Type: StepTypeMutation, Mutation: &MutationStepConfig{Concept: "v1:x:y"}}}}
	exec := NewExecution("demo", "test")
	exec.ID = "run-2"
	j.stepRunning(context.Background(), exec, auto.Steps[0], 0, 1)
	res := &StepResult{StepId: "one", Status: "failed", Error: "boom", StartedAt: time.Now(), CompletedAt: time.Now()}
	j.stepFinished(context.Background(), exec, auto.Steps[0], res)
	exec.Fail(errBoom)
	j.closeRun(context.Background(), exec)
	_, args := argsOf(t, rec.calls[1])
	if args["status"] != "failed" || args["errorMessage"] != "boom" {
		t.Errorf("failed step: %v", args)
	}
	if _, present := args["result"]; present {
		t.Error("a failed step carries no result")
	}
	_, args = argsOf(t, rec.calls[3])
	if args["status"] != "failed" || args["errorMessage"] != "boom" {
		t.Errorf("failed run: %v", args)
	}
}

func TestJournal_NilIsANoOp(t *testing.T) {
	var j *workJournal
	j.openRun(context.Background(), &Automation{Name: "x"}, NewExecution("x", "t"), nil)
	j.closeRun(context.Background(), NewExecution("x", "t"))
	if newWorkJournal(nil, nil) != nil {
		t.Fatal("no executor means no journal")
	}
}
```

`errBoom` is `var errBoom = errors.New("boom")` at the top of the test file (add the `errors` import).

Run: `go test -count=1 -run 'TestWorkStepId|TestStepKindFor|TestJournal' ./component/automations/`
Expected: FAIL, `undefined: workStepId` and friends.

- [ ] **Step 2: Write the journal**

```go
package automations

// journal.go -- the work journal (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section D).
//
// Every automation execution is a v1:work:run row and every step a
// v1:work:step row, written at the boundaries the executor already has:
// the run opens before the first step; a step is written at `running`
// BEFORE its body executes (the intent) and again at `done`, `failed` or
// `skipped` AFTER (the receipt); the run closes with its terminal status.
// resume.go reads these rows back. The checkpoint side-record they
// replace is gone.
//
// THE WRITER IS A SYNTHETIC CLUSTER ACTOR. The engine blanks the owner it
// would otherwise stamp for such an actor (rowauthz_nonprincipal_owner.go),
// so the rows are the deployment's, readable through the composite tier's
// cluster-owner escape. A goal-owned run (epic A2) runs its journal under
// the owner's borrowed authority instead; nothing here assumes the rows
// are unowned beyond the actor journalContext installs.
//
// A JOURNAL WRITE NEVER FAILS THE RUN. The run is the work and the
// journal is its record; a failed write is logged at Warn and counted,
// and the automation continues. The alternative -- failing a sweep
// because its record could not be written -- would let a journal outage
// stop the cluster.
//
// TWO THINGS ARE DELIBERATELY NOT RECORDED. Resolved step arguments, which
// may carry resolved secrets (epic A2 decides redaction), and a step's
// children (forEach and parallel branches), which ride inside the parent's
// trimmed result exactly as they ride inside AutomationExecution.Steps.
//
// A SANDBOXED DRY-RUN WRITES NOTHING, for the reason it minted no
// checkpoint (memql#2932): nothing resumes a preview, and the write would
// escape the sandbox.
//
// AN AUTOMATION THAT REACTS TO WORK ROWS IS NOT JOURNALED. Its own step
// rows would re-fire its trigger, and a feedback loop through the graph
// is the one failure the design's event-sourced substrate makes easy.
// journalSkipsAutomation is the guard; keep it beside the trigger check.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// workJournalActor names the synthetic principal the journal writes as.
// It is a label for log lines and the token subject, never an owner: the
// engine blanks it on write because the actor is Synthetic.
const workJournalActor = "cluster:work-journal"

// journalExecutor is the one seam the journal needs from the engine: run a
// rendered MemQL call. *memql.MemQLEngine satisfies it in production; a
// recording fake does in tests, which is what lets the exact call strings
// be asserted without a database.
type journalExecutor interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

type workJournal struct {
	exec   journalExecutor
	logger *slog.Logger
	nodeId string
}

// newWorkJournal returns nil when there is no executor, and every method
// on a nil journal is a no-op, so the executor can hold one field and
// never branch on it.
func newWorkJournal(exec journalExecutor, logger *slog.Logger) *workJournal {
	if exec == nil {
		return nil
	}
	return &workJournal{
		exec:   exec,
		logger: logger,
		nodeId: strings.TrimSpace(os.Getenv("MEMQL_NODE_ID")),
	}
}

// journalContext installs the synthetic cluster actor and internal origin
// the @serverOnly work mutations require. Modelled on the seed
// materializer's systemActorContext (component/memql/seed_materializer.go).
func journalContext(ctx context.Context) context.Context {
	claims := map[string]any{"sub": workJournalActor, "role": "system"}
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: workJournalActor, Claims: claims})
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId:    workJournalActor,
		Role:      auth.RoleOwner,
		Unranked:  true,
		Synthetic: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

// workTriggerPattern matches a trigger event that names a work-namespace
// concept: graph.node.<verb>.<partition>.v1:work:<concept>, in either the
// partition-segment or the bare form.
var workTriggerPattern = regexp.MustCompile(`^graph\.node\.[a-z]+\.(\*\.|[^.]+\.)?v1:work:`)

// journalSkipsAutomation reports whether an automation must not be
// journaled because its trigger reacts to work rows.
func journalSkipsAutomation(a *Automation) bool {
	if a == nil || a.Trigger == nil {
		return false
	}
	return workTriggerPattern.MatchString(strings.TrimSpace(a.Trigger.Event))
}

// stepIdUnsafe matches every character a MemQL short id may not carry.
var stepIdUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// workStepId is the step row's short id: the run id and the step key,
// joined so a parallel branch id like `layer0.sales` stays a legal short
// id. Deterministic, so a retry writes a NEW VERSION of the SAME row.
func workStepId(runId, stepKey string) string {
	return runId + "-" + stepIdUnsafe.ReplaceAllString(stepKey, "-")
}

// stepKindFor derives the spec's step kind from the automation step type.
// Every type is deterministic except function, whose logic may reach a
// prompt; that is the loader rule epic A2 adds, so A1 leaves it empty.
func stepKindFor(step *Step) string {
	if step == nil {
		return ""
	}
	if step.Type == StepTypeFunction {
		return ""
	}
	return "deterministic"
}

// stepCallSummary names what a step invoked, by name only -- never the
// resolved arguments.
func stepCallSummary(step *Step) map[string]any {
	call := map[string]any{"construct": string(step.Type)}
	switch {
	case step.Query != nil:
		call["name"] = step.Query.Query
	case step.Mutation != nil:
		call["name"] = step.Mutation.Concept
	case step.Function != nil:
		call["name"] = step.Function.Name
	case step.Automation != nil:
		call["name"] = step.Automation.Name
	case step.Action != nil:
		call["name"] = step.Action.Ref
	case step.Webhook != nil:
		call["name"] = step.Webhook.URL
	case step.Event != nil:
		call["name"] = step.Event.Topic
	}
	return call
}

// journalArgs renders one call in the form the engine already accepts
// from Go callers: name({"arg": value, ...}) -- see mintSkill in
// integrations/planner/mint_skill_handler.go.
func journalArgs(name string, args map[string]any) (string, error) {
	payload, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("work journal: marshal %s args: %w", name, err)
	}
	return name + "(" + string(payload) + ")", nil
}

func (j *workJournal) call(ctx context.Context, name string, args map[string]any) {
	if j == nil {
		return
	}
	query, err := journalArgs(name, args)
	if err != nil {
		j.warn(name, err)
		return
	}
	if _, err := j.exec.Execute(journalContext(ctx), query); err != nil {
		j.warn(name, err)
	}
}

func (j *workJournal) warn(name string, err error) {
	if j == nil || j.logger == nil {
		return
	}
	j.logger.Warn("work journal write failed; the run continues", "component", ComponentName, "mutation", name, "error", err)
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// openRun writes the run row. Called after the executor's own refusal
// gates (args validation, dedup, the cluster guard) so a refused fire
// leaves no row.
func (j *workJournal) openRun(ctx context.Context, automation *Automation, exec *AutomationExecution, triggeringEvent *events.Event) {
	if j == nil || automation == nil || exec == nil {
		return
	}
	args := map[string]any{
		"runId":                 exec.ID,
		"automationName":        automation.Name,
		"templateFingerprint":   automation.DefinitionFingerprint(fingerprintEngine),
		"input":                 exec.Input,
		"inputFingerprint":      exec.InputFingerprint,
		"triggeredBy":           exec.TriggeredBy,
		"callerSuppliedPayload": exec.CallerSuppliedPayload,
		"mode":                  "live",
		"status":                "running",
		"nodeId":                j.nodeId,
		"initialChainHead":      exec.InitialChainHead,
		"startedAt":             rfc3339(exec.StartedAt),
	}
	if triggeringEvent != nil {
		args["triggerEvent"] = map[string]any{
			"topic":   triggeringEvent.Topic,
			"kind":    triggeringEvent.Kind.String(),
			"payload": triggeringEvent.Payload,
		}
	}
	j.call(ctx, "createWorkRun", args)
}

// stepRunning writes the intent version of a step row.
func (j *workJournal) stepRunning(ctx context.Context, exec *AutomationExecution, step *Step, seq int, attempt int) {
	if j == nil || exec == nil || step == nil {
		return
	}
	j.call(ctx, "createWorkStep", map[string]any{
		"stepId":         workStepId(exec.ID, step.ID),
		"runId":          exec.ID,
		"key":            step.ID,
		"seq":            seq,
		"stepType":       string(step.Type),
		"kind":           stepKindFor(step),
		"call":           stepCallSummary(step),
		"status":         "running",
		"attempt":        attempt,
		"idempotencyKey": exec.ID + ":" + step.ID + ":" + strconv.Itoa(attempt),
		"startedAt":      rfc3339(time.Now()),
	})
}

// stepFinished writes the receipt version of a step row and a heartbeat on
// the run. The result is the trimmed MinimalStepResult shape, the same
// shape the checkpoint carried and resume rehydrates from.
func (j *workJournal) stepFinished(ctx context.Context, exec *AutomationExecution, step *Step, result *StepResult) {
	if j == nil || exec == nil || step == nil || result == nil {
		return
	}
	status := "done"
	switch result.Status {
	case "failed":
		status = "failed"
	case "skipped":
		status = "skipped"
	}
	args := map[string]any{
		"stepId":            workStepId(exec.ID, step.ID),
		"status":            status,
		"resultFingerprint": StepDeterministicFingerprint(step, result),
		"finishedAt":        rfc3339(result.CompletedAt),
		"durationMs":        result.Duration.Milliseconds(),
	}
	if result.Error != "" {
		args["errorMessage"] = result.Error
	}
	if status == "done" {
		trimmed := ToMinimalStepResults(map[string]*StepResult{step.ID: result})
		if m, ok := trimmed[step.ID]; ok && m != nil {
			args["result"] = m
		}
	}
	j.call(ctx, "updateWorkStep", args)
	j.call(ctx, "updateWorkRun", map[string]any{
		"runId":       exec.ID,
		"heartbeatAt": rfc3339(time.Now()),
		"chainHead":   exec.ChainHead,
		"stepOrder":   exec.StepOrder,
	})
}

// stepSkipped writes a step whose condition decided it would not run: one
// row at `skipped`, with no intent version, because nothing was intended.
func (j *workJournal) stepSkipped(ctx context.Context, exec *AutomationExecution, step *Step, seq int) {
	if j == nil || exec == nil || step == nil {
		return
	}
	now := rfc3339(time.Now())
	j.call(ctx, "createWorkStep", map[string]any{
		"stepId":    workStepId(exec.ID, step.ID),
		"runId":     exec.ID,
		"key":       step.ID,
		"seq":       seq,
		"stepType":  string(step.Type),
		"kind":      stepKindFor(step),
		"call":      stepCallSummary(step),
		"status":    "skipped",
		"attempt":   1,
		"startedAt": now,
	})
}

// closeRun writes the terminal status. The executor's status vocabulary
// (completed / failed / cancelled) maps onto the spec's.
func (j *workJournal) closeRun(ctx context.Context, exec *AutomationExecution) {
	if j == nil || exec == nil {
		return
	}
	status := "succeeded"
	switch exec.Status {
	case "failed":
		status = "failed"
	case "cancelled":
		status = "cancelled"
	}
	finished := exec.CompletedAt
	if finished.IsZero() {
		finished = time.Now()
	}
	args := map[string]any{
		"runId":      exec.ID,
		"status":     status,
		"finishedAt": rfc3339(finished),
		"chainHead":  exec.ChainHead,
		"stepOrder":  exec.StepOrder,
		"outcome":    map[string]any{"executorStatus": exec.Status},
	}
	if exec.Error != "" {
		args["errorMessage"] = exec.Error
	}
	j.call(ctx, "updateWorkRun", args)
}
```

If `auth.IsInternalOrigin` does not exist under that name, find the predicate `auth.ContextWithInternalOrigin` pairs with (`grep -n 'func.*InternalOrigin' component/auth/*.go`) and use it in the test.

Run: `go test -count=1 -run 'TestWorkStepId|TestStepKindFor|TestJournal' ./component/automations/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add component/automations/journal.go component/automations/journal_test.go
git commit -m "automations: the work journal writer"
```

### Task 9: Wire the journal into the executor

**Files:**
- Modify: `component/automations/executor.go` (the `Executor` struct, `NewExecutor`, `executeWithEvent`'s step loop, the two `saveCheckpointOnFailure` call sites)
- Test: `component/automations/journal_test.go` (one more test), using the existing `recordingRegistry` in `run_relay_test.go` as the model for a fake step registry

**Interfaces:**
- Consumes: Task 8.
- Produces: `Executor.journal *workJournal`, built in `NewExecutor` as `newWorkJournal(opts.Engine, opts.Logger)` and set to nil when `opts.SandboxRun` is true.

- [ ] **Step 1: Write the failing test**

Append to `journal_test.go`:

```go
// journalProbeRegistry runs every step as a success and records nothing
// else; it exists so the executor's own loop drives the journal.
type journalProbeRegistry struct{}

func (journalProbeRegistry) Execute(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	now := time.Now()
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": true}, StartedAt: now, CompletedAt: now}, nil
}

func TestExecutor_JournalsEveryStepBoundary(t *testing.T) {
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{StepRegistry: journalProbeRegistry{}})
	e.journal = newWorkJournal(rec, nil)
	auto := &Automation{Name: "demo", Steps: []*Step{
		{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}},
		{ID: "b", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}, Condition: "false"},
	}}
	exec, err := e.Execute(context.Background(), auto, "test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exec.Status != "completed" {
		t.Fatalf("status = %q", exec.Status)
	}
	var names []string
	for _, c := range rec.calls {
		n, _ := argsOf(t, c)
		names = append(names, n)
	}
	want := []string{"createWorkRun", "createWorkStep", "updateWorkStep", "updateWorkRun", "createWorkStep", "updateWorkRun"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("journal calls = %v, want %v (open; a running; a done + heartbeat; b skipped; close)", names, want)
	}
	_, skipped := argsOf(t, rec.calls[4])
	if skipped["key"] != "b" || skipped["status"] != "skipped" {
		t.Errorf("skipped step: %v", skipped)
	}
}

func TestExecutor_SandboxRunWritesNoJournal(t *testing.T) {
	e := NewExecutor(ExecutorOptions{StepRegistry: journalProbeRegistry{}, SandboxRun: true})
	if e.journal != nil {
		t.Fatal("a sandboxed dry-run must not journal: nothing resumes a preview and the write would escape the sandbox")
	}
}
```

Run: `go test -count=1 -run 'TestExecutor_Journals|TestExecutor_SandboxRun' ./component/automations/`
Expected: FAIL, `e.journal undefined`.

- [ ] **Step 2: Add the field and construct it**

In `component/automations/executor.go`, add to the `Executor` struct (next to `sandboxRun`):

```go
	// journal writes the run and step rows (journal.go). Nil under a
	// sandboxed dry-run and when no engine is configured; every method on a
	// nil journal is a no-op.
	journal *workJournal
```

In `NewExecutor`, after the struct literal is built and `sandboxRun` is set, add:

```go
	if !opts.SandboxRun && opts.Engine != nil {
		e.journal = newWorkJournal(opts.Engine, opts.Logger)
	}
```

(`opts.Engine` is `*memql.MemQLEngine`, which satisfies `journalExecutor`; passing a nil pointer would make a non-nil interface, hence the explicit nil check.)

- [ ] **Step 3: Hook the boundaries**

In `executeWithEvent`, at the line `// Execute steps` (about line 648, immediately before `stepCtx := &StepContext{`), add:

```go
	if !journalSkipsAutomation(automation) {
		e.journal.openRun(ctx, automation, exec, triggeringEvent)
	} else if e.logger != nil {
		e.logger.Debug("work journal skipped: the automation reacts to work rows", "automation", automation.Name)
	}
	journal := e.journal
	if journalSkipsAutomation(automation) {
		journal = nil
	}
```

Then, using `journal` (the local) in the loop:

1. In the skipped-step branch, immediately after `notifyStepObserver(ctx, skipResult)` (about line 719), add `journal.stepSkipped(ctx, exec, step, stepIndex)`.
2. Immediately before `result, err := e.executeStep(ctx, step, stepCtx)` (about line 731), add `journal.stepRunning(ctx, exec, step, stepIndex, 1)`.
3. Immediately after `notifyStepObserver(ctx, result)` inside `if result != nil {` (about line 745), add `journal.stepFinished(ctx, exec, step, result)`.
4. In the `ErrorStrategyRetry` branch, where the retried result is recorded and `notifyStepObserver(ctx, result)` is called (about line 784), add `journal.stepFinished(ctx, exec, step, result)` after it, and before the retry's `executeStep` call add `journal.stepRunning(ctx, exec, step, stepIndex, attempt+1)` where `attempt` is the loop's retry counter variable in that branch (read the branch; the counter is the `for` index of the retry loop, so pass its value plus one).
5. Replace BOTH `e.saveCheckpointOnFailure(ctx, automation, exec, step, stepIndex, chainHead, err, triggeringEvent)` calls (about lines 790 and 796) with `journal.closeRun(ctx, exec)` (each sits right after `exec.Fail(err)`).
6. At the cancellation branch `exec.Cancel()` (about line 668), add `journal.closeRun(ctx, exec)` immediately after it.
7. Immediately after `exec.Complete()` (about line 814), add `journal.closeRun(ctx, exec)`.

Delete `saveCheckpointOnFailure` and `newCheckpointFromExecution` from `executor.go` (they have no callers now). Leave `checkpoint.go` for Task 11.

Run: `go build ./component/automations/ && go test -count=1 -run 'TestExecutor_Journals|TestExecutor_SandboxRun|TestJournal' ./component/automations/`
Expected: PASS.

- [ ] **Step 4: Run the package and commit**

```bash
go test -count=1 ./component/automations/
git add component/automations/executor.go component/automations/journal_test.go
git commit -m "automations: journal every run and step boundary from the executor"
```

### Task 10: Resume from the journal

**Files:**
- Modify: `component/automations/resume.go` (the loading half replaced; the rehydration half kept), `component/automations/scheduler.go` (`ResumeAutomation`)
- Create: `component/automations/resume_test.go`

**Interfaces:**
- Consumes: `workRunById`, `workStepsForRun` (Task 3); `memql.MaterializeRows`; `journalContext` (Task 8).
- Produces: `type RunJournal struct { RunId, AutomationName, TemplateFingerprint, TriggeredBy string; Input any; InputFingerprint string; TriggerEvent map[string]any; CallerSuppliedPayload bool; ChainHead, InitialChainHead string; StepOrder []string; Steps map[string]*MinimalStepResult; FailedStep string }`; `func LoadRunJournal(ctx, exec journalExecutor, runId string) (*RunJournal, error)`; `func ValidateRunJournal(j *RunJournal, automation *Automation, idEngine *id.Engine) error`; `func (e *Executor) ResumeFrom(ctx, journal *RunJournal, automation *Automation, opts *ResumeOptions) (*AutomationExecution, error)`; errors `ErrRunNotFound`, `ErrRunJournalInvalid`, `ErrAutomationChanged` (kept), `ErrNonRetryableStep` (kept).

- [ ] **Step 1: Write the failing tests**

```go
package automations

import (
	"errors"
	"testing"

	"github.com/znasllc-io/memql/core/id"
)

func TestValidateRunJournal_RefusesAChangedAutomation(t *testing.T) {
	auto := &Automation{Name: "demo", Steps: []*Step{{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}}}}
	j := &RunJournal{RunId: "run-1", AutomationName: "demo", TemplateFingerprint: "stale", FailedStep: "a"}
	if err := ValidateRunJournal(j, auto, id.New()); !errors.Is(err, ErrAutomationChanged) {
		t.Fatalf("err = %v, want ErrAutomationChanged", err)
	}
	j.TemplateFingerprint = auto.DefinitionFingerprint(id.New())
	if err := ValidateRunJournal(j, auto, id.New()); err != nil {
		t.Fatalf("matching fingerprint refused: %v", err)
	}
}

func TestValidateRunJournal_NeedsARunAndAFailedStep(t *testing.T) {
	if err := ValidateRunJournal(nil, &Automation{}, id.New()); !errors.Is(err, ErrRunJournalInvalid) {
		t.Fatalf("nil journal: %v", err)
	}
	if err := ValidateRunJournal(&RunJournal{RunId: "r", AutomationName: "demo"}, &Automation{Name: "demo"}, id.New()); !errors.Is(err, ErrRunJournalInvalid) {
		t.Fatalf("no failed step: %v", err)
	}
}

func TestRunJournalFromRows_TakesTheLatestVersionOfEachStep(t *testing.T) {
	run := map[string]any{
		"id": "v1:work:run:run-1", "automationName": "demo", "templateFingerprint": "fp",
		"triggeredBy": "event", "callerSuppliedPayload": true, "input": map[string]any{"k": "v"},
		"chainHead": "h2", "initialChainHead": "h0", "stepOrder": []any{"a", "b"},
	}
	steps := []map[string]any{
		{"id": "v1:work:step:run-1-a", "key": "a", "status": "done", "result": map[string]any{"stepId": "a", "status": "completed", "result": 1}},
		{"id": "v1:work:step:run-1-b", "key": "b", "status": "failed", "errorMessage": "boom"},
	}
	j, err := runJournalFromRows(run, steps)
	if err != nil {
		t.Fatalf("runJournalFromRows: %v", err)
	}
	if j.RunId != "run-1" || j.AutomationName != "demo" || !j.CallerSuppliedPayload || j.ChainHead != "h2" {
		t.Errorf("run fields: %+v", j)
	}
	if j.FailedStep != "b" {
		t.Errorf("failed step = %q, want b", j.FailedStep)
	}
	if got := j.Steps["a"]; got == nil || got.Status != "completed" {
		t.Errorf("done step a not rehydrated: %+v", got)
	}
	if _, present := j.Steps["b"]; present {
		t.Error("a failed step is not a completed result")
	}
	if len(j.StepOrder) != 2 {
		t.Errorf("stepOrder = %v", j.StepOrder)
	}
}

func TestResumeRetryableRule(t *testing.T) {
	if !IsStepRetryable(StepTypeQuery) || IsStepRetryable(StepTypeMutation) || IsStepRetryable(StepTypeWebhook) {
		t.Fatal("read-only steps retry freely; mutation and webhook need AllowSideEffects")
	}
}
```

Run: `go test -count=1 -run 'TestValidateRunJournal|TestRunJournalFromRows|TestResumeRetryableRule' ./component/automations/`
Expected: FAIL, `undefined: RunJournal`.

- [ ] **Step 2: Rewrite the loading half of resume.go**

Replace the file header and the `ResumeFrom` signature. Keep the body of `ResumeFrom` from the line `// Inject system actor for automation execution` onward UNCHANGED except for the four substitutions named below. New top of file:

```go
package automations

// resume.go -- resume a run from the work journal (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section D).
//
// The journal rows replace the checkpoint side-record: a run's own row
// carries what the checkpoint carried (the input envelope, the trigger,
// the caller-supplied flag, the chain heads, the step order, the
// automation fingerprint) and each step row carries its trimmed result.
// LoadRunJournal reads them under the journal's own cluster actor, AFTER
// the caller's handler has enforced who may resume -- the rows are the
// deployment's, and an admin who may resume must not be refused by the
// tier on the read.
//
// THE SECURITY RULE IS UNCHANGED (memql#2888, memql#2890): internal
// origin on resume requires a TRUSTED source AND a trigger payload the
// caller did not supply. CallerSuppliedPayload rides on the run row for
// exactly that reason.
//
// THE RETRYABLE RULE IS THE IDEMPOTENCY RULE'S A1 FORM (spec section D):
// a completed step is served from the journal and never re-run; a step
// whose type has no external effect (query, shape, function, forEach,
// parallel, switch, automation) is re-run; a mutation, webhook, event or
// action step at the resume point needs AllowSideEffects, because the
// journal cannot yet tell whether its far side already holds a receipt.
// Epic A2 wires the receipts and narrows this to "retried when
// idempotent by key".

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

var (
	ErrRunNotFound       = errors.New("work run not found")
	ErrRunJournalInvalid = errors.New("work run journal is invalid")
	ErrAutomationChanged = errors.New("automation definition changed since the run started")
	ErrNonRetryableStep  = errors.New("step is not safely retryable (mutation, webhook, event or action)")
)

// RunJournal is what resume needs from the rows: the run's envelope and
// the completed steps' trimmed results.
type RunJournal struct {
	RunId                 string
	AutomationName        string
	TemplateFingerprint   string
	TriggeredBy           string
	Input                 any
	InputFingerprint      string
	TriggerEvent          map[string]any
	CallerSuppliedPayload bool
	ChainHead             string
	InitialChainHead      string
	StepOrder             []string
	// Steps holds the completed (done) steps by key, in the MinimalStepResult
	// shape the evaluator is rehydrated from.
	Steps map[string]*MinimalStepResult
	// FailedStep is the key of the step at `failed` or `running` with no
	// receipt -- the default resume point.
	FailedStep string
}

// ResumeOptions configures resume behavior.
type ResumeOptions struct {
	// FromStep overrides the resume point (defaults to the failed step).
	FromStep string
	// AllowSideEffects permits retrying mutation / webhook / event / action steps.
	AllowSideEffects bool
}

// IsStepRetryable reports whether a step type can be re-run with no
// external effect. Moved here from checkpoint.go.
func IsStepRetryable(stepType StepType) bool {
	switch stepType {
	case StepTypeMutation, StepTypeWebhook, StepTypeEvent, StepTypeAction:
		return false
	}
	return true
}

// LoadRunJournal reads one run and its steps.
func LoadRunJournal(ctx context.Context, exec journalExecutor, runId string) (*RunJournal, error) {
	if exec == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	runId = strings.TrimSpace(runId)
	if runId == "" {
		return nil, fmt.Errorf("%w: empty run id", ErrRunJournalInvalid)
	}
	jctx := journalContext(ctx)
	runCall, err := journalArgs("workRunById", map[string]any{"runId": runId})
	if err != nil {
		return nil, err
	}
	res, err := exec.Execute(jctx, "query "+runCall)
	if err != nil {
		return nil, fmt.Errorf("load run %s: %w", runId, err)
	}
	runs := memql.MaterializeRows(res)
	if len(runs) == 0 {
		return nil, ErrRunNotFound
	}
	stepCall, err := journalArgs("workStepsForRun", map[string]any{"runId": runId})
	if err != nil {
		return nil, err
	}
	res, err = exec.Execute(jctx, "query "+stepCall)
	if err != nil {
		return nil, fmt.Errorf("load steps of run %s: %w", runId, err)
	}
	return runJournalFromRows(runs[0], memql.MaterializeRows(res))
}

// runJournalFromRows folds one run row and its step rows into a RunJournal.
// Row ids arrive canonical (v1:work:run:<short>); the short id is what the
// executor minted.
func runJournalFromRows(run map[string]any, steps []map[string]any) (*RunJournal, error) {
	if run == nil {
		return nil, ErrRunNotFound
	}
	j := &RunJournal{
		RunId:                 shortId(stringField(run, "id")),
		AutomationName:        stringField(run, "automationName"),
		TemplateFingerprint:   stringField(run, "templateFingerprint"),
		TriggeredBy:           stringField(run, "triggeredBy"),
		Input:                 run["input"],
		InputFingerprint:      stringField(run, "inputFingerprint"),
		CallerSuppliedPayload: boolField(run, "callerSuppliedPayload"),
		ChainHead:             stringField(run, "chainHead"),
		InitialChainHead:      stringField(run, "initialChainHead"),
		Steps:                 map[string]*MinimalStepResult{},
	}
	if ev, ok := run["triggerEvent"].(map[string]any); ok {
		j.TriggerEvent = ev
	}
	if order, ok := run["stepOrder"].([]any); ok {
		for _, s := range order {
			if k, ok := s.(string); ok {
				j.StepOrder = append(j.StepOrder, k)
			}
		}
	}
	for _, row := range steps {
		key := stringField(row, "key")
		if key == "" {
			continue
		}
		switch stringField(row, "status") {
		case "done":
			m := &MinimalStepResult{StepId: key, Status: "completed"}
			if r, ok := row["result"].(map[string]any); ok {
				_ = mapStructFromPayload(r, m)
				m.StepId = key
			}
			j.Steps[key] = m
		case "failed", "running":
			if j.FailedStep == "" {
				j.FailedStep = key
			}
		}
	}
	return j, nil
}

// ValidateRunJournal is the resume precondition: a run, a resume point,
// and an automation that has not changed underneath it.
func ValidateRunJournal(j *RunJournal, automation *Automation, idEngine *id.Engine) error {
	if j == nil {
		return ErrRunJournalInvalid
	}
	if j.RunId == "" || j.AutomationName == "" {
		return fmt.Errorf("%w: missing run id or automation name", ErrRunJournalInvalid)
	}
	if j.FailedStep == "" {
		return fmt.Errorf("%w: run %s has no failed or unfinished step to resume from", ErrRunJournalInvalid, j.RunId)
	}
	if j.TemplateFingerprint != "" && automation != nil && idEngine != nil {
		if current := automation.DefinitionFingerprint(idEngine); current != "" && current != j.TemplateFingerprint {
			return ErrAutomationChanged
		}
	}
	return nil
}

func shortId(canonical string) string {
	if i := strings.LastIndex(canonical, ":"); i >= 0 {
		return canonical[i+1:]
	}
	return canonical
}

func stringField(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func boolField(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}
```

`mapStructFromPayload` lives in `checkpoint.go` today; move it into `resume.go` verbatim in Task 11 when that file is deleted (for this task, leave it where it is; both files are in the package).

Then in the kept body of `ResumeFrom`:
1. Change the signature to `func (e *Executor) ResumeFrom(ctx context.Context, journal *RunJournal, automation *Automation, opts *ResumeOptions) (*AutomationExecution, error)` and the nil check to `if journal == nil { return nil, ErrRunJournalInvalid }`.
2. Replace `ValidateCheckpoint(checkpoint, automation, fingerprintEngine)` with `ValidateRunJournal(journal, automation, fingerprintEngine)`.
3. Replace every `checkpoint.FailedAt.StepId` with `journal.FailedStep`, `checkpoint.ExecutionId` with `journal.RunId`, `checkpoint.CallerSuppliedPayload` with `journal.CallerSuppliedPayload`, `checkpoint.StepResults` with `journal.Steps`, `checkpoint.Input` with `journal.Input`, `checkpoint.InputFingerprint` with `journal.InputFingerprint`, `checkpoint.ChainHead` / `InitialChainHead` with the journal's, `checkpoint.StepOrder` with `journal.StepOrder`, and the trigger-context read with `journal.TriggerEvent` (build the `events.Event` the old code built from `TriggerContext.Event`, now from that map).
4. Keep the resumed execution ON THE SAME RUN ID: after `exec := NewExecution(automation.Name, triggeredBy)` add `exec.ID = journal.RunId`, and before the resumed steps run add `e.journal.reopenRun(ctx, exec)`; add that method to `journal.go`:

```go
// reopenRun flips a failed run back to running for a resume; the retried
// steps write new versions with attempt incremented.
func (j *workJournal) reopenRun(ctx context.Context, exec *AutomationExecution) {
	if j == nil || exec == nil {
		return
	}
	j.call(ctx, "updateWorkRun", map[string]any{
		"runId":  exec.ID,
		"status": "running",
	})
}
```

and have the resumed step loop pass `attempt` = (the number of existing versions is not known here) `2` to `stepRunning` for the resume point and `1` for steps after it -- a resumed step is by definition at least its second attempt.

- [ ] **Step 3: Re-point the scheduler**

In `component/automations/scheduler.go`, replace the body of `ResumeAutomation` so it loads the journal:

```go
func (s *Scheduler) ResumeAutomation(ctx context.Context, executionId string, fromStep string) (*AutomationExecution, error) {
	journal, err := LoadRunJournal(ctx, s.engine, executionId)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	automation, ok := s.automations[journal.AutomationName]
	s.mu.RUnlock()
	if !ok {
		automation, err = s.loader.LoadByName(journal.AutomationName)
		if err != nil {
			return nil, fmt.Errorf("automation %q not found: %w", journal.AutomationName, err)
		}
	}
	opts := &ResumeOptions{FromStep: fromStep}
	return s.scheduleExecutor.ResumeFrom(ctx, journal, automation, opts)
}
```

The `executionId` parameter is the run id; the HTTP contract (`POST /automations/resume`, `component/server/server.go`) does not change.

Run: `go build ./... 2>&1 | head` from the repo root, then `go test -count=1 -run 'TestValidateRunJournal|TestRunJournalFromRows|TestResumeRetryableRule' ./component/automations/`
Expected: builds; PASS.

- [ ] **Step 4: Adapt the tests that named the checkpoint**

```bash
grep -rln 'ExecutionCheckpoint\|SaveCheckpoint\|LoadCheckpoint\|ResumeFrom(\|ToMinimalStepResults\|ResumeAutomation(' --include=*_test.go component app cmd
```
Expected: `component/automations/step_origin_test.go`, `component/automations/steps/dryrun_source_trust_2890_db_test.go`, `component/server/trigger_handler_test.go`. In each: a test that built an `ExecutionCheckpoint` to call `ResumeFrom` now builds a `RunJournal` with the same fields (`ExecutionId` becomes `RunId`, `FailedAt.StepId` becomes `FailedStep`, `StepResults` becomes `Steps`); a test that asserted a checkpoint ROW was or was not written (the 2890 dry-run test) now asserts a `v1:work:run` row was or was not written for the execution id (`query workRunById(runId: ...)` under `journalContext`). The property each test guards is unchanged; only the record it reads changed.

```bash
go test -count=1 ./component/automations/... ./component/server/
git add component/automations/resume.go component/automations/resume_test.go component/automations/scheduler.go component/automations/journal.go component/automations/step_origin_test.go component/automations/steps/dryrun_source_trust_2890_db_test.go component/server/trigger_handler_test.go
git commit -m "automations: resume from the work journal"
```

### Task 11: Retire the checkpoint

**Files:**
- Delete: `component/automations/checkpoint.go`
- Modify: `component/automations/types.go` (remove `ExecutionCheckpoint`, `StepFailure`, `TriggerContext`; keep `MinimalStepResult`), `component/automations/resume.go` (receives `mapStructFromPayload`, `ToMinimalStepResults`, `shouldIncludeResult`, `extractCheckpointPayload` if still referenced, `jsonString` if still referenced), `dsl/memql/concepts.memql` (remove `concept checkpoint`), `component/database/memory-nodes/concept_ids.go` (remove `ConceptMemQLCheckpoint` and its `AllFilesystemConcepts` entry), `component/automations/steps/dryrun.go` (the comment naming `SaveCheckpoint`)

- [ ] **Step 1: Move the helpers, delete the file**

Move `ToMinimalStepResults`, `shouldIncludeResult` and `mapStructFromPayload` from `checkpoint.go` into `resume.go` verbatim. Delete `checkpoint.go`. Remove the three checkpoint structs from `types.go` and the `CheckpointConcept` / `DefaultCheckpointTTL` / `ErrCheckpointNotFound` / `ErrCheckpointExpired` / `ErrCheckpointInvalid` declarations wherever they are. Remove `concept checkpoint { ... }` from `dsl/memql/concepts.memql` (leave the file's other content). Remove `ConceptMemQLCheckpoint` from `concept_ids.go`, including the `// memql (filesystem-backed only)` entry in `AllFilesystemConcepts()`. In `steps/dryrun.go`, rewrite the comment that names `SaveCheckpoint` to say the sandbox writes no journal rows (journal.go).

```bash
go build ./... && go vet ./component/automations/ ./component/database/...
grep -rn 'checkpoint' --include=*.go component/automations component/database/memory-nodes component/server app | grep -vi 'checkpoint-restore\|// ' | head
```
Expected: builds; the grep prints nothing but comments.

- [ ] **Step 2: Regenerate and gate**

```bash
go run ./cmd/memqllint dsl/
make sdk-gen && make sdk-gen-check
go test -count=1 -run 'TestUndeclaredRowAuthzPopulationOnlyShrinks|TestEngineLoad' ./component/memql/
make test
git add -u component/automations dsl/memql/concepts.memql component/database/memory-nodes/concept_ids.go sdk/go/client sdk/ts/src/generated
git status --short
git commit -m "automations: retire the checkpoint side-record; the journal is the record"
```

### Task 12: Database-gated tests for the journal

**Files:**
- Create: `component/automations/journal_db_test.go`

- [ ] **Step 1: Write the test**

Model the engine bootstrap on the package's `TestMain` and `openTestDB` (`cluster_guard_db_test.go`): the schema is applied by `dbtest.EnsureSchema`, the DSN comes from `dbtest.DSN()`, an unreachable database calls `dbtest.Unreachable`. Build a real `*memql.MemQLEngine` the way the package's other db tests do (`grep -n 'NewMemQLEngine\|memql.New' component/automations/*_db_test.go` shows the constructor and options in use).

```go
package automations

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
)

// failOnceRegistry fails step "b" on its first execution and succeeds after,
// which is the shape a resume has to prove: the journal holds a done "a",
// a failed "b", and the resumed run re-runs "b" alone.
type failOnceRegistry struct{ failed bool }

func (r *failOnceRegistry) Execute(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	now := time.Now()
	if step.ID == "b" && !r.failed {
		r.failed = true
		return &StepResult{StepId: step.ID, Status: "failed", Error: "first time fails", StartedAt: now, CompletedAt: now}, errors.New("first time fails")
	}
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": step.ID}, StartedAt: now, CompletedAt: now}, nil
}

func TestJournal_DB_RowsWrittenAndResumed(t *testing.T) {
	engine := openTestEngine(t) // the package's db-test engine helper; see the TestMain in this package
	reg := &failOnceRegistry{}
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: reg})
	name := fmt.Sprintf("journalProbe%d", time.Now().UnixNano())
	auto := &Automation{Name: name, Steps: []*Step{
		{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}},
		{ID: "b", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}, OnError: ErrorStrategyStop},
	}}

	exec, _ := e.Execute(context.Background(), auto, "test")
	if exec.Status != "failed" {
		t.Fatalf("first run status = %q, want failed", exec.Status)
	}

	// The rows: one run at failed, a done and a failed step.
	journal, err := LoadRunJournal(context.Background(), engine, exec.ID)
	if err != nil {
		t.Fatalf("LoadRunJournal: %v", err)
	}
	if journal.AutomationName != name || journal.FailedStep != "b" {
		t.Fatalf("journal = %+v", journal)
	}
	if got := journal.Steps["a"]; got == nil || got.Status != "completed" {
		t.Fatalf("step a not journaled as done: %+v", got)
	}

	// Resume re-runs only b, on the same run id.
	resumed, err := e.ResumeFrom(context.Background(), journal, auto, &ResumeOptions{})
	if err != nil {
		t.Fatalf("ResumeFrom: %v", err)
	}
	if resumed.ID != exec.ID {
		t.Fatalf("resumed run id = %s, want the original %s", resumed.ID, exec.ID)
	}
	if resumed.Status != "completed" {
		t.Fatalf("resumed status = %q", resumed.Status)
	}
	if _, ran := resumed.Steps["a"]; ran && resumed.Steps["a"].Result == nil {
		t.Fatal("step a must be served from the journal, not re-run")
	}
	after, err := LoadRunJournal(context.Background(), engine, exec.ID)
	if err != nil {
		t.Fatalf("LoadRunJournal after resume: %v", err)
	}
	if after.FailedStep != "" {
		t.Fatalf("after resume the run still reports a failed step: %q", after.FailedStep)
	}
}

func TestJournal_DB_SandboxWritesNothing(t *testing.T) {
	engine := openTestEngine(t)
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: journalProbeRegistry{}, SandboxRun: true})
	auto := &Automation{Name: fmt.Sprintf("sandboxProbe%d", time.Now().UnixNano()), Steps: []*Step{{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}}}}
	exec, _ := e.Execute(context.Background(), auto, "test")
	if _, err := LoadRunJournal(context.Background(), engine, exec.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("a sandboxed run must leave no row; got %v", err)
	}
}
```

`openTestEngine(t)` is whichever helper this package's existing db tests use to build an engine on `dbtest.DSN()`; if none exists, add one to this file that opens the DB as `openTestDB` does and constructs the engine the way `component/memql`'s `engine_load_smoke_test.go` does, calling `dbtest.Unreachable` when the ping fails. Both tests skip when no database is reachable and FAIL under `MEMQL_REQUIRE_DB=1`.

- [ ] **Step 2: Run on the db lane**

```bash
MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<dsn> go test -count=1 -run 'TestJournal_DB' ./component/automations/
```
Expected: PASS. (The shared throwaway Postgres is on port 15434 per the project memory; a peer session's run can red it.)

- [ ] **Step 3: Commit**

```bash
git add component/automations/journal_db_test.go
git commit -m "automations: db-gated journal and resume tests"
```

### Task 13: Docs and PR 1 of 2

- [ ] **Step 1: Re-point the resume paragraph**

```bash
grep -n 'checkpoint' docs/public/language/memql.md docs/public/operate/*.md CLAUDE.md component/CLAUDE.md | head
```
For each hit that describes resuming an automation from a checkpoint, rewrite the sentence to say the run is resumed from its work journal (`v1:work:run` and `v1:work:step` rows) by run id, and that a sandboxed dry-run writes no rows. Do not touch hits that are about database checkpoints or unrelated words.

- [ ] **Step 2: Gates, push, PR**

```bash
make arch-model && make arch-model-check
make test
go test -count=1 .
git add -u docs CLAUDE.md component/CLAUDE.md component/architecture
git commit -m "docs: resume reads the work journal"
git push -u origin feat/work-spine-a1-rows-and-journal
gh pr create --repo znasllc-io/memql --title "work spine A1, PR 1 of 2: the work and skills namespaces, and the journal replaces the checkpoint" --body-file - <<'EOF'
The work namespace (goal, run, step, modelCall, approval, observation) with the composite tier, its server-only reads and writes and broadcast routing rules; the skills namespace moved out of agents with skillEdge; and the journal: every automation execution now writes a v1:work:run row and one v1:work:step row per step at each boundary, under a synthetic cluster actor; resume loads those rows instead of the 24-hour checkpoint side-record, under the same source-trust rule; the checkpoint concept and its files are gone. An automation that reacts to work rows is not journaled, so the graph cannot feed the journal back into itself.

Design record: docs/superpowers/specs/2026-09-05-work-spine-design.md (section D).

Ships in PR 1 of 2 of epic A1. Closes #4963, #4964. PR 2 (the retirements and the portal Nexus deletion) follows.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01LXdTWPHNiDZUUgyggSAaoT
EOF
gh pr checks <n> --repo znasllc-io/memql --watch
gh pr merge <n> --repo znasllc-io/memql
```


---

## PR 2 of 2 -- retire

Branch `feat/work-spine-a1-retire` off `main` after PR 1 merged. This PR is mostly deletion. Every "delete" below is `git rm`; every "move" is `git mv` followed by import edits. After each task, `go build ./... && go vet ./...` from the repo root is the check that nothing dangling remains, and the named tests are the check that nothing that mattered went with it.

### Task 14: Move the authored-action helpers out of the harness module

**Files:**
- Move: `component/harness/actionpin` -> `component/actions/pin`, `component/harness/parambind` -> `component/actions/bind`, `component/harness/surfaceresolver` -> `component/actions/surfaceresolver`, `component/harness/actionreplay` -> `component/actions/fingerprint`
- Modify: `component/automations/steps/action.go`, `component/actions/surfaces.go`, `component/actions/go.mod`, `component/actions/go.sum`, and every `_test.go` under the moved directories

**Interfaces:**
- Produces: the same exported API under the new import paths: `github.com/znasllc-io/memql/component/actions/pin`, `.../bind`, `.../surfaceresolver`, `.../fingerprint`. Package names become `pin`, `bind`, `surfaceresolver`, `fingerprint`.

- [ ] **Step 1: Confirm the four packages are pure**

```bash
for p in actionpin parambind surfaceresolver actionreplay; do echo "$p: $(grep -h '"github.com/znasllc-io/memql/' component/harness/$p/*.go | grep -v _test | sort -u | wc -l) internal imports"; done
```
Expected: `0` for each. If one prints more than 0, read the import; a dependency on `component/harness` root types means that type moves with it into `component/actions`.

- [ ] **Step 2: Move and rename**

```bash
git mv component/harness/actionpin component/actions/pin
git mv component/harness/parambind component/actions/bind
git mv component/harness/surfaceresolver component/actions/surfaceresolver
git mv component/harness/actionreplay component/actions/fingerprint
sed -i 's/^package actionpin$/package pin/' component/actions/pin/*.go
sed -i 's/^package parambind$/package bind/' component/actions/bind/*.go
sed -i 's/^package actionreplay$/package fingerprint/' component/actions/fingerprint/*.go
```
(`surfaceresolver` keeps its package name.) Then in `component/automations/steps/action.go` and `component/actions/surfaces.go` replace the four import paths and every `actionpin.` / `parambind.` / `actionreplay.` selector with `pin.` / `bind.` / `fingerprint.`. In `component/actions/go.mod` remove the `require github.com/znasllc-io/memql/component/harness` line and its `replace` line; run `cd component/actions && go mod tidy && cd -`.

- [ ] **Step 3: Build and test the moved packages**

```bash
go build ./component/actions/... ./component/automations/...
go test -count=1 ./component/actions/... ./component/automations/steps/
```
Expected: PASS. (The `action_*_test.go` files in `component/automations/steps` exercise the authored step end to end and are the proof the primitive survived.)

- [ ] **Step 4: Commit**

```bash
git add -A component/actions component/automations/steps/action.go
git commit -m "actions: move the authored-action helpers out of the harness module"
```
(`git add -A` scoped to the two paths above is a rename-aware stage of exactly those trees; do not widen it.)

### Task 15: Retire the action capture library

**Files:**
- Delete: `component/harness/actionplan`, `component/harness/actiontrace`, `component/harness/actiontrust`, `app/integrations_harness_replay.go`, `app/integrations_harness_trace.go`, `integrations/planner/action_substitution.go` (+ its `_test.go`), `integrations/actionsearch/`, `cmd/action-upgrade/`, `component/memql/mint_action_required_fields_3619_test.go`
- Modify: `app/plugins_core.go` (the `actionsearch` import), `integrations/planner/agent_loop.go` (the `maybeSubstituteActionStep` call sites), `component/memql/executor_mutation.go` (the `embedActionIntent` hook), `component/memql/openai_embedding.go` (comment), `component/memql/ambient_domain_test.go` (the actions-mutation pins), `component/memql/rowauthz_undeclared_gate_test.go` (entries for deleted queries), `dsl/actions/concepts.memql`, `dsl/actions/queries.memql`, `dsl/actions/mutations.memql`, `dsl/actions/shapes.memql`

- [ ] **Step 1: Delete the Go**

```bash
git rm -r component/harness/actionplan component/harness/actiontrace component/harness/actiontrust integrations/actionsearch cmd/action-upgrade
git rm app/integrations_harness_replay.go app/integrations_harness_trace.go integrations/planner/action_substitution.go component/memql/mint_action_required_fields_3619_test.go
ls integrations/planner/action_substitution_test.go 2>/dev/null && git rm integrations/planner/action_substitution_test.go
```
Remove the `_ "github.com/znasllc-io/memql/integrations/actionsearch"` line from `app/plugins_core.go`. In `integrations/planner/agent_loop.go`, find every call to `maybeSubstituteActionStep` (`grep -n maybeSubstituteActionStep integrations/planner/*.go`) and delete the call and the branch that consumed its result, leaving the plain dispatch path. In `component/memql/executor_mutation.go`, delete the `embedActionIntent` function and the `if conceptMeta.Name == memorynodes.ConceptActionsAction { ... }` block that called it; the observation hook next to it stays for Task 17.

- [ ] **Step 2: Trim the DSL**

In `dsl/actions/concepts.memql`: delete `concept candidate` and its `use harness.concepts.{ plan, step }` import (and any `@relationship(... target=plan|step ...)` on `action` that pointed at harness concepts; `grep -n 'target=plan\|target=step' dsl/actions/concepts.memql`). Keep `action` and `surface` exactly as they are.
In `dsl/actions/mutations.memql`: delete `recordActionCandidate`, `mintAction`, `reinforceAction`, `confirmAction`, `deprecateAction`, `decayAction`. Keep `registerSurface`, `setSurfaceAvailability`, `bumpActionVersion`.
In `dsl/actions/queries.memql`: delete `actionCandidatesForPlan`, `actionByInputFingerprint`, `actionByTemplateFingerprint`, `actionsPendingConfirm`, and the `searchActions` builtin. Keep `actionById`, `actionByIdAndVersion`, `activeActionsForOwner`, `surfacesForOwner`.
In `dsl/actions/shapes.memql`: delete any shape bound to `candidate`; keep the rest.

```bash
go run ./cmd/memqllint dsl/
go build ./... && go vet ./component/memql/ ./integrations/planner/ ./app/
```

- [ ] **Step 3: Tests and gates**

In `component/memql/ambient_domain_test.go`, the table that pins `"bumpActionVersion": "v1:actions:action"` and its siblings: delete the rows for the mutations removed above, keep `bumpActionVersion`. In `component/memql/rowauthz_undeclared_gate_test.go`, delete the entries for `actionCandidatesForPlan`, `actionByInputFingerprint`, `actionByTemplateFingerprint`, `actionsPendingConfirm` if present. In `component/healing/repair_loop.go` nothing changes: `RemediationFeeder` is an interface no production code wired.

```bash
make sdk-gen && make sdk-gen-check
go test -count=1 ./component/memql/ ./integrations/planner/ ./component/healing/...
git add -u . && git status --short | grep -v '^[MD] ' ; git commit -m "actions: retire the capture library, keep the authored primitive"
```
(`git add -u` stages tracked modifications and deletions only; the `git status` line must print nothing, meaning no untracked file was swept in.)

### Task 16: Retire the harness spine

**Files:**
- Delete: `component/harness/` (everything that remains), `cmd/harness-eval/`, `app/integrations_harness_init.go`, `component/memql/harness_step_validation.go` and `harness_step_validation_test.go`, `component/memql/harness_pack_disabled_db_test.go`, `dsl/harness/prompts/decomposeGoal.tmpl`
- Modify: `app/integrations_planner.go` and `app/integrations_agent.go` (the `a.setupHarnessReconciler()` calls), `component/memql/executor_mutation.go` (the step guard block), `component/memql/inner_loop.go` (comments), `component/memql/concept_resolver.go` and `component/language/ast/ast.go` and `component/memql/sense/hover.go` (comments that use the harness plan as the example), `component/memql/ambient_domain_test.go` (the ambiguous-name case), `integrations/agent/subagent.go`, `go.work`, `scripts/ci/db-gated-packages.sh`, `.github/workflows/ci.yml`, `component/database/memory-nodes/concept_ids.go`

- [ ] **Step 1: Delete the reconciler, the planner and their wiring**

```bash
git rm -r component/harness cmd/harness-eval
git rm app/integrations_harness_init.go component/memql/harness_step_validation.go component/memql/harness_step_validation_test.go component/memql/harness_pack_disabled_db_test.go
```
Remove the `a.setupHarnessReconciler()` call from `app/integrations_planner.go` (line 18 at df33cef4b) and `app/integrations_agent.go` (line 23). Delete `app/integrations_harness_action_step_test.go` if it exists (`ls app/*harness*`). In `component/memql/executor_mutation.go` delete the block:

```go
	if conceptMeta.Name == memorynodes.ConceptHarnessStep {
		if err := e.validateHarnessStepTransition(ctx, payload, mutation.ID); err != nil {
			return nil, meta, err
		}
	}
```
and its comment. In `component/database/memory-nodes/concept_ids.go` delete `ConceptHarnessPlan`, `ConceptHarnessStep`, `ConceptHarnessObservation` and their comment block (Task 17 re-points the one remaining reader of the observation constant).

- [ ] **Step 2: The subagent file keeps its role helpers**

In `integrations/agent/subagent.go`, delete `SpecialistStep`, `SpecialistObservation`, `ScopedContextOptions`, `BuildScopedSpecialistContext` and `AggregateSpecialistObservations` (and their tests in `subagent_test.go`); keep `HarnessRole`, `HarnessRoleHintKey`, `ResolveHarnessRole`, `RoleAllowsTool`, `ScopeToolsForRole`, `ScopeToolDefinitionsForRole`, `IsResume`, `ScopeToolsForDeliverableSurface`, `IsProduceArtifactExecutionTurn`, `IsBackgroundLane`, which `replier.go` reads. Rewrite the file header so it describes the assistant-versus-worker tool boundary the chat path enforces and no longer names the harness reconciler.

```bash
grep -n 'BuildScopedSpecialistContext\|AggregateSpecialistObservations\|SpecialistStep' integrations/agent/*.go app/*.go
```
Expected: no hits outside tests you just edited.

- [ ] **Step 3: Workspace, CI, scripts**

- `go.work`: delete the `./component/harness` line.
- `scripts/ci/db-gated-packages.sh`: delete the `"component/harness"` entry.
- `.github/workflows/ci.yml`: delete the `- 'component/harness/**'` path-bucket line (about line 590) and the `harness eval (regression gate)` step (the `- name: harness eval ...` block with its `run: go run ./cmd/harness-eval`, about lines 840-845); update the two comments naming `harness-eval` (about lines 35 and 692). Deleting a whole `- name:` block leaves the neighbouring steps' `with:` blocks untouched; re-read the surrounding YAML after the edit.

```bash
go build ./... && go vet ./...
go test -count=1 ./scripts/ci/
```

- [ ] **Step 4: The comments and the ambiguous-name test**

`component/memql/concept_resolver.go`, `component/language/ast/ast.go` and `component/memql/sense/hover.go` use `harness.plan` versus `planner.plan` as the worked example of a bare name shared across namespaces. The RULE stays; change the example to `v1:worker:invocation` versus `v1:observability:invocation`, which is the pair that still exists. In `component/memql/ambient_domain_test.go`, rewrite the harness case the same way: the source that imports `worker.concepts.{ invocation }` resolves to `v1:worker:invocation`, the observability-domain source resolves to the observability one. `component/memql/inner_loop.go`'s observation-sink comments now say the sink will write `v1:work:observation` rows (epic A2).

```bash
go test -count=1 -run 'TestAmbient|TestConceptResolver' ./component/memql/
git add -u . && git status --short | grep -v '^[MD] ' ; git commit -m "harness: retire the reconciler, the planner and the pack wiring"
```

### Task 17: The memory namespace

**Files:**
- Create: `dsl/memory/concepts.memql`, `dsl/memory/mutations.memql`, `dsl/memory/builtins.memql`, `dsl/memory/logic.memql`, `dsl/memory/automations.memql`, `dsl/memory/prompts.memql`, `dsl/memory/prompts/consolidateMemory.tmpl` (all moved from `dsl/harness/`)
- Delete: `dsl/harness/` (what remains), `dsl/harness_pack.go`
- Rename: `component/memql/harness_consolidation.go` -> `memory_consolidation.go` (+ its test)
- Modify: `dsl/embed.go` (`all:memory`), `integrations/harnessrecall/recall.go`, `component/memql/executor_mutation.go` (the observation hook), `dsl/pack_enablement_test.go`, `integrations/embedding/embedding.go` (comment), `component/memql/engine.go` (comment)

**Interfaces:**
- Produces: `v1:memory:belief` (the harness `semanticMemory`, renamed), `v1:memory:consolidationCursor`; mutations `createMemoryBelief`, `reinforceMemoryBelief`, `decayMemoryBelief`, `pruneMemoryBelief`, `advanceMemoryConsolidationCursor`; builtin `recall` (same name, same executor `integration.harnessRecall.recall`, default concept `v1:work:observation`); logic + automation + prompt `consolidateMemory` (same names).

- [ ] **Step 1: Move the DSL**

```bash
mkdir -p dsl/memory/prompts
git mv dsl/harness/prompts/consolidateMemory.tmpl dsl/memory/prompts/consolidateMemory.tmpl
git mv dsl/harness/logic.memql dsl/memory/logic.memql
git mv dsl/harness/automations.memql dsl/memory/automations.memql
git mv dsl/harness/prompts.memql dsl/memory/prompts.memql
```
Create `dsl/memory/concepts.memql` with the header below and then the `concept semanticMemory { ... }` and `concept consolidationCursor { ... }` blocks copied verbatim from `dsl/harness/concepts.memql`, with `concept semanticMemory` renamed to `concept belief`, every `v1:harness:semanticMemory` in doc text changed to `v1:memory:belief`, and `sourceEpisodes`' description changed to name `v1:work:observation`, `run` and `step` as the episodic node ids:

```memql
// concepts.memql
//
// The memory namespace: consolidated beliefs and the per-owner
// consolidation watermark, moved from the harness pack in epic A1 of the
// work spine (docs/superpowers/specs/2026-09-05-work-spine-design.md,
// section F). The episodic memory those beliefs distil from is the work
// journal's v1:work:observation. Both concepts declare the composite tier
// (spec section I); the mutations stamp ownerUserId from the actor.

use identity.concepts.{ user }
```
Each concept keeps `@rowAuthz(owner="ownerUserId")` if it already declares one; if a concept declares NO tier, add `@rowAuthz(owner="ownerUserId", clusterOwner)`.

Create `dsl/memory/mutations.memql` with `use memory.concepts.{ belief, consolidationCursor }` and the five mutation blocks copied verbatim from `dsl/harness/mutations.memql` under these renames: `createHarnessSemanticMemory` -> `createMemoryBelief`, `reinforceHarnessSemanticMemory` -> `reinforceMemoryBelief`, `decayHarnessSemanticMemory` -> `decayMemoryBelief`, `pruneHarnessSemanticMemory` -> `pruneMemoryBelief`, `advanceHarnessConsolidationCursor` -> `advanceMemoryConsolidationCursor`; every `mutate semanticMemory` becomes `mutate belief`. Do NOT copy `createHarnessPlan`, `addHarnessStep`, `readyHarnessStep`, `startHarnessStep`, `completeHarnessStep`, `failHarnessStep` or `recordHarnessObservation`.

Create `dsl/memory/builtins.memql` with the `recall` builtin block copied verbatim from `dsl/harness/queries.memql` (annotations included), changing the default-concept text in its `@description` from `v1:harness:observation` to `v1:work:observation`. Do NOT copy `harnessTrace` (Task 18 replaces it).

In the moved `logic.memql`, `automations.memql` and `prompts.memql`: rewrite the `use harness.` imports to `use memory.`, and the comments that name the harness mutations to the new names. The `consolidateMemory` names stay, so `shippedAutomationCount` is unchanged.

```bash
git rm -r dsl/harness dsl/harness_pack.go
```
In `dsl/embed.go` add `all:memory` to the directive (alphabetical, after `all:library`) and delete the comment paragraph that explains why `harness` was absent from the list.

- [ ] **Step 2: Re-point the Go readers**

- `component/memql/harness_consolidation.go` -> `git mv` to `memory_consolidation.go`; same for its test. Inside, rename nothing functional; update the header comment to name `dsl/memory/automations.memql` and the new mutation names.
- `integrations/harnessrecall/recall.go`: change the default concept literal `v1:harness:observation` (two sites, lines 15 and 125 at df33cef4b) to `v1:work:observation`; its tests likewise. The plugin name `harnessRecall` and the executor `integration.harnessRecall.recall` stay, so the DSL declaration you moved keeps working. Delete the `memql.BindPluginToPack("harnessRecall", "harness")` line: the pack is gone.
- `component/memql/executor_mutation.go`: the observation embed hook `if conceptMeta.Name == memorynodes.ConceptHarnessObservation {` becomes `if conceptMeta.Name == memorynodes.ConceptWorkObservation {`; update its comment.
- `dsl/pack_enablement_test.go`: any case that used `HarnessPackDomain` as its subject now uses a fixture domain registered in the test with `RegisterTree` over an in-memory `fstest.MapFS` holding one minimal `concepts.memql` (the `testPackTree` helper the deleted db test defined is the shape to copy).
- `integrations/embedding/embedding.go` and `component/memql/engine.go`: the comments that name the harness now name the memory namespace.

```bash
go run ./cmd/memqllint dsl/
go build ./... && go vet ./...
make sdk-gen && make sdk-gen-check
go test -count=1 ./component/memql/ ./integrations/harnessrecall/ ./dsl/ ./component/automations/
```
Expected: PASS, and `strict_automation_boot_test.go` still counts the same shipped automations.

- [ ] **Step 3: Commit**

```bash
git add -A dsl/memory dsl/harness dsl/harness_pack.go dsl/embed.go component/memql integrations/harnessrecall integrations/embedding/embedding.go dsl/pack_enablement_test.go sdk/go/client sdk/ts/src/generated
git status --short | grep -v '^[MDRA] ' ; git commit -m "memory: beliefs, the cursor, consolidation and recall move out of the harness pack"
```


### Task 18: workTrace replaces harnessTrace

**Files:**
- Move: `integrations/harnesstrace/` -> `integrations/worktrace/`
- Modify: the moved `plugin.go` and `trace.go` (+ tests), `app/plugins_core.go` (the import path), `dsl/memory/builtins.memql` (the builtin declaration)

**Interfaces:**
- Produces: builtin `workTrace { runId string! }` with `@executor("integration.workTrace.trace")`, returning the run's timeline: every run version, every step version and every observation, ordered by `createdAt`, in the same envelope shape `harnessTrace` returned (the renderer in the VS Code extension reads it; `grep -rn harnessTrace editors/ clients/ --include=*.ts` names the consumers to update in the same change).

- [ ] **Step 1: Move and rename**

```bash
git mv integrations/harnesstrace integrations/worktrace
sed -i 's/^package harnesstrace$/package worktrace/' integrations/worktrace/*.go
```
In `plugin.go`: register as `memql.RegisterPlugin("workTrace", ...)`, drop the `BindPluginToPack` line, and expose the capability `integration.workTrace.trace`. In `trace.go`: the reader queries `v1:work:run`, `v1:work:step` and `v1:work:observation` by run id through the bun reads it already has (replace the three harness concept literals; the row shapes differ only in field names, `key` for the step name and `status` values from section B). Update `app/plugins_core.go`'s import to `integrations/worktrace`.

- [ ] **Step 2: Declare the builtin**

Append to `dsl/memory/builtins.memql`:

```memql
/// Fetch one run's full execution timeline: every run and step version and every observation,
/// ordered by createdAt. The OS Nexus (sub-project B) reads the rows live; this is the one-call
/// form the VS Code panel and the cockpit use.
@enabled
@description("One run's timeline from the work journal, ordered by createdAt.")
@executor("integration.workTrace.trace")
builtin workTrace {
  runId  string!  @description("The v1:work:run id.")
}
```

- [ ] **Step 3: Tests, consumers, commit**

Rewrite `integrations/worktrace/trace_test.go` fixtures from harness rows to work rows (the fields named above). Update every TypeScript consumer `grep -rln 'harnessTrace' editors clients sdk/ts/src --include=*.ts --include=*.tsx` found to call `workTrace` with `runId`.

```bash
go run ./cmd/memqllint dsl/
make sdk-gen && make sdk-gen-check
go test -count=1 ./integrations/worktrace/
git add -A integrations/worktrace integrations/harnesstrace app/plugins_core.go dsl/memory/builtins.memql sdk/go/client sdk/ts/src/generated editors clients/portal/src clients/os/src
git status --short | grep -v '^[MDRA] ' ; git commit -m "worktrace: the run timeline builtin over work rows"
```

### Task 19: Documentation

**Files:**
- Modify: `docs/public/overview/why-memql-harness.md`, `docs/public/concepts/modules.md`, `README.md`, `GLOSSARY.md`, `CLAUDE.md`, `docs/public/build/build-tags.md`, `docs/public/operate/lifecycle-runbook.md`, `docs/public/language/functions.md`, `docs/public/language/authoring-rules.md`, `docs/public/language/sense.md`, `docs/public/concepts/composed-views.md`, `docs/public/operate/forge.md` (each only where it names the harness pack, the reconciler, or `harnessTrace`)

- [ ] **Step 1: The overview doc**

`docs/public/overview/why-memql-harness.md` keeps its filename and front-matter (many links point at it) and is rewritten so every claim points at the code that now backs it: the title becomes `The Harness -- Why MemQL Ships One`; the opening paragraph says the harness is the platform's work spine rather than a module; the sections that pointed at `component/harness` (the reconciler, the outer-loop planner, the action library) now point at `component/automations` (the executor and the journal, `journal.go` and `resume.go`), `dsl/work/concepts.memql` (the rows), `dsl/memory` (recall and consolidation), `component/planner/budget.go` and `component/memql/ai_guard.go` (the budgets), and the fleet router; the paragraph about enabling or disabling the module is deleted; a closing paragraph links the design record. Keep it under its current length.

- [ ] **Step 2: The pack proof, the README, the glossary, CLAUDE.md**

- `docs/public/concepts/modules.md`: the paragraph "The platform proves this on itself: the harness ... is a pack" becomes a paragraph about `examples/referencepack` as the worked pack, with the same three facts (its tree, its Go half, what disabling it does).
- `README.md` line 9: "The harness is one module of the platform" becomes "The harness is the platform's work spine, and clients are what you build on it."
- `GLOSSARY.md` line 19: the description of the harness doc drops "the module".
- `CLAUDE.md`: in "Feature Notes", replace the "Nexus -- the portal's living map of a goal" section with a four-line pointer: Nexus is being rebuilt on MemQL OS (sub-project B) over the work spine, the design record is the spec above, and the portal's Nexus pages were deleted in this epic with the pure scene library moved to `clients/os/src/nexus/scene/`. In "Planner / Knowledge / Validation" add one sentence: every automation execution is journaled as `v1:work:run` and `v1:work:step` rows (`component/automations/journal.go`) and resume reads them. Remove the sentence in "Coding Agent -- the container-executor seam" that names the harness if one does. Do not restructure anything else: root CLAUDE.md is gate-scanned.
- The other listed docs: `grep -n 'harness' <file>` and rewrite only the sentences that describe the pack, the reconciler or `harnessTrace`; a sentence using "harness" in its ordinary sense stays.

- [ ] **Step 3: Gates**

```bash
go test -count=1 -run 'TestDocs|TestNoVendorDomainLiterals|TestDocumentedTestCommandCoversTheEngine|TestNoDatabaseProductClaims' .
git add README.md GLOSSARY.md CLAUDE.md docs/public
git commit -m "docs: the harness is the work spine; the pack and its proof are gone"
```

### Task 20: The portal's Nexus goes; the scene library moves to the OS

**Files:**
- Create: `clients/os/src/nexus/concepts.ts`, `clients/os/src/nexus/scene/{world,layout,events,scene,receipt}.ts`, `clients/os/src/nexus/scene/fixtures/index.ts`, `clients/os/test/nexus/scene.test.ts`, `clients/os/test/nexus/receipt.test.ts`
- Move: `clients/portal/src/nexus/scene/{registry.tsx,conceptGraph.ts,ConceptGraphScene.tsx,ConceptGraphCanvas.tsx}` -> `clients/portal/src/scenes/`
- Delete: the rest of `clients/portal/src/nexus/`, `clients/portal/test/{nexusConstructs,nexusFeed,nexusMap,nexusReceipt,nexusReplay,nexusRoutes}.test.tsx`, `nexusScene.test.ts`, `goalsRun.test.tsx`, `newGoal.test.tsx`, `mapMaterials.test.ts`, `test/support/nexusHarness.tsx`
- Modify: `clients/portal/src/app/routes.tsx`, `nav.ts`, `guides/entries.ts`, `widgets/registry.tsx`, `pages/ArrangedSection.tsx`, `pages/useRegenerate.ts`, `compose/ComposerSection.tsx`, `test/nav.test.ts`, `test/app.test.tsx`, `test/liveAdoption.test.tsx`, `test/conceptGraph.test.ts`; `portal_view_composition_test.go`, `portal_control_vocabulary_test.go`, `portal_subscription_routing_test.go`, `component/node/routing_reach_test.go`
- Create: `clients/portal/test/scenes.test.ts`

- [ ] **Step 1: Move the pure library into the OS**

```bash
mkdir -p clients/os/src/nexus/scene/fixtures clients/os/test/nexus
git mv clients/portal/src/nexus/concepts.ts clients/os/src/nexus/concepts.ts
for f in world layout events scene receipt; do git mv clients/portal/src/nexus/scene/$f.ts clients/os/src/nexus/scene/$f.ts; done
git mv clients/portal/src/nexus/scene/fixtures/index.ts clients/os/src/nexus/scene/fixtures/index.ts
git mv clients/portal/test/nexusScene.test.ts clients/os/test/nexus/scene.test.ts
```
In `clients/os/test/nexus/scene.test.ts` rewrite the imports from `../src/nexus/scene/...` to `../../src/nexus/scene/...`. Add a top-of-file comment to `clients/os/src/nexus/scene/world.ts`: the library was moved from the portal in the work spine's epic A1; sub-project B re-points `PLAN_CONCEPT_ID` and `TASK_CONCEPT_ID` in `concepts.ts` to `v1:work:run` and `v1:work:step`, and the OS map is 2D by owner requirement (epic memql#4785: no WebGL in the OS; adapt the Deployables app's 2D map), so the portal's WebGL renderer is deleted with the pages and only this layout library survives.

From `clients/portal/test/nexusReceipt.test.tsx`, extract the assertions that import only `receipt`, `scene` and `fixtures` into `clients/os/test/nexus/receipt.test.ts` (plain vitest, no React); delete the rest with the file.

```bash
make os-typecheck && make os-test
```

- [ ] **Step 2: Keep the Views scene registry, drop the goal map**

```bash
mkdir -p clients/portal/src/scenes
git mv clients/portal/src/nexus/scene/registry.tsx clients/portal/src/scenes/registry.tsx
git mv clients/portal/src/nexus/scene/conceptGraph.ts clients/portal/src/scenes/conceptGraph.ts
git mv clients/portal/src/nexus/scene/ConceptGraphScene.tsx clients/portal/src/scenes/ConceptGraphScene.tsx
git mv clients/portal/src/nexus/scene/ConceptGraphCanvas.tsx clients/portal/src/scenes/ConceptGraphCanvas.tsx
git rm -r clients/portal/src/nexus
git rm clients/portal/test/nexusConstructs.test.tsx clients/portal/test/nexusFeed.test.tsx clients/portal/test/nexusMap.test.tsx clients/portal/test/nexusReceipt.test.tsx clients/portal/test/nexusReplay.test.tsx clients/portal/test/nexusRoutes.test.tsx clients/portal/test/goalsRun.test.tsx clients/portal/test/newGoal.test.tsx clients/portal/test/mapMaterials.test.ts clients/portal/test/support/nexusHarness.tsx
```
In `clients/portal/src/scenes/registry.tsx`: delete the `GoalMapScene` lazy import and the `goalMap` entry from `SCENES`; fix `../../ui` to `../ui`. In `ArrangedSection.tsx`, `useRegenerate.ts` and `ComposerSection.tsx` change `../nexus/scene/registry` to `../scenes/registry`. In `routes.tsx` delete the `NexusRoutes` import, the `nexus/*` route and its comment lines. In `nav.ts` delete the `nexus` destination. In `guides/entries.ts` delete the `nexus` entry. In `widgets/registry.tsx` the comment that names `nexusMap.test.tsx` now names `scenes.test.ts`. `test/conceptGraph.test.ts` imports move to `../src/scenes/conceptGraph`.

Create `clients/portal/test/scenes.test.ts` carrying the one guard `nexusMap.test.tsx` held: only `src/scenes/ConceptGraphCanvas.tsx` may import `three`, `@react-three/fiber` or `@react-three/drei` (copy the walk-and-grep assertion from the deleted file and change its allowlist to that one path).

Update `test/nav.test.ts` and `test/app.test.tsx` (the Nexus destination and route expectations go), and `test/liveAdoption.test.tsx` (the `unboundSubscriptions` allowance for `nexus/feed/useGoalWorld.ts` goes).

```bash
make portal-typecheck && make portal-test
```

- [ ] **Step 3: The Go gates**

- `portal_view_composition_test.go` (about line 444): delete the `{"clients/portal/src/nexus/goalPage.ts", "nexus.goal"},` row.
- `portal_control_vocabulary_test.go` (about line 118): delete the `"clients/portal/src/nexus/replay/Scrubber.tsx": ...` entry.
- `portal_subscription_routing_test.go` (about line 185): delete the `{"clients/portal/src/nexus/feed/useGoalWorld.ts", "v1:authoring:construct"},` row and its two comment lines.
- `component/node/routing_reach_test.go` (about line 88): the reason string `"the Agents view and Nexus (nexus/feed/useGoalWorld.ts)"` becomes `"the Agents view"`.

```bash
go test -count=1 -run 'TestPortal|TestConvergedPages|TestRoutingReach' . ./component/node/
```

- [ ] **Step 4: Commit**

```bash
git add -A clients/os/src/nexus clients/os/test/nexus clients/portal/src clients/portal/test portal_view_composition_test.go portal_control_vocabulary_test.go portal_subscription_routing_test.go component/node/routing_reach_test.go
git status --short | grep -v '^[MDRA] ' ; git commit -m "portal: delete Nexus; the pure scene library moves to the OS (D7)"
```

### Task 21: PR 2 of 2

- [ ] **Step 1: Every gate**

```bash
make arch-model && make arch-model-check
go run ./cmd/memqllint dsl/
make sdk-gen-check
make test
go test -count=1 .
make portal-typecheck && make portal-test
make os-typecheck && make os-test
MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<dsn> go test -count=1 ./component/automations/ ./component/memql/ ./integrations/harnessrecall/ ./integrations/worktrace/
```
Commit the regenerated architecture model (`git add component/architecture && git commit -m "arch: regenerate after the harness retirement"`).

- [ ] **Step 2: Push and open the PR**

```bash
git push -u origin feat/work-spine-a1-retire
gh pr create --repo znasllc-io/memql --title "work spine A1, PR 2 of 2: retire the harness spine and the capture library; Nexus leaves the portal" --body-file - <<'EOF'
The authored-action helpers move into component/actions; the action capture library, the harness reconciler, its planner, its pack and its CI lane are deleted; beliefs, the cursor, consolidation and recall move to the memory namespace with recall reading v1:work:observation; workTrace replaces harnessTrace over work rows; the portal's Nexus pages are deleted and the pure scene library moves to clients/os (decision D7).

Design record: docs/superpowers/specs/2026-09-05-work-spine-design.md (sections F, I, K; decision D7 and the action-boundary amendment).

Ships in PR 2 of 2 of epic A1, after PR 1. Closes #4965.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01LXdTWPHNiDZUUgyggSAaoT
EOF
gh pr checks <n> --repo znasllc-io/memql --watch
gh pr merge <n> --repo znasllc-io/memql
```

---

## Plan self-review

- **Spec coverage.** Section A (three graphs): Tasks 2-6. Section B (goal, run, step, derived kind, footprint declared once): Tasks 2, 8; the loader rule for `function` steps and `@effects` on builtins are epic A2 and the plan says so where `kind` is written. Section D (journal, replay modes, side effects, approval, observation, waits, retention, feeds): Tasks 2-5, 8-12 for what A1 owns; modelCall serving, approvals, waits and retention are epic A2 by section K. Section F (retired, moved, re-pointed): Tasks 11, 14-18. Section I (tiers, routing, generated artifacts, CLAUDE.md): Tasks 5, 6, 19. Section J (tests): Tasks 8-10, 12, 20. Section K (two PRs, the owner's rule): Tasks 13 and 21; Task 7 is a checkpoint on the first branch. D7: Task 20. D8: no migration anywhere.
- **Placeholders.** None: every DSL file is written out, every Go file that is new is written out, every deletion names its files, and every edit of an existing file names the anchor.
- **Type consistency.** `journalExecutor.Execute` returns `(*memql.ExecuteResult, error)`, the engine's signature (component/memql/engine.go:592). `LoadRunJournal` takes a `journalExecutor` so the db test passes the engine and the unit test a fake. `ResumeFrom` takes `*RunJournal` in Task 10 and the scheduler passes one in the same task. `workStepId` is used by both `stepRunning` and `stepFinished`. The constants added in Task 5 are the ones Task 16 leaves in place and Task 17 reads (`ConceptWorkObservation`).
- **Two facts to re-verify at execution, because they were read at df33cef4b and the tree moves:** the exact line anchors in `executor.go` (Task 9) and the tests that name the checkpoint (Task 10 step 4). Both tasks give the grep that finds them.
