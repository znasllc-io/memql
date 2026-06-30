---
title: Construct invocation & action syntax -- kind-prefixed calls, named args, the simplified action, and the body rule
audience: internal
status: accepted
area: internal
sinceVersion: 0.9.7
owner: znas
---

# ADR: Construct invocation & action syntax

> **Status: ACCEPTED (owner sign-off 2026-06-29).** This ADR makes how
> constructs are **invoked** and how **actions/bodies** are written uniform,
> explicit, and machine-checkable -- the call-site analog of the explicit `use`
> block. It is the durable record of the design decided in the
> `CONSTRUCT_INVOCATION_SYNTAX_HANDOFF.md` brainstorm; that handoff is a
> temporary file and is deleted once the epic
> ([#2322](https://github.com/znasllc-io/memql/issues/2322), stories
> [#2323](https://github.com/znasllc-io/memql/issues/2323)--[#2330](https://github.com/znasllc-io/memql/issues/2330))
> lands. It builds on, and is coordinated with, the
> [behavioral-constructs ADR](./dsl-behavioral-constructs-adr.md), the
> [spec/shape binding ADR](./spec-shape-binding-adr.md), and the
> [core-builtins & collections ADR](./core-builtins-and-collections-adr.md) --
> all three touch construct bodies, so the migrations rebase cleanly.

## Context

The DSL already made *declared dependencies* explicit at the file top: every
construct a file consumes is named in a `use <namespace>.<kind>s.{ ... }` block,
and the core-builtins ADR drew the ambient-vs-imported line for built-ins. But
the **call site** stayed ambiguous: `createNode({ id: x })`, `existingCluster()`,
`serviceVersion({})` and `spec("requiresOwner")` are all parsed as an
undistinguished `name(args)` function call (`FunctionCallExpr{Name, Args}` in
`component/language/ast/ast.go`). From the call site alone you cannot tell whether
a name is a read (`query`), a write (`mutation`), a pure decision (`logic`), an
external side effect (`capability`), or a language primitive. CQS -- the contract
that a `logic` never writes and a `query` never mutates -- is enforced only later,
semantically, in `component/memql/callgraph`.

This ADR makes the call site carry that information **syntactically**.

## Decision 1 -- kind-prefixed invocation

Invoking any **construct** is `<kind> <name>(<args>)`, where `<kind>` is one of:
`logic`, `query`, `mutation`, `spec`, `action`, `capability`, `builtin`, `tool`,
`automation`.

**Language primitives stay bare** (no kind prefix):

- operators -- `coalesce`, `concat`, `addDuration`, date extractors, ...
- collection / lambda methods -- `where`, `select`, `first`, `count`, ... (per
  the core-builtins ADR's LINQ-style library)
- control flow -- `if`, `for`, `return`

Payoff: the **CQS nature of every call is visible at the call site**
(`query` = read, `mutation` = write, `logic` = pure decision,
`capability` = external side effect). A `mutation` or `capability` keyword inside
a `logic` body is now an **immediate syntactic violation**, not something the
call-graph pass has to infer. The kind keyword is the visual marker of "declared
construct dependency", mirroring ambient-vs-imported from the core-builtins work.
Generation and verification become unambiguous.

```
// before                              // after
existing := existingCluster()          existing := query existingCluster()
createNode({ id: args.node.id })       mutation createNode(id: args.node.id)
serviceVersion({})                     builtin serviceVersion()
spec("requiresOwner")                  spec requiresOwner
```

## Decision 2 -- named args in parens; empty = `()`

Pass args **by name directly in the parens**, with a colon separator, and drop the
object-literal wrapper:

```
mutation createNode(id: args.node.id, nodeType: args.node.type)
```

- empty argument lists are written `()`, **never** `({})`.
- nested values still work where needed (`payload: { ... }`); only the **outer**
  object wrapper disappears.

**Safety audit (required by Story 3, confirmed here).** No engine path
distinguishes a nil args map from an empty one, so dropping the wrapper is
behavior-preserving. At the parser, `FunctionCallExpr.Args` is *always* a non-nil
`map[string]any` (initialized even for `()`), and no Go executor branches on
`args == nil`; consumers test `len(args)` or range the map. `()` and the legacy
`({})`/`( )` therefore reduce to the same empty-map AST. This invariant MUST be
re-confirmed by the implementing session against
`component/language/parser`, `component/memql`, and the step/action executors
before the migration lands, and is part of Story 3's acceptance.

## Decision 3 -- the `action` construct, simplified

The action construct is reduced to **args + a single capability call**:

```
use capabilities.integration.github.{ tagRelease }

@description("Tag a GitHub release for the shipped version.")
action tagRelease {
  args { repo string @required; version string @required }
  capability tagRelease(repo: args.repo, version: args.version)
}
```

Dropped from the legacy action:

| Dropped | Where it goes |
|---|---|
| `@kind` | composites are `automation`s now; primitives need no marker |
| `@sideEffect` | moves **onto the capability** declaration (unspoofable) |
| `@reliability` | machine-managed runtime state, not source |
| `intent` | merged into `@description` (also the planner's retrieval embedding source) |
| `argTemplate` / `$params.X` | replaced by the typed `capability <name>(...)` call |
| `params` | renamed to `args` |

The body is the single `capability <name>(...)` call -- **no `body { }`, no
`return`**. (`action tagRelease` calling `capability tagRelease(...)` is not a real
collision: an action body may call only its one imported capability, so the bare
name resolves unambiguously.)

## Decision 4 -- capabilities are imported; the declaration shape

Capabilities (`fs.*`, `shell.*`, `http.*`, `integration.*`, `mcp.*`) are the most
side-effecting things in the system, so they are **never ambient, always
imported**, at the **verb** level -- consistent with every other `use`:

```
use capabilities.integration.github.{ tagRelease }   // -> call: capability tagRelease(...)
```

A capability is declared like a typed, side-effect-classified `builtin` -- no body
(it is surface-backed). `@sideEffect` lives **here**, where it cannot be spoofed by
an action:

```
@sideEffect("write")
@description("Create a git tag + GitHub release for a version.")
capability integration.github.tagRelease {
  args { repo string @required; tag string @required }
}
```

The capability **catalog** (the full vocabulary) and the **surface-coverage /
capability->surface resolver** are deferred to Story 8.

## Decision 5 -- `body { }` is the procedural marker

`body { }` wraps imperative, multi-statement content. It is **mandatory on
`logic`** (always, even one-liners) and **used by nothing else**. Its presence
therefore *means* "procedural code here" -- a real, enforced signal.

- spec / trait -> bare `return <expr>`
- action -> the single `capability ...(...)` call
- query / mutation -> declarative clauses (`filter` / `shape` / `sort` /
  `insert` / `update`)
- shape / concept -> field declarations
- automation -> `step ...` blocks
- builtin / tool -> Go-backed (`@executor` / `@handler`), no DSL body

A `logic` without `body { }` and any non-`logic` construct *with* `body { }` are
both rejected -- enforced in the parser, the authoring-sandbox cross-reference
pass, and CI.

## Spec & trait predicate forms

`spec <name>` is the **predicate form** -- a kind-prefixed boolean reference
(`spec requiresOwner`, no parens). It **replaces the stringly `spec("name")`
form**, which is rejected with a migration-pointing error. `trait <name>` is the
analogous form for traits.

Bare predicate references inside a query `filter` (e.g. `filter active &&
isActiveRecord`) remain valid: there they compose as row predicates and parse to
`SpecReferenceExpr`, the natural declarative form. The kind-prefixed `spec <name>`
/ `trait <name>` forms are the explicit, unambiguous way to name a predicate where
a stringly call was previously used (notably caller-context specs that were
invoked as `spec("name")`).

## Invocation + body reference (consolidated)

| Construct | Invoke as | `body { }`? |
|---|---|---|
| `concept` | -- (bound in signatures) | no |
| `shape` | -- (bound in signatures) | no (field paths) |
| `spec` | `spec <name>` (predicate, no parens) | no (bare `return`) |
| `trait` | `trait <name>` | no (bare `return`) |
| `query` | `query <name>(args)` | no (clauses) |
| `mutation` | `mutation <name>(args)` | no (clauses) |
| `logic` | `logic <name>(args)` | **yes (mandatory)** |
| `action` | `action <name>(args)` (from an automation) | no (single `capability` call) |
| `capability` | `capability <name>(args)` (only inside an action) | no (declaration: `args` only) |
| `automation` | `automation <name>(args)` / triggered | no (`step` blocks) |
| `builtin` | `builtin <name>(args)` | no (Go-backed) |
| `tool` | invoked by AI / MCP | no (`@handler`) |
| **language** (operators, collection lambdas, control flow) | **bare, no kind prefix** | n/a |

## Consequences

- **Positive.** CQS becomes a syntactic, call-site check; the action surface
  shrinks to its essence; `@sideEffect` is unspoofable; `body { }` is a reliable
  signal; generation/verification is unambiguous; the call site matches the
  explicitness of the `use` block.
- **Cost.** A single tree-wide invocation migration (Story 3) across the authored
  `.memql` tree, plus parser/cross-ref/CI work. Coordinated with the three
  sibling ADRs so the body-touching migrations rebase cleanly.
- **Out of scope (Story 8, deferred).** The full capability catalog and the
  capability->surface coverage/resolver.

## Rollout

Stories under epic [#2322](https://github.com/znasllc-io/memql/issues/2322):
ADR (1) -> parser kind-prefixed invocation + named args + spec predicate (2) ->
capability declaration + imports (5) -> body-rule enforcement (6) -> tree-wide
migration (3) -> simplified action (4) -> reference skeletons + construct matrix
(7). Story 8 (catalog + resolver) is deferred. On completion,
`CONSTRUCT_INVOCATION_SYNTAX_HANDOFF.md` is deleted.
