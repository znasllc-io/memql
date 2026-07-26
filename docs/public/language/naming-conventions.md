---
title: MemQL Naming Conventions
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Naming Conventions

> Last Updated: June 11, 2026

## Construct Prefixes

Use kind-specific prefixes so intent is obvious at call sites and in diffs.

- Queries: `query*` (for example `queryActiveSpaces`)
- Mutations: `mutation*` (for example `mutationCreateSpace`)
- Specs and traits: named for the predicate they express, no kind prefix
  (for example `isHumanParticipant`, `isActiveRecord`, `statusIsActive`).
  The `spec` / `trait` keyword already marks the kind at the declaration,
  and the call site reads better without it:
  `filter spaceId==args.spaceId && isActiveRecord`.
- Logic: `logic*` (see the accuracy note below; the shipped names are
  bare, e.g. `bootstrapSession`)
- Automations: verb-first names, no prefix (for example
  `bootstrapSession`, `autoJoinSI`)
- Shapes: `<concept><Projection>`, no kind prefix (for example
  `participantFull`, `spaceCard`)

> **Accuracy note (#2853).** The `query*` / `mutation*` / `logic*` prefixes
> above are not what the shipped tree does: **no** construct carries them
> (0 of 197 queries, 0 of 213 mutations, 0 of 33 logic functions in the
> shipped tree, excluding the non-embedded `_reference/` skeleton -- the
> real names are `activeHumanParticipants`, `addAgentToSpace`,
> `bootstrapSession`). The spec / trait entry was corrected in #2806
> because its examples named constructs that do not exist, so a reader who
> copied them wrote an import that does not resolve. The remaining three
> are a live convention question -- abandon the prefix, or rename the
> corpus and gate it -- and are tracked separately rather than decided
> here.

Examples:

```memql
use identity.concepts.{ user }
use identity.shapes.{ userFull }

query user queryUserById {
  args {
    userId  string  @required
  }
  filter  row.id == args.userId
  shape   userFull
}

mutation user mutationArchiveUser {
  args {
    userId  string  @required
  }
  update {
    id:     args.userId
    status: "archived"
  }
}

spec space spaceIsPublic {
  return visibility == "public"
}

trait isActiveRecord {
  return active == true
}
```

Constructs live in one consolidated file per kind per namespace
(`dsl/<namespace>/queries.memql`, `dsl/<namespace>/mutations.memql`,
...), so file names never carry an individual construct's name.

An automation step references a logic construct by the same name the
file-top import names -- `step decide { logic bootstrapSession ( event )
}` resolves through `use cognition.logic.{ bootstrapSession }`. There is
no prefixed/bare split between the two.

## Why This Matters

- Improves readability in automations with many calls.
- Makes CQS intent visible before compile-time checks.
- Reduces naming collisions when files are grouped by domain.

## Enforcement

None, by design. The naming-prefix lint (`naming.query-prefix` /
`naming.mutation-prefix` / `naming.spec-prefix`) was **retired** in the
grammar redesign (epic #2031, C2/#2042): references resolve structurally
by slot keyword plus enclosing concept, so a construct's name is free.
`component/language/compiler/linter.go` records this in its own header,
and `TestCompileSource_NoNamingWarnings` fails the build if any
`naming.*` warning is emitted.

What replaced it is a structural check rather than a spelling one: the
dependency-tree validator (C3/#2043,
`component/memql.ValidateDependencyTree`) resolves every reference at
load, so a name that does not exist fails then instead of being warned
about.
