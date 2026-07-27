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
tree (excluding the non-embedded `_reference/` skeletons), **0 of 1091
construct DECLARATIONS carry a kind prefix**, across all 16 declaration
keywords. The six kinds the retired rule actually named account for 666 of
those: 0/199 queries, 0/213 mutations, 0/33 logic, 0/30 traits, 0/185 seeds,
0/6 specs. The prefix rule this document used to state was never followed by
anything.

Those counts come from the gate itself, which reads the lexer's token stream.
Two earlier drafts undercounted, and both were caught by review rather than by
the gate: a regex version reported 506 / 25 seeds (blind to the 160 seeds whose
names contain `-`, a legal identifier character), and the first token version
reported 1081 (blind to the 10 terse `automation X @trigger(...) => logic X`
declarations, which carry no brace at all). The measurement and the enforcement
are the same code path, which is the only way the number stays true.

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

It covers **all 16 declaration keywords**, and both halves are derived
from the parser: twelve from `parser.TopLevelDeclKeywords` (its dispatch
table) and the four struct forms the rewriter lowers -- `query` /
`mutate` / `logic` / `automation` -- from `parser.StructFormKeywords`
(built from `structFormSteps`, the rewrite chain itself). Adding a
construct kind to either list extends the gate automatically.

`TestDeclKeywordSetMatchesTheParser` then pins the resulting set by name,
so a kind that is added, removed, or *renamed* fails the test and forces a
deliberate update here. A rename is the case a count alone misses --
`mutation` became `mutate` in #2036 without moving any total.

> **This part was wrong in the first published version**, which pinned the
> four rewriter forms by hand under the claim that "the parser exports no
> list for them." `parser.StructFormKeywords` had been exported the whole
> time. Worse, the drift guard asserted
> `len(declKeywordPrefixes) == len(TopLevelDeclKeywords) + len(rewriterLoweredKeywords)`
> -- a tautology, because the map is built by iterating exactly those two
> slices, so it could never fail on the hand-maintained half it existed to
> guard. Round-3 review caught both. The lesson is the one this page keeps
> re-learning: an assertion about the grammar needs a probe, not a reading.

It finds declarations by walking the **lexer's token stream**, not by
matching a regex over the source. That is deliberate: the first version
used a regex and was narrower than the grammar in four ways at once --
it could not see names containing `-` (legal, and used by 160 of the 185
seeds), it required the keyword at column 0 though the parser accepts
leading whitespace, it covered 6 keywords rather than 16, and it counted
braces inside string literals and comments as real syntax.

The token rewrite then turned out to be narrower than the grammar too,
in three further ways: it anchored on `{` and so missed the 10 **terse
automations** (`automation X @trigger(...) => logic X`), which have no
body; it forbade only `mutation` on `mutate` declarations and so let
`mutateArchiveUser` through; and its word-boundary test was ASCII and
camelCase-only, so a kebab-case prefix and a non-ASCII uppercase letter
both evaded.

The lesson is the durable part: a gate that re-implements the grammar
drifts from it, and even one that reuses the lexer can still assume more
structure than the language requires. `TestNoKindPrefixGateIsLive` pins
that the gate actually fires on all seven shapes, so "0 prefixed" can
never be a synonym for "scanned nothing" -- read it before changing the
scan.

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
