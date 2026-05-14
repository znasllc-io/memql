# MemQL Specifications

> Last Updated: 2026-05-13

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
- **Context-specs** -- the body references `caller.X` only
  (e.g. `caller.role`, `caller.isClusterOwner`). The expression
  evaluates in-process; called from policies via `spec("name")` for
  caller-based checks like "is admin", "owns partition", etc.

Bodies that mix both flavors (row + caller references in the same
expression) are rejected at load time.

## Authoring rules

- Body is a single boolean expression. No `ctx`, no `return`, no
  parameter.
- Side-effect free. Specs cannot call mutation functions, and
  cannot call other procedural DSL receivers.
- Prefer `spec*` naming for row-specs (matches the call sites in
  query filter clauses). Caller-only context-specs may drop the
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

Called by bare reference inside a query's `filter` clause:

```memql
query queryHumanParticipants {
  args {
    spaceId  string  @required
  }
  concept v1:cognition:participant
  filter  payload.spaceId==ctx.spaceId; specIsHumanParticipant
  shape   participantFull
}
```

### Context-spec (in-process)

```memql
@enabled
@description("Caller holds an admin or owner role")
spec requiresAdmin {
  caller.role == "admin"
}
```

Called from a policy body via the `spec("name")` builtin:

```memql
@tier("bff")
@description("Gate the admin settings panel")
func (Policy) canViewAdminSettings(_ any) bool {
  ctx.output = spec("requiresAdmin")
  return ctx, nil
}
```

## CQS interaction

Compile-time CQS validation enforces:

- Query -> Mutation: not allowed
- Spec -> Mutation: not allowed
- Mutation -> Mutation: not allowed (single `insert(...)` per body)

This keeps the read path side-effect-free and makes the SQL-pushdown
case for row-specs always safe.

## Migration nudge from policies

A policy whose body would compile as a pure context-spec (caller-only
boolean, no policy-only annotations like `@audited` / `@cacheable`,
no sub-policy calls) is rejected at load time. The loader emits a
hint pointing the author at the spec form: move the file under
`dsl/v1/specs/...`, swap the receiver to `spec`, and have any caller
use `spec("name")` instead of `policy("name")`.
