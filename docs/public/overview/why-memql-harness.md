---
title: The Harness -- Why MemQL Ships One
audience: public
status: stable
area: overview
sinceVersion: 0.9.0
owner: znas
---

# The Harness -- Why MemQL Ships One

**MemQL is the AI platform where the harness is rows.** Every goal, run,
step, model call, approval, skill and belief is a typed, authorized,
replayable row in one time-series memory graph. Graph engineering with
the ontology built in.

Most "AI frameworks" hand you parts -- a model client, a prompt template,
a chain, maybe a tool interface -- and leave the harness for you to
build. The ones that do ship a harness keep its state somewhere else:
in process memory, in a queue, in a side table nobody queries. MemQL
ships the harness as the platform's own work spine, and its state is the
same graph everything else lives in.

This page is the proof, not the pitch. Every claim below either points at
the test that backs it, or says plainly that it is not published yet and
names the test that will. That rule is the point: a claim moves onto this
page when its test is green on `main`, not when the design is agreed.

**And the rule is now enforced rather than observed.** A claim that rests on
a NUMBER carries a marker naming the figure it rests on, and
`TestPublishedClaimsRestOnAScorecardNumber` fails the build when the newest
committed scorecard does not carry that figure, reports it as unmeasured, or
measures it differently. The mirror half fails when something in the
"does NOT make yet" table below has quietly become measurable. The figures
come from [the proving scorecard](proving-scorecard.md), which the proving
suite regenerates on every pull request.

Design record:
[the work spine](../../superpowers/specs/2026-09-05-work-spine-design.md).

## The frame: graph engineering

The current stage of harness design is **graph engineering** -- treating
an agent system as a graph of typed nodes and edges rather than a chain
of prompts. The survey literature (arXiv 2608.21156) names the open
problems that stage runs into: what SUBSTRATE the graph lives on, how it
is GOVERNED, what the OS around it looks like, and whether a system may
rewrite itself. "The Log is the Agent" (arXiv 2605.21997) argues the
execution log is the primary artifact rather than a by-product.

MemQL answers the first three by construction and gates the fourth
behind a person:

| Open problem | Where MemQL's answer is |
|---|---|
| **Substrate** | The work graph is not a record ABOUT the run kept somewhere else -- it IS the run. A run is `v1:work:run` and a step is `v1:work:step`: ordinary rows in the same time-series memory graph everything else lives in, read with the same DSL and gated by the same per-row authorization. |
| **Governance** | Per-row authorization is the only gate, and the work rows declare a tier like any other concept. An approval is a row (`v1:work:approval`) bound to the hash of the exact artifact approved. |
| **The OS** | Node types, identity, the fleet router, budgets and the event mesh are the platform, and the harness inherits them rather than reimplementing them. |
| **Self-evolution** | GATED, deliberately. The platform can author and promote constructs, and every promotion passes a human gate. Nothing here rewrites itself unattended, and this page will not claim it does. |

## What a harness actually has to do

An agent in a demo is a `while` loop around one model call. An agent in
production has to run a tool-calling loop that terminates and prove why;
remember across turns, sessions and restarts; not bankrupt you when a
model gets stuck; route work when it outgrows one process; and be
inspectable afterwards -- what ran, what it cost, why it decided that.

Those are the things teams rebuild badly. MemQL makes them rows.

## The proof

### 1. Every execution is a run, and every step is a row

The automation executor opens a `v1:work:run` row before the first step,
writes a `v1:work:step` row at `running` BEFORE each body and again at
`done` / `failed` / `skipped` after, and closes the run with its terminal
status. The intent-then-receipt shape is what makes a crash legible: a
step written at `running` with no receipt is a run whose node died
mid-step, and it is resumable from exactly there.

`component/automations/journal.go`. Tests:
`TestExecutor_JournalsEveryStepBoundary` (the boundaries, in order),
`TestJournal_DB_RowsWrittenAndResumed` and
`TestJournal_DB_AnUnfinishedStepIsResumable` (against a real Postgres).

### 2. Resume reads the journal, on the same run

A failed run is resumed from its own rows: the completed steps are served
back from the journal and never re-run, and the failed one is retried on
the SAME run id, so a reader asking what happened to run X gets one story
rather than two executions sharing a prefix. There is no side-record --
the 24-hour checkpoint this replaces is deleted, with no shim.

