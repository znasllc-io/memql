---
title: Spec/Shape binding contract -- signature binding, return bodies, ambient gateways
audience: internal
status: accepted
area: internal
sinceVersion: 0.9.6
owner: znas
---

# ADR: Spec/Shape binding -- the static-access contract

> **Status: ACCEPTED (owner sign-off 2026-06-27).** This ADR freezes the
> static-data half's *binding model*: how a `spec` declares the single data
> surface it predicates over, and how a `shape` is the only gateway to ambient
> (row / actor) data. It is the static-layer complement to the behavioral-
> constructs ADR (`dsl-behavioral-constructs-adr.md`) and carries the same
> weight. It is a decision record; the implementation is tracked by the stories
> under epic #2281.
>
> Date: 2026-06-27.

## 1. Context

MemQL's opinionated-but-explicit thesis is that **the access surface is fully
declared**: the author declares dependencies, the engine builds a real
dependency tree, and that tree buys a consistency free-form LLM harnesses
cannot match. The static-data constructs -- `concept`, `shape`, `spec`,
`trait`, `query`, `mutation` -- are where that surface lives.

Before this ADR the binding model leaked:

- **Specs derived their data surface implicitly.** A spec's body was a bare
  boolean expression (no `return`); the engine *inferred* the evaluation
  strategy by walking field references -- `payload.X` / row-intrinsics meant a
  SQL **row-spec**, `actor.X` meant an in-process **context-spec**, and a body
  that mixed both was rejected. The surface was a side effect of the body, not
  a declaration.
- **`@shape("...")` was an optional, unvalidated pin.** It documented the shape
  a spec read, but nothing verified it resolved and the eval strategy ignored
  it. (Distinct from the `@useShape` / `@use*` annotation family retired in
  #301 -- do not confuse the two.)
- **Ambient data had no single gateway.** Specs reached `actor.*` and `row.*`
  directly. A concept's identity blurred with its row metadata and with the
  caller envelope.

The result: a spec's true dependencies (which concept? which shape? which
ambient surface?) were not declared and not edges in the dependency tree.

## 2. Decision

**One principle.** *A concept is purely its domain payload properties. Row
metadata (`row.*`) and the caller/auth envelope (`actor.*`) are ambient data
that must be explicitly opted into -- and shapes are the only gateway.*

### 2.1 Spec rules

1. **Signature binding.** A spec binds **exactly one shape XOR concept** in its
   signature: `spec <boundName> <specName>`. `<boundName>` resolves through the
   file-top `use` import; shape-vs-concept is disambiguated by the import's
   source path (`...shapes.{ }` vs `...concepts.{ }`).
2. **`return` body.** The body **`return`s a boolean** -- e.g.
   `spec deployment deploymentInProgress { return status == "in_progress" }`.
   The old bare-expression form is rejected with a migration-pointing error.
3. **Bare field access.** The body reads fields by **bare name** -- no
   `payload.`, no `<shapeName>.`, no `<conceptName>.` prefix. The signature
   already names the single bound context, so bare names are unambiguous.
4. **No ambient in spec bodies.** A spec body **may never reference `actor.*` or
   `row.*`**, and a spec signature **may never carry `@actor` / `@row`**. To
   predicate on caller or row data, bind a shape that projects it and read the
   projected key bare.
5. **Classification follows the binding** (not the body):
   - concept-bound, or bound to an `@row` shape -> **row-spec** (compiles to a
     SQL `WHERE` fragment);
   - bound to an `@actor` shape -> **context-spec** (evaluates in-process).
   The old "mixed body" rejection is **obsolete**: one binding = one surface, so
   a mix is unrepresentable by construction.

### 2.2 Shape rules

1. **`@row` / `@actor` are shape-only markers.** `@row` unlocks `row.*` (id,
   createdAt, createdBy, ...) in the shape body; `@actor` unlocks `actor.*`
   (userId, role, identityId, isClusterOwner, rank, now, config). At least one
   is required; both is allowed (a mixed shape).
2. **Body access.** Payload props are **bare**; ambient data uses the explicit
   `row.` / `actor.` prefix. Each entry **projects to a flattened key**:
   `actor.role` -> key `role`, `row.createdBy` -> key `createdBy`,
   `payload.name` (or bare `name`) -> key `name`.
3. **Signature.** `shape <Concept> <name>` (concept-bound -- the bound concept
   resolves through the file-top `use ...concepts.{ }` import) or `shape <name>`
   (a caller/trait shape with no concept, e.g. `actorEnvelope`). An `@actor`
   shape carries no signature concept; an `@row` shape must.
4. **Composition** via `include <otherShape>` is unchanged (transitive;
   cycles + key collisions are errors).

### 2.3 `@shape` annotation -- removed

The `@shape("...")` spec annotation is **deleted**. The binding moves to the
signature. `ValidateSpecBindings` changes from "verify `@shape(...)` resolves"
to: **verify the signature binding resolves to an imported shape or concept,
and every bare field in the body is a projected key of that binding.**

### 2.4 `actor` vs `user` -- kept separate

- **`actor`** is the **caller/auth envelope** (who is acting, from the JWT):
  `userId`, `role`, `identityId`, `isClusterOwner`, `rank`, `now`, `config`.
  Reachable **only** inside `@actor` shapes.
- **`user`** is a **domain concept** (the user record): `primaryEmail`,
  `lastSeenAt`, `deletionScheduledAt`, ... Bound like any concept. It is **not**
  folded into the envelope. (The `user.implicit` / `user.explicit` wiring is
  confirmed during Story 8.)

