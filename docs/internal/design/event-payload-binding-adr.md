---
title: Event payload binding -- payload-as-args, bare-field reads, punning, and the automation envelope
audience: internal
status: accepted
area: internal
sinceVersion: 0.12.0
owner: znas
---

# ADR: Event payload binding for automations

> **Status: ACCEPTED (owner sign-off 2026-07-03).** This ADR makes automation
> inputs **typed and signature-bound**, the way every other construct's inputs
> already are. It records the design locked under epic
> [#2352](https://github.com/znasllc-io/memql/issues/2352) (stories
> [#2363](https://github.com/znasllc-io/memql/issues/2363)--[#2367](https://github.com/znasllc-io/memql/issues/2367))
> and builds on the
> [behavioral-constructs ADR](./dsl-behavioral-constructs-adr.md) (automations
> may be triggered OR invoked by reference, section 2.2) and the
> [construct-invocation ADR](./construct-invocation-syntax-adr.md) (named-only
> call args). Releases remain 0.x; nothing here implies a 1.0 version bump.

## Context

Automations are the one behavioral construct still reading their input through
a raw, untyped envelope. The DSL binding built by
`component/automations/executor.go` (`buildEventEnvelope`) exposes exactly
three keys -- `event.topic`, `event.kind`, `event.payload` -- and everything an
automation actually consumes lives under `event.payload.<field>` with no
declared shape anywhere:

- **Payload contracts are comments.** The `deploy.requested` payload is
  documented in a ~60-line comment block at the top of
  `dsl/deployment/automations.memql`. Nothing validates that an emitter sends
  `environment`; nothing lets Sense autocomplete it; a typo'd
  `event.payload.enviroment` resolves to nil and the automation keeps running.
  This is the same silent-nil failure class the fail-loud program
  ([#2351](https://github.com/znasllc-io/memql/issues/2351)) exists to kill.
- **The language already solved this twice.** Queries and mutations bind their
  concept in the signature and read bare payload properties in the body (epic
  #2292 removed the `payload.` prefix there); specs bind a shape or concept in
  the signature and read bare fields (epic #2281). Automations are the
  inconsistency.
- **The Go envelope is richer than the DSL sees.** `events.Event` carries
  `Timestamp`, `Metadata` (actor), `OriginNodeId`, and `Partition` -- all
  dropped at the DSL boundary. The `_automation.memql` reference skeleton
  documents `event.actor.id`, which does not exist in the binding (a doc bug);
  emitters compensate by hand-stamping `triggeredBy` into payloads.
- **Automations have two entry modes but no shared contract.** Per the
  behavioral ADR section 2.2 an automation may be triggered by an event or
  invoked by reference with params. Today the trigger mode is untyped and the
  invoke mode has nowhere to declare its params.

## Decision 1 -- payload-as-args

An automation declares its input contract in an `args { }` block, exactly like
logic/query/mutation (same field grammar: `<name> <type> [@required] [@enum]
[@description]`; `@default` remains invalid on args fields per #991 --
`coalesce` in the body stays the canonical defaulting mechanism).

```
@trigger(event="deploy.requested", concept="v1:cluster:deployment")
automation deployEngineCluster {
  args {
    environment     string   @required @enum("development","staging","production")
    engineNodeTypes []string @required
    workdir         string   @required
  }

  step gate { action runDeployGate(environment, workdir) }   // punning, Decision 5
  switch environment { ... }
  forEach nt in engineNodeTypes { ... }
}
```

- **Trigger mode:** the scheduler binds `event.payload` into `args`, validated
  (Decision 2), before any step runs.
- **Invoke-by-reference mode:** the caller's params bind to the same
  `args { }`. One contract serves both entry modes.
- The args block **replaces** the payload comment block as the machine-checked
  emitter/consumer contract -- which is also what the planner needs to generate
  automations safely.

**Rejected alternatives.**

- *A separate `@event` shape kind bound in the trigger annotation.* Shapes are
  projections of concepts/the actor envelope; an event payload is neither. It
  would also split the trigger contract from the invoke-by-reference contract,
  leaving two input declarations on one construct.
- *Bare-read sugar without a schema.* Bare names are only safe when a declared
  shape resolves them at load time. Sugar over an untyped map turns every typo
  into a silent nil -- strictly worse than today's explicit `event.payload.X`,
  and opposite to the fail-loud program. Schema-then-sugar or nothing.

## Decision 2 -- fire-time validation, loud refusal

When a trigger fires for an automation with an `args { }` block:

- **Missing `@required` field or type/enum mismatch -> the automation refuses
  to fire, loudly**: a Warn log naming the automation, topic, and the failing
  field, plus a skip counter (surfaced through the load/run report machinery
  landing with [#2357](https://github.com/znasllc-io/memql/issues/2357)).
  Never a silent nil, never a partial run.
- **Unknown extra payload fields are tolerated** (tolerant-reader): emitters
  must be able to add fields before every consumer declares them. Extra fields
  are simply not bound; a Debug-level note records them. Strictness lives on
  the fields the automation *declares*, not on the payload's totality.
- `@trigger @filter(...)` expressions evaluate against the same validated args
  binding (plus the envelope intrinsics of Decision 4).
- An automation **without** an `args { }` block keeps today's untyped
  `event.payload.X` access only until the migration (Decision 6) completes;
  after it, an event-triggered automation with no args block is a load error.

## Decision 3 -- bare-field resolution and shadowing

Inside an automation body, a bare identifier resolves in this order:

1. **Reserved engine names** -- `now`, `actor`, `partition`, `config`,
   `trace`, `event`, `args`, `steps` (never shadowable).
2. **Loop variables** in scope (`forEach nt in ...`).
3. **Declared step names** (step-result references, `gate.result`).
4. **args fields.**

Anything else is a **load error** (unknown identifier), not a nil.

Shadowing is rejected at load, mirroring the reserved-name rule: a step name or
loop variable that collides with an args field, or an args field that collides
with a reserved name, fails the automation's load with a message naming both
sites. Explicit `args.X` remains valid everywhere as the disambiguating form.

## Decision 4 -- the envelope: intrinsics stay prefixed, actor gets exposed

- `event.topic` and `event.kind` remain the prefixed escape hatch, mirroring
  how `row.` / `actor.` prefixes work in shapes. Payload access moves to args;
  the envelope is for envelope things.
- **`event.actor` is added** to the binding, populated from the event's
  `Metadata` actor entry (`event.actor.id` at minimum). This makes the
  reference skeleton's documented-but-phantom `event.actor.id` real and lets
  emitters stop hand-stamping `triggeredBy` into payloads (the deploy payload
  migrates in [#2366](https://github.com/znasllc-io/memql/issues/2366)).
- **`event.timestamp`** (RFC3339, the event's occurrence time, distinct from
  the reserved `now` captured at eval start) is added alongside it.
- `OriginNodeId` and `Partition` stay dropped (partition retires with #56
  phase 8; origin routing is not an authoring concern).

## Decision 5 -- named-arg punning

Under the construct-invocation ADR, call args are named-only; a bare identifier
in argument position is currently a parse error. That grammatical space becomes
sugar:

```
action runDeployGate(environment, workdir)
// means
action runDeployGate(environment: environment, workdir: workdir)
```

- Generic across **all** named-arg call sites (logic bodies, automation steps,
  every construct invocation) -- not automation-specific.
- Mixed forms are allowed: `f(environment, workdir: someExpr)`.
- The punned identifier resolves by the ordinary scope rules (Decision 3 in
  automations; args/locals in logic). An unknown punned name is a load error,
  never nil.

## Decision 6 -- migration is a hard cut with a codemod

Per the pre-release no-shim policy, once the tree migrates
([#2367](https://github.com/znasllc-io/memql/issues/2367)):

- The codemod synthesizes each triggered automation's `args { }` block from its
  existing `event.payload.X` reads plus the payload comment blocks (the
  deployment block is authoritative for `deploy.requested`), rewrites
  `event.payload.X` to bare `X`, and puns call args where names align.
- The parser then **rejects `event.payload.`** with a migration-pointing error
  (the #2335 rejection-with-hint pattern), and rejects event-triggered
  automations that declare no args block.
- The terse single-step form (`automation NAME @trigger(...) => logic X`)
  is unchanged: it forwards the payload as the target logic's `event` arg and
  declares no args block of its own. A terse automation that wants typed inputs
  graduates to the full form.
- The product pack migrates in the same window (coordinated pack PR, as in
  bff#153).

## Consequences

- **Positive.** Emitter/consumer contracts become machine-checked; Sense gains
  payload autocomplete and typo diagnostics for free; automations read like
  queries (signature declares, body reads bare); one contract serves trigger
  and invoke-by-reference; generation and verification of automations by the
  planner becomes schema-guided; call sites shed the `event.payload.`
  ceremony without losing fail-loud behavior.
- **Cost.** Parser + scheduler + evaluator work (args block on automations,
  fire-time validation, resolution/shadowing rules, punning); a tree-wide
  codemod plus a coordinated pack migration; any existing name collisions
  surfaced by the shadowing rule must be renamed during migration.

## Rollout

Stories under epic [#2352](https://github.com/znasllc-io/memql/issues/2352):
ADR (this document, #2362) -> args block + trigger binding + validation
(#2363) -> bare-field resolution + shadowing (#2364) -> punning (#2365, with
#2366 envelope actor/timestamp in parallel) -> tree-wide migration + legacy
rejection (#2367). Sequenced before the automation-purity epic
([#2353](https://github.com/znasllc-io/memql/issues/2353)) so automation
conditions are rewritten once, against the bare-args vocabulary.
