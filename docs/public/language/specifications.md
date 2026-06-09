---
title: MemQL Specifications
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

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
- **Context-specs** -- the body references `actor.X` only
  (e.g. `actor.role`, `actor.isClusterOwner`). The expression
  evaluates in-process; called from policies via `spec("name")` for
  actor-based checks like "is admin", "owns partition", etc.

Bodies that mix both flavors (row + actor references in the same
expression) are rejected at load time.

## Authoring rules

- Body is a single boolean expression. No `ctx`, no `return`, no
  parameter.
- Side-effect free. Specs cannot call mutation functions, and
  cannot call other procedural DSL receivers.
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

Called by bare reference inside a query's `filter` clause:

```memql
query queryHumanParticipants {
  args {
    spaceId  string  @required
  }
  concept v1:cognition:participant
  filter  payload.spaceId==args.spaceId; specIsHumanParticipant
  shape   participantFull
}
```

### Context-spec (in-process)

```memql
@enabled
@description("Actor holds an admin or owner role")
spec requiresAdmin {
  actor.role == "admin"
}
```

Called from a policy body via the `spec("name")` builtin:

```memql
@tier("bff")
@description("Gate the admin settings panel")
policy canViewAdminSettings {
  return spec("requiresAdmin")
}
```

(The legacy `func (Policy) ... { ctx.output = ...; return ctx, nil }`
shape was retired in memql#302 / #303 -- the rewriter emits the
canonical `return <expr>, nil` form and the loader rejects author-
written procedural shapes at parse time. Don't author it.)

## CQS interaction

Compile-time CQS validation enforces:

- Query -> Mutation: not allowed
- Spec -> Mutation: not allowed
- Mutation -> Mutation: not allowed (single `insert(...)` per body)

This keeps the read path side-effect-free and makes the SQL-pushdown
case for row-specs always safe.

## Migration nudge from policies

**The rule (locked Decision 2 of the MVP-foundation work).** A policy
is rejected at load time when ALL of the following hold:

1. It carries none of the policy-only annotations:
   `@audited`, `@cacheable`, `@traces_persisted`, `@frontend_visible`,
   `@returns_trace`.
2. Its body contains zero `policy(...)` and zero `spec(...)`
   sub-routine calls.
3. It returns `bool`.
4. Its body reads only `actor.*` (no `ctx.*` / `args` / `payload`
   references).

When all four are true the policy is structurally a context-spec and
must be authored as a spec instead. The loader emits a precise
migration message naming the target spec file path:

```
policy "canViewAdminSettings" has no policy-only annotations and no
sub-policy calls (body reads only actor.* and contains no
policy()/spec() calls); author as a spec instead. Move to
dsl/specs/<namespace>/canViewAdminSettings.memql, change the receiver
to `spec`, and replace `policy("canViewAdminSettings")` calls with
`spec("canViewAdminSettings")`.
```

**Why the four conditions instead of two.** Decision 2 nominally
lists conditions (1) and (2). The implementation in
`component/memql/policy_function_loader.go` additionally requires
(3) bool return and (4) no `args`/`ctx`/`payload` reads, both as
guard rails preventing false-positive rejections of obviously-policy
bodies that happen to lack annotations -- a policy that returns
`string` (vendor name) or that reads caller-passed args is
clearly not a spec.

**Why the rule exists.** Specs are the atomic boolean primitive.
Policies compose decisions across many specs, sub-policies, and
config. Letting both share the same author shape leads to drift:
two ways to write the same thing, two surfaces to maintain, no
clear answer to "when do I write a spec vs a policy?". The rule
collapses the ambiguity.
