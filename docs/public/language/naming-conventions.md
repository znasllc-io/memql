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

- Queries: what they return -- `libraryArtifactsByKind`,
  `workRunsForGoal`, `userById`
- Mutations: the verb -- `createLibraryFolder`, `moveArtifactToFolder`,
  `archiveUser`. (The declaration keyword and the call kind are one word,
  `mutation`, and it does not belong in the name either.)
- Logic: the verb -- `indexArtifact`, `generateResponse`
- Specs and traits: the predicate they express -- `isNotArchived`,
  `isActiveRecord`, `requiresOwner`
- Automations: verb-first -- `indexArtifact`, `releaseWorkspaceOnRunTerminal`
- Shapes: `<concept><Projection>` -- `artifactFull`, `folderCard`
- Seeds: the thing being seeded -- `sofia`, `plannerAgent`

### Why

The keyword already marks the kind at the declaration -- `query user
userById { ... }` -- so a prefix restates in the name what the grammar
states one token earlier. Call sites read better without it -- a query's
filter applying a trait:

<!-- corpus: 2026/examples/naming-conventions/artifacts-by-folder.memql -->
```memql fragment
  filter  row => row.folderId == args.folderId && isActiveRecord(row)
```

and an automation's statement calling a logic:

<!-- corpus: 2026/examples/naming-conventions/index-artifact.memql -->
```memql fragment
  decide := logic indexArtifact(event: event)
```

This is also what the codebase has always done. Measured across the shipped
tree (excluding the non-embedded `_reference/` skeletons), **0 of 1261
construct DECLARATIONS carry a kind prefix**, across all 17 declaration
keywords. The six kinds the retired rule actually named account for 774 of
those: 0/245 queries, 0/267 mutations, 0/33 logic, 0/33 traits, 0/186 seeds,
0/10 specs. The prefix rule this document used to state was never followed by
anything. (These counts drift as the tree grows; re-run `go test
./test/dslconformance/ -run TestNoKindPrefixInConstructNames -v` for the
current numbers -- the gate's own log line is the source of truth, not this
paragraph.)

Those counts come from the gate itself, which reads the lexer's token stream.
Two earlier drafts undercounted, and both were caught by review rather than by
the gate: a regex version reported 506 / 25 seeds (blind to the 160 seeds whose
names contain `-`, a legal identifier character), and the first token version
reported 1081 (blind to the 10 terse `automation X @trigger(...) => logic X`
declarations, which carried no brace at all -- a form since retired,
memql#5370). The measurement and the enforcement
are the same code path, which is the only way the number stays true.

Declarations, precisely: a call site may still name a prefixed construct that
does not exist in this tree, because a product bundle supplies it at runtime
rather than declaring it here. Those are out of scope for the gate, which walks
the embedded tree.

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

<!-- corpus: 2026/examples/naming-conventions/named-for-what-they-do.memql -->
```memql
use identity.concepts.{ user }
use identity.shapes.{ userFull }
use library.concepts.{ folder }

query user userByPrimaryEmail {
  args {
    primaryEmail  string  @required
  }
  filter  row => row.primaryEmail == args.primaryEmail
  shape   userFull
}

mutation user archiveUser {
  args {
    userId  string  @required
  }
  update {
    id:      args.userId
    active:  false
  }
}

spec folder folderIsArchived = row => row.archived == true

trait isRetired = row => row.retired == true
```

Each construct in that block is one the engine loads: every cross-file name
it uses is imported at the top, every field it writes or projects is one its
concept declares, and no name it declares is one a SHIPPED construct resolves
by bare name. That last one is the trap: declaring `isActiveRecord` again
breaks the shipped filters that apply it, and declaring `userById` again breaks
the handler of the tool the engine generates for it -- the duplicate's own file
loads, and the tree around it stops.

Constructs live in one consolidated file per kind per namespace
(`dsl/<namespace>/queries.memql`, `dsl/<namespace>/mutations.memql`,
...), so file names never carry an individual construct's name.

An automation's statement calls a logic construct by its declared name --
`decide := logic indexArtifact(event: event)` names the logic itself. A
construct declared in the same namespace needs no import; one declared in
another is brought into scope by a file-top `use <ns>.logic.{ indexArtifact }`.
There is no prefixed/bare split between the two.


## Enforcement

The no-prefix rule IS gated: `TestNoKindPrefixInConstructNames`
(`test/dslconformance/naming_conventions_test.go`) fails if any construct is named with
its own kind as a prefix. That is what stops this page and the tree
drifting apart again -- the previous rule was documented for months while
nothing in the corpus followed it, and nothing noticed.

It covers **all 17 declaration keywords**, and both halves are derived
from the parser: thirteen from `parser.TopLevelDeclKeywords` (its dispatch
table) and the four struct-form keywords -- `query` / `mutation`, which
the rewriter lowers, and `logic` / `automation`, which the statement
parser reads as written -- from `parser.StructFormKeywords`. Adding a
construct kind to either list extends the gate automatically.

`TestDeclKeywordSetMatchesTheParser` then pins the resulting set by name,
so a kind that is added, removed, or *renamed* fails the test and forces a
deliberate update here. A rename is the case a count alone misses --
`mutation` became `mutate` in #2036, and `mutate` became `mutation` again
in memql#5370 (D13), without moving any total.

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
leading whitespace, it covered 6 keywords rather than 17, and it counted
braces inside string literals and comments as real syntax.

The token rewrite then turned out to be narrower than the grammar too,
in three further ways: it anchored on `{` and so missed the 10 **terse
automations** (`automation X @trigger(...) => logic X`, since retired),
which had no body; it forbade only `mutation` on `mutate` declarations and
so let `mutateArchiveUser` through; and its word-boundary test was ASCII and
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
