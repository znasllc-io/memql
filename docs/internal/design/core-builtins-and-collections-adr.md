---
title: Core built-ins boundary -- ambient primitives vs imported core, the collection/lambda library, and temporal visibility
audience: internal
status: accepted
area: internal
sinceVersion: 0.9.6
owner: znas
---

# ADR: Core built-ins boundary -- ambient vs imported `core`, the collection/lambda library, and temporal visibility

> **Status: ACCEPTED (owner sign-off 2026-06-29).** This ADR freezes the
> **built-in surface** of the MemQL DSL into the same opinionated-but-explicit
> contract as the rest of the language: every source of nondeterminism or
> ambient input is a **declared dependency**, while pure language primitives
> stay terse. It also specifies a single LINQ-style **collection/lambda
> library** (replacing the inconsistent legacy surface) and the **temporal-
> access** (`asOf`) visibility rule. It is a decision record, not an
> implementation -- the implementation is tracked by epic
> [#2298](https://github.com/znasllc-io/memql/issues/2298) (stories
> [#2299](https://github.com/znasllc-io/memql/issues/2299)
> ..[#2306](https://github.com/znasllc-io/memql/issues/2306)).
>
> Complement to [`spec-shape-binding-adr.md`](./spec-shape-binding-adr.md)
> (the static-access binding model) and
> [`dsl-behavioral-constructs-adr.md`](./dsl-behavioral-constructs-adr.md)
> (the logic/action/automation call-graph). This ADR is the **expression-level**
> third leg: what the bare functions an author calls inside a body are, and which
> of them are free vs which must be imported.
>
> Date: 2026-06-29. Supersedes `CORE_BUILTINS_LAMBDAS_HANDOFF.md` (deleted on
> epic completion -- see story
> [#2306](https://github.com/znasllc-io/memql/issues/2306)).

## 1. Context

The rest of the DSL is now disciplined about ambient state. Concepts are bound
by signature; specs declare the single data surface they predicate over; shapes
are the only gateway to row/actor data; queries are the only time-travel surface;
`logic` is pure, `action` touches the world, `automation` is the only reactive
construct. Every cross-file dependency travels through a file-top `use` import,
so a reader can see exactly what a construct leans on.

The **built-in surface** -- the bare functions a body calls (`coalesce`,
`concat`, `now`, `timestamp`, `year`, `first`, `filter`, ...) -- never got the
same treatment. It has three problems:

1. **The clock has three redundant spellings.** `now`, `now()`, and
   `timestamp()` all read the wall clock and are in fact the *same* primitive
   (2.1), but a reader has to know all three. The fix is one author spelling --
   the bare reserved `now` -- so the clock dependency is a single, legible
   declared name rather than a scattered call surface.

2. **The collection surface is incoherent.** There are *two* overlapping ways to
   work with a set of rows: grammar builtins (`first` / `last` / and the
   parser's notion of `filter` / `map` / `count`) **and** methods on the engine
   result-set type (`.First()` / `.Nodes()` / `.Len()` / `.Empty()` /
   `.Count()`). They disagree on naming, on whether they take a lambda, and on
   where they are legal. An author has to know both.

3. **Temporal access has no contract surface.** `asOf` time-travel is a query
   clause, but a query that returns *latest-mode* (clock-dependent) data looks
   identical on its contract to one that returns immutable historical data.

The built-in SoT today is `component/language/dslspec/builtins.go` (+
`lexicon.go`), driven into Sense and pinned by `dslspec/drift_test.go` against
`parser.CallableBuiltins` / `parser.CallableAccessors` (#2155). That table is
where the classification this ADR specifies gets recorded.

### 1.1 Grounding (verified against `main`, 2026-06-29)

The handoff's premise list assumed a broader impure set than the tree actually
carries. Ground truth, which this ADR builds on:

| Handoff item | Reality on `main` | Disposition |
|---|---|---|
| `now` reads the clock | `now` is a **reserved engine identifier** (the current RFC3339 timestamp, evaluated in-engine), in the same bucket as `actor` / `partition` / `config` | **stays ambient** (see 2.1) |
| `timestamp` reads the clock (distinct from `now`) | `now`, `now()`, `timestamp()` are the **same** primitive (one AST node, one evaluator); all ~80 `timestamp()` + ~27 `now()` call sites just mean "current time" | **collapse to bare `now`** (see 2.1) |
| `globalVariable` is a builtin | it is a **query** (`use platform.queries.{ globalVariable }`) reading `v1:platform:globalVariable` | **already imported -- no change** |
| `env` is a builtin | `env("...")` is a **provider `auth {}`-block-only** construct, resolved at provider registration; not callable in logic/automation/query bodies | **already constrained -- out of scope** |
| `uuid` / random | **does not exist** in the tree | forward-looking rule only (see 2.1) |

The practical consequence: the language is *already* mostly explicit about
ambient input. The real expression-level migration is narrow -- collapsing the
three redundant clock spellings (`now` / `now()` / `timestamp()`) to the single
ambient reserved `now` -- and the bulk of this epic's work is the
**collection/lambda library** (2.2) and the **temporal contract** (2.3).

## 2. Decision

### 2.1 The purity line for built-ins

**The test.** *Given identical explicit arguments, does the call always return
the same value while observing nothing outside those arguments?*

- **Yes -> ambient.** It is an operator / language primitive. No `use`.
- **No** (reads clock / config / environment / randomness / temporal state)
  **-> imported from `core`.** The `use` block reveals every source of
  nondeterminism.

**The clock is ONE primitive -- it collapses to the ambient reserved `now`
(owner ruling 2026-06-29, revised after implementation grounding).** The
handoff's premise was that `now` and `timestamp()` are two distinct clock reads,
one of which (`timestamp()`) should be imported. The implementation says
otherwise: `now`, `now()`, and `timestamp()` are ONE primitive -- they evaluate
through the same `RuntimeEvaluator.EvaluateTimestamp()` (a fresh `time.Now()`),
with no eval-start-vs-fresh distinction. Given that, gating an alias of `now`
behind an import would be ceremony with no semantic payoff. The clock collapses
to a single spelling and the call-forms are removed outright. Therefore:

- **The author surface is the bare reserved identifier `now`** -- the single
  way to express "current time", in the same bucket as `actor` / `partition` /
  `config`. The parser maps bare `now` directly to the canonical
  `TimestampExprFunc` node (`parseValue` / `parsePrimary`), so it resolves to the
  clock identically in every position: mutation values, comparison RHS (filter
  pushdown via `executor_filter`), and logic / automation operands. Its presence
  in a body *is* the declared dependency; nondeterminism stays legible without an
  import line.
- **The `now()` / `timestamp()` call-forms are RETIRED in the parser**
  (`CallableRetired` -> a migration-hint parse error). Writing them is an error,
  not just a lint finding; the `TestNoClockCallForms` conformance gate (`dsl/`)
  is the belt-and-suspenders tree-wide report. (Internally, the automations
  string-evaluator still resolves the serialized `"timestamp()"` form that
  `compiler/automation_generator` emits for a `TimestampExprFunc` -- that is an
  engine-internal representation, never an author surface.)
- **No `core` import for the clock.** `timestamp()` is not imported because it is
  semantically `now`.
- **`uuid()` / random** (none exist today) are the forward-looking members of
  `core` *if* a genuinely-new nondeterministic primitive is introduced -- one
  that is NOT an alias of an existing reserved identifier.

So `DependencyCore` and the intrinsic `core` namespace remain defined (the
machine-readable scaffold from Story 2), but `CoreBuiltinNames()` is **empty
today**. This is the honest outcome of the grounding in 1.1: the language was
already explicit about ambient input, so the expression-level work reduces to
*collapsing the redundant clock spellings to one reserved name*. The
substance of this epic is the collection library (2.2) and temporal visibility
(2.3).

**Classification of the current `dslspec` table.** Every entry stays in its
`dslspec` category; the `Dependency` axis (Story 2) flags the would-be `core`
members. The complete ruling:

| Built-in | `dslspec` category | Ruling | Why |
|---|---|---|---|
| `coalesce`, `cond`, `concat`, `lower`, `upper`, `trim`, `hash`, `shortId`, `canonicalId`, `toString`, `contains` | expr | **ambient** | pure deterministic operators |
| `addDuration`, `daysBetween`, `subtractTimestamps`, `year`, `quarter`, `month`, `dayOfMonth`, `isAnniversary`, `isFirstDayOfQuarter` | expr | **ambient** | pure given a passed timestamp (no clock read) |
| `memqlVersion` | expr | **ambient** | constant per running binary; deterministic within a deploy (documented exception -- it is build metadata, not runtime ambient input) |
| `first`, `last` | expr | **fold into collection lib** (2.2) | collection ops -- retired as grammar builtins |
| bare `now` | reserved keyword | **ambient (the one clock primitive)** | engine context (with `actor`/`partition`/`config`); the sole author spelling |
| `now()` / `timestamp()` (call forms) | (retired) | **removed -> bare `now`** | aliases of `now`; retired in the parser (`CallableRetired` -> parse error), also caught tree-wide by `TestNoClockCallForms` |
| `uuid` / random | (absent) | **`core` (future)** | the only genuine `core` candidate -- a nondeterministic primitive that is not an alias of a reserved identifier |
| `globalVariable` (query) | -- | **already imported** | a `platform` query, not a builtin |
| `env(...)` (provider auth) | -- | **already constrained** | provider-registration-only; not a body builtin |
| `actor`, `item`, `index`, `event`, `field`, `var`, `step`, `input`, `error` | accessor | **ambient** (unchanged) | bound-context accessors, resolved deterministically within their scope |
| `ai`, `node`, `children`, `parent`, `payload`, `similar`, `embed`, `systemVar`, `secret`, `systemSecret` | registry | **out of scope** | runtime-registry builtins; their nondeterminism is already mediated by the integration/registry layer, not the expression grammar. Revisited only if one is ever lifted into the ambient grammar. |

### 2.1.1 The `core` bundle -- defined, empty today

A `core` import would be the canonical "this body reads a nondeterministic
primitive" signal. The machine-readable scaffold exists -- `BuiltinDependency` /
`DependencyCore` on `dslspec.Builtin`, and `dslspec.CoreBuiltinNames()` (Story
[#2300](https://github.com/znasllc-io/memql/issues/2300)) -- but
**`CoreBuiltinNames()` is empty today**, because the only candidate (the clock)
collapsed to the ambient reserved `now` (2.1) and the other handoff candidates
(`globalVariable`, `env`) were already a query / provider-scoped (1.1).

When the `core` namespace gains its first real member (`uuid` / random), it is
defined as an **intrinsic (loader-level) namespace, not a `dsl/core/*.memql`
construct file** -- a grammar-level primitive evaluated in-engine has no
integration executor a DSL `builtin` declaration could point at (a
`builtin uuid { @executor(...) }` would fail load with "unknown executor"). At
that point the loader recognises `core` as a virtual import namespace whose
legal members are `CoreBuiltinNames()`, the name is removed from the ambient
resolver, and a body that calls it without the import is a load error. None of
that is wired now because there is nothing to wire -- the scaffold is in place
so adding the first member is a small, well-scoped change rather than a redesign.

### 2.2 The collection/lambda library (ambient language, pure bodies)

A single LINQ-style, method-chained library with arrow lambdas replaces the
legacy surface:

```memql
nodes.where(n => n.status == "active").orderByDesc(n => n.createdAt).first()
```

Rules:

- **Ambient** -- they are built-in collection methods and pass the purity test;
  no `use`.
- **Lambda bodies must be pure** -- no mutations, no actions, no triggers. Any
  *impure* call inside a lambda still obeys 2.1: a future `core` primitive
  (`uuid` / random) used inside a lambda is imported like anywhere else. Ambient
  context (`now`, `actor`) is readable; impurity stays visible without bloating
  the `use` block with `where` / `first`.
- **Replaces, not adds.** The grammar builtins `first` / `last` / `filter` /
  `map` / `count` and the engine result-set methods `.First()` / `.Nodes()` /
  `.Len()` / `.Empty()` / `.Count()` are retired (story
  [#2303](https://github.com/znasllc-io/memql/issues/2303)).
- **Scope.** Legal in **`logic`** and **automation `forEach`**. **Rejected in
  `spec`** (a spec is an atomic boolean over one bound thing -- no collection).
  **Rejected in query `filter`** (those push down to SQL; lambdas are in-memory
  post-fetch).

The method surface:

| Expression | Lambda | Returns | logic | `forEach` | spec | query filter |
|---|---|---|---|---|---|---|
| `where(x => bool)` | yes | collection | yes | yes | no | no |
| `select(x => v)` | yes | collection | yes | yes | no | no |
| `first(x => bool?)` / `last()` / `single(x => bool?)` | opt | item | yes | yes | no | -- |
| `any(x => bool?)` / `all(x => bool)` | opt/yes | bool | yes | yes | no | -- |
| `count(x => bool?)` | opt | int | yes | -- | no | -- |
| `sum` / `min` / `max` / `avg(x => n)` | yes | number | yes | -- | no | -- |
| `orderBy` / `orderByDesc(x => k)` | yes | collection | yes | yes | no | -- |
| `take(n)` / `skip(n)` / `distinct(x => k?)` | opt | collection | yes | yes | no | -- |
| `groupBy(x => k)` / `reduce(seed, (acc,x) => v)` | yes | groups/any | yes | -- | no | -- |
| `empty()` / `contains(v)` | no | bool | yes | yes | no | -- |

**Guardrail (story [#2304](https://github.com/znasllc-io/memql/issues/2304)).** A
lint warns when a `where()` / etc. runs over an **unfiltered full-concept fetch**
-- that should be a query `filter` / SQL pushdown, not an in-memory scan.

### 2.3 Temporal access (`asOf`) stays a query clause

- `asOf latest` / `asOf <ts>` is a **query-only clause** (alongside `filter` /
  `shape` / `sort` / `paginate`); it compiles to time-travel against the graph.
  It is **rejected** in logic / automation / spec bodies.
- It is **not** an importable `core` construct -- `latest` is a temporal
  *coordinate*, not a callable. The temporal dependency is declared **through the
  query** a body imports (queries are the CQRS read side; logic/automation never
  time-travel directly).
- For `now`-parity visibility: **`latest`-mode is surfaced on the query's own
  contract** (signature / annotation) so consumers see the result is
  time-dependent (story
  [#2305](https://github.com/znasllc-io/memql/issues/2305)).
- `asOf <explicit timestamp>` is **deterministic** (historical state is
  immutable) and needs no marker.

## 3. The construct matrix (the whole model, consolidated)

This synthesizes the behavioral-constructs ADR, the spec/shape redesign, and the
decisions above. Published as durable reference by story
[#2306](https://github.com/znasllc-io/memql/issues/2306).

| Construct | Purpose | Signature / binding | Returns | May call / compose | Trigger? | Graph | Pure / side-effects | `use`-imported |
|---|---|---|---|---|---|---|---|---|
| `concept` | Data type (memory-node schema) | `concept <name>` + payload fields | -- (declaration) | -- | no | defines rows | declaration | yes (`...concepts`) |
| `shape` | Projection template; the **only** gateway to row/actor data | `shape <Concept> <name>` / `shape <name>`; `@row`/`@actor` | flattened projected keys | field paths (payload bare; `row.`/`actor.` gated) | no | reads (projection) | pure | yes (`...shapes`) |
| `spec` | Atomic boolean predicate | `spec <shape\|concept> <name>` | `return <bool>` | bound shape/concept fields (bare); ambient operators | no | concept/`@row`->SQL; `@actor`->in-proc | pure | yes (`...specs`) |
| `trait` | Concept-agnostic row predicate (deliberately unbound) | `trait <name>` | bool | `payload.X`; ambient operators | no | row predicate | pure | yes (`...traits`) |
| `query` | Read the graph (CQRS read) | `query <Concept> <name>` | rows | `filter` (+specs/traits), `shape`, `sort`, `paginate`, `asOf` | no | reads (SQL, time-travel) | pure read | yes (`...queries`) |
| `mutation` | **One** atomic graph write (CQRS write) | `mutate <Concept> <name>` | written row | one write; read-modify-write own aggregate; ambient builtins; bare `now` | no | writes one aggregate | side-effect: graph write | yes (`...mutations`) |
| `logic` | Pure decision + composition | `logic <name>` | a value | queries, logic, ambient ops, **collection lambdas**, `use core.{...}` | no | via queries only | **pure** (no writes/actions/triggers) | yes (`...logic`) |
| `action` | **One external** capability on a surface | `action <name>` `@kind` `@sideEffect` + `capability` | capability result | one capability / `integration.*` on a surface | no | never (external only) | side-effect: external | yes (library) |
| `automation` | The **only** reactive + composing construct | `automation <name>` `@trigger(...)` / terse `=> logic` | -- | logic, query, mutation, action, sub-automation; `switch`/`forEach`(+collection lambdas)/`parallel` | **YES (only here)** | via its steps | orchestration | yes (`...automations`) |
| `builtin` | Go-backed capability | `builtin <name>` `@executor("integration.*")` | value | (Go impl) | no | depends | read-only->query/logic; side-effecting->**action-only** | yes (`common.builtins` / `core`) |
| `tool` | External (AI/MCP) invocation surface | `tool <name>` `@handler(query\|function\|webhook)` `@mcp` | wrapped result | wraps a query / logic / webhook | no (invoked) | via wrapped | governed surface | yes (`...tools`) |
| `prompt` | AI prompt schema / template | `prompt <name>` `@templateFile` `@model` | prompt | -- | no | -- | declaration | yes (`...prompts`) |
| `policy` | AI provider-selection only (#984) | `policy <name>` | selection | provider rules | no | -- | pure decision | yes (`...policies`) |
| `provider` | AI/model provider definition | `provider <name>` `@model` | -- | `env(...)` in `auth {}` | no | -- | declaration | yes (`...providers`) |

**Not constructs (the language itself, no `use`):** pure operators (`coalesce`,
`concat`, `addDuration`, the date extractors), the collection/lambda methods
(`where` / `first` / ...), control flow (`if` / `for` / `return`), and the
reserved engine identifiers (`now`, `actor`, `partition`, `config`, `trace`).
**Ambient-but-imported from `core` (future):** `uuid` / random -- a
nondeterministic primitive that is not an alias of a reserved identifier. Empty
today.

## 4. Worked examples (target syntax)

The current time is the bare reserved `now` -- one spelling, no import, no call
parens:

```memql
// Current time = bare `now` (ambient). The now() / timestamp() call-forms are
// banned in authored files by TestNoClockCallForms.
logic logicExpiryFromNow {
  args { ttlHours int @required }
  body { return addDuration(now, concat("PT", toString(args.ttlHours), "H")) }
}

logic logicIsStale {
  args { lastSeen string @required, windowMinutes int @required }
  body {
    return subtractTimestamps(now, args.lastSeen) >
           addDuration("PT0S", concat("PT", toString(args.windowMinutes), "M"))
  }
}

// FUTURE (when a real core member lands): a genuinely-new nondeterministic
// primitive that is NOT an alias of a reserved name is imported, e.g.
//   use core.builtins.{ uuid }
//   logic logicNewId { body { return uuid() } }
```

Collection lambdas in logic + `forEach`, rejected in a spec:

```memql
logic logicActiveAdminCount {
  args { members []object @required }
  body { return args.members.where(m => m.role == "admin" && m.active).count() }
}

// REJECTED at load: collection op in a spec.
spec participant specIsActiveAdmin {
  return participants.where(p => p.active).any()   // load error: no collections in specs
}
```

Temporal visibility on a query contract:

```memql
@latestMode    // surfaced on the contract: this query is time-dependent
query node queryLiveNodes {
  asOf latest
  filter  type == "node"
  shape   nodeCard
}

query node queryNodesAt {       // asOf <ts> -> deterministic, no marker
  args { at string @required }
  asOf args.at
  shape nodeCard
}
```

## 5. Migration plan (epic #2298)

| Story | What | Size |
|---|---|---|
| 1 (#2299) | This ADR | docs only |
| 2 (#2300) | Tag `dslspec` entries ambient/import; stand up `dsl/core/` + `core.builtins` | small |
| 3 (#2301) | Collapse the clock to bare `now`: migrate ~80 `timestamp()` + ~27 `now()` -> bare `now` (behavior-preserving); parse bare `now` directly to `TimestampExprFunc` (`parseValue` / `parsePrimary`) so it resolves to the clock in filter RHS + logic/automation operands (+ `ast_converter` case for the validation path); RETIRE the `now()` / `timestamp()` call-forms in the parser (`CallableRetired`); `TestNoClockCallForms` gate; remove the `now` / `timestamp` dslspec accessor entries; revert `timestamp`'s Story-2 `core` flag (bundle now empty). | medium |
| 4 (#2302) | Implement the collection/lambda library + arrow lambdas; pure-body + scope enforcement | large |
| 5 (#2303) | Retire `.First()`/`.Nodes()`/`.Len()`/`.Empty()`/`.Count()` + `first`/`last`/`filter`/`map`/`count` grammar builtins; migrate | large |
| 6 (#2304) | In-memory-vs-SQL guardrail lint | small |
| 7 (#2305) | `asOf` query-only enforcement + `latest`-mode on the contract | medium |
| 8 (#2306) | Publish the matrix to durable reference; update `_reference` skeletons; delete the handoff file | small |

Each story runs `make test` / `make lint` / the DSL load + drift gates before
closing. If implementation forces a contract change, this ADR is updated in the
same PR and the owner flagged.

## 6. Consequences

- **Positive.** The clock is one legible reserved name (`now`) instead of three
  spellings. One coherent collection surface instead of two. Temporal
  time-dependence is legible on a query's contract. The built-in surface now
  obeys the same explicit-dependency thesis as concepts/specs/shapes/queries.
- **Cost.** A medium, behavior-preserving call-site migration (the clock
  collapse), a large new library plus the retirement of the legacy collection
  surface, and the conformance gate.
- **`core` is empty today, by grounding not omission.** `globalVariable` is
  already a query, `env` is already provider-scoped, and the clock collapsed to
  ambient `now` -- so there is no current `core` member. The scaffold
  (`DependencyCore`, `CoreBuiltinNames()`, the intrinsic-namespace plan) is in
  place so the first genuine nondeterministic primitive (`uuid` / random) is a
  small change.
- **Pre-release, no shims.** Per the repo's no-backwards-compat rule, the legacy
  collection surface and the `now()` / `timestamp()` author-surface call forms are
  retired, not deprecated; consumers
  are migrated in the same change.
