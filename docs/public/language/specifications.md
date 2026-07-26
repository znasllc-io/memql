---
title: MemQL Specifications
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Specifications

> Last Updated: 2026-06-11

## What Specs Are

Specs are atomic, named boolean predicates declared in struct form. A
spec **binds exactly one shape XOR concept in its signature** and the
body **`return`s a boolean** over **bare** field names (epic #2281):

```memql
spec <boundName> <name> {
  return <boolean-expression>
}
```

`<boundName>` resolves through the file-top `use` import (a shape import
binds a shape, a concept import binds a concept). The binding picks how
the spec is evaluated:

- **Row-specs** -- bind a concept or a `@row` shape. The expression
  compiles into a SQL `WHERE` fragment and pushes down to the database
  for filtering.
- **Context-specs** -- bind an `@actor` shape (the only gateway to the
  auth envelope). The expression evaluates in-process against the auth
  context; invoked via the `spec("name")` builtin, or from Go via
  `engine.EvaluateSpec(ctx, "name")`, for actor-based checks like
  "is admin", "owns partition", etc.

A spec body never reads `actor.*` / `row.*` directly -- bind a shape
that projects it and read the projected key bare. A `trait` is the one
deliberately-unbound row predicate (bare payload fields, validated at
the call site).

## Authoring rules

- Body is a single `return <boolean expression>` over bare field names
  (no `payload.` prefix -- the binding names the surface).
- Side-effect free. Specs cannot call mutation functions or logic
  functions.
- Named for the predicate they express, with no kind prefix
  (`requiresAdmin`, `isActiveRecord`). The `spec` / `trait` keyword
  already marks the kind at the declaration.
- The `@shape("name")` annotation is removed; the legacy
  `func (Spec) name(ctx any) bool { ... }` form and the older
  bare-expression body (no `return`) are retired and rejected with a
  migration hint.

## Examples

### Row-spec (SQL pushdown)

```memql
use cognition.concepts.{ participant }

@description("Matches participants that completed verification")
spec participant isVerifiedParticipant {
  return verified == true
}

@description("Active records created by system automation")
spec participant systemActive {
  return active == true && createdBy == "system:automation"
}
```

Called by bare reference inside a query's `filter` clause. The
query binds its concept in the signature (`query <Concept> <name>`)
and pulls cross-file constructs in via file-top `use` imports;
predicates compose with the Go boolean grammar (`&&` / `||` / `!`):

```memql
use cognition.concepts.{ participant }

query participant queryHumanParticipants {
  args {
    spaceId  string  @required
  }
  filter  spaceId==args.spaceId && isHumanParticipant
  shape   participantFull
}
```

(The legacy `;`-AND / `,`-OR filter separators are retired and
rejected at parse time -- `&&` / `||` are the only connectives.)

### Context-spec (in-process)

```memql
use common.shapes.{ actorEnvelope }

@description("Caller must hold owner or admin role to use the Deployment Console.")
spec actorEnvelope requiresOwnerOrAdmin {
  return role == "admin" || role == "owner"
}
```

(The `actorEnvelope` `@actor` shape is the gateway to the auth envelope;
the spec reads its projected key -- `role` -- by bare name. The
signature binding is verified at load: it must resolve to an imported
shape or concept.)

Context-specs are invoked via the `spec("name")` builtin or, from Go,
`engine.EvaluateSpec(ctx, "name")` against the request's auth context.

## CQS interaction

Compile-time CQS validation enforces:

- Query -> Mutation: not allowed
- Spec -> Mutation: not allowed
- Mutation -> Mutation: not allowed (single `insert { ... }` /
  `update { ... }` block per body)

This keeps the read path side-effect-free and makes the SQL-pushdown
case for row-specs always safe.

## Specs vs policies

**Specs are the only DSL surface for boolean predicates.** The
decision-policy tier that once hosted caller-based authz /
feature-gating decisions (`func (Policy)` bodies, `@tier` /
`@audited` / `@traces_persisted` annotations, `engine.EvaluatePolicy`)
was retired in memql#984 -- it carried zero live constructs and the
machinery has been fully removed. The parser rejects authored
decision-policy bodies.

What remains:

- The live `policy` construct is an **empty-bodied AI
  provider-selection record** (`@primary` / `@fallback` /
  `@maxLatencyMs` / `@preferredRole`), consolidated in
  `dsl/policies/policies.memql` and consumed by the AI Router. It is
  not a predicate surface.
- Caller-context boolean checks (is admin, owns partition,
  permission gates) are authored as **context-specs** in
  `dsl/<namespace>/specs.memql` and invoked via `spec("name")` /
  `engine.EvaluateSpec`.
- Risk/scope decision logic lives in Go (`component/safety`).