The security rule the checkpoint carried carries here unchanged: internal
origin on resume requires a trusted SOURCE and a trigger payload the
caller did not supply, because otherwise a refused call is the thing that
mints a resumable token (memql#2888, memql#2890).
`component/automations/resume.go`. Tests:
`TestJournal_DB_RowsWrittenAndResumed`,
`TestRunRowCarriesTheCallerSuppliedFlag`,
`TestWorkRunConceptDeclaresTheFlag`, and `TestDryRunWritesNoWorkJournal`
-- a preview must leave nothing resumable.

### 3. One log serves audit, replay and memory

The same rows answer three questions that are usually three systems.
`workTrace` renders a run's full timeline from every version of its run
and step rows plus its observations, ordered by `createdAt`
(`integrations/worktrace`). Resume replays from those rows (above).
And `recall` reads `v1:work:observation` as episodic memory, blending
semantic similarity with recency and consolidating into durable beliefs
under `v1:memory:belief` rather than dumping everything into a vector
store and hoping.

Nothing is logged twice, because nothing is logged: the record is the
state.

### 3a. A resumed run re-executes nothing it had already finished

A run stopped mid-plan is resumed from its own journal, and the steps that
had already completed are served back rather than run again.
<!-- proving: metric=durability.resumedStepsReExecuted arm=platform value=0 -->

The same run's side effects are not delivered twice.
<!-- proving: metric=durability.duplicatedSideEffects arm=platform value=0 -->

Both are measured, not asserted: the proving corpus stops a run mid-plan
against a fake external world that counts every delivery and records
duplicates rather than refusing them, then resumes it. And both come with a
NEGATIVE CONTROL on the same scenario -- the bare-loop arm, which has no
journal, restarts from the beginning and does re-deliver. A counter that
never rises on any path reads as zero forever, so the suite fails if the
control ever reads zero.

`cmd/memql-bench`, `test/proving/scenarios/durability.*`. The figures are on
[the scorecard](proving-scorecard.md).

### 3b. A goal the catalog already holds is compiled without a model

An exact catalog match returns from `component/work.Decide` with
`NeedsModel` false and `NeedsTriage` false, so the compile pass reaches no
provider at all -- not even the cheap triage classifier.
<!-- proving: metric=amortizedCost.compileCallsOnCatalogHit arm=platform value=0 -->

Its control is a scenario whose goal the catalog does not hold, which must
reach one.

### 4. A safety and cost spine that is on by default

- **A process-wide LLM rate ceiling** at the provider chokepoint
  (`component/memql/ai_guard.go`), so no code path can stampede a
  provider.
- **Per-plan token budgets** enforced *before* each call
  (`component/planner/budget.go`): work parks instead of making the next
  call when it would exceed a cumulative, persisted ceiling that survives
  retries.
- **Loop breakers** -- repeat-failure and redelegation-refusal guards
  stop the classic "model apologizes and tries the same thing forever".
- **An up-front estimate and approval gate**, and model tiering that is
  cheap by default and escalates only on an explicit stuck signal.

Every side-effecting step is performed under an idempotency key, and the
proving suite checks it as a pass-or-fail property rather than scoring it.
<!-- proving: metric=governance.effectsWithReceipts arm=platform value=1 -->

Read [LLM cost control](../ai/llm-cost-control.md) before touching any of
it; it is defense in depth and every layer is load-bearing.

### 5. Behavior is declared, and the declaration is the authorization

The DSL declares a system's behavior as data -- `concept`, `query`,
`mutation`, `automation`, `logic`, `tool`, `prompt`, `provider`, `spec`,
`shape` -- and the same declarations drive validation, per-row
authorization (classified and test-enforced in
`test/dslconformance/conformance_test.go`) and the generated reference.
The work rows are not special: they declare a tier and are gated by it
like every other concept.

### 6. It is multi-node, authenticated and observable already

Those properties belong to the platform and the harness inherits them.
The same code compiles by build tag into a mesh of node types that
discover each other and bridge events with dedup and TTL; there is a real
identity service (magic-link, passkeys, JWT, JWKS) and machine
credentials for service-to-service calls; and the run, step, goal and
approval rows carry broadcast routing rules, so a run's status flips on
the replica executing it and reaches the person watching from a different
one.

## Claims this page does NOT make yet

These are designed, specified and not shipped. They appear above when
their tests are green on `main`, and not before.

| Claim | Proven by | Lands in |
|---|---|---|
| Every model call a run makes is journaled | a `v1:work:modelCall` row per request, counted against the calls actually made. The writer EXISTS since memql#4999 -- the engine's model seam journals every call that reaches it and serves a replay from the journal -- but this suite still cannot count it: the CI tier's model responses come from a cassette through a fake step registry, so a scenario's model call never touches the provider chain or the seam. Counting rows against calls would be 0/0 dressed as a ratio, so the figure reports `notMeasurableOnReplay` and names why. <!-- proving-pending: metric=governance.modelCallsJournaled arm=platform value=1 --> | the live tier, which calls a provider down the real path |
| A REPLAY serves every model call from the journal | `component/work.DecideServe` exists and is pure; its one caller -- the executor's model-call seam -- does not, so a run opened in replay mode records its intent and serves nothing. What the suite measures today is the adjacent and genuinely-built claim above: a RESUME re-executes no completed step | the remaining half of epic A2 |
| Skill selection reads the capability graph structurally, not by vector match alone | typed `v1:skills:skillEdge` neighbours, proposed at compile and committed by a successful run | epic A3 |

## How developers use it

1. **Declare** your concepts, tools and automations in `.memql` files.
2. **Drive** it from the **Cockpit** (terminal-native ops) or **MemQL OS**
   in a browser.
3. **Run** it as one binary locally or as the node mesh for scale; same
   DSL, same behavior, only configuration changes.
4. **Extend** in Go only when you need to, through self-registering
   plug-ins with a narrow `PluginContext`.

> Next: [MemQL vs. agent libraries](vs-other-harnesses.md) -- an honest
> comparison with the Go and Python field, or
> [What is MemQL](what-is-memql.md) for the whole platform picture.
