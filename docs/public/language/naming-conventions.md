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
- Logic: `logic*` (for example `logicAutoJoinSI`)
- Automations: verb-first names, no prefix (for example
  `bootstrapSession`, `autoJoinSI`)
- Shapes: `<concept><Projection>`, no kind prefix (for example
  `participantFull`, `spaceCard`)

> **Accuracy note (#2853).** The `query*` / `mutation*` / `logic*` prefixes
> above are not what the shipped tree does: **no** construct carries them
> (0 of 197 queries, 0 of 213 mutations, 0 of 35 logic functions -- the
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

spec space statusIsActive {
  return status == "active"
}

trait isActiveRecord {
  return active == true
}
```

Constructs live in one consolidated file per kind per namespace
(`dsl/<namespace>/queries.memql`, `dsl/<namespace>/mutations.memql`,
...), so file names never carry an individual construct's name.

One asymmetry to know: automation step bodies reference logic
constructs by the bare, un-prefixed name -- `step run { logic
autoJoinSI { event: event } }` resolves to `logic logicAutoJoinSI`
through the file-top `use cognition.logic.{ logicAutoJoinSI }`
import.

## Why This Matters

- Improves readability in automations with many calls.
- Makes CQS intent visible before compile-time checks.
- Reduces naming collisions when files are grouped by domain.

## Enforcement

Compiler lint emits non-fatal warnings for naming mismatches:

- `naming.query-prefix`
- `naming.mutation-prefix`
- `naming.spec-prefix`

Warnings can be promoted to errors using strict compile settings.
