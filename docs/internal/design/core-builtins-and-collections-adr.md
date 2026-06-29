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

1. **Nondeterminism is invisible.** `now` and `timestamp()` read the wall clock,
   but they are ambient grammar accessors: nothing in a construct's `use` block
   reveals that it depends on the clock. This is exactly the ambient-input
   problem the import model solved everywhere else.

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
| `now` reads the clock | `now` is a **reserved engine identifier** (eval-start RFC3339 stamp, captured once per eval), in the same bucket as `actor` / `partition` / `config` | **stays ambient** (see 2.1) |
| `timestamp` reads the clock | all ~80 `timestamp(...)` call sites are **no-arg** `timestamp()` (a fresh wall-clock read); zero use the string-arg parse form | **imported `core`** (see 2.1) |
| `globalVariable` is a builtin | it is a **query** (`use platform.queries.{ globalVariable }`) reading `v1:platform:globalVariable` | **already imported -- no change** |
| `env` is a builtin | `env("...")` is a **provider `auth {}`-block-only** construct, resolved at provider registration; not callable in logic/automation/query bodies | **already constrained -- out of scope** |
| `uuid` / random | **does not exist** in the tree | forward-looking rule only (see 2.1) |

The practical consequence: the language is *already* mostly explicit about
ambient input. The real expression-level migration is narrow (the `timestamp()`
clock read), and the bulk of this epic's work is the **collection/lambda
library** (2.2) and the **temporal contract** (2.3).

## 2. Decision

### 2.1 The purity line for built-ins

**The test.** *Given identical explicit arguments, does the call always return
the same value while observing nothing outside those arguments?*

- **Yes -> ambient.** It is an operator / language primitive. No `use`.
- **No** (reads clock / config / environment / randomness / temporal state)
  **-> imported from `core`.** The `use` block reveals every source of
  nondeterminism.

**One refinement on the clock (owner ruling 2026-06-29).** The eval-start
timestamp is *engine-provided context*, not a free clock read. It is captured
once per evaluation and is stable for the whole call -- exactly like `actor`,
`partition`, and `config`, which are also ambient because they are declared
engine context surfaced as reserved names. Therefore:

- **bare `now`** -- the reserved eval-start identifier -- **stays ambient.**
  `createdAt: now` in a mutation is unchanged. This keeps the single most common
  idiom terse and is consistent with the other reserved engine names.
- The **call form `now()`** is redundant with bare `now` and is **retired** in
  favour of it (migrated, not imported).
- **`timestamp()`** is a genuine *fresh wall-clock read at the point of call*
  (distinct from the eval-start stamp), so it is **imported from `core`**. A body
  that reads the live clock declares it: `use core.{ timestamp }`.
- **`uuid()` / random** (none exist today) are **`core`** when introduced.

**Classification of the current `dslspec` table.** Every entry stays in its
`dslspec` category; the new axis is an ambient-vs-`core` flag. The complete
ruling:

| Built-in | `dslspec` category | Ruling | Why |
|---|---|---|---|
| `coalesce`, `cond`, `concat`, `lower`, `upper`, `trim`, `hash`, `shortId`, `canonicalId`, `toString`, `contains` | expr | **ambient** | pure deterministic operators |
| `addDuration`, `daysBetween`, `subtractTimestamps`, `year`, `quarter`, `month`, `dayOfMonth`, `isAnniversary`, `isFirstDayOfQuarter` | expr | **ambient** | pure given a passed timestamp (no clock read) |
| `memqlVersion` | expr | **ambient** | constant per running binary; deterministic within a deploy (documented exception -- it is build metadata, not runtime ambient input) |
| `first`, `last` | expr | **fold into collection lib** (2.2) | collection ops -- retired as grammar builtins |
| bare `now` | reserved keyword | **ambient** | eval-start engine context (with `actor`/`partition`/`config`) |
| `now()` (call form) | accessor | **retire -> bare `now`** | redundant with the reserved identifier |
| `timestamp()` | accessor | **import `core`** | fresh wall-clock read -- nondeterministic |
| `uuid` / random | (absent) | **import `core`** (future) | nondeterminism |
| `globalVariable` (query) | -- | **already imported** | a `platform` query, not a builtin |
| `env(...)` (provider auth) | -- | **already constrained** | provider-registration-only; not a body builtin |
| `actor`, `item`, `index`, `event`, `field`, `var`, `step`, `input`, `error` | accessor | **ambient** (unchanged) | bound-context accessors, resolved deterministically within their scope |
| `ai`, `node`, `children`, `parent`, `payload`, `similar`, `embed`, `systemVar`, `secret`, `systemSecret` | registry | **out of scope** | runtime-registry builtins; their nondeterminism is already mediated by the integration/registry layer, not the expression grammar. Revisited only if one is ever lifted into the ambient grammar. |

### 2.1.1 The `core` bundle

