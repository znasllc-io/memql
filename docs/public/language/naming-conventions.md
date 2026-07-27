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

## Construct Names Carry No Kind Prefix

**Decided (memql#2853): constructs are named for what they do, never for
what kind they are.** No `query*`, `mutation*`, `logic*`, `spec*`, `trait*`
or `seed*` prefix.

- Queries: what they return -- `activeHumanParticipants`,
  `audioOverridesForSpace`, `userById`
- Mutations: the verb -- `addAgentToSpace`, `createGreetingUtterance`,
  `archiveUser`. (Note the declaration keyword is `mutate`, while the
  invocation verb inside a logic body is `mutation` -- the parser's tests
  call that pair "the canonical footgun distance". Neither belongs in the
  name.)
- Logic: the verb -- `bootstrapSession`, `generateResponse`
- Specs and traits: the predicate they express -- `isHumanParticipant`,
  `isActiveRecord`, `requiresOwnerOrAdmin`
- Automations: verb-first -- `bootstrapSession`, `autoJoinSI`
- Shapes: `<concept><Projection>` -- `participantFull`, `spaceCard`
- Seeds: the thing being seeded -- `sofia`, `plannerAgent`

### Why

The keyword already marks the kind at the declaration -- `query user
userById { ... }` -- so a prefix restates in the name what the grammar
states one token earlier. Call sites read better without it:

```memql
filter  spaceId == args.spaceId && isActiveRecord
step decide { logic bootstrapSession ( event: event ) }
```

This is also what the codebase has always done. Measured across the shipped
tree (excluding the non-embedded `_reference/` skeletons), **0 of 506
construct DECLARATIONS carry a kind prefix** -- 0/199 queries, 0/213
mutations, 0/33 logic, 0/30 traits, 0/25 seeds, 0/6 specs. The prefix rule
this document used to state was never followed by anything.

Declarations, precisely: a handful of *call sites* still name prefixed
constructs that do not exist in this tree (`dsl/cognition/logic.memql` calls
`mutation mutationCreateCanvasState(...)`, which is supplied by a product
bundle at runtime, not declared here). Those are out of scope for the gate,
which walks the embedded tree.

> **History.** This page previously mandated `query*` / `mutation*` /
> `logic*`. #2806 corrected the spec/trait entries, which were worse than
> aspirational -- their examples named constructs that do not exist
> (`traitIsActiveRecord`, `specIsHumanParticipant`), so a reader who copied
> them wrote a `use` import that silently fails to resolve until the query
> first runs (#2783). #2853 measured the remaining three, found the same
> 0% adherence, and the owner ruled to abandon the prefix rather than
> rename 445 constructs plus every call site and the generated SDK
> surface.

Examples:

```memql
use identity.concepts.{ user }
use identity.shapes.{ userFull }

query user userById {
  args {
    userId  string  @required
  }
  filter  row.id == args.userId
  shape   userFull
}

mutate user archiveUser {
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


## Enforcement

The no-prefix rule IS gated: `TestNoKindPrefixInConstructNames`
(`dsl/naming_conventions_test.go`) fails if any construct is named with
its own kind as a prefix. That is what stops this page and the tree
drifting apart again -- the previous rule was documented for months while
nothing in the corpus followed it, and nothing noticed.

The old *opposite* lint (`naming.query-prefix` / `naming.mutation-prefix`
/ `naming.spec-prefix`), which required the prefix, was **retired** in the
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