### 2.5 Collision rule

Because payload props are bare and ambient data is prefixed, the names **`row`
and `actor` are reserved as payload field names** -- the only ambiguity. A
concept payload field named `row` or `actor` is rejected at load.

### 2.6 The contract in one table

| Accessor | In a shape body | In a spec body |
|---|---|---|
| payload props (bare) | yes | yes (bare) |
| `row.*` | yes, with `@row` | never -- bind a `@row` shape, read its key bare |
| `actor.*` | yes, with `@actor` | never -- bind an `@actor` shape, read its key bare |

### 2.7 Traits -- the resolved open question

**Decision: a `trait` stays the one deliberately *unbound* row predicate.** It
carries **no signature binding**.

Rationale: a trait is by nature *polymorphic* -- `traitIsActiveRecord`
(`active == true`), `statusRowTrait`, `deletedRowTrait`, etc. are scaffolds
meant to be reused across *any* concept that carries the field. Forcing a single
concept/shape binding would defeat that purpose; minting a generic "trait shape"
per trait would add ceremony to the one construct designed to avoid it.

Concretely, under this ADR a trait:

- has signature `trait <name>` (no bound name);
- `return`s a boolean over **bare payload field names only** (consistent with
  the new spec body form);
- **may not** reference `row.*` or `actor.*` (it binds no `@row` / `@actor`
  shape, so it has no ambient gateway -- traits are pure payload predicates);
- is validated against the concrete concept at its **call site** (a query
  `filter`, or composition into a concept/shape-bound spec), exactly as today --
  the bare field must exist on whatever concept includes it.

This makes the trait the sole, explicit exception to signature binding, and the
exception is principled: it is unbound *because* it is generic, and it stays
inside the payload surface so it never needs an ambient gateway.

## 3. Worked examples (target syntax)

```memql
// CALLER predicate -- @actor shape is the gateway; spec reads the key bare.
@actor
shape actorEnvelope { actor.userId; actor.role; actor.isClusterOwner }

use common.shapes.{ actorEnvelope }
spec actorEnvelope requiresOwner { return role == "owner" }

// CONCEPT predicate -- payload props bare, no payload./concept. prefix.
use cluster.concepts.{ deployment }
spec deployment deploymentInProgress { return status == "in_progress" }

// ROW-metadata predicate -- bind a @row shape, read its projected keys bare.
@row
shape deployment deploymentRow { status; row.createdBy }

use cluster.shapes.{ deploymentRow }
spec deploymentRow authoredBySeed { return createdBy == "seed" && status == "succeeded" }

// TRAIT -- deliberately unbound, bare payload field, validated at call site.
trait traitIsActiveRecord { return active == true }
```

## 4. Migration plan

The repo is pre-release: no compat shims, no deprecation window (per the branch
workflow). The contract changes and every consumer changes with it.

1. **Story 2 -- parser/grammar.** Accept the `spec <boundName> <specName>`
   signature and `return <bool>` bodies; reject the bare-expression form with a
   migration-pointing error. Files: `component/language/parser/spec_decl.go`,
   `component/language/ast/ast.go`, `component/memql/spec_converter.go`.
2. **Story 3 -- concept flattening + access enforcement.** A concept bound in a
   spec/shape exposes payload props by bare name; a concept binding cannot reach
   `row.*` / `actor.*`; reserve `row` / `actor` as payload names.
3. **Story 4 -- `@row` / `@actor` as shape-only gateways.** Accept the markers
   only on shapes; in shape bodies gate `row.*` behind `@row` and `actor.*`
   behind `@actor`; a spec inherits ambient access only by binding such a shape.
4. **Story 5 -- remove `@shape`.** Delete its parsing/handling; rework
   `ValidateSpecBindings` to the signature model.
5. **Story 6 -- validator/crossref + dependency edges.** Enforce one binding per
   spec, bare-only access, `return` present; emit the `spec -> shape|concept`
   edge into the authoring-sandbox crossref
   (`component/memql/authoring_sandbox_crossref.go`).
6. **Story 7 -- reference skeletons.** Rewrite `dsl/_reference/_spec.memql` and
   `_shape.memql` to this contract.
7. **Story 8 -- migrate the tree.** Migrate `dsl/deployment/specs.memql` (the
   three `@shape("actorEnvelope")` specs) and audit + migrate every spec
   tree-wide that uses `@shape` or references `actor.*` / `row.*` directly;
   create the small `@actor` / `@row` shapes needed; resolve `actor` vs `user`.

Gate each story on `make test`, `make lint`, and the DSL load + drift gates
(`TestEngineInitLoadsFullDSL`, the conformance suite). If implementation forces
a contract change, update this ADR in the same PR and flag the owner.

## 5. Consequences

- **Positive.** A spec's data surface is a single declared binding and a real
  dependency-tree edge; ambient access is auditable (every `row.*` / `actor.*`
  read traces to a marked shape); concept identity is clean (payload only);
  bodies are uniform (`return <bool>`) with the rest of the DSL; the "mixed
  body" failure mode disappears by construction.
- **Cost.** A one-time tree migration and a parser/validator change. Specs that
  predicated on ambient data now require an explicit `@row` / `@actor` shape --
  more declarations, but each is the dependency made visible, which is the point.
- **Non-goals.** This ADR does not touch query/mutation binding (already
  signature-bound, #47-#49), the behavioral constructs, or the runtime
  evaluation engines beyond routing on the new binding.
