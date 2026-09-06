# The Work Spine -- Design

- **Date:** 2026-09-05
- **Status:** approved in the 2026-09-05 brainstorm. The eight forks below
  (D1-D8) were put to the owner as selectable options and each was answered;
  they are not open questions. Everything else is a recommendation with its
  rationale and what it rejected.
- **Program:** sub-project A of a five-part program agreed in the same
  brainstorm -- A the spine (this record), B Nexus on MemQL OS, C the
  Materializer, D the Files places, E portal removal -- plus F, the removal
  of cognition, spaces and voice, added during the brainstorm when the owner
  identified them as the previous product's conversational substrate.
  Order: A, then B; C, D and E follow; F after B so nothing is deleted before
  its replacement exists.
- **Scope:** `dsl/work/` (new, from `dsl/planner/`), `dsl/skills/` (new, from
  the skill concept and `dsl/agents/skills/`), `dsl/memory/` (new, from
  `dsl/harness/`), `component/automations/` (step versions, journal-based
  resume, waits), `component/work/` (new: compile, the symptom classifier,
  promotion, budgets), `integrations/planner/` (reduced to compile, miss and
  the reactive loop), `integrations/agent/` (subrun steps, `runScript`),
  `component/harness/`, `dsl/harness/` and `dsl/actions/` (retired),
  `component/node/routing.go`, `clients/os/src/apps/training/`,
  `clients/portal/src/nexus/` (deleted; the pure scene library moves), and
  the generated artifacts.
- **Extends:**
  [2026-08-22-nexus-living-map-of-a-goal-design.md](2026-08-22-nexus-living-map-of-a-goal-design.md)
  (the scene library and the feed rules, which sub-project B inherits),
  [2026-08-22-subscription-row-authz-design.md](2026-08-22-subscription-row-authz-design.md)
  (admission on live feeds),
  [2026-09-03-logs-design.md](2026-09-03-logs-design.md) (archive-then-delete
  retention, reused for the journal),
  [2026-09-03-deployables-run-design.md](2026-09-03-deployables-run-design.md)
  (exactly-once by node and heartbeat, the abandoned sweep, reused for runs),
  [2026-08-22-local-apps-as-execution-surfaces-design.md](2026-08-22-local-apps-as-execution-surfaces-design.md)
  and
  [2026-08-22-fleet-machines-routing-and-workbenches-design.md](2026-08-22-fleet-machines-routing-and-workbenches-design.md)
  (bindings and where a step runs).
- **Closes:** three epics to be filed from section K (A1, A2, A3).

---

## Why

The owner's brief, in its own terms: a person gives a goal; the system works
it out once, records the steps it took as atomic, variable-taking,
machine-agnostic units, and from then on replays them without a model unless
reasoning is genuinely needed -- when the internet goes out, when the database
changed under the automation, when a human has to decide. Reasoning, when it
happens, asks a few questions first: is this temporary, does it need fixing,
or does it need a person. Steps compose into larger automations without being
rewritten. Runs may take days or weeks. The whole thing must be visible and
manageable from one place, which will be the Nexus app on MemQL OS.

What the tree holds today, verified at `df33cef4b`:

- **That idea is implemented three times, partially, and none of the three is
  the default execution path.** Authoring capture (`v1:authoring:*`) records a
  finished plan as a `.memql` bundle after the fact, with compose-first
  catalog reuse and three gates, and never replaces execution. The action
  library (`v1:actions:*`) records the capability calls a model step made and
  replays them by input fingerprint, behind a flag that is off by default and
  keyed to the harness spine rather than the planner's. Self-healing
  (`component/healing`) proposes typed patches when a declared precondition
  misses, and only fires on preconditions somebody wrote.
- **There are two plan spines.** `v1:planner:plan/task/taskState` is what the
  Planner Agent drives and what the portal's Nexus draws.
  `v1:harness:plan/step/observation` is what the reconciler drives and what
  the action library and agent subagent dispatch key on. They meet at one
  substitution seam.
- **The planner keys every plan to a cognition space**, surfaces every human
  gate as a canvas card on that space, and the engine tree registers no
  canvas-state concept, so in an engine-only cluster those approvals are
  already invisible. The OS Ask surface is a direct provider chat stream and
  touches neither.
- **The automation runtime is a durable-execution kernel in embryo**: parallel,
  conditional and foreach steps, sub-automation calls, preconditions, a
  checkpoint row (`v1:memql:checkpoint`, 24-hour TTL, consulted only after a
  failure), resume from a step, a deterministic step fingerprint, and a
  per-step observer seam.

