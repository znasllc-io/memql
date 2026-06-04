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
  rules in `docs/core/memql-authoring-rules.md`.
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
  writing a runtime file, the planner writes a *capability*.
- The planner's existing **budget / model-tiering / convergence guards** (#818-#843)
  wrap the design/repair loop so authoring spend is bounded like any other plan.
