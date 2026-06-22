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

Specs are atomic, named boolean predicates declared in struct form:

```memql
spec NAME {
  <single-boolean-expression>
}
```

They are evaluated in one of two ways, picked by the engine based on
which fields the body references:

- **Row-specs** -- the body references `payload.X` and/or row
  intrinsics (`id`, `concept`, `type`, `createdAt`, `createdBy`,
  `schema`). The expression compiles into a SQL `WHERE` fragment
  and pushes down to the database for filtering.
- **Context-specs** -- the body references `actor.X` only
  (e.g. `actor.role`, `actor.isClusterOwner`). The expression
  evaluates in-process against the auth-context envelope; invoked
  via the `spec("name")` builtin, or from Go via
  `engine.EvaluateSpec(ctx, "name")`, for actor-based checks like
  "is admin", "owns partition", etc.

Bodies that mix both flavors (row + actor references in the same
expression) are rejected at load time.

## Authoring rules

- Body is a single boolean expression. No `ctx`, no `return`, no
  parameter.
- Side-effect free. Specs cannot call mutation functions or logic
  functions.
- Prefer `spec*` naming for row-specs (matches the call sites in
  query filter clauses). Actor-only context-specs may drop the
  prefix when the name reads more naturally (`requiresAdmin`).
- The legacy `func (Spec) name(ctx any) bool { return <expr> }`
  form is retired; the parser rejects it with a migration hint.

## Examples

### Row-spec (SQL pushdown)

```memql
@enabled
@description("Matches participants with human participantType")
spec specIsHumanParticipant {
  payload.participantType == "human"
}

@enabled
@description("Active records created by system automation")
spec specSystemActive {
  payload.active == true && createdBy == "system:automation"
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
  filter  payload.spaceId==args.spaceId && specIsHumanParticipant
  shape   participantFull
}
```

(The legacy `;`-AND / `,`-OR filter separators are retired and
rejected at parse time -- `&&` / `||` are the only connectives.)

### Context-spec (in-process)

```memql
use common.shapes.{ actorEnvelope }

@enabled
@description("Caller must hold owner or admin role to use the Deployment Console.")
@shape("actorEnvelope")
spec requiresOwnerOrAdmin {
  actor.role == "admin" || actor.role == "owner"
}
```

(`@shape("name")` is an optional pin -- the eval strategy comes from
the body's field references, but when present the engine verifies the
body reads a subset of the named shape's projected fields.)

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