The owner asked for a deep dive into "graph engineering", the current stage
of harness design, to decide whether it is worth building on, keeping the
essence above without forcing anything. The research is summarized next; the
verdict is that it is worth it, and that MemQL is closer to it than any
framework read, because the newest part of the field is converging on the
substrate MemQL already is.

---

## What the research says

The lineage, with dates. Prompt engineering and context engineering optimize
one model call. Harness engineering (Anthropic's long-running harness,
November 2025; OpenAI's post, early 2026) is everything outside the model:
context delivery, tools, sandbox, approvals, state carried across sessions.
Loop engineering (June 2026) designs the loop that prompts the model: what
happens each iteration and when to stop, verified deterministically rather
than by model confidence. Graph engineering (the August 2026 survey and the
practitioner posts) externalizes the relationships among tasks, components
and runtime state as explicit, dynamic, evolving graphs, of three kinds: task
organization, agent coordination, runtime state. Ontology engineering is what
the survey names as next: shared semantics, so every node means the same
thing by a concept.

The critiques adopted here: a loop is a one-node graph, so loops and graphs
are time versus structure, not a progression; graphs assume the task
decomposes, and LangChain moved its own deep-research agent back to a loop
because it did not; the self-improving layer is the least built and least
proven part in the survey's own words.

Three findings shaped this design:

1. **The runtime-state graph is where the real work is, and it is
   event-sourced.** "The Log is the Agent" (May 2026) inverts the agent: an
   append-only event log is the source of truth, the working graph is a
   deterministic projection of it, behaviors react to graph changes and emit
   events, every object carries a caused-by pointer, and a model call is
   recorded once and served from a content-addressed cache on replay, which
   makes replay exact and forking free. The durable-execution runtimes
   converged on the same primitives independently: journal every step
   boundary, record the model output and never re-call it, make an approval a
   durable event tied to the hash of what was approved, version the prompt and
   the tools because versioning is part of replay correctness, and hold one
   global retry budget per run. Schema-grounded state mutation instead of
   agent dialogue (PatchBoard) is the same instinct one level up.
2. **Repair beats resample.** The debugging papers stopped rerunning failed
   runs: localize the failed node, keep the prefix, intervene there. The
   symptom list they converged on -- a constraint violated, progress stalled
   or repeated, a runtime condition unresolved, plan and outcome disagree --
   is the owner's "temporary, needs fixing, or needs a human".
3. **Skills, not specialists, are the unit of capability.** The skills
   reference architecture states that skills (instructions, scripts,
   resources, metadata, disclosed progressively) replace specialist agents,
   because a skill is invoked and an agent deliberates. Selecting skills at
   scale is structural, since skills depend on, conflict with and duplicate
   one another, which similarity search cannot see (SkillDAG). The
   harness-evolution paper found the model that writes a skill update can be
   small while the executor is where capability matters.

The numbers that changed a decision:

| Finding | Result |
|---|---|
| Schema-grounded state mutation vs LangGraph dialogue, matched ALFWorld episodes (PatchBoard) | 84.6% vs 30.8% success; 45k vs 368k tokens per success |
| Localized repair vs unguided rerun of a failed run (SymTrace) | 20.2% vs 6.9% of failures repaired |
| Replay reproduces the failure vs full rerun (SymTrace) | 80.8% vs 68.0% |
| Skill updates written by a 9B model vs an Opus-class model | comparable gains |

Ontology engineering is not a later phase for MemQL. Concept schemas with
typed fields and enums, relationships with a closed engine type and an open
domain verb, shapes and specs, data origins, row tiers, and a loader that
refuses boot on a construct naming an unknown field are that layer, and this
design uses it: every step's inputs and outputs are concept-typed, every
postcondition is a spec, every permission is a row tier or a safety decision,
and a step declares its effects the way the typed-action papers ask.

---

## Locked decisions

- **D1 -- The three-graph hybrid.** One spine of goal, run and step rows; skills
  as the capability unit; a journal at every step boundary; repair from the
  failed node. Chosen over keeping the current mechanisms and over adopting a
  framework.
- **D2 -- Spine first, then Nexus.** Nexus's shape is the spine's shape, so B
  is built once. Chosen over Nexus on today's rows with an adapter, and over
  writing all five specs before any code.
- **D3 -- Cut every tie to spaces and cognition; remove them later.** Goal,
  run and step carry no space. Approvals are spine rows. Training is re-keyed
  to Library files inside this sub-project. Spaces, cognition and voice become
  sub-project F, scheduled after B. The agent concept needs no decision here:
  work never creates or routes to an agent row.
- **D4 -- Names.** `v1:work:*` for goal, run, step, modelCall, approval,
  observation and responsibility; `v1:skills:*` for skill and skillEdge;
  `v1:memory:*` for belief and consolidationCursor. Templates stay
  `v1:authoring:*`. Pre-release: a rename is a sweep, not a migration.
- **D5 -- Never a silent edit.** Every healing patch goes through an approval
  row, even on the run's own draft template.
- **D6 -- One approval concept for every human gate**, replacing the plan's
  feedback fields, the canvas cards and the safety gate's ask sink.
- **D7 -- The portal's Nexus is deleted in epic A1** and its pure scene
  library is moved into a shared client package. The portal's other pages are
  untouched until sub-project E.
- **D8 -- Rows that exist today stay untouched and unread.** No migration, no
  shim; a later retention sweep may remove them.

---

## A. The three graphs

1. **The work graph.** A goal has one or more runs; a run has steps; a step's
   edges are dependsOn, data flow and condition. The authored form of a run
   is an automation construct in the catalog. The compiled form is rows,
   which is what Nexus draws and what a person reads.
2. **The capability graph.** Skills are the nodes; edges are typed
   (dependsOn, conflictsWith, specializes, duplicates). A binding is not a
   row: it is recorded on the step it was made for.
3. **The journal.** MemQL is append-only, so a step's intent-before and
   receipt-after are two versions of the one step row; there is no separate
   receipt concept. Beside the step rows sit modelCall rows keyed by request
   hash, approval rows carrying the hash of what was approved, and
   observation rows for recall. The journal is the runtime-state graph and
   the episodic memory at once.

---

## B. The work graph

### goal (`v1:work:goal`)

The intent, owned by a person. `@rowAuthz(owner="ownerUserId", clusterOwner)`.

| Field | Meaning |
|---|---|
| `ownerUserId!` | the person; stamped from the actor, `@serverSet` |
| `statement!` | the goal in the person's words |
| `origin!` | `user`, `responsibility`, `system` |
| `responsibilityId` | when origin is responsibility |
| `accountIds[]` | optional account tags, a record and never a visibility scope |
| `input` | a typed object, the shape the chosen template's `args` declares |
| `ceilings` | `{tokenBudget, costCeiling, wallClockMs, maxRetries, maxModelCalls, maxEvents}`, inherited by every run |
| `status!` | `open`, `active`, `closed` |
| `requestedVia` | `api`, `ask`, `nexus`, `responsibility`, `library`, `materializer` |
| `closedAt`, `closeReason` | |

Execution state does not live here, so a goal that has been re-run, forked or
repaired still reads as one thing.

### run (`v1:work:run`)

One execution against a goal. Same tier; `ownerUserId` copied from the goal.

| Field | Meaning |
|---|---|
| `goalId!` | |
| `templateConstructId`, `templateVersion` | the automation construct compiled to, once known |
| `variables` | the bound `args` |
| `mode!` | `live`, `replay`, `fork` |
| `forkedFromRunId`, `forkAtStepKey` | when mode is fork |
| `replayPolicy` | `strict` (default), `permissive` |
| `status!` | `compiling`, `running`, `waiting`, `succeeded`, `failed`, `cancelled`, `abandoned` |
| `waitingOn` | `{kind: approval|feedback|timer|external|subrun, subject, since}` |
| `spent` | `{tokens, tokensSubscription, tokensLocal, cost, modelCalls, retries, events, wallClockMs}` |
| `nodeId`, `heartbeatAt`, `stoppedAt` | exactly-once and the abandoned sweep, the deployment-run pattern |
| `cancelRequested`, `cancelledBy` | |
| `expectedFootprint`, `actualFootprint` | see Footprint in this section |
| `outcome`, `errorCode`, `errorMessage` | |
| `startedAt`, `finishedAt` | |
| `summary` | folded from the journal before its retention window closes |

A run is created the moment a goal is accepted, in `compiling`, so the model
calls compilation makes have a home from the first one. Cost accounting keeps
the three token counters the planner keeps today: the dollar ceiling excludes
subscription and local spend, the loop caps include every call.

### step (`v1:work:step`)

A node of a run. Same tier.

| Field | Meaning |
|---|---|
| `runId!`, `key!` | the stable key from the template |
| `kind!` | `deterministic`, `reasoning`, `decision`, `human`, `loop`, `subrun` |
| `call` | `{constructKind, name, args}` |
| `dependsOn[]` | step keys |
| `input`, `result`, `resultFingerprint` | |
| `binding` | `{skillIds, providerPolicy, provider, model, surface, machineLabels, workerId, nodeId}` recorded at dispatch |
| `expectedFootprint`, `actualFootprint` | |
| `postcondition` | `{kind: spec|schema|check, ref, passed, message}` |
| `status!` | `pending`, `ready`, `running`, `waiting`, `done`, `failed`, `skipped`, `cancelled` |
| `symptom` | `""`, `transient`, `environment`, `contract`, `plan`, `human` |
| `attempt` | a retry is a new version of the same row with attempt incremented |
| `idempotencyKey` | derived from run, key and attempt |
| `childRunId` | subrun |
| `approvalId` | human |
| `resumeAt`, `externalKey` | waits |
| `startedAt`, `finishedAt`, `durationMs`, `tokens`, `cost` | |
| `errorCode`, `errorMessage` | |

A subrun step opens a child run for a sub-goal and parks until it finishes;
it replaces both the planner's sub-plans and the agent's subagent dispatch. A
loop step is a bounded agent loop; its inner calls are journal rows attached
to the step and never steps of their own (the Nexus design's D2, kept).

### Kind is derived, not declared

A call into a query, mutation, logic or builtin is `deterministic` unless it
transitively reaches a prompt, in which case it is `reasoning`. A spec call is
`decision`. The two new human step forms, `approval` and `feedback`, park the
run and resume on an approval row. A `wait` step with `until` (a timer) or
`for` (an external key) parks the same way. The loader refuses a step
annotated deterministic that reaches a prompt.

### Footprint is declared once, on the capability

A builtin declares its effects (`@effects(concepts=[...], files, machine,
external, spend)`); a mutation's effect is its concept; a query has none. A
step's expected footprint is the union over what it calls; the actual
footprint is observed at the receipt. This is the typed-action claim of the
ontology papers, and it is what the safety gate reads before a side effect.

### The authored form is the automation grammar, unchanged

`args` is the variable list: paths, customer names and machine labels are
arguments, and the relativize-literal patch turns a captured literal into one.
Steps, `parallel`, `if`, `forEach`, sub-automation calls and `precondition`
blocks keep their meaning. The additions are the `approval`, `feedback` and
`wait` step forms and `@effects` on builtin declarations; final syntax is a
plan-level decision, gated by the parser tests.

### Compile

Catalog exact match on the normalized statement and input shape, then
near-match at or above the existing threshold with a gap list, then the cheap
triage classifier. Trivial becomes a one-step run with one reasoning step.
Sectionable goes to the existing deterministic parallel generator. Everything
else runs one reasoning step (`compileGoal`) that emits the run's steps as an
automation draft, which must pass Gate 1 (the sandbox compile-and-bind) before
the run proceeds. The draft is stored as a `v1:authoring:construct` in draft
status bound to the run; promotion is a later act (section E).

---

## C. The capability graph

### skill (`v1:skills:skill`), grown from today's row

Kept: `slug`, `name`, `description`, `category`, `tags`, `domainIds`,
`liveSourceIds`, `toolSlugs`, `tier`, `predefined`, `active`, and provenance,
now `originatingGoalId` and `mintedByRunId`.

Added:

| Field | Meaning |
|---|---|
| `instructions` | the three-tier progressive-disclosure body: name and description always loaded, the body on activation, references on demand |
| `scripts[]` | `{platform: linux|darwin|windows|any, artifactId, entry, argsSchema}`; `artifactId` is a Library file, content-addressed |
| `resources[]` | Library file ids |
| `constructRefs[]` | the catalog constructs this skill contributes |
| `effects` | the declared footprint of its scripts |
| `version` | |
| `reliability`, `reinforceCount`, `lastReinforced` | the trust ladder, moved from the action library |
| `status` | `candidate`, `active`, `deprecated` |

Predefined skills are seeded from `dsl/skills/*.memql`, moved from
`dsl/agents/skills/`.

### skillEdge (`v1:skills:skillEdge`)

`fromSkillId!`, `toSkillId!`, `type!` in `dependsOn`, `conflictsWith`,
`specializes`, `duplicates`; `evidence[]` as `{runId, stepKey}`; `proposedBy`
in `system`, `user`; `status` in `proposed`, `committed`. Compile may propose
an edge; a run that succeeds commits it. Selection is vector match plus typed
neighbors plus conflict signals -- the propose-then-commit protocol.

### A specialist is a skill bundle

A skill whose constructs and scripts compose other skills through dependsOn
edges, with a name and instructions a person can read. Minting keeps its
three gates: catalog search before mint (an existing row covering more than
the threshold rejects the mint), the standing authorization row with its tier
allowlist, and an approval row when the mint is outside the envelope. The
model that writes a mint may be a cheap one.

### The binding

Made per step at dispatch and recorded on the step. For a reasoning or loop
step the skills come from structural retrieval over the graph; for a
deterministic step the skill is whichever one owns the construct or script
called. The provider policy resolves to a model at dispatch: cheap by default,
a local model on the fleet when the policy allows one and one is online. The
surface is `inProcess`, `workbench`, `machine` or `localModel`. A machine
requirement is a label set or an argument, never an id; the router's
exact-match rule and its named refusal (`no_worker_available`) are unchanged.
Delegation remains a preference with a fallback.

### Where a step runs

Deterministic DSL calls run in-process on the agent node. Scripts run on the
workbench by default and on a fleet machine when the environment hint or the
labels say so. A script is shipped by content hash: a `runScript` builtin
composes the existing file-write and exec primitives, verifies the hash on the
far side, and records the receipt. When a run discovers a useful script on a
machine, capture copies it into the Library under the skill, so a template
never names a path on a machine.

### Agents

Work never routes to an agent row. The agent concept is untouched until
sub-project F. The standing authorization row (`v1:agents:agentAuthorization`)
stays as the record governance reads: computer-use scope, the mint envelope,
the tier allowlist.

---

## D. The journal

### Step versions are the step's journal

Every transition writes a version: `ready`; `running` with the binding and the
bound input recorded before anything executes; `done` or `failed` with the
result, its fingerprint, the observed footprint, timings and cost. A step at
`running` past the run's heartbeat with no receipt is the case durable
execution warns about, and recovery consults the receipt or the idempotency
key before doing anything again.

### modelCall (`v1:work:modelCall`)

One row per model request: `runId!`, `stepKey`, `ownerUserId!`,
`requestHash!` over provider, model, settings, messages, tools and output
schema, `provider!`, `model!`, `settings`, `promptRef` and `promptVersion`,
`inputTokens`, `outputTokens`, `cost`, `latencyMs`, `served!` in `live`,
`journal`, `local`, `response`, `error`. Owner tier plus cluster owner. A
hypertable with retention; never broadcast.

### Replay has three modes

- A **live** run of a template with new variables serves nothing from the
  journal. Its deterministic steps are the reuse; its reasoning steps call a
  model.
- A **replay** of one run serves every model call from the journal. A hash
  mismatch (the prompt or the model changed) raises a divergence pinned to
  the first step that differs, unless the run's `replayPolicy` is permissive,
  in which case a fresh call is made and journaled.
- A **fork** serves the shared prefix from the journal and runs live from the
  fork step.

Journal serving never crosses goals. A reasoning step is not memoized across
goals unless its template says so, because cross-goal reuse of an answer is
not what replayable means; the template's deterministic steps are.

### Side effects

A step that writes outside the graph runs under an idempotency key derived
from run, step key and attempt. External deliveries go through the existing
outbox (`v1:platform:outboundRequest`) with its delivery lifecycle. On resume,
a running step whose far side holds a receipt for the key is done; one without
a receipt goes to the symptom classifier, which may hand it to a person. The
old rule that a mutation or webhook step is never retried becomes: retried
when idempotent by key, parked otherwise.

### approval (`v1:work:approval`)

One concept for every human gate. `runId!`, `stepKey`, `ownerUserId!`,
`kind!` in `sideEffect`, `scopeElevation`, `budget`, `skillMint`, `feedback`,
`planReview`; `subject`; `artifactHash!` over the exact thing being approved
(the command, the patch, the message, the draft template); `question` and
`options` for feedback; `evidence` as `{tier, reason, ruleId, source}` from the
classifier; `requestedAt!`, `decidedBy`, `decidedAt`, `decision` in `""`,
`approved`, `rejected`, `answered`; `answer`; `expiresAt`. It replaces the
plan's feedbackRequest and feedbackResponse, the canvas cards and the safety
gate's ask sink. An approval never carries to a modified artifact: resume
compares the hash. It broadcasts.

### observation (`v1:work:observation`)

Moved from the harness unchanged in meaning: `runId!`, `stepKey`,
`ownerUserId!`, `kind!` in `tool_result`, `error`, `note`, `decision`, `data`,
`content!`, `embedding`. The recall builtin and belief consolidation read it as
before. The journal is the episodic memory.

### Waits

A human step parks the run with `waitingOn`. A timer wait carries `resumeAt`
and the cron leader's sweep resumes it. An external wait resumes when an
inbound row (`v1:platform:inboundRequest`) matches its key. No process is held
open for any of them.

### Retention

modelCall and observation rows follow the log store's rule: archive to blob
storage first (`journal/<day>/<concept>.ndjson.gz`), delete second, and no
archive means no delete. Defaults `MEMQL_WORK_MODELCALL_RETENTION_DAYS=90`
and `MEMQL_WORK_OBSERVATION_RETENTION_DAYS=180`, registered in the env
manifest. Before a run's journal ages out, its `summary` is folded onto the
run row, so a run stays readable after its detail is gone.

### Live feeds

goal, run, step and approval broadcast (created and updated); modelCall and
observation are on-demand reads that say when they were read. The routing
rules land in `component/node/routing.go` with the concepts, or the OS list
freezes after load.

---

## E. The loop

### Execute

The existing automation executor on the agent node runs the graph and writes
step versions as it goes. A step becomes ready when its dependencies are done;
a parallel block fans out; a subrun step opens a child run and parks; a loop
step runs a bounded tool loop under its own budget. A run is claimed by one
node with a heartbeat; a step that needs a machine forwards over the existing
worker forward; a run whose node dies is marked abandoned by the sweep and
resumed elsewhere from the journal.

### Verify

Every step has a postcondition: a spec over the result, a schema, or a
deterministic check. Most deterministic steps get one for free -- a
mutation's postcondition is that the row exists with the fields it wrote, a
query's is its shape. A postcondition failure is a failed step with the
symptom `contract`. A step with no postcondition cannot be called
deterministic.

### Miss: the symptom classifier

On a failure, a precondition miss, a postcondition failure or a stalled loop,
deterministic rules run first: network errors, timeouts and rate limits read
as `transient`; permission, not-found and a literal that does not hold here
read as `environment`; a violated contract reads as `contract`; the same
action repeated reads as stalled and escalates. When the rules have no
opinion, one cheap model call (`classifySymptom`) reads the trace and answers
with evidence. The classifier is a decision step; its calls are journaled.
Five symptoms, five acts:

| Symptom | Act |
|---|---|
| `transient` | retry with backoff inside the run-wide retry budget; past it, `human` |
| `environment` | the healing loop proposes its typed patches (relativize the literal, add the guard, rebind the input); an approval of kind `planReview` carries them to a person (D5) |
| `contract` | repair from this step: re-run its reasoning with the violation as guidance, bounded; never from the start |
| `plan` | re-plan the gap from this step (`replanGap`): a reasoning step emits the remaining steps as a new template version, prefix kept |
| `human` | an approval of kind `feedback` with the evidence; the run parks |

### Promote

A successful run updates its template's reliability: a catalogued template
whose fingerprints matched climbs; a draft that succeeded passes the dry-run
gate and waits for the person's activation (Gate 3); a template that has
succeeded several times is offered back as a responsibility with its
arguments as the form. Reliability decays on mismatch and disuse as the ladder
already does. Skill edges proposed at compile commit on success. Capture runs
in-line at the end of the run rather than as a detached job, and its own
failures are journaled on the run.

### Govern

The run's ceilings -- tokens, cost with subscription and local spend counted
separately, model calls, wall-clock, retries and events -- park the run with
an approval of kind `budget` when exceeded. The process-wide model-call
ceiling and the identical-request breaker (`ai_guard.go`) stay. Every step
whose footprint includes a side effect passes the safety classifier chain and
the decision policy: allow runs; ask writes an approval of kind `sideEffect`
carrying the tier, reason and rule id; deny fails the step as `human`. The
standing authorization row turns an ask into an allow; the person's kill
switch still stops everything.

---

## F. Retired, moved, re-pointed

Retired:

- **`v1:planner:plan`, `task`, `taskState`**, replaced by goal, run and step.
  `dsl/planner/` becomes `dsl/work/`; its queries, mutations and shapes are
  rewritten to the new rows; `responsibility` moves with it.
- **`v1:harness:plan`, `step`**, folded into the work graph, and
  `component/harness`'s reconciler with them: the automation executor is the
  reconciler now. `harnesstrace` reads work rows. `observation` moves to
  work; `semanticMemory` and `consolidationCursor` move to `v1:memory:belief`
  and `v1:memory:consolidationCursor`; `harnessrecall` is unchanged.
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
- **`v1:memql:checkpoint`, `checkpoint.go`, `resume.go`**, replaced by step
  versions and resume from the journal; the retryable-step logic becomes the
  idempotency rule of section D.
- **The planner agent as a persona.** `plannerAgent.tmpl` becomes three
  prompts: `compileGoal`, `classifySymptom`, `replanGap`. The `agent_loop*.go`
  family becomes the compile and miss handlers. `createSpecialist`,
  `extendSpecialist` and `spawnTrainingPlan` become skill mint and skill
  training under the same gates. The planner agent row is no longer seeded.
  `integrations/agent/subagent.go` becomes subrun steps.
- **Post-hoc capture as a detached job**
  (`agent_loop_authoring_capture.go`), replaced by in-line capture.
- **Three flags** -- `MEMQL_ACTION_REPLAY_ENABLED`,
  `MEMQL_AUTHORING_CAPTURE_ENABLED`, `MEMQL_AUTHORING_CAPTURE_MODE` --
  replaced by `MEMQL_WORK_CAPTURE_ENABLED` (default on) as the one operator
  kill switch.

Kept and re-pointed: responsibilities and the reactive loop (they create
goals); the authoring pipeline in full (design pass, emit, the three gates,
the catalog, promotion); healing; the safety gate and decision policy; the
triage classifier, now inside compile; the per-run budget check
(`component/planner/budget.go` moves to `component/work`); the delegation
resolver, the container-executor seam and the fleet router; the sectionable
generator; the Library's own analysis pass.

---

## G. Training re-keyed

Upload goes through the Library route (`POST /artifacts`), which already
accepts any type. The file's analysis pass becomes a system-origin goal
running the deterministic template (extract, chunk, embed, summarize), so the
Training app's live feed is run rows keyed by file id, and training into a
domain stays `libraryTrainFile`. The space attachment path and the
active-space hook leave `clients/os/src/apps/training/`; the attachment
handler itself stays for cognition until sub-project F.

---

## H. Entry points and node types

In this sub-project: a `createGoal` mutation reachable from the SDK and the
API (plus `cancelGoal`, `forkRun`, `replayRun`, `decideApproval`); the
reactive loop for responsibilities; the Library analysis pass. Nexus's New
goal and Ask-to-goal are sub-project B; the Materializer is C.

No new node type. The planner node keeps compile, the reactive loop and the
sweeps; the agent node runs steps.

---

## I. Tiers, routing, generated artifacts

Every work and memory concept declares
`@rowAuthz(owner="ownerUserId", clusterOwner)`; `v1:skills:skill` and
`v1:skills:skillEdge` declare `@rowAuthz(public, requiresIdentity)`, because
the predefined catalog is read by every signed-in person's agents (amended by
the A1 plan). That settles the old question
of plans needing a granted tier through space participation (memql#4366):
a goal is owned. Declaring a tier narrows every existing read, so the
executor, the sweeps and compile run under the maintenance actor or the goal
owner's borrowed authority, never bare -- the worker store's pattern.

Broadcast routing rules land with the concepts. The generated artifacts
follow in the memory's order: lint, the SDK generation check, the
architecture model, the embed inventory if a migration lands, and the
undeclared-tier ratchet, which shrinks. The CLAUDE.md sections on the
planner, the coding-agent seam and Nexus are rewritten in the same change.

Client-reachable reads: `goalsForOwner`, `runsForGoal`, `stepsForRun`,
`approvalsPending`, `modelCallsForRun`, `observationsForRun`,
`skillsForOwner`, `skillEdgesForSkill`. The queries over modelCall and
observation carry the "read at" timestamp the OS shows for on-demand reads.

---

## J. Testing

- **Pure, no engine.** Compile order; the derived-kind rule (the loader
  refuses a deterministic step that reaches a prompt); the footprint union;
  the symptom rules table; the three replay modes; the idempotency rule on
  resume; an approval whose artifact hash no longer matches refuses to resume.
- **Database-gated, on the db-tests lane with `MEMQL_REQUIRE_DB=1`.** A step
  row has a running version before the effect and a done version after. A
  replay serves every model call from the journal and a counting fake
  provider records zero calls -- the headline test. A goal that fully matches
  the catalog makes zero calls; a symptom the rules classified makes zero
  calls. Retention archives before it deletes and refuses to delete without
  an archive. Recall works over observations under the new name. Every new
  concept declares its tier and the shrink-only ratchet shrinks. The
  executor's database tests live in their own package, because the engine
  package's budget sits at its timeout.
- **Cross-node, as in-process hop tests.** A run claimed on one node,
  abandoned by the sweep, resumed on another from the journal alone. A step
  needing a machine held by a sibling replica forwards over the worker
  forward. An approval decided through the bff resumes a run parked on the
  agent node, asserting the work-namespace wildcard rules the way the
  planner's were.
- **Cost.** The process-wide ceiling and the breaker gate every call the new
  prompts make; the per-run ceilings park the run with a budget approval.
- **Generated artifacts.** Lint, SDK generation check, architecture model,
  embed inventory, the undeclared-tier ratchet. No HTTP route is added.
- **OS.** The Training app's tests over the new feed, run from inside
  `clients/os/`.

---

## K. Delivery

Three epics, at most TWO PRs each (the owner's rule, 2026-09-05: PRs are
the bottleneck), each PR closing several issues, the
grouping stated in the epic body and on every child, the issues carrying the
`claude` label.

| Epic | Contents | PRs |
|---|---|---|
| A1, rows and journal | work, skills and memory concepts with tiers and routing rules; step versions, modelCall, approval, observation; resume from the journal; retire checkpoint, the harness spine and the action library; move the scene library out of the portal and delete the portal's Nexus (D7) | 2, sequential |
| A2, compile and the loop | `createGoal`; compile with the three prompts; postconditions; the symptom classifier; healing wired to approvals; promote; budgets and the safety gate; in-line capture; responsibilities re-pointed; the planner agent retired | 2, after A1 |
| A3, skills and Training | skill grown, skillEdge, structural retrieval, `runScript`, bindings and the mint gates; Training re-keyed and the OS app updated | 2, after A1, parallel with A2 |

---

## L. Out of scope and follow-ups

- Sub-projects B (Nexus), C (Materializer), D (Files places), E (portal
  removal) and F (cognition, spaces, voice, and the fate of the agent
  concept).
- The Library's `memory` concept and the harness's consolidated beliefs are
  two memory models; they should become one, later, in their own record.
- Self-evolving skills beyond the trust ladder: the least proven layer in the
  literature, deliberately behind the ladder.
- Cross-goal memoization of reasoning steps.
- A cluster-wide operator view of every person's goals.

---

## M. References

- [Graph Engineering in the Era of LLM Agents](https://arxiv.org/abs/2608.21156)
- [Awesome Graph Engineering survey](https://github.com/DEEP-JLU/Awesome-Graph-Engineering)
- [3 Years of Graph Engineering with LangGraph](https://www.langchain.com/blog/3-years-of-graph-engineering-with-langgraph)
- [Graph Engineering for Multi-Agent Systems](https://www.truefoundry.com/blog/graph-engineering-enterprise-guide)
- [The Log is the Agent](https://arxiv.org/abs/2605.21997)
- [Agent Harness vs Loop vs Graph Engineering](https://www.analyticsvidhya.com/blog/2026/08/agent-harness-loop-graph-engineering/)
- [Harness engineering vs loop engineering](https://datasciencedojo.com/blog/harness-engineering-vs-loop-engineering/)
- [Harnessing Agent Skills](https://arxiv.org/pdf/2606.20631)
- [Harness as an Asset (CAAF)](https://arxiv.org/pdf/2604.17025)
- [OpenAI: Harness engineering](https://openai.com/index/harness-engineering/)
- [Anthropic: Effective harnesses for long-running agents](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents)
- [Durable execution for AI agent runtimes](https://zylos.ai/research/2026-04-24-durable-execution-agent-runtimes/)
- [Checkpoints are not durable execution](https://www.diagrid.io/blog/checkpoints-are-not-durable-execution-why-langgraph-crewai-google-adk-and-others-fall-short-for-production-agent-workflows)
- [PatchBoard](https://arxiv.org/abs/2605.29313)
- [Repair or Resample (SymTrace)](https://arxiv.org/html/2608.25920)
- [AgentRx](https://arxiv.org/abs/2602.02475)
- [AgentTether](https://arxiv.org/abs/2607.06273)
- [SkillDAG](https://arxiv.org/abs/2606.03056v2)
- [Harness Updating Is Not Harness Benefit](https://arxiv.org/abs/2605.30621)
- [Sovereign Agentic Loops](https://arxiv.org/abs/2604.22136)
- [Zep: a temporal knowledge graph architecture for agent memory](https://arxiv.org/abs/2501.13956)
- [Agentic Ontology of Work](https://www.skan.ai/whitepapers/agentic-ontology-of-work)
- [Provably Auditable and Safe LLM Agents from Human-Authored Ontologies](https://arxiv.org/pdf/2606.04903)