`use core.{ ... }` is the canonical "this body is nondeterministic" signal, and
a reader scanning imports should see `core` as its own line, not buried among
`common` helpers. The author-facing syntax is therefore `use core.{ timestamp }`,
exactly parallel to `use common.builtins.{ ... }`.

**The `core` namespace is intrinsic (loader-level), not a `dsl/core/*.memql`
construct file** (decided in story
[#2300](https://github.com/znasllc-io/memql/issues/2300) once the mechanism met
the grammar). Its members are **engine intrinsics**: `timestamp()` is parsed as
a grammar accessor (`parser.parseTimestampAccessor`) and evaluated in-engine
(`component/memql/mutation_templates.go`, `case "now", "timestamp"`). It has no
integration executor a DSL `builtin` declaration could point at -- a
`builtin timestamp { @executor(...) }` would fail load with "unknown executor".
So the bundle is defined in Go, not in a `.memql` file:

- **Membership** is `dslspec.CoreBuiltinNames()` -- the set of `dslspec.Builtins`
  entries flagged `DependencyCore` (the machine-readable classification added in
  Story 2). Initial membership: **`timestamp`** (the no-arg wall-clock read).
  Deliberately thin -- the tree is already disciplined elsewhere -- but it
  establishes the pattern and is the home for `uuid` / random and any future
  nondeterministic primitive.
- **Resolution + enforcement** (Story
  [#2301](https://github.com/znasllc-io/memql/issues/2301)): the loader
  recognises `core` as a virtual import namespace whose only legal members are
  `CoreBuiltinNames()`; the impure names are removed from the *ambient* resolver
  so a body that calls `timestamp()` without `use core.{ timestamp }` is a
  **load error** with a migration hint. Sense offers an imported builtin in
  completion only when the file has the import. The `dslspec` drift guard stays
  green because the classification is an orthogonal field, not a name change.

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
  *impure* call inside a lambda still obeys 2.1: a lambda that reads the clock
  must `use core.{ timestamp }`. Impurity stays visible without bloating the
  `use` block with `where` / `first`.
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
**Ambient-but-imported from `core`:** `timestamp()`, `uuid` / random -- the
nondeterministic reads.

## 4. Worked examples (target syntax)

A logic that reads the live clock declares it; one that only stamps eval-start
does not:

```memql
// Pure: only eval-start stamp -> bare now, no core import.
logic logicExpiryFromNow {
  args { ttlHours int @required }
  body { return addDuration(now, concat("PT", toString(args.ttlHours), "H")) }
}

// Impure: a fresh wall-clock read -> use core.{ timestamp }.
use core.{ timestamp }
logic logicIsStale {
  args { lastSeen string @required, windowMinutes int @required }
  body {
    return subtractTimestamps(timestamp(), args.lastSeen) >
           addDuration("PT0S", concat("PT", toString(args.windowMinutes), "M"))
  }
}
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
| 3 (#2301) | Make `timestamp()` resolve only via `use core.{ timestamp }`; retire `now()` call-form -> bare `now`; migrate sites (~80 `timestamp()`, ~27 `now()`). Prefer bare `now` where eval-start semantics suffice, reserving `core.timestamp()` for genuine call-time reads. | medium |
| 4 (#2302) | Implement the collection/lambda library + arrow lambdas; pure-body + scope enforcement | large |
| 5 (#2303) | Retire `.First()`/`.Nodes()`/`.Len()`/`.Empty()`/`.Count()` + `first`/`last`/`filter`/`map`/`count` grammar builtins; migrate | large |
| 6 (#2304) | In-memory-vs-SQL guardrail lint | small |
| 7 (#2305) | `asOf` query-only enforcement + `latest`-mode on the contract | medium |
| 8 (#2306) | Publish the matrix to durable reference; update `_reference` skeletons; delete the handoff file | small |

Each story runs `make test` / `make lint` / the DSL load + drift gates before
closing. If implementation forces a contract change, this ADR is updated in the
same PR and the owner flagged.

## 6. Consequences

- **Positive.** Nondeterminism is visible at the import line. One coherent
  collection surface instead of two. Temporal time-dependence is legible on a
  query's contract. The built-in surface now obeys the same explicit-dependency
  thesis as concepts/specs/shapes/queries.
- **Cost.** A medium call-site migration (`timestamp()`), a large new library
  plus the retirement of the legacy collection surface, and a parser change to
  de-register the impure names from the ambient set.
- **Deliberately small `core` initial membership.** Because `globalVariable` is
  already a query and `env` is already provider-scoped, `core` starts with just
  `timestamp`. That is a feature, not a gap: it confirms the language was already
  mostly explicit, and `core` is the durable home for future nondeterministic
  primitives (`uuid` / random).
- **Pre-release, no shims.** Per the repo's no-backwards-compat rule, the legacy
  collection surface and `now()` call form are deleted, not deprecated; consumers
  are migrated in the same change.
