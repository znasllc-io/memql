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

Specs are atomic, named boolean predicates. A spec **binds exactly one
shape XOR concept in its signature** (epic #2281), and its body is a
lambda: one parameter, then one boolean expression that reads what the
signature binds through that parameter:

```text
spec <boundName> <name> = row => <boolean expression>
```

`<boundName>` resolves through the file-top `use` import (a shape import
binds a shape, a concept import binds a concept). The binding picks how
the spec is evaluated:

- **Row-specs** -- bind a concept or a `@row` shape. The expression
  compiles into a SQL `WHERE` fragment and pushes down to the database
  for filtering.
- **Context-specs** -- bind an `@actor` shape (the only gateway to the
  auth envelope). The parameter is spelled `actor` and is the envelope:
  `spec actorEnvelope requiresOwner = actor => actor.role == "owner"`.
  The expression evaluates in-process against the auth context; apply
  it to `actor` in a filter (`requiresOwner(actor)`), or call it from Go
  with `engine.EvaluateSpec(ctx, "name")`.

The body reads every field through the parameter -- `row.archived`,
`row.createdAt` -- never as a bare name. A `trait` is the one
deliberately-unbound row predicate: `trait isActiveRecord = row =>
row.active == true`, with its fields checked against the concrete
concept where it is applied.

## Authoring rules

- The body is one boolean expression after the lambda header. There is
  no truthiness: `row => row.title` is refused, because a string is not
  a condition.
- A spec or trait is applied to its receiver, like a function:
  `isArchivedArtifact(row)`, `requiresOwner(actor)`.
- Side-effect free. Specs cannot call mutation functions or logic
  functions.
- Named for the predicate they express, with no kind prefix
  (`requiresOwner`, `isActiveRecord`). The `spec` / `trait` keyword
  already marks the kind at the declaration.
- The brace body `spec <boundName> <name> { return <expression> }` is
  retired in edition 2026: the parser refuses it and names
  `memqlmigrate --rewrite=expressions`, which writes the lambda form.
  The `@shape("name")` annotation and the legacy
  `func (Spec) name(ctx any) bool { ... }` form are retired too.

## Examples

### Row-spec (SQL pushdown)

<!-- corpus: 2026/examples/specifications/archived-artifacts.memql -->
```memql fragment
use library.concepts.{ artifact }
use library.shapes.{ artifactFull }

@description("Matches archived artifacts")
spec artifact isArchivedArtifact = row => row.archived == true

@description("Archived artifacts filed by system automation")
spec artifact systemArchivedArtifact = row => row.archived == true && row.createdBy == "system:automation"
```

Applied to the row inside a query's `filter` clause. The query binds its
concept in the signature (`query <Concept> <name>`) and pulls cross-file
constructs in via file-top `use` imports; predicates compose with `&&`,
`||`, `!` and parentheses:

<!-- corpus: 2026/examples/specifications/archived-artifacts.memql -->
```memql fragment
query artifact archivedArtifacts {
  args {
    folderId  string  @required
  }
  filter  row => row.folderId == args.folderId && isArchivedArtifact(row)
  shape   artifactFull
}
```

(Both blocks are parts of one file -- the two `use` lines above are its
imports, and the query resolves `isArchivedArtifact` and `artifactFull`
through them.)

(The `;`-AND / `,`-OR separators are retired: the parser refuses them
and names `&&` / `||`. The pre-v1 bare reference `isArchivedArtifact`
is what `memqlmigrate --rewrite=expressions` rewrites to
`isArchivedArtifact(row)`.)

### Context-spec (in-process)

<!-- corpus: 2026/examples/specifications/requires-cluster-owner.memql -->
```memql
use common.shapes.{ actorEnvelope }

@description("Caller must hold the owner role -- the rollback gate (#1876).")
spec actorEnvelope requiresClusterOwner = actor => actor.role == "owner"
```

(The shipped spec of exactly this shape is `requiresOwner`, in
`dsl/deployment/specs.memql`. The example declares its own name because a
shipped construct resolves `requiresOwner` BY BARE NAME: three shipped queries
apply `requiresOwner(actor)` in their filters, and a second declaration makes
that name ambiguous, so those filters stop lowering and the engine refuses the
load. The rule is about bare-name resolution, not about duplicate names in
general -- a spec or trait applied in a shipped filter, or a query or logic a
shipped tool's `@handler` names, is what breaks; a second `provider`, `shape`
or `builtin` of a shipped name loads.)

**A ROLE COMPARISON IS THE ONE THING THIS FORM IS NOW WRONG FOR** (epic
memql#5166). `requiresOwner` survives because `owner` is the cluster-owner tier
rather than a rung anybody authors around; the three specs that named admin and
developer are DELETED, because a slug comparison cannot see a role a cluster
authored for itself -- `role == "admin"` is false for a rank-250 role holding
every principal verb. Write `@requiresRank("<slug>")` for a floor on the ladder
or `@requiresCapability("<verb>", "<resource>")` for a grant; both are validated
at load and enforced at execution.

(The `actorEnvelope` `@actor` shape is the gateway to the auth envelope;
the spec reads its projected key -- `role` -- through its `actor`
parameter. The signature binding is verified at load: it must resolve
to an imported shape or concept.)

A context-spec is applied to `actor` in a filter --
`filter row => requiresOwner(actor) && row.status == "open"` -- or, from
Go, evaluated with `engine.EvaluateSpec(ctx, "name")` against the
request's auth context.

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
  provider-selection record** (`@primary`, repeatable `@fallback`, and
  `@description`; see [policy](attribute-matrix.md#policy) in the
  attribute matrix), consolidated in `dsl/policies/policies.memql` and
  consumed by the AI Router. It is not a predicate surface.
- Caller-context boolean checks (is admin, owns partition,
  permission gates) are authored as **context-specs** in
  `dsl/<namespace>/specs.memql` and applied as `name(actor)` /
  evaluated with `engine.EvaluateSpec`.
- Risk/scope decision logic lives in Go (`component/safety`).
