---
title: Planner-authored automations -- DESIGN (#954)
audience: internal
status: historical
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Planner-authored automations -- DESIGN (#954)

Status: APPROVED ARCHITECTURE; build not started. The three load-bearing
decisions below were taken with the code owner (DSL architect). Child issues
#955-#961 carry the build, sequenced validation-first.

Successor to the shipped reactive-harness epic #629. The reactive harness gave
users **Responsibilities** (standing NL directives). This epic compiles a
Responsibility into a real, running `.memql` automation -- the capability the
DSL was built to host.

## Problem

The planner today is a pure orchestration/decision engine: it decomposes goals,
assigns specialists, enforces budgets, and -- in the `produceArtifact` path --
has an agent write a *runtime file* (spreadsheet, doc, code) via the workbench.
It authors **zero** DSL. Nothing in the system generates `.memql`, validates it
out-of-band, or activates an authored construct at runtime.

The intent is for the planner to do exactly that: take a user's standing intent
and produce the `.memql` syntax for an **automation plus its dependency closure**
(logic, shapes, traits, specs, policies, mutations, queries, prompts), where the
automation describes event-triggered steps and can call AI functions, the
similarity function, sub-logic, sub-automations, and so on. Then validate the
whole bundle through a dry run before it goes live.

## How fit is the DSL? (the brainstorm's headline)

**The language and execution surface are production-fit today.** Everything an
authored automation needs already works for hand-authored DSL:

- **Discovery is solved.** Runtime introspection builtins exist: `concepts()`,
  `functions()`, `help(name)`, `tools()`, `shapeTemplates()`, `shapeHelp(name)`,
  `validate(concept, payload)`, `previewInsert(...)`. An authoring agent asks the
  live engine what exists instead of hallucinating names.
- **Authoring guidance is solved.** `dsl/_reference/` skeletons + the 23 hard
  rules in `docs/public/language/authoring-rules.md`.
- **The in-automation call surface is rich.** From an automation body you can
  already reach `si()` (structured-output prompts), `similarTo()` (pgvector),
  `embedChunk`, `webSearch`, `fetchUrl`, queries, mutations, sub-logic,
  sub-automations, sub-policies, `publishEvent`, webhooks, and control flow
  (parallel/foreach/switch).
- **Structured generation has a home.** `ChatStructuredProvider.CallChatStructured`.

So we are **not** extending the language. The honest split is roughly **~20%
"is the DSL capable" (yes)** and **~80% "build the safe authoring lifecycle"**:
the sandbox compile/bind harness, the behavioral dry-run, the mutable authored
runtime, the catalog + matching, and the planner design/repair loop -- plus
closing the isolated-semantic-validation gaps (field existence,
event-pattern-vs-real-concept, cross-construct resolution, filter typecheck) that
today only run inside a live `Init()`.

## The hard constraint that shapes everything

A single bad construct fails `engine.Init()` and **bricks the whole cluster**
(authoring rules #1a, #19). After boot, every core construct registry
(functions, specs, shapes, prompts, tools, providers, policies) and the
automation scheduler are **immutable**. The only proven mutable-after-startup
path is `IntegrationRegistry` (thread-safe `RegisterIntegration`).

Therefore: **authored DSL must never enter the global boot path, and must never
be able to fail core `Init()`.** This is non-negotiable and drives decision 1.

## Locked decisions

### 1. Sandboxed authored-construct runtime

Authored constructs are stored as **graph rows** and compiled + executed in a
**separate, dynamically-mutable layer** that reads the sealed core registry but
is never part of core `Init()`.

```
Core engine (immutable, boot-loaded)
  registries: funcs / specs / shapes / ...   [SEALED]
        |  read-only introspection
        v
Authored-construct runtime (mutable, owner-scoped)
  v1:authoring:construct rows
    -> isolated compile + bind
    -> tiered sandbox dry-run
    -> register into authored scheduler
  (pause / retire / version live)   CANNOT fail core Init()
```

Rejected alternatives: hot-registering authored constructs into the core
registries (shares blast radius with the platform's own DSL -- unsafe against the
cluster-brick failure mode); disk-overlay + rolling reload (files are global, not
per-user; reload is operationally heavy; one bad file blocks a cluster-wide
reload).

### 2. Compose-first, author-the-gap

The planner discovers existing core + previously-authored constructs via
introspection and **reuses** them wherever possible; it authors **net-new**
dependency constructs only when nothing fits, and **promotes** those into a
**per-owner reusable catalog** so the 2nd/3rd automation stands on the 1st's
shapes/specs. The platform compounds.

- **Matching:** index every core + cataloged construct by description + signature,
  embed it, and use `similarTo()` to match "I need a predicate for active,
  non-deleted rows of concept X" to an existing construct before authoring one.
- **Resolution precedence:** core (sealed) -> owner-catalog -> bundle-local.
- **Dedup gate:** check the catalog before authoring a dep; promote-with-provenance
  after activation.
- **Tradeoff (eyes open):** reuse creates coupling -- editing a shared cataloged
  construct has blast radius. Mitigation: a **dependency graph** over the catalog,
  so before mutating a shared construct we run impact analysis ("N active
  automations depend on this") and re-validate the dependents in the sandbox
  before the change goes live. Build the edges from day one.

Rejected alternatives: author the full closure every time (multiplies
validation/generation surface, near-duplicate constructs proliferate, no
compounding); automation-only with deps required to pre-exist (lowest risk but
blocks novel automations until their deps are added through another path).

### 3. Tiered dry-run fidelity

A green dry-run must be *trustworthy*. Fidelity tier:

```
si() / similarTo()  -> REAL (metered)      # see the ACTUAL AI output / retrieval
webSearch / fetchUrl -> REAL (metered)
mutations            -> ephemeral sandbox partition (real engine, throwaway data)
webhooks / POST out  -> recorded + BLOCKED
```

Green means logic, data shape, and real AI behavior are all verified, with zero
prod impact. **Full sandbox-live** (everything real in a sandbox partition,
webhooks fired at a capture sink) is retained as an **optional final staging
pass** right before activation -- not run every iteration.

Rejected alternatives: fully mocked (a green run only proves it wires up +
type-checks, says nothing about real behavior); full sandbox-live on every run
(highest cost + largest isolation blast radius).

## End-to-end pipeline

```
Responsibility (NL intent)              [v1:planner:responsibility -- exists]
   |  intake / clarify loop              [exists]
   v
Design pass (structured output)          [planner -- NEW, #960]
   |  introspect + similarTo over catalog -> reuse-or-author decision
   v
Bundle authored                          [v1:authoring:bundle/construct -- NEW, #955]
   reuse: core + owner-catalog ; author-the-gap: net-new deps
   v
GATE 1 -- Isolated compile + bind         [registry clone + full binders -- NEW, #956]
   |  parse, cycles, concept/field/spec/shape binding, conformance,
   |  authz classification, tier checks.  fails safely; never touches core
   |  errors -> repair loop back to design pass
   v
GATE 2 -- Behavioral dry-run (tiered)     [sandbox partition + interception -- NEW, #958]
   |  reads real/metered ; writes -> sandbox ; webhooks recorded-blocked
   |  -> trace + side-effect manifest + cost estimate
   v
GATE 3 -- User approval                   [reuse planner approval-gate pattern -- #961]
   |  (optional full-sandbox-live staging pass here)
   v
Promote + register                       [authored runtime + scheduler -- NEW, #959]
   bundle -> active ; deps -> owner-catalog (embedded for future reuse, #957)
   runs under author's authz envelope ; per-automation circuit breaker ;
   leader-gated in cluster ; live pause / retire / version
```

## Build sequence (validation-first)

The validation/dry-run harness is built **before** the generator: it is the crux
the owner named, generation without trustworthy validation is dangerous given the
cluster-brick risk, and the harness is independently valuable (it would catch
hand-authored mistakes today too).

| Issue | Component | Blocked by |
|---|---|---|
| #955 (A) | Authored-construct concepts + lifecycle | -- (foundation) |
| #956 (B) | Isolated compile-and-bind harness (Gate 1) + semantic-validation gaps | #955 |
| #957 (C) | Catalog + `similarTo` matching + dependency graph | #955 |
| #958 (D) | Tiered behavioral dry-run sandbox (Gate 2) | #955, #956 |
| #959 (E) | Authored-construct runtime + scheduler (mutable, owner-scoped) | #955, #956 |
| #960 (F) | Planner design/repair loop (Responsibility -> bundle) | #956, #957 |
| #961 (G) | Approval + authority/capability gating + governance + activation | #958, #959, #960 |

## Authority, safety, governance

- Authored automations run under the **author's authz envelope** -- per-row authz
  enforces no privilege escalation.
- An authored automation calling computer-use / webhooks / mutations clears the
  **same scope grants + kill-switch** the planner already enforces. Reuse, don't
  reinvent.
- **Per-automation circuit breaker** fault-isolates a misbehaving authored
  automation (auto-pause + surface); never cascades.
- **Global kill-switch** for all authored automations.
- **Audit** event on every activation / retirement.
- **Edit/version semantics:** editing a Responsibility or a shared cataloged dep
  triggers impact analysis -> re-validate dependents in sandbox -> version /
  supersede.

## Open threads (deferred to their own sessions)

- **Syntax & ergonomics** of authored bundles -- what the `.memql` actually looks
  like, how bundles + dependency reuse are expressed, and how the planner emits
  it. (Owner explicitly deferred this.)
- **Multi-node ownership** specifics -- which node owns an owner's authored
  automations; leader-gating like the core scheduler (#561).
- **Catalog GC / retention** -- retiring unused cataloged constructs.

## Relationship to existing systems

- **`v1:planner:responsibility`** (reactive-harness, #629) is the human-facing
  front door; the authored bundle is its compiled artifact. Reuse its
  draft/active/paused/archived lifecycle and intake/clarification flow.
- **`produceArtifact`** is the sibling of the new authoring job: instead of
  writing a runtime file, the planner writes a *capability*. As of epic #1160
  it is also the **trigger**: a completed `produceArtifact` / `adHocAction`
  task is post-hoc CAPTURED into a bundle (see below).
- The planner's existing **budget / model-tiering / convergence guards** (#818-#843)
  wrap the design/repair loop so authoring spend is bounded like any other plan.

## Connected: everyday-task capture (epic #1160, #1161)

#955-#961 built the full pipeline above as **building blocks** -- design pass,
emit/repair, the gates, the runtime, versioning, the catalog -- each
independently unit-tested. They were NOT chained on a live trigger: nothing ran
the Responsibility/task -> bundle flow end to end (`runDesignPass` /
`emitAndRepairBundle` / `handoffToGate2` had zero non-test callers). Epic #1160
wires the spine.

`integrations/planner/agent_loop_authoring_capture.go` is the live orchestrator.
On every user-facing one-off task (`produceArtifact` / `adHocAction`) reaching
`succeeded`, the `AuthoringCaptureDispatcher` runs the pipeline **post-hoc** in a
detached goroutine and persists a stored, versioned `v1:authoring:bundle`:

```
Plan succeeded (produceArtifact / adHocAction)        [trigger -- #1161]
   |  AuthoringCaptureDispatcher.HandlePlanUpdated -> claim once
   v
runDesignPass (goal = design statement)               [reuse-or-author]
   v
emitAndRepairBundle (GATE 1 + repair loop)            [validated | failed]
   v
persist: createAuthoringBundle (sourcePlanId)         [v1:authoring:bundle]
         + createAuthoringConstruct (per dep)
         + recordBundleValidation
   v
handoffToGate2 (GATE 2 behavioral dry-run)            [dryRunPassed | failed]
         + recordBundleDryRun
   v
TERMINAL = dryRunPassed   (NOT activated -- GATE 3 approval + activation are a
                           later user action via the #1162 surface)
```

Design decisions (owner):

- **Post-hoc capture, not author-and-run.** The task runs exactly as today (the
  deliverable is never at risk); the bundle is authored AFTER the fact and is
  NOT activated. Capture records what ran; it does not replace execution.
  Promoting a captured/edited bundle to a live executing automation
  (author-and-run) is the documented follow-up.
- **Default-on for every task**, gated by `MEMQL_AUTHORING_CAPTURE_ENABLED`
  (default on; an ops kill-switch) and bounded by the same process-wide LLM
  ceiling + latching kill-switch (#1141) and the per-plan repair budget (#819).
- **Idempotent**: `sourcePlanId` on the bundle + `authoringBundleForPlan`
  skip a re-delivered terminal event (belt-and-suspenders with the in-process
  claim across restarts / cross-node re-delivery, #1155). `sourcePlanId` is also
  the lookup key for the #1162 view/edit/export surface.

The three Gate seams (`CompileBundle` / `CatalogNearMatches` / `RunBundleDryRun`)
need the concrete `*MemQLEngine`, which the planner's narrow `Engine` interface
does not expose; they are reached through the app-level `CognitionEngineAdapter`
and obtained by the orchestrator via a `captureEngine` type assertion, so a
binary without the authoring seams linked simply skips capture (never a hard
failure).

## Multi-phase composition (issue #1163)

A long, sequential Responsibility decomposes into ordered PHASES. The design
pass (`authoringDesign`) optionally emits a `phases[]` array (each: name +
purpose + its own dependency closure) instead of a flat `dependencies` list;
single-phase duties stay flat (the common case, unchanged).

The emit pass (`agent_loop_authoring_phases.go`) then:

- emits one sub-automation + its authored closure PER PHASE, reusing the same
  `authoringEmit` prompt the single-phase path uses (a phase IS "one automation
  + its closure"), and
- DETERMINISTICALLY synthesizes the headline automation in Go -- it chains the
  phase sub-automations with ordered `step` blocks, each phase after the first
  gated on the prior step succeeding
  (`if steps.<prior>.status == "success" { automation … }`, memql#1366).
  The executor runs top-level steps sequentially in list order, so the chain
  is a real sequence (phase 0 -> phase 1 -> …). Synthesizing in Go
  (not via the model) makes the chaining unverifiable-by-fumble and
  unit-testable without an engine; the synthesized headline is proven to compile
  through the real Gate-1 sandbox.

The phase sub-automations + headline are trigger-less -- invoked via the `step {
automation … }` kind, not a real-world event -- which fits the capture path (a
one-off task has no recurring trigger; the bundle is a replayable record). Gate
1/repair, capture persistence, and `bundleAutomationSource` operate on the flat
construct list, so a multi-phase bundle is just "more constructs, one of which
is the headline" -- no downstream change.

### Concurrent phases (issue #1164)

Each phase carries an optional `dependsOn[]` (the earlier phases it waits on).
The synthesizer (`buildPhaseLayers`, Kahn topological layering) groups phases
into ordered LAYERS: layer 0 is every phase with no unmet dependency, layer k is
every phase whose deps all sit in earlier layers. Phases in the same layer are
mutually independent, so:

- a layer with ONE phase emits a sequential `step` named after the phase;
- a layer with 2+ independent phases emits a `parallel { branches: […], wait:
  "all", failFast: true }` step (named `layer<k>`) -- they run CONCURRENTLY
  (grammar shipped in memql#1368; the emission was parked between PR #1367
  and #1368 while the struct-form grammar had no parallel step);
- layer k>0 is gated `if steps.<priorStep>.status == "success"`, so layers run
  in order while phases within a layer fan out; `failFast: true` makes any
  branch failure fail the layer, which skips everything downstream.

When NO phase declares `dependsOn` the synthesizer defaults to a strict
sequential chain (the #1163 shape -- you can't assume two phases are independent
without an explicit edge). A dependency cycle or unknown reference degrades to a
by-index sequence rather than deadlocking. The synthesized parallel headline is
proven to compile through the real Gate-1 sandbox. This is the authoring half of
#1164; the planner's own task-DAG (`v1:planner:task.dependsOn[]` + concurrent
task dispatch) remains a separate follow-up.

### Generated parallel plan-automations for sectionable deliverables (issue #1394)

The same `synthesizePhasedHeadline` parallel synthesizer now also serves the
DELIVERABLE path (not just the standing-Responsibility authoring path). The
cheap `goalComplexityTriage` classifier (`agent_loop_triage.go`, volume-aware
since #1393) gained a SECTIONABLE shape: alongside its complexity verdict it
emits `sectionable` + a `sections[]` list + an `assembly` intent when a
non-trivial deliverable is one conceptually-simple deliverable made of
INDEPENDENT sections -- the "10 German folk tales, each a complete story" case.

When triage classifies a goal as non-trivial AND sectionable,
`maybeGenerateSectionable` (`agent_loop_sectionable_generate.go`)
DETERMINISTICALLY synthesizes a parallel plan-automation instead of marching the
sections through a serial decompose chain:

- one production sub-automation PER section (a bounded agent turn, each writing
  its section to the per-plan workspace) -- these are the layer-0 phases;
- one assemble+verify sub-automation that `dependsOn` every section -- so it
  becomes the gated step after the parallel layer;
- the headline, built by `synthesizePhasedHeadline`: the sections collapse into
  one `parallel { wait:"all" failFast:true branches:[…] }` layer-0 block and the
  assemble step runs gated on `steps.layer0.status == "success"`.

The bundle (sections + assemble + headline + a small logic closure) compiles
through the SAME Gate-1 sandbox the authoring pipeline uses, then is persisted
through the authoring-bundle pipeline (`createAuthoringBundle` +
per-construct rows + a validated record), stamped with `sourcePlanId`. The LLM
only decides sectionability + the section list; the parallel STRUCTURE is
deterministic Go (so it can't be fumbled and is unit-testable without an engine,
`agent_loop_sectionable_test.go`). Wallclock/budget: each branch is an ordinary
bounded turn, so the per-plan token budget + the process-wide rate ceiling gate
the fan-out width; the branch COUNT itself is bounded by `maxSectionFanout`.
Structurally the deliverable is immune to the single-turn timeout that
hard-routing a many-section deliverable to one direct turn hit (#1393) -- N
parallel section turns + 1 assembly. Every miss (not sectionable, no Gate-1 seam
linked, compile failure, persist error) declines and the Plan falls through to
the normal bounded decompose loop -- generation is an OPTIMIZATION, never a hard
requirement. Kill-switch: `MEMQL_PLANNER_SECTIONABLE_ENABLED=0`.
