---
title: MemQL Authoring Rules & Gotchas
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Authoring Rules & Gotchas

A running list of rules, conventions, and constraints that bite humans
and AI agents writing MemQL `.memql` files. Every entry here came
from a real bug we hit during development -- this document exists to
make sure the same trap doesn't get sprung twice.

When you find a new gotcha, **add it here**. Future you (and every
other agent) will thank you.

> **Companion reference:** every name the engine reserves -- top-level
> identifiers, row intrinsics, actor-envelope fields, construct
> keywords, annotation names, import aliases -- is indexed in
> [memql-reserved.md](reserved.md). Read that doc before
> picking a field or arg name; this doc is for gotchas that survive
> the name check.

---

## Rule #1 — One write per mutation body

This is the foundational rule of the mutation surface. Every other
rule below is a gotcha; this one is the contract.

**Rule.** A mutation body contains exactly one `insert` block or
exactly one `update` block. Two writes in one mutation is a
parse-time error.

Right -- one bare insert. The target concept comes from the
`mutate <Concept> <name>` signature; restating it is retired.

```memql
use cognition.concepts.{ space }

mutate space createSpace {
  args { name string @required }
  insert {
    name: args.name
    status: "active"
    createdAt: now
    createdBy: actor.userId
  }
}
```

Wrong -- two writes in one body. The parser rejects it.

```memql retired
mutate space createSpaceAndGrantOwner {
  args { name string @required }
  insert { ... }                  // ERROR -- only one write allowed
  insert { ... }
}
```

**Why.** Every mutation is a single observable write. Audit trails are
per-row. Event emission is one event per row. Mutations cannot read,
cannot call other mutations, and cannot loop -- the read path stays
side-effect-free and SQL push-down stays safe. This is the CQS
backbone the engine relies on.

**Multi-write flows compose via an automation.** When the product
needs "create the row + grant access," write the second mutation as
an event-triggered automation that fires on the first row's
creation. The two writes happen sequentially; ordering is explicit;
the user sees one product action even though two rows land.

The canonical worked example is **workspace creation**:

```memql
use platform.concepts.{ partition }
use identity.mutations.{ grantPartitionAccess }

// 1. The product calls this mutation. (`@default` is not valid on
//    an args field -- apply defaults in the body via `??`.)
mutate partition createPartition {
  args {
    name      string  @required
    type      string
  }
  insert {
    name: args.name
    partitionType: args.type ?? "standard"
    status: "active"
    createdAt: now
    createdBy: actor.userId
  }
}

// 2. An automation fires on the row landing and grants the
//    creating user owner access. Note the step calls the logic
//    construct by its bare (un-prefixed) name.
@trigger(event="node.created", concept="v1:platform:partition", partition="*")
/// Grant the partition creator owner access on first landing.
automation autoBootstrapWorkspaceOwnerAccess {
  step grant {
    logic grantOwnerOnPartitionCreate ( event )
  }
}

logic grantOwnerOnPartitionCreate {
  args { event object @required }
  body {
    return grantPartitionAccess(userId: args.event.payload.createdBy, partitionId: args.event.payload.id, role: "owner")
  }
}
```

The product calls `createPartition` once. The automation
takes care of the second write. The user gets one product action;
the engine gets two atomic rows with clean audit trails.

**Cross-references**: see the cognition + partition / workspace
creation flow in `dsl/cognition/automations.memql` and
`dsl/identity/automations.memql` for live examples of this pattern.

**Sense diagnostics for these gotchas** land at edit time in Cockpit
(see [MemQL Sense & the DSL Spec](sense.md)). The rules live in
`component/memql/sense/authoring_rules.go` and cover the most
frequently hit traps:

- `directive-in-body` (error) — catches gotcha #1 (directives inside
  function bodies) before engine init fails.
- `name-too-long`, `name-has-whitespace`, `name-dash-boundary`
  (warning/error) — coarse checks matching the spirit of gotcha #6.
- `deprecated-array-syntax` (hint) — points at
  `memqlmigrate --rewrite=slice-syntax` for the Phase 6 rollout.

---

## 1. Query-level directives are NOT valid inside function bodies

**Rule.** `sort()`, `paginate()`, `asOf()`, `select()`, `withDepth()`,
`count()`, and `shape()` are query-level *directives*. They wrap an entire
expression at the **outermost** layer of a query string and only work
when called by the top-level query parser. The **function loader**
(which validates `.memql` function definitions at engine init) treats
every bare call name in a function body -- e.g. a `logic` body -- as a
reference to another registered function, and since `sort` /
`paginate` / etc. aren't registered functions, the engine init fails
with:

```
function "<name>" references unknown function "sort"
```

If you put a directive inside a function body, the entire engine
refuses to start. The primary node crashes. Cognition / agent / planner
can't attach. Whole cluster bricked.

**Wrong:**

```memql retired
use cognition.queries.{ activeSpaceIds }

// `sort` is not a registered function -- engine init fails.
logic listSpacesSorted {
  args { event object @required }
  body {
    return sort(activeSpaceIds({}), "name", "asc")
  }
}
```

**Right -- struct queries have dedicated clauses.** Sorting,
windowing, and latest-per-id snapshots are `sort` / `paginate` /
`asOf` clauses on the struct query itself, not directive calls
(live examples: `spaceUtterances` in
`dsl/cognition/queries.memql`, `staleClusterNodes` in
`dsl/cluster/queries.memql`):

**`asOf` takes a caller-chosen instant, not only a literal** (memql#2992).
The clause accepts an RFC3339 string, the bare word `latest`, or
`args.<name> ?? latest` — the fallback is part of the caller-arg form
rather than an option on it (memql#3028):

```memql
query deployment deploymentsForCluster {
  args {
    clusterId  string!
    asOf       datetime
  }
  filter  clusterId == args.clusterId
  shape   deploymentFull
  asOf    args.asOf ?? latest
}
```

**The `?? latest` fallback is required** (memql#3028). One clause serves
both callers: omit the argument and the behaviour is byte-identical to
`asOf latest`, so an existing query can adopt the form without changing
anything for callers that pass nothing.

The bare `asOf args.at` is **rejected at parse**, with a message naming
the fix. It briefly parsed, and its failure was discoverable nowhere
before production — not at load, not at lint, and not in a test unless
someone wrote one that omits the argument. Omitting the argument is the
common path for this construct, so a query authored that way works in
its author's test and fails for its ordinary callers.

**For a mandatory instant, declare the argument `@required` and keep the
fallback.** The fallback is then unreachable and the failure lands at the
argument boundary with a usable message rather than inside temporal
resolution — strictly better than what the bare form gave, which is why
requiring the coalesce costs no expressiveness on the authored surface.

One consequence to know about: a query carrying the fallback is marked
`LatestMode` (time-dependent) on its contract, and that marking cannot see
`@required`, so the mandatory-instant pattern above is marked time-dependent
even though its fallback can never fire. Conservative in the safe direction,
and nothing currently gates or caches on the marker.

This matters because a declared `asOf latest` cannot be time-travelled by
wrapping either (`asOf(...)` over a query that declares its own reports
*"multiple asOf() directives are not supported"*), so before memql#2992 a
point-in-time read was reachable **only** by hand-building a runtime query
string. A consumer that calls named queries — `component/deploycontrol` —
could not reach it at all.

Note the value is validated as RFC3339 at call time, so a malformed
instant is an error rather than a silent fall back to `latest`.

```memql
use cognition.concepts.{ context }

/// Latest space-context row for a space.
query context latestSpaceContextForSpace {
  args {
    spaceId  string  @required
  }
  filter  spaceId == args.spaceId
  sort    "row.createdAt", "desc"
  paginate 1
  shape   spaceContextFull
}
```

(The historical receiver form `func (Query) queryListPartitions(_ any)
(any, error) { return sort(...), nil }` hit this trap constantly; the
receiver form itself is now retired and rejected at parse time, so
the directive-in-body variant of the bug can only appear in `logic`
bodies.)

**Where directives DO work**: in raw query strings sent through
`MemqlClientMessage.Stream` (the public RPC), e.g.
`sort(concept==v1:cluster:node, "name", "asc")`. That goes
through the top-level parser, which knows about directives.

---

## 2. Function-call arguments are named, not an object literal

**Rule (obsolete example rewritten -- object-literal call args were
retired entirely).** This section used to document a bare-vs-quoted
distinction between two accepted spellings of an object-literal call
(`createPartition({name: "test", ...})` vs
`createPartition({"name": "test", ...})`). That premise is gone: a
call's argument list is no longer an object literal at all. The only
accepted call form is **named arguments** --
`fn(key: value, key2: value2, ...)`, with an empty call as `fn()`:

```memql fragment
createPartition(name: "test", partitionType: "standard")
spaceParticipants(spaceId: "space-123", participantType: "si")
```

There is nothing left to have a bare-vs-quoted split, because a named-arg
call has no object literal for a key to live inside -- `key` is a bare
identifier by construction, matched positionally against the callee's
declared arg names. A quoted key (`"name": "test"`) is a **parse error**
in this position, not an accepted alternate spelling:

```memql retired
// REJECTED -- object-literal call args are retired; this parses as
// neither a named-arg call nor a valid expression.
createPartition({"name": "test", "partitionType": "standard"})
```

The public RPC (`ExecuteQuery`), the CLI/SDK call builders, the
function-definition parser, and the automation-DSL parser all use the
named-arg form now -- there is no strict vs. relaxed split, and no
object-literal call form to fall back to. There is no `memqlmigrate
--rewrite=...` mode for this specific conversion (see the `rewriters`
map in `cmd/memqlmigrate/main.go`); a stray `fn({...})` call anywhere in
`.memql` source is a load-time parse error naming the fix
(`fn(k: v, ...)`, empty = `fn()`).

---

## 3. Concept scope: `@scope` is retired (#56)

**Rule.** Concepts no longer declare a scope. The partition-scoped vs
`@scope("global")` split went away with the partition removal (#56):
every concept lives in the default partition, and the concept loader
rejects the annotation at load time:

```
`@scope` is retired -- remove the annotation; every concept lives in the default partition post-#56
```

**Retired form (rejected at load):**

```memql retired
// REJECTED -- concept-level @scope is gone.
@scope("global")
concept node { ... }
```

Descriptions source from `///` doc comments first (#2634; the PREFERRED
spelling, gate-enforced on the engine tree since #2636 -- @description
remains the compatibility fallback, and the ~500-character editorial
target is a hint-severity sense diagnostic (#2703), not a hard gate): a `///` block
immediately above any describable declaration (or above an `args{}` field)
IS its description, winning over `@description` when both are present --
never concatenated; `@description` remains valid as the fallback form.

The full concept-annotation author surface is `@description`,
`@version`, `@namespace`, `@type`, and `@displayCard`. `@namespace`
absent defaults to the containing `dsl/<domain>/` directory (#2614);
write it only colon-scoped or pinned (`namespace.pin`), and NEVER move
a `.memql` file between domain directories casually -- file location is
id-bearing and the load guard errors on an unpinned mismatch -- see
[#7](#7-annotations-on-concepts-where-to-put-new-ones).

**A `use` path's leading segment is a NAMESPACE, not a directory**
(memql#2945). This is the rule the whole import system turns on, and it is
worth stating plainly because for most domains the distinction is invisible:
an unpinned domain's directory name and its namespace are the same string, so
either model gives the same answer.

They come apart under `namespace.pin`. The engine never consults the
filesystem to resolve an import -- it takes the leading segment as a hint and
matches it as `:<segment>:` against canonical ids. So a segment names every
domain that *assembles* under it: the directory of that name, plus any
directory whose `namespace.pin` points there. `memqllint` resolves it the same
way, because a lint exists to predict boot; where the two disagreed, the lint
was wrong.

**Importing a concept from a pinned domain: use the PIN, not the directory**
(memql#2901). When `namespace.pin` or `@namespace` sends a directory's
concepts to a different namespace -- `dsl/deployment` declaring `deployment`,
which assembles to `v1:cluster:deployment` -- and that name is declared more
than once in the tree, the directory-named import does not work:

- **no import** -- boot's namespace hint is the file's own directory
  (`deployment`), which cannot match `:cluster:`. That is the ambiguity being
  reported;
- **`use deployment.concepts.{ deployment }`** -- an import of the file's own
  domain. For a file sitting directly in `deployment/` the same-domain-use
  gate (#2617) strips it, and worse, it silences the lint while boot still
  cannot bind -- a green CI shipping a tree that fails at startup;
- **`use cluster.concepts.{ deployment }`** -- **this is the one that works.**
  The path names the namespace the concept assembles under, so boot binds it
  and the lint accepts it. It works whether or not a `cluster/` directory
  exists: with one, the namespace simply covers both directories.

That last point is the #2945 correction. This section previously said the
import worked *only* when no `cluster/` directory existed, and that if one
existed and declared no such concept the only fixes were to rename a concept
or unpin the domain and re-key its ids. That was a description of a lint bug,
not of the language: `memqllint` was doing a directory lookup where boot does
a namespace match, so it rejected a spelling the engine accepts -- and the
remedy it prescribed was a data migration in place of a one-line import.

The one case that still has no import spelling is a genuine collision: the
pin's own directory declaring the *same* name, so two concepts assemble to the
same canonical id. Fix that by renaming one of them.

Lane 2's diagnostic names the pinned-namespace import as the remedy, rather
than the generic "import it via a use declaration" that points at the spelling
which does not work.

(Unrelated: **seed** constructs have their own `@scope("perUser")`
annotation -- see `dsl/agents/trainerAgent.memql`. That is a seed
materialization mode, not the retired concept-level scope.)

---

## 4. Mutation functions can't be wrapped with directives

**Rule.** You cannot wrap a mutation function call with `shape()`,
`paginate()`, `sort()`, `select()`, `asOf()`, or `withDepth()`. The
parser rejects it.

Mutations return a single inserted node, not a queryable result set.

---

## 5. `concept==X` returns the LATEST version per id

**Rule.** When you query a concept without `asOf()`, the engine
internally calls `loadLatestNodes` and returns one row per id
(the latest by `createdAt`). The time-series of historical versions
is preserved in the database but not surfaced.

**Implication.** Re-inserting the same id appends a new row; the
new version becomes the visible one, the old version is invisible
to plain queries. Use the two-argument
`asOf(<expr>, "2026-01-01T00:00:00Z")` from the top-level parser
if you need a historical snapshot -- the one-argument form does not
parse, and the wrapped query must declare no `asOf` of its own
(memql#2992); struct queries
carry an `asOf latest` clause for explicit latest-per-id reads (see
`staleClusterNodes` in `dsl/cluster/queries.memql`).

Consumers should still dedupe defensively -- the engine might
surface multiple historical rows in some shape paths.

---

## 6. Name shape (cluster, partition, anything that becomes an id)

**Rule.** Names that become ids should be **DNS-label shape**:
`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`, **max 50 chars**, lowercase,
inner dashes only (no leading or trailing). Why:

- The id ends up in event topic strings (`graph.node.created.<partition>.<concept>`).
  Topics need predictable, dot-free, whitespace-free segments.
- Storage IDs are case-insensitive in effect because the engine
  always lowercases.
- Partition names appear as path-style prefixes; readability matters.

Server side, an `args { name string @required }` declaration only
checks the type -- there is no engine-side shape validation on
names today. **Don't trust the wire.** If you write a server-side
mutation that takes a name, validate the shape on the server before
persisting.

---

## 7. Annotations on concepts: where to put new ones

**Rule.** Concept-level annotations live at the top of the
`.memql` file, BEFORE the `concept Foo {` declaration. The parser
collects them in a loop until it hits the `concept` keyword. To
add a new annotation, edit `component/database/memory-nodes/concept_parser.go`:

1. Add a field to `parsedConcept`.
2. Add a case to `applyConceptAnnotation()`.
3. Add the field to `Concept` struct in `concept.go`.
4. Map it through in `ParseConceptMemQL()`.

Existing concept annotations: `@description`, `@version`,
`@namespace`, `@type`, `@displayCard`. Anything else is rejected at
load with `unknown concept annotation @<name>`; `@scope` gets a
dedicated retirement error (see
[#3](#3-concept-scope-scope-is-retired-56)).

---

## 7b. Parking a declaration with `/* */` detaches the annotations above it

**Rule.** A declaration's `@`-annotation preamble is the run of contiguous
`@` and `//` lines directly above it. A block comment ends that run, so
annotations sitting above a parked declaration belong to **nothing**:

```memql retired
@executor("integration.workbench.dispatchHost")
@description("does real work")
/*
builtin zzParked {
  a string
}
*/
builtin zzLive {          // <- loads with NO @executor
  b string
}
```

`zzLive` is registered without the executor and cannot be dispatched.

The cost depends on the annotation, and the builtin above is the **least** bad
case: the loader does at least say `@executor is required for builtin
functions`, though that points you at writing a *second* executor rather than at
moving the one you already wrote. The cases with teeth are the silent ones — a
query losing `@public`, or a concept losing `@rowAuthz`, loses the declaration
its authorization is read from, and there the loader raises **nothing at all**.

The same rule catches a **file header** that a banner comment detaches:

```memql retired
@version("1.0.0")
@namespace("knowledge")
/* ------------------------- concepts ------------------------- */
@description("A trained document.")
concept document {
  title string @required
}
```

`@version` and `@namespace` here belong to nothing, and the declarations below
register under the defaults instead. A blank line between the header and the
banner ends the run before the comment does — which is why the engine's own tree
is unaffected, and it is the fix for this shape.

**Park the annotations with the declaration**, inside the comment:

```memql
/*
@executor("integration.workbench.dispatchHost")
@description("does real work")
builtin zzParked {
  a string
}
*/
```

...or move them below the comment if they were written for the live one.

`memqllint` reports this as an error naming both lines and quoting the orphaned
run (memql#2965). It is reported rather than repaired on purpose: which
declaration the annotations were written for is a question only the author can
answer, and in the shape above they sit directly on top of the parked one.

---

## 8. The `_system` partition is reserved

**Rule.** Partition names starting with `_` are reserved. The
DNS-label name shape from [#6](#6-name-shape-cluster-partition-anything-that-becomes-an-id)
rejects leading underscores, so users can't choose `_system` (or
`_anything`) for their partition.

`_system` is the engine's internal bookkeeping partition (a #56
phase-8 vestige). Treat it as internal -- never surface it in
user-facing partition lists.

---

## 9. Insert id semantics: explicit vs derived

**Rule.** When a mutation's `insert { ... }` block sets `id:`
explicitly (`insert { id: args.name, ... }`), the engine computes
the storage id as:

```
{concept}:{id-segment}
```

Where:
- `concept` = the concept bound by the `mutate <Concept> <name>`
  signature
- `id-segment` = the trimmed value of the `id:` field

(The leading `{partition}:` segment was retired in #56 phase 6; ids
are now a plain `{concept}:{shortId}`. See
[identifiers.md](../concepts/identifiers.md).)

If you omit `id:`, the engine derives a content hash from the payload.
Same payload twice ⇒ same id ⇒ a new time-series row under that id.
Different payload ⇒ different id ⇒ a different row.

**Common bug**: forgetting to set `id:` means duplicate inserts
create new ids instead of new versions of the same id.

### 9b. A per-caller singleton derives its id from the actor (memql#4746)

When a concept holds exactly one row per person -- a settings blob, a
saved layout -- derive the id from `actor.userId` and write it with a
single `insert{}`. `insert{}` is create-or-upsert at the engine's one
write chokepoint (memql#1709), so the first call creates and the rest
overwrite, and "one row per person" is true by construction rather than
by a read the writer hopes was fresh.

```memql fragment
@actor
mutate desktop saveMyDesktop {
  args {
    revision  int!
    document  object!
  }
  insert {
    accept { revision, document }
    stamp {
      id: hash(actor.userId)
      ownerUserId: actor.userId
    }
  }
}
```

`hash()` and not `actor.userId` itself: an actor id is canonical
(`v1:identity:user:<slug>`), and `core/id.ValidateShortId` refuses a
canonical id carrying a **different** concept's prefix. Unprefixed, per
section 20. The alternative -- a caller-minted id plus a create/update
pair, chosen by a read (`createRoutingPolicy` / `updateRoutingPolicy`) --
is what you need when the id is genuinely the caller's; for a singleton
it lets two tabs that both read "no row" both create one.

`actor.*` INSIDE a call works in every value position, id derivations
included, since memql#4746. Before it, `id: actor.userId` rendered and
`id: hash(actor.userId)` did not: the two spellings lower to different
AST nodes and only one had an evaluator case, so the mutation passed
`memqllint`, passed strict boot, and failed at render on every call. If
you meet `unsupported expression in mutation template` for a node the
grammar plainly accepts, that is this class (memql#2909 / memql#2925),
not your authoring.

---

## 10. Subscriptions and event topic shape

**Rule.** Graph CDC event topics are **4 segments**, with no partition
segment:

```
graph.node.{created|updated|deleted}.{concept}
```

e.g. `graph.node.created.v1:notes:note`. The old `{partition}` segment
between the action and the concept was retired in #56 -- topics are
concept-keyed, not partition-keyed.

**Composing that topic string is the SERVER's job, not the client's**
(memql#2460). A graph subscription (`SUBSCRIPTION_KIND_GRAPH_EVENTS`)
carries a structured `concept` + a set of `actions`; the engine composes
the bus topic from those, and the legacy free-text `filter` field is
**rejected** for graph subscriptions -- it survives only for the
non-graph subscription kinds (`TELEMETRY`, `MESSAGE`, `QUERY_SPEC`,
`AI_STREAM`, `DOMAIN_EVENTS`, `AUTOMATION_EVENTS`). Empty `concept` means
all concepts; empty `actions` means all actions. The SDKs wrap this as
`SubscriptionManager.subscribeGraph(handler, { concept, actions })`
(TS) / `SubscribeGraph(ctx, GraphSubscribeOptions{...})` (Go).

`@trigger(event=..., concept=..., partition="*")` in
`dsl/*/automations.memql` still carries the `partition="*"` kwarg as a
separate #56 phase-8 vestige on the DSL trigger surface -- unrelated to
the client subscription wire, and tracked by its own caveat where this
doc discusses `@trigger`.

Full detail: [events.md](../concepts/events.md#subscribing-to-events).

---

## 11. Role enum: owner / admin / developer / writer / reader

**Rule.** The unified role spectrum is **owner / admin / developer /
writer / reader** -- five values, as returned by `AllRoles()` in
`component/auth/rbac.go`. This applies to:

- `v1:identity:user.role` (cluster-wide, and the only role a user has
  -- the per-partition grant that used to carry a second one went with
  partitioning in #56)
- `v1:identity:delegation.roleCeiling`
- `v1:data:policy.revertMinRole`
- The `UserRole` proto enum
- `component/auth/rbac.go` (`RoleOwner`, `RoleAdmin`, `RoleDeveloper`,
  `RoleWriter`, `RoleReader`)

`developer` is **live**, not legacy: it is engineering power rather
than admin power (authoring, inline DSL, deploy / cut-version, but no
user management -- MCP epic #1529). An earlier revision of this
section listed it among the retired values; `migrateRole` does not
touch it.

The genuinely retired values are **manager**, **user**, **advocate**,
**member** and **guest**. Legacy data is migrated at read time by
`migrateRole` in `rbac.go`:

- `manager` -> `writer`
- `user` / `advocate` / `member` / `guest` -> `reader`

**If you add a new concept with a role enum, use the five current
values only.** Don't add legacy values "for compatibility" -- the
migrator already handles old rows.

**Ordering.** `RoleLevel` returns: owner=0, admin=1, **developer=1**,
writer=2, reader=3 (and any unknown role falls to 3). Lower number =
higher privilege; note that admin and developer deliberately share a
level rather than ranking against each other. `RoleAtMost(a, b)`
returns the more-restrictive of the two (useful for delegation
ceilings).

---

## 11b. cond() for conditional values -- not `if` at expression position

**Rule.** When you need a conditional value inside an expression (a
mutation payload, an argument, a function body), use `cond(predicate,
thenValue, elseValue)`. The `if` keyword is reserved for the
control-flow statement (`if condition { step }` in automations) and
does NOT work as a value-returning expression.

```memql retired
# Wrong -- parse error
role: if existingOwners.empty { "owner" }

# Right
role: cond(existingOwners.empty, "owner", "reader")
```

Previously the builtin was named `if()`. It was renamed to `cond()` so
the AST no longer collides visually with the `if` statement.
`cond()` requires all three arguments; there is no implicit else.

**Where cond() is evaluated.**

- Inside a mutation write block (`insert { x: cond(...) }`): the
  mutation-template evaluator handles it.
- As a function-call arg in an automation step
  (`createUser({role: cond(...)})`): the function-step arg resolver
  resolves it at arg-resolution time before renderMemQLValue quotes
  the result for the outgoing query. See
  `component/automations/steps/function.go::resolveArgValueRef`.

Other expression builtins (`coalesce`, `concat`, `hash`, `first`,
`last`, `lower`, `upper`, `trim`, ...) are evaluated by the MemQL
engine when the outgoing query executes, so they don't need
arg-resolution-time handling.

---

## 11c. Logic-body expression grammar: what works in each position (#2542)

**Rule.** A logic body evaluates value-expressions IN-MEMORY, and the
supported grammar differs by POSITION. memqllint (parse + boot-parity
Init) accepts a superset of what the runtime evaluates, so the table
below is authoritative: anything marked "no" is either a lint/boot
rejection or an unsupported shape -- use the working idiom in the last
column instead. `int / int` is integer division (#2316); use a float
operand (`* 1.0`, or the `good * 100 / total` percent idiom) for a
fractional ratio. Division / modulo by zero is a clean logic error, not
a panic.

The five value positions:

- **return** -- the terminal `return <expr>`.
- **step RHS** -- an intermediate `x := <expr>`.
- **cond pred** -- the predicate of `cond(predicate, then, else)` (and
  the `if <condition>` step condition).
- **cond branch** -- the `then` / `else` VALUE of a `cond(...)`.
- **projection / lambda** -- an object-literal value or lambda body
  inside a collection chain (`select(g => { k: <expr> })`,
  `where(m => <expr>)`).

| Form | return | step RHS | cond pred | cond branch | projection / lambda | Working idiom / note |
|---|---|---|---|---|---|---|
| Arithmetic `a / b`, `a * 100 / c`, `a - b` | yes | yes | -- | yes | yes | Integer division truncates; use a float operand for a ratio. |
| Collection chain `rows.where(m => ...).count()` | yes | yes | yes (bare boolean chain) | yes | yes | `.first().field` DotAccess and `.sum/min/max/avg/count(lambda)` included. |
| Date builtins `daysBetween(...)`, `addDuration(...)` | yes | yes | -- | yes | yes | #2541; the calendar extractors (`year` et al.) were retired under 2026.08 (#2707). |
| `cond(...)` (nested) | yes | yes | -- | yes | yes | Connectives (`&&`/`||`) are NOT allowed inside a cond arg -- nest cond or use an `if` step. |
| Comparison, **expression-led** `(a - b) > 0`, `0 < count`, `rows.count() >= 1` | yes | no (parse-rejected) | yes | no | yes (lambda body) | The LHS must be non-identifier-led: parenthesize the arithmetic / lead with a literal / end in a call. |
| Comparison **over a chain aggregate** `rows.count(m => ...) > 0` | single-return only | no | yes | no | yes (lambda body) | As a cond PREDICATE this is the #2542 item-2 headline. As a bare multi-step `return`, wrap it: `return cond(rows.count(m => ...) > 0, thenV, elseV)`. |
| Comparison, **scalar / identifier-led** `x > 10`, `role == "admin"` | boolean-condition return only | no (parse-rejected) | yes, if the identifier is a BOUND LOCAL | no | yes (lambda body) | Fine as a cond/`if` predicate and inside a boolean-condition return (`a.empty() && x == "b"`). As a cond predicate the identifier must be bound by a step of the same body; an UNBOUND one is a LOAD rejection (#3024) because it resolves against an empty scope and the cond becomes a constant -- read an argument as `args.x`. A BARE `return x == 5` is a LOAD rejection (#2693; it previously loaded green and mis-routed to a store query) -- write it expression-led `(x) == 5` / literal-led `5 == x`, boolean-condition (`... && x == 5`), or `return cond(x == 5, true, false)`. The same comparison as a `coalesce`/`concat` arg is likewise rejected. |
| Comparison over an **ambient** `actor.role == "owner"`, `config.x == "on"` | -- | -- | yes | -- | -- | #3024: the actor / partition / now / config envelope is threaded through arg expansion, so an ambient cond predicate discriminates like an `args.` one. With no authenticated caller the envelope denies rather than omitting keys (#2801). |
| Arithmetic over a comparison `a - b > 0` | REJECTED | REJECTED | REJECTED | REJECTED | REJECTED | The unparenthesized-comparison trap: `a - b > 0` parses as `a - (b > 0)`. Lint/boot rejects it -- parenthesize `(a - b) > 0`. |

**Notes.**

- **The `a - b > 0` trap is a lint/boot error (#2542).** A trailing bare
  identifier operand folds the comparison into the arithmetic
  (`a - (b > 0)`), so the arithmetic operand is a boolean -- never valid.
  `convertArithmeticExpr` + `validateLogicArithmeticOperands` reject it at
  load with the parenthesise fix; the lint/boot-parity pass surfaces it.
- **Identifier-led comparison as a scalar VALUE is mis-routed.** The
  engine has no scalar plan-root branch for a Field-led comparison
  (`args.n == 5`), so it is executed as a store query and returns the
  wrong value. Always write a value-position comparison expression-led
  (parenthesise the left operand, or lead with the literal) or wrap it in
  `cond(...)`. This is why a comparison is a first-class `cond` PREDICATE
  but not a `cond` BRANCH value -- an identifier-led comparison in
  branch-value position is a load rejection (#2655; it previously loaded
  green and silently returned its own source text on the multi-step path).
  **#2693** extends that load rejection from the cond BRANCH to the two
  adjacent value positions that shared the same silent-wrong class: a
  bare/terminal `return args.n == 5` (mis-routed as a store query, since a
  logic value step is a named-call query and the inline `query { ... }`
  filter form does not parse in a logic body), and the same comparison
  laundered through a `coalesce`/`concat` arg. The gate stays OFF for a
  comparison that is a direct `&&`/`||`/`!` operand -- that is the legal
  boolean-condition return (`a.empty() && x == "b"`).
- **cond predicates** accept a bare boolean chain (`x.any()`), a scalar
  comparison (`r > 50`), an equality over a **bound local** (`role ==
  "x"`, where an earlier step binds `role`), a comparison
  over a chain aggregate (`x.count(m => ...) > 0`, wave 3), and a
  coalesce-led equality in either spelling (`coalesce(args.b, "") ==
  "y"` / `args.b ?? "" == "y"`, #2612) -- including inside NESTED
  cond predicates, where that shape was previously a load rejection and
  the bind-then-compare workaround (`z := coalesce(...); cond(z == ...)`)
  was required. The workaround remains valid; it is no longer necessary.
  The predicate aggregate is resolved through the in-memory collection
  evaluator, not a lexicographic string compare. A **string builtin**
  operand (`lower(args.b) == "y"`, also `upper`/`trim`/`hash`/`shortId`/
  `concat`) is likewise evaluated in memory (#2656): before that it
  loaded green and compared the builtin's SOURCE TEXT, so it was
  always-false in every shape -- the door form, the parenthesised form,
  and bind-then-compare alike.
- **An UNBOUND bare identifier is a load rejection** (#3024). `role ==
  "x"` is only legal when some step of the same body binds `role`; in a
  single-statement body nothing does, so the identifier resolves against
  an empty scope and the comparison is a CONSTANT -- the else branch for
  every input, loading green and linting green. Read an argument as
  `args.role`, or bind the local first. This is #2962's mechanism in the
  spelling authors reach for first, which is why it is refused rather
  than left to be discovered as a gate that never fires.
- **Ambient predicates evaluate** (#3024): `cond(actor.role == "owner",
  ...)`, and the same for `config.` / `partition` / `now`. The ambient
  envelope is resolved once per call and threaded through arg expansion,
  so these discriminate exactly like the `args.` ones; the multi-step
  path binds the same envelope onto its own evaluator, so a predicate
  answers identically whichever way the body is written. They were
  briefly a load rejection -- expansion received only args, so an
  ambient comparison fell through and took the else branch for every
  input -- and that refusal is gone now that the envelope reaches the
  predicate. With no authenticated caller the envelope DENIES (every key
  present, owner bits false, #2801) rather than leaving keys absent,
  because an absent key is what makes a negated gate read true.
- **A reserved root is not the same as a resolvable one.** The envelope
  carries exactly `actor.` / `config.` / `partition` / `now`. `trace` is
  reserved -- no local or payload field may shadow it -- but nothing
  supplies it, so a `trace.`-rooted cond predicate is a **load error**,
  not an evaluation. The same goes for a path the envelope has no key
  for: an unknown `actor.` member (the auth envelope is a closed set,
  #2623) or a `config.` key outside the `policy_exposable.go`
  allow-list. Each would otherwise resolve to nothing and constant-fold,
  which is the silent gate this whole rule exists to prevent, so it is
  refused loudly instead.

---

## 12. `partition` is still a reserved payload field -- pick another name

**Rule.** `partition` is one of the engine's reserved payload-level
fields (see [#19](#19-reserved-intrinsics-do-not-redeclare-id--createdby--createdat--partition)
for the full set). Declaring a concept property named `partition`
fails `ensureReservedFieldsNotDeclared` at startup:

```
concept v1:example:thing definition schema declares reserved property "partition"
```

Pick an explicit alternative. Live examples in the tree:
`v1:identity:user.activePartitionId` and
`v1:identity:invitation.partitionId`.

**Why it bites you -- and why the old reason is no longer the reason.**
This section used to say the PK for partition-scoped rows is
`(partition, id, createdAt)` and that a payload field of the same name
would shadow the PK column. **That is no longer true** (memql#3305).
Partitioning was retired in #56: `"MemoryNodes"` has no `partition`
column at all and its primary key is `(id, "createdAt")` -- read it in
`component/database/memory-nodes/migrations/20260324000000_initial_setup.up.sql`,
whose own comment says "no partition column post-#56 phase 3".

The name nevertheless remains in `reservedPayloadFields`
(`component/database/memory-nodes/constants.go`), so the rule stands
and the startup check still fires. What changed is the rationale: the
reservation is now a **retired-name guard** rather than a
column-shadowing guard. Keeping it means a concept cannot quietly
reintroduce a field whose name implies a tenancy dimension the engine
no longer has -- which, given how much stale documentation described
partitions as live, is worth keeping rather than reclaiming.

The canonical example this section used to cite,
`v1:identity:partitionAccess.partitionName`, **does not exist**; neither
field nor concept survives. See
[access-model.md](../operate/auth/access-model.md) for what replaced
partition-based isolation.

Full reserved list lives in `component/database/memory-nodes/constants.go`.
As of Phase 1 of the language-improvements plan, the check also runs
at mutation time (`executor.executeInsert`) -- so an
`insert { partition: ... }` write fails with the same error shape
instead of silently stripping the field.

---

## 13. Step references are validated + topologically sorted at compile time

**Rule.** A step's condition or arg referencing another step's result
(`foo.first().payload.x`, `foo.empty()`, etc.) is
validated at compile time. The compiler:

- collects every step ID into a symbol table;
- extracts step references from both condition strings AND function-call
  arguments (query strings, mutation payloads, nested expressions);
- rejects unknown references (catches typos);
- **topologically sorts** steps by their dependency graph so every step
  executes after all its dependencies, regardless of source order.

**Forward references are now supported.** Steps can be declared in any
order -- the compiler reorders them automatically. Cycles produce a
clear compile-time error.

Example of a typo that surfaces at compile time:

```memql fragment
checkUser := userById({ userId: args.event.payload.userId })

result := if cehckUser.empty() {   // typo: cehckUser -> checkUser
  createUser({...})
}
```

The compiler emits:

```
automation "bootstrapUser": step "result" references unknown step "cehckUser" -- check for a typo, or add the step
```

Example of a cycle (would deadlock at runtime):

```memql fragment
a := if b.empty() { queryFoo({}) }
b := if a.empty() { queryBar({}) }
```

The compiler emits:

```
automation "test": dependency cycle among steps [a b]
```

---

## 14. Function naming: the construct name says what it does

**Rule.** A construct is named for what it does, not for its kind --
the declaration keyword already carries that: `activeHumanParticipants`,
`addAgentToSpace`, `isHumanParticipant`, `isActiveRecord`,
`bootstrapSession`. No `query*` / `mutation*` / `logic*` / `spec*` /
`trait*` / `seed*` prefix -- settled in #2853, which measured 0 of 1091
shipped declarations carrying one. (See naming-conventions.md.)
Constructs live in one consolidated file per kind per namespace
(`dsl/<namespace>/<construct>s.memql`), so the file name never
carries an individual construct's name.

```
dsl/cognition/queries.memql     query space activeSpaces { ... }
dsl/cognition/mutations.memql   mutate space createSpace { ... }
dsl/common/specs.memql          spec actorEnvelope requiresAdmin { ... }
dsl/common/traits.memql         trait isActiveRecord { ... }
dsl/cognition/logic.memql       logic bootstrapSession { ... }
```

Note the declaration keyword on the mutation line: it is `mutate`.
`mutation` is the *invocation* verb used inside a logic body, and the
parser's own tests call that pair "the canonical footgun distance".

**Why it bites you.** Callers (the product frontend, automations, Go
integration code) name constructs as a string, so a name is a wire
contract: renaming one breaks every caller silently, and a name that
never existed fails only when the caller first runs. Historically the
tree mixed prefixed and unprefixed names and the frontend hit runtime
"function not found" errors as a result -- the fix was consistency, not
a particular prefix.

Enforcement: `TestNoKindPrefixInConstructNames`
(`test/dslconformance/naming_conventions_test.go`) fails on any declaration named with
its own kind as a prefix, across all 16 declaration keywords.

The old *opposite* lint, which REQUIRED the prefix, was retired in epic
#2031 (C2/#2042) -- `component/language/compiler/linter.go` records this
in its header, and `TestCompileSource_NoNamingWarnings` fails the build
if any `naming.*` warning is emitted. References resolve structurally:
the dependency-tree validator (C3/#2043) fails a reference that does not
exist at load time.

An automation step calls a logic construct by the same name the
file-top import names -- `step decide { logic bootstrapSession ( event )
}` resolves through `use cognition.logic.{ bootstrapSession }` (see
`dsl/cognition/automations.memql`). The corpus uses the paren call form
throughout.

Automations are event-triggered, not called by name, so they use
verb-first names with no prefix (`autoJoinSI`, `bootstrapSession`,
`purgeExpiredArchivedSpaces`). Builtins, tools, prompts, providers,
and shapes are out of scope for this rule and use their own
conventions (shapes are conventionally `<concept><Projection>`, e.g.
`participantFull`, `spaceCard`).

---

## 15. Write-block sugar: `accept { ... }` / `stamp { ... }` (and the bare-mirror shorthand)

**Rule.** The preferred spelling of a mutation write block is the
accept/stamp form (#2035/#2592, shipped by #2593): `accept { a, b }`
lists the public fields the mutation accepts from its caller -- each
name auto-binds to its same-named declared arg (`a` means
`a: args.a`, load-validated against the `args { ... }` block) -- and
`stamp { key: value, ... }` carries the server-set fields. Nested
inside `insert { ... }` / `update { ... }` the enclosing block spells
the write kind; the top-level bare form (accept/stamp with no write
block) means insert.

```memql fragment
// Preferred -- the corpus form after the #2616 migration.
insert {
  accept { slug, name, rank, description }
  stamp {
    id: args.roleId ?? args.slug
    predefined: args.predefined ?? false
    active: args.active ?? true
  }
}
```

**All-or-nothing.** A write block never mixes loose `key: value`
fields with a nested `accept`/`stamp` -- the desugar rebuilds the
body from the blocks alone, so a loose field would be dropped and the
rewriter rejects the mix at load. Move every server-set field into
`stamp { ... }` or stay fully longhand.

**The bare-mirror shorthand remains valid longhand.** Inside a
longhand write block, a bare `args.ident` with no `key:` prefix is
shorthand for `ident: args.ident` (the key is the arg path's final
segment; single-segment paths only -- `args.user.id` needs the
verbose `userId: args.user.id` form). The conformance gate
(`test/dslconformance/no_bare_mirror_runs_test.go`) collapses provably-safe mirror
runs into accept/stamp via `memqlmigrate --rewrite=accept-stamp`;
blocks it cannot prove safe (comments worth keeping, nested object
values, single mirrors) stay longhand deliberately.

```memql fragment
// Longhand with bare mirrors -- still valid where the gate allows it.
// This block stays longhand deliberately: the multi-line computed id
// is exactly the shape the codemod refuses to reflow.
insert {
  id: concat("si-", hash(concat(
    canonicalId(args.agentId, agent), ":",
    canonicalId(args.spaceId, space)
  )))
  args.spaceId
  args.agentId
  participantType: "si"
  args.displayName
  status: "active"
}
```

**Constraints.**

- **Simple identifier only.** The arg path must match
  `[A-Za-z_][A-Za-z0-9_]*`. Dotted paths (`args.user.id`) are NOT
  eligible; write those as `userId: args.user.id` explicitly. The
  parser rejects shorthand with dotted paths instead of inventing a
  garbage field named `user.id`.
- **Bare `args.X` only.** `coalesce(args.x, default)`,
  `concat(args.a, ":", args.b)`, `cond(...)`, and other wrapping
  expressions keep the explicit `key:` prefix. Only a plain
  `args.name` expression can be shorthand.
- **No effect on the `args { ... }` block.** That block is a type
  declaration, not a value map; its lines stay in the
  `<name> <type> [@required] ...` form.

**Why it bites you (if you don't know about it).** Reviewing PRs
you'll see some mutations declaring 20-field payloads and some
declaring 20-field payloads with half the repetition. Both are valid
and equivalent. Under the hood the struct-form rewriter translates
`args.X` to the engine-internal `ctx.X` and the expansion lives in
the mutation-template parser
(`component/memql/mutation_templates.go`); step-call args in
automations / logic bodies get the equivalent dotted-path shorthand
from
`component/language/compiler/automation_generator.go::tryParseBarePathShorthand`
(see [#17](#17-automation-step-args-shorthand-bare-dotted-path-infers-the-key)).
Authors never write `ctx.X` -- it is not part of the author surface.

---

## 16. Shape bodies: the key comes from the path's terminal segment

**Rule.** Shapes are struct-form path lists. Each body line is a
projection path. A payload property is written by **bare name**
(`name`, `description`) -- the concept is bound by the
`shape <Concept> <name>` signature, so `` is removed; row
metadata stays `row.X` (`row.id`, `row.createdAt`) and the auth
envelope stays `actor.X` (`actor.userId`). The projected field is
keyed by the path's **terminal segment**. Every shape declares its
kind via `@row` (concept payload + row intrinsics) and/or `@actor`
(engine envelope, no signature concept). The explicit `X`
form is rejected at load.

```memql
use agents.concepts.{ agent }

@row
/// Full agent projection
shape agent agentFull {
  row.id
  name
  description
  row.createdAt
}
```

`include` is NOT a shape verb (memql#3621). It was documented for a
long time and never implemented -- a body is a path list, so
`include agentFull` parsed as two payload properties and projected two
always-null keys. It is rejected at load now; repeat the paths, or drop
the body entirely and take the default projection over the bound
concept (memql#2035).

Every body path is checked against the bound concept at load: a bare
payload property must be a declared field of that concept (a bare
`createdAt` is `payload.createdAt`, not the intrinsic -- write
`row.createdAt`), two paths may not collapse onto the same terminal
key, and the declared kind must match the body (`actor.*` needs
`@actor`, `row.*` / bare payload needs `@row`, at least one required).

**Retired forms (rejected at parse time):** the receiver form and
its template wrapper are gone --

```memql retired
// REJECTED -- func (Shape), @template, and node("...") are retired.
func (Shape) agentFull {
  @template({
    node("id"),
    node("name")
  })
}
```

The terminal-segment keying carried over from the old `node("...")`
shorthand: `name` projects as `name`, exactly like
`node("name")` did. Live examples sit in every
`dsl/<namespace>/shapes.memql` file.

---

## 17. Automation step-args shorthand: bare dotted path infers the key

**Rule (updated for G5, memql#2367 + Story 9, #2335).** An automation
declares a typed `args { }` contract; the trigger binds the event payload
to it (loud refusal on a violated contract) and step bodies read the
fields BARE -- `event.payload.<field>` reads are retired and rejected
with a migration hint, and the legacy `name({ ... })` object-literal call
wrapper is rejected at parse time. A bare simple identifier in a
construct-call arg position is PUNNED (`f(spaceId)` ==
`f(spaceId: spaceId)`, G3 #2365); step results read via
`steps.<name>.result`.

```memql
automation sendGreeting {
  args {
    spaceId          string @required
    siParticipantId  string @required
  }
  step decide {
    logic composeGreeting ( spaceId )              // punned
  }
  step send {
    mutation sendTextUtterance (
      spaceId,                                     // punned bare field
      participantId: siParticipantId,              // renamed key
      text:          steps.decide.result           // step-result read
    )
  }
}
```

**Constraints.**

- **At least two dotted segments required.** Single identifiers like
  `allAgents` are NOT eligible -- they'd collide with step-reference
  semantics where `allAgents` means "the `allAgents` step's result".
  Use `allAgents.nodes()` in a `for` loop, not inside an object arg.
- **Every segment must be a simple identifier.** Method calls
  (`.nodes()`), index access (`.nodes()[0]`), and call arguments
  (`concat(...)`) all disqualify the value.
- **Terminal segment must match what you intend as the key.** If the
  path's terminal segment isn't the field you want
  (`registerNode.result.node.id` -> `id`, not `registerNode`), use
  the verbose form (`nodeId: registerNode.result.node.id`).

---

## 18. Object-literal keys: unquoted identifiers only

**Rule.** Inside MemQL `{...}` object literals, keys MUST be unquoted
identifiers (`name:`, `spaceId:`, `createdAt:`). Quoted-string keys
(`"name":`, `"spaceId":`) were historically allowed by the parsers
for JSON interop but are not idiomatic MemQL and must not appear in
new code.

```memql fragment
// Correct -- mutation write block
insert {
  name: args.name
  spaceId: args.spaceId
  active: true
  metadata: { source: "import" }   // unquoted key in a nested object VALUE
}

// Wrong -- unnecessary quotes on simple-identifier keys
insert {
  "name": args.name
  metadata: { "source": "import" }
}
```

> **Where this still applies (and where it no longer does).** This rule
> is about `{...}` object literals: a mutation write block
> (`insert { ... }` / `update { ... }`) and a nested object-typed field
> VALUE (`metadata: { ... }`) both remain genuine object literals, and
> both still accept a quoted key -- so the bare-vs-quoted style choice
> still applies there. **Function-call arguments are a different
> surface and no longer apply here at all**: a call's argument list is
> named args (`fn(key: value, ...)`), not an object literal, so there is
> no `{...}` for a key to be quoted or unquoted inside -- see
> [#2](#2-function-call-arguments-are-named-not-an-object-literal). The
> `createUser({ userId: ... })` call-style example this section used to
> show here was retired along with object-literal call args generally.

**Why it bites you.** Mixed quoting styles in the same codebase make
every review a guessing game. All .memql files before this rule had
unquoted keys except a handful in inline `shape(...)` templates that
used JSON-style quoting; the blast radius on a frontend/Go consumer
is small because the parsers accept both, but the inconsistency is
what blocked us from spotting earlier bugs (quoted keys don't
participate in the bare-`args.X` / dotted-path shorthand from rules
#15 and #17 because shorthand only triggers when the key is absent).

**Exception.** Quoted keys are accepted when the key content isn't a
valid identifier -- for example a key with a hyphen or space, or
JSON blobs embedded verbatim in a string value (those aren't
MemQL-parsed at all). Reach for quoted keys ONLY when the name cannot
be expressed as `[A-Za-z_][A-Za-z0-9_]*`; everything else is a
style violation.

Where the parsers stand today:

- `component/memql/mutation_templates.go::parseObjectKey` -- accepts
  both; prefer unquoted.
- `component/language/parser/parser.go::parseObject` -- accepts
  both; prefer unquoted.

Enforcing via a linter rule is tracked as a follow-up; for now treat
this as a PR-review checklist item.

---

## 19. Reserved intrinsics: do not redeclare `id` / `createdBy` / `createdAt` / `partition`

**Rule.** The engine auto-stamps a small set of intrinsic fields on
every inserted node version. They live on the row itself, not in the
payload. Declaring any of them as a payload property in a concept
schema is rejected at concept-load time by
`ensureReservedFieldsNotDeclared`:

```
concept v1:foo:bar definition schema declares reserved property "createdBy"
```

If a single concept fails to load, the whole concept loader bails --
which means **no concepts get registered**, the BFF can't serve any
graph queries, and the entire cluster is bricked at startup.

The reserved set today: the row's storage columns -- `id`, `createdAt`,
`createdBy`, `partition`, `concept`, `payload`, `schema`, `type`,
`provenance` -- plus the engine namespaces a filter resolves at the
head of a path: `row`, `actor`, `args`, `now`, `config`, `trace`,
`meta`. Full list in
`component/database/memory-nodes/constants.go`.

The second group joined the list in memql#3613. Each of them was
declarable, and a concept declaring one registered with the field
intact -- while every filter naming it bare read the ENGINE NAMESPACE
instead. `provenance` was fully silent (the push-down and the
in-process post-filter agreed on the same wrong field, so the query
returned the wrong rows with no error) and `actor` was silent AND
authorization-relevant (`filter actor.userId == args.v` const-folded
to true whenever the caller passed their own id, so the predicate
contributed nothing and the query returned every row). Matching is
case-insensitive and by whole name, so `Provenance` is refused while
`arguments`, `metadata`, and `rowCount` are ordinary properties.

Practical consequences for concept authors:

- **`createdBy`**: never declare it. The engine sets it from the
  request actor on every insert. If you need a separate
  "issued by some other actor" field (a row created by one user but
  about another), use a payload field with a distinct name. See
  `v1:identity:invitation.inviterId` for a live example -- the
  inviter is recorded explicitly rather than inferred from
  `createdBy`.
- **`partition`**: see [#12](#12-partition-is-still-a-reserved-payload-field----pick-another-name).
  The name is still reserved; pick something explicit like
  `partitionId`.
- **`id` / `createdAt`**: same -- the engine owns them.

Practical consequences for mutation authors:

- In an `insert { ... }` block, `createdBy: actor.userId` /
  `createdAt: now` stamp the firing actor and eval-time timestamp
  (the live pattern -- see `dsl/calendar/mutations.memql`). Never
  stamp `createdBy` from a caller-passed arg; whoever fires the
  mutation IS the recorded creator.
- Don't take a `createdBy` arg in your mutation's `args { ... }`
  block. It's noise on the wire and a footgun if a caller ever sets it.

This bit hard in 2026-04-29: a partition concept added a `createdBy`
payload field, which made the loader refuse the entire concept set.
Cognition / agent / planner all dropped off the mesh because the
primary couldn't serve queries. The fix was a one-line concept-schema
delete plus dropping the matching `createPartition` arg.

---

## 20. Foreign-key id derivation: normalise before hashing

When a mutation derives a deterministic id by hashing foreign-key
args (the participant id pattern: `id = hash(spaceId + ":" + userId)`),
the args MUST be normalised first, with `canonicalId(value, <concept>)`
by default -- see below for when `shortId(value)` is the right choice
instead, and what it does not give you.
The hash is byte-level, so two callers passing the same logical
reference under different shapes (`"user-abc"` vs
`"_system:v1:identity:user:user-abc"`) hash to different strings and
produce DUPLICATE rows with distinct ids.

```memql retired
// Wrong -- bare-vs-canonical input shape changes the participant id
insert {
  id: hash(concat(args.spaceId, ":", args.userId))
  ...
}

// Right -- canonicalId() collapses both forms to the same string, AND
// each part is hashed before concatenation so the composite cannot
// alias. The second argument is the imported concept short-name
// (resolved against the file-top `use ...concepts.{ space, user }`
// imports).
insert {
  id: hash(concat(
    hash(canonicalId(args.spaceId, space)),
    hash(canonicalId(args.userId,  user))
  ))
  ...
}

// Wrong -- joining with a separator first. `hash(concat(a, ":", b))`
// ALIASES whenever a part can contain the separator: ("chat", "k:1")
// and ("chat:k", "1") derive one id, so two distinct rows collapse into
// one. A canonicalId() part happens to be safe today only because its
// fixed `v1:ns:concept:` prefix makes the split recoverable -- that is a
// property of the data shape, not a constraint, and it stops holding the
// moment a part is caller-supplied (memql#3009).
```

(Don't prefix the hash with the concept name -- `id:
concat("participant-", hash(...))` duplicates information already in
the canonical id position, and `test/dslconformance/conformance_test.go`'s
`TestNoShortIdConceptPrefix` rejects known concept-name prefixes
outright. The shortId is the bare hash / uuid / slug.)

`canonicalId(value, concept)` -- `concept` is an imported concept
short-name (the stringly-typed `"v1:ns:name"` literal is retired):

- bare slug → prepends `<concept>:` (no partition prefix -- partitioning
  and `@scope` were both retired in #56; the composed form is the plain
  `{concept}:{shortId}` shape, see `component/memql/partition_context.go`'s
  `canonicalizeIdValue`)
- already-canonical, matching concept → returns as-is
- canonical for a different concept → errors loudly (catches type-tag
  typos like passing `userId` to `canonicalId(..., space)`)
- an unimported / unknown concept name → errors at load
- empty string → returns empty (optional foreign keys stay null)

The engine ALSO auto-canonicalizes `@relationship`-tagged payload
fields at insert time (`canonicalizeRelationshipFields` in
`component/memql/partition_context.go`), so `userId == arg(...)`
queries work with canonical-stored values. But the id derivation
runs BEFORE the payload auto-canon, so `canonicalId()` in the id
template is still required for stable deterministic ids.

**`shortId(value)` also satisfies this rule, with two caveats.** It maps
the canonical and bare forms of a normal id onto the bare form, and it
takes no concept argument — so it has none of `canonicalId()`'s
resolution prerequisites (see memql#2976 for a pack where those cannot
be met). Use it when `canonicalId()` will not resolve. Otherwise prefer
`canonicalId()`, because:

- **`shortId()` cannot catch a wrong concept tag.** `canonicalId()`
  errors loudly when handed an id tagged for another concept (the bullet
  above); `shortId()` strips the tag and returns a plausible bare value,
  so a `user` id passed where a `deployment` id belongs derives a valid
  row. That check is a feature of `canonicalId()`, not an inconvenience.
- **`shortId()` is not idempotent on every input**, so on its own it does
  not collapse *every* bare/canonical pair. Measured:
  `shortId("v1:cluster:deployment:v2:x:y:z")` is `"v2:x:y:z"` while
  `shortId("v2:x:y:z")` is `"z"` — the pair forks. It strips exactly ONE
  canonical prefix, and the residual class turns on whitespace and on
  empty segments, not on segment counts alone (two attempts to state it
  more precisely than this were wrong; memql#2981 carries the measured
  predicate).

  **The rule that follows.** A bare `B` and its canonical form derive one
  id **when** `B` is a fixed point of `shortId()`. That is sufficient, not
  a biconditional, and stating it as "exactly when" is false in both
  directions: `"v1:v1:v1: "` is not a fixed point yet both spellings
  collapse to the same value anyway, and `""` IS a fixed point yet
  `shortId("v1:cluster:deployment:")` is not `""`, so the pair forks. The
  empty case is exactly the class the two earlier formulations missed;
  saying "exactly when" here would be the third.

  So where a hashed id is derived from a caller-supplied FK, constrain the
  arg with an **allowlist** — pin the prefix to that concept's own
  canonical form, and permit only characters a short id can contain:

  ```memql fragment
  deploymentId  string! @pattern("^(?:v[0-9]+:cluster:deployment:)?[A-Za-z0-9_.-]+$")
  ```

  An allowlist rather than "not whitespace, not colon", for two reasons
  found the hard way. A denylist has to enumerate `unicode.IsSpace`
  exactly, and RE2's `[[:space:]]` is **ASCII only** while `shortId()`
  trims with `strings.TrimSpace` — a guard written that way closes the
  fork for U+0020 and leaves it open for U+00A0, U+2028 and four others.
  And the `\x{...}` class that would enumerate them correctly **cannot be
  authored**: the DSL lexer rejects `\x` escapes, so that pattern parses
  as text and fails at load.

  Pin the prefix to the concept. An unpinned `v<digits>:<lower>:<word>:`
  accepts `v1:ns:Name:x`, which strips to the same short id as `x` — two
  distinct arguments on one composite id, the §20 collision this section
  is about, on the leading part instead of the trailing one.

  This is stricter than "is a fixed point", deliberately. It rejects
  values whose `shortId` is a fixed point (`"v1:v1"`, `"v1:a:b:"`) and
  that is the safe direction to be wrong in.

  That is one expression covering both halves of the residual class, and
  it keeps the canonical form this section recommends. Prefer it to
  making `shortId()` idempotent: the same primitive is the wire-egress
  bare-ifier (memql#2441), so looping it to a fixpoint changes every id
  handed to a client, and destroys more of a genuinely-bare colon-bearing
  value. Landed in memql#2981; gated by
  `TestDeploymentIDPatternClosesTheBareCanonicalFork`.

  What holds without any constraint: every SHORT id this tree mints is
  colon- and whitespace-free, so `shortId()` is exact for those and for
  the canonical forms built around them.

Compliant mutations (audit done 2026-05-06), in
`dsl/cognition/mutations.memql`:
`joinSpaceAsHuman`, `joinSpaceAsSI`, `createGreetingUtterance`,
`createSessionForParticipant`, `sendTextUtterance`,
`sendSpeechUtterance`, `sendActionUtterance`,
`sendRealtimeTranscriptUtterance`.

Compliant via `shortId()` (memql#2925), in
`dsl/deployment/mutations.memql`: `createDeploymentNodeSpec`,
`updateDeploymentNodeSpec`. `create` also **stamps** the normalised
value into the payload field rather than accepting it raw, because
`nodeSpecsForDeployment` filters on that field — a normalised key over a
raw payload value would collapse two shapes onto one timeline whose
stored value is whichever was written last. `update` is a read-merge and
does not write the field at all, so it inherits whatever `create`
stamped.

**Known exceptions — hashed FK id derivations that do NOT normalise.**
Nothing enforces this rule automatically, so it is listed here or it is
invisible:

- `createAccountEntitlement` (`dsl/identity/mutations.memql`) — hashes
  `args.accountId`, which is an FK. Pre-dates this list rather than
  being a deliberate carve-out; not yet triaged.

If you add a hashed FK id derivation that cannot normalise, add it here
with the reason. A rule with no gate and a stale roster is not a rule.

**A separate rule normalisation does not give you: the separator.**
Normalising an FK makes the same logical reference hash to one string.
It does nothing about the *composite*. Neither normaliser guarantees a
colon-free result — `shortId("d:x")` is `"d:x"` — so an unconstrained
`hash(concat(a, ":", b))` is not injective:

```
("d:x", "y")   and   ("d", "x:y")   ->   hash("d:x:y")
```

Two different pairs, one id, one timeline. Whichever wrote last wins and
the reader silently gets the wrong row.

**The rule: in `hash(concat(a, sep, b))`, every part after the first must
be free of `sep`.** The *leading* part may contain it freely. With the
trailing part separator-free the split at the last `sep` is unique, so
equal concatenations force equal parts — that is the whole of the
argument, and it generalises to any number of parts.

This matters because the leading part is usually the one you cannot
constrain. `createDeploymentNodeSpec` accepts a canonical
`v1:cluster:deployment:<short>` id on purpose, and `@pattern` validates
the **raw** arg — before `shortId()` runs — so a colon ban there would
reject the exact shape the normalisation exists to support. Constraining
the trailing part alone is both sufficient and compatible:

```memql fragment
args {
  deploymentId  string!                        // canonical or bare, normalised below
  nodeType      string! @pattern("^[^:]+$")    // trailing part: no separator
}
insert {
  id: hash(concat(shortId(args.deploymentId), ":", args.nodeType))
  ...
}
```

This is not the only way to close it, and the choice is a trade rather
than a rule. Changing the separator, or length-prefixing the parts,
closes the hazard **by construction** — the derivation stops being
ambiguous instead of the input being forbidden — but both change
`hash(...)` for every existing row, colon-bearing or not, so both are a
migration. Constraining the trailing part rejects input nobody currently
sends and leaves every derived id byte-identical, at the cost of leaving
the derivation itself non-injective and permanently dependent on the
guard.

memql#2980 took the constraint because it needed no migration. That is a
statement about what shipped, not a ruling that construction is the
wrong answer: how much an id migration costs depends on how many rows
exist, which is a question about the deployment rather than the code.

**memql#3009 took the other side of that trade, and the split between
them is the useful rule.** Reach for the constraint when the trailing
part is drawn from a **known set** — a `nodeType`, an enum — where
forbidding a character costs the caller nothing. Reach for construction
when it is not:

```memql fragment
id: concat("utt-", hash(concat(
  hash(args.partitionId),
  hash(canonicalId(args.participantId, participant)),
  hash(args.action.type),
  hash(args.action.idempotencyKey)
)))
```

`hash()` is sha256-hex, so every part renders to exactly 64 characters
and the concatenation has exactly one decomposition. No separator, no
constraint on what a caller may send, injective by construction.

`sendActionUtterance` needed it because the constraint was **both
unavailable and wrong**. Unavailable: `action` is declared `object!`, so
`type` and `idempotencyKey` are nested in an unstructured object and
`validateArgsField` only matches `patternRegex` on *declared* fields —
there is nowhere to hang the annotation and nothing would enforce it.
Wrong: `idempotencyKey` is a caller-chosen opaque string, so banning a
colon rejects `"order:123"` to work around an internal encoding choice.
**Do not push the engine's hashing problem into the caller's key space.**

Construction is also the only answer when a part is engine-derived but
separator-bearing. `hash(concat(args.nodeType, ":", now))` aliased with
no caller involvement at all, because an RFC3339 timestamp always
carries colons.

**Where the tree stands.** Every composite id derivation in
`dsl/cognition/` and `dsl/cluster/` uses construction (memql#3009); the
two in `dsl/deployment/` use the constraint (memql#2980). Both are
gated — `TestConvertedIdDerivationsKeepPerPartHashing` and
`TestCompositeHashedIdTrailingPartRejectsTheSeparator` — and **both
gates check by path, not by shape**. A new file adopting the separator
form trips neither. The tree-wide shape detector (find every
`id: hash(concat(...))`, require its parts constrained or digested) is
the durable answer, has its own false-positive design problem, and is
not built.

`@pattern` on an args field is genuinely enforced, unlike some of the
concept-field annotations: it is compiled at load (`convertArgsField`),
matched on every call (`validateArgsField`), and
`executeMutationFunctionCall` validates before rendering the template —
`engine.go`'s call is the only non-test caller of
`renderMutationTemplate`, so no call path reaches the hash unchecked.

It is enforced **server-side only**. The generated SDK carries no arg
constraints — `CreateDeploymentNodeSpecArgs.NodeType` is a bare `string`
— so a client learns about the rule from a call-time error, not from its
own types. `make sdk-gen-check` reporting no drift says nothing about
this, because the SDK has never expressed arg constraints at all.

Landed in memql#2980. Gated by
`TestCompositeHashedIdTrailingPartRejectsTheSeparator` (`dsl/`), which
checks two mutations **by name** and is not a tree-wide detector.

**The tree carries other instances of this shape, and some of them are
live examples of the hazard rather than of the fix.** `grep -rn
"hash(concat(" dsl/` finds around a dozen. Several are safe only
incidentally — the trailing part is a `canonicalId()` result whose fixed
`v1:<ns>:<concept>:` prefix happens to make the split recoverable — and
at least one is not safe: `sendActionUtterance`
(`dsl/cognition/mutations.memql`) hashes `args.action.type` and
`args.action.idempotencyKey`, which live inside an untyped `object!` and
therefore **cannot** carry `@pattern` at all, so `("chat", "k:1")` and
`("chat:k", "1")` derive one id from client-supplied input. Tracked in
memql#3009; do not read this section as a statement that the tree
complies with it.

A shape detector — find every `id: hash(concat(...))` and require its
trailing parts to be constrained — is the gate that would make the rule
true tree-wide. It does not exist yet.

The historical `concat("ga-", hash(actor))` pattern in the auto-join
path is gone entirely: the logic (`dsl/cognition/logic.memql`) now
resolves the assistant via `assistantAgentForUser` + the space
row's `ownerUserId` (memql#273, locked in by
`TestAutoJoinSILocksInOwnerUserIdResolution`), and shortId prefixes
like `ga-` are banned by `TestNoShortIdConceptPrefix`.

---

## 21. Argument resolution: `args.X` for caller-passed, bare names for engine

**Rule.** Every DSL construct declares its inputs through one of
three canonical forms:

- **Struct query / mutation**: `args { ... }` sub-block INSIDE the
  construct body.
- **Logic**: `args { ... }` block inside the construct body, ahead
  of `body { ... }` (`logic NAME { args { ... } body { ... } }`).
- **Automation**: no declared args — the triggering event is bound
  as `args` (see below).
- **Builtin / tool / prompt**: body fields directly — the body IS
  the schema (no `args` wrapper).

The body references caller-passed args as `args.X`. Engine-provided
values use bare top-level identifiers: `now`, `actor.X`, `partition`,
`config.X`. `ctx` is gone from the author surface entirely; the
rewriter translates `args.X` -> `ctx.X` for the engine runtime so
nothing changes underneath.

**Reserved engine names** (an args field colliding with one of these
is rejected at load time): `now`, `actor`, `partition`, `config`,
`trace`.

**An arg description is a `///` doc comment, never `@description`**
(memql#3336). The `///` block on the line(s) immediately above an
`args { ... }` field IS that argument's description: it lands on the
field's AST slot, and it is what the corpus, the SDK generator, and the
LSP's `memql/runnableConstructs` read. `@description` on an args field
is **rejected at load** -- there is no AST slot for it, so it used to be
accepted and then silently thrown away. The identical annotation on a
`tool` / `prompt` / `builtin` field is untouched: those bodies ARE the
schema and do retain it. Same de-overload as `@default` on an args
field, which is likewise rejected (#991).

```memql retired
query space activeSpaces {
  args {
    /// The owner whose spaces to list.
    ownerId string @required                        // correct
    limit   number @description("page size")        // REJECTED at load
  }
  filter  ownerId == args.ownerId && isActiveRecord
  shape   spaceFull
}
```

The declaration-level `@description` on the construct itself stays
load-bearing (it is the fallback for the construct's own `///` block).
To fix an existing file, run `memqlmigrate --rewrite=args-description`,
which strips the annotation; re-add the prose as a `///` comment.

**Right (struct form — the canonical author surface):**

```memql
use cognition.concepts.{ utterance, space, participant }
use common.traits.{ isActiveRecord }

/// Insert a chat utterance
mutate utterance sendUtterance {
  args {
    spaceId  string  @required
    content  string  @required
  }
  insert {
    space:     args.spaceId
    content:   args.content
    createdAt: now
    createdBy: actor.userId
  }
}

/// Active spaces visible to caller
query space activeSpaces {
  args {
    ownerId  string  @required
  }
  filter  ownerId == args.ownerId && isActiveRecord
  shape   spaceFull
}

// Spec — struct form. Binds one shape XOR concept in the signature;
// the body returns a boolean over bare field names. No args.
spec participant isGuestParticipant {
  return isGuest == true
}
```

**Policies take no args at all.** The live `policy` construct is an
empty-bodied AI provider-selection record (the decision-policy tier
that once carried `func (Policy)` bodies with `@tier` / `@audited`
is retired, #984 — caller-context boolean checks belong in
context-specs named as bare filter conjuncts):

```memql
@primary("streamClaudeSonnet")
@fallback("stream54Pro")
/// Default chat policy for non-operator agents.
policy balancedChat { }
```

**Wrong (rejected at registration):**

```memql retired
// Legacy func (Spec) form — specs are struct-form now.
func (Spec) example(ctx any) bool {
  return true
}

// args.X is the only way to reach caller-passed fields.
mutate space example {
  args { x string @required }
  insert {
    field: ctx.x   // ctx is not in scope inside struct-form bodies
  }
}
```

**Procedural form (internal post-rewrite shape, not for authors).**
The struct-form rewriter emits a `func (Receiver) NAME(ctx any)
(any, error) { return <expr>, nil }` shape for the engine parser.
The `ctx` parameter name is a placeholder identifier only; the body
references `args.X` directly (the parser recognises both `args.X`
and `ctx.X` and resolves them to the same caller-arg AST node).
**Don't author that shape.** The struct form is the surface every
author works with.

For Logic bodies the author surface is ctx-free: write
`body { ... ; return <expr> }`, reach inputs via `args.X`, never
write `ctx.output = ...`.

**Why `args.X` is required (not bare).** In a mutation's `insert`
block, the keys ARE bare field names of the row's payload. Saying
`spaceId: args.spaceId` keeps the LHS (concept payload key) and RHS
(caller arg) visually distinct. The same precedent applies to query
filters: `spaceId == args.spaceId` reads correctly without
needing the reader to guess which side is concept-field vs caller-arg.

**For automations:** the triggering event payload is bound as
`args`, so `args.topic`, `args.kind`, and `args.payload.<field>`
reach the event from inside the automation body.

---

## 21b. Nested object blocks are CLOSED

A nested block that declares sub-fields rejects undeclared keys, exactly as the
top level always has (memql#3641):

```memql
concept user {
  preferences {
    theme               enum("light", "dark", "system")
    computerUseEnabled  bool  @default("true")
  }
}
```

`{"preferences": {"computerUseEnbaled": true}}` is now REFUSED. Before the flip
it was stored: the typo sat beside the real field, nothing read it, and the
computer-use kill switch kept its old value with no error at either end
(memql#3623). A key nothing can read is not data, and storing it only delays
the discovery.

Two things this does NOT touch:

- **A bare `object` field** (`payload object`) declares no sub-fields, so it is
  free-form by construction and unaffected.
- **`@variant` arms**, whose fields live in their own `oneOf` branch.

`@open` is the escape, for a block that is free-form BY DESIGN -- keys as data
rather than schema. It takes the typed spelling, because a block-bodied
property accepts no annotation in either other position (memql#3623,
memql#3692):

```memql fragment
metadata  object  @open {
  knownKey  string
}
```

Reach for a DECLARATION first. Nearly every block that had an undeclared key
when this landed wanted the key declared, not the block opened -- the sub-field
was real, and the schema had simply stopped describing the row.

## 22. Tree-wide conformance gates (`test/dslconformance/conformance_test.go`)

CI enforces a set of static rules over every loaded `.memql` file.
A PR that violates any of them fails before the engine ever parses
the change. The gates, with their test names:

> **Contract gates run at LOAD time, not only in CI** (memql#3629).
> Five of the gates below -- retired operator forms, the two `row.`
> namespace rules, the per-row authz user-scope bucket, and the admin-gate
> composition rule -- live in `component/memql/dslgate` and are run by
> `MemQLEngine.Init` over the merged tree. A violation lands on the
> `LoadReport` and **strict boot refuses it**, with `MEMQL_DSL_ALLOW_SKIPS`
> as the operator break-glass, exactly like a construct that fails to parse.
>
> That is what covers a **product DSL bundle** delivered at runtime through
> `MEMQL_DSL_PATH` -- the primary delivery path under platform consolidation
> (memql#2472), and a tree no Go test in this repo ever walks. `cmd/memqllint`
> drives the same `Init`, so a bundle author gets the verdict offline before
> the deploy rather than as a CrashLoop after it.
>
> The tests below run the **same detector** over this repo's corpus rather
> than a second copy of the rule: the recurring defect in this area is two
> detectors drifting, always fail-open (memql#2779, memql#3612, memql#2875).
> The remaining gates are house style -- naming, redundant annotations,
> canonical short forms -- and stay test-only on purpose: failing a fleet's
> boot over a convention would be worse than the convention drifting.

- **Canonical filter prefixes** (`TestFilterSyntaxCanonical`).
  Filter predicates reference payload fields as `<field>` -- bare, with
  no prefix. The `payload.<field>` and `<conceptName>.<field>` forms are
  both rejected.
- **Row intrinsics use the `row.` namespace**
  (`TestFilterIntrinsicsUseRowNamespace`, memql#2779). In a filter the
  row envelope is addressed through `row.` -- `row.id`, `row.concept`,
  `row.type`, `row.createdAt`, `row.createdBy`, `row.provenance.<leaf>`.
  The bare spelling (`filter id == args.x`) is retired.

  Why: a filter mixes two field surfaces under one syntax. Payload
  properties are bare, so a bare `id` is indistinguishable from a payload
  property by shape alone -- yet the two compile to completely different
  SQL (a table column vs a JSONB path). `row.` names the envelope
  explicitly and lines the filter up with the namespaces you already
  write (`args.X`, `actor.X`, `config.X`) and with shape bodies, which
  have always projected `row.id` / `row.createdAt`.

  ```memql fragment
  filter  row.id == args.clusterId      // correct -- the row envelope
  filter  id == args.clusterId          // rejected -- bare intrinsic
  filter  region == args.region         // correct -- payload property, bare
  filter  row.region == args.region     // rejected -- not a row intrinsic
  ```

  Scope: **filter predicates.** A spec/trait body reads its
  signature-bound fields bare and rejects `row.*` outright (epic #2281) --
  the binding lives in the signature there. Mutation `insert` / `update`
  blocks write `id:` / `createdAt:` as target keys rather than
  references, and are unaffected. Sort keys are covered by their own gate,
  below.
- **Sort keys use the `row.` namespace**
  (`TestSortKeysUseRowNamespace`, memql#2786). The ordering half of the
  rule above: in an authored sort clause the row envelope is addressed
  through `row.`, and the bare spelling is retired.

  Why: the same ambiguity. `sort "id"` can name the row id or a payload
  property called `id`, and the two compile to completely different
  `ORDER BY` expressions -- a table column vs `payload #>> '{id}'`.

  ```memql fragment
  sort  "row.createdAt", "desc"   // correct -- the row envelope
  sort  "createdAt", "desc"       // rejected -- bare intrinsic
  sort  "version", "desc"         // correct -- payload property, bare
  ```

  `provenance` has no sort form: it is object-valued with no ordering, so
  `row.provenance` is rejected outright.

  Scope: **authored `.memql` only.** The runtime and SDK sort surfaces
  keep accepting bare keys from callers -- `compileSortField` still
  resolves them -- exactly as the filter gate leaves the runtime filter
  surface alone.
- **Mandatory trait specs** (`TestNoInlineTraitablePredicates`).
  When a trait in `dsl/common/traits.memql` covers a predicate, the
  filter must call the trait, not inline the comparison:
  `isActiveRecord` (not `active == true`),
  `isNotDeleted` (not `deleted != true`),
  `statusIsActive` (not `status == "active"`), and so
  on for the status / identity-type / deletion-scheduled traits.
  Concept-specific predicates (`ownerUserId == args.userId`)
  stay inline.
- **No concept-name shortId prefixes** (`TestNoShortIdConceptPrefix`).
  Derived ids are the bare unique part (uuid / hash / slug) — never
  `concat("agent-", ...)` or another concept-name / sub-type prefix.
  See [#20](#20-foreign-key-id-derivation-normalise-before-hashing).
- **Typed @relationship targets** (`TestRelationshipTargetsUseImports`,
  memql#1067). `@relationship(..., target=user, ...)` names an
  imported concept; the `target="v1:..."` canonical-string form is
  rejected.
- **Per-row authz classification** (`TestPerRowAuthzClassification`).
  Every query / mutation that touches a user-scope field
  (`ownerUserId`, `userId`, `createdBy`, ...)
  must either carry a caller-scope check (`actor.userId` in the
  filter / write), an admin gate (`actor.isClusterOwner == true`, or an admin
  context-spec such as `requiresAdmin` / `requiresOwnerOrAdmin`, named
  as a bare top-level conjunct), or an explicit `@public` annotation
  acknowledging the intent. Anything else hard-fails.
- **Actor vocabulary** (`TestNoCallerVocabulary`, #221). `caller.X`
  and `@caller` are retired; write `actor.X` and `@actor`.
- **Pagination authoring rule** (`TestPaginationAuthoringRule`,
  memql#1965 — see [#23](#23-list-returning-queries-must-declare-their-bound)).
  Every list-returning query declares `paginate` / `sort` or
  `@unbounded("reason")`. **Enforcing** (since the issue 5.3 backfill,
  memql#1967): the tree-wide hard-fail trips on any unmarked list query,
  so a freshly-authored list read with no bound fails CI.

Companion gates in sibling files lock in the operator and binding
grammar:

- `TestNoRetiredOperatorForms` (#977,
  `test/dslconformance/no_retired_operators_test.go`): filters use the single Go
  boolean grammar — `&&` / `||` with parens (no `!`: it parses and is
  then refused at load on every converter surface, memql#3630). The
  `;`-AND and `,`-OR separators, the `has` membership operator (use
  `in`), and the `?.` optional-chain prefix (use `when(args.x) { ... }`)
  are rejected **by this gate**, which is a line-oriented TEXT SCAN over
  the embedded `dsl/` tree — not by the parser, which still accepts all
  four, and not by the engine, which still computes `;` as AND and `,`
  as OR. That is why a `,` inside parentheses was an authorization
  bypass and was closed here rather than in the grammar (memql#3612).
  The scan itself now runs at load time over whatever tree the node
  mounted, so a product bundle at `MEMQL_DSL_PATH` is covered too
  (memql#3629); this test runs it over the embedded corpus.
- `TestNoInfixWordAndOr` (#973,
  `test/dslconformance/no_word_logical_operators_test.go`): the English `and` / `or`
  infix forms are rejected.
- `TestNoRetiredBindingForms` (#988, `test/dslconformance/no_named_writes_test.go`):
  named writes (`insert <concept> {` / `update <concept> {`) are
  rejected — the write target comes from the
  `mutate <Concept> <name>` signature, the block is bare
  `insert {` / `update {`. `canonicalId(x, "v1:ns:name")` string
  literals and `concat("v1:ns:concept:", id)` are rejected — pass
  the imported concept short-name.

---

## 23. List-returning queries must declare their bound

**Rule.** A **list-returning** query must declare how it is bounded:
either a `paginate` / `sort` directive, or an explicit
`@unbounded("reason")` annotation. A query that pulls a row set with
no bound silently fetches the whole table — the trap this rule exists
to stop (epic 5, memql#1965).

**What counts as "list-returning" (the exact, deterministic rule).**
A query is list-returning when its `shape` projects a row set
**without a unique-key equality filter**. Concretely:

- **Single-row read — EXEMPT.** The filter contains a `row.id == <expr>`
  equality on the row's primary intrinsic. It reads at most one row, so
  it is not a list. A *guarded* `when(args.x) { row.id == ... }` does
  **not** count — the id filter is conditional, so the query can still
  return the full set when the arg is omitted.
- **Aggregate — EXEMPT.** The query carries a `count` clause. It returns
  a `{count: N}` number, not rows.
- **Bounded list — COMPLIANT.** The query declares `paginate` (an
  explicit window) or `sort` (an explicit ordering — "give me the
  latest N").
- **Unbounded-marked — COMPLIANT (and auditable).** The query carries
  `@unbounded("reason")`. The reason is **required**; it documents why
  the full set is a legitimate read (small bounded catalog, sweep job,
  etc.) and is enumerated by the audit report.
- **Unmarked list — VIOLATION.** None of the above. This is the set the
  rule targets.

```memql
// Single-row read — exempt (row.id == equality).
query space spaceMeta {
  args { spaceId string @required }
  filter  row.id==args.spaceId
  shape   spaceFull
}

// Bounded list — compliant (paginate window).
query space firstTenSpaces {
  filter  active==true
  paginate 10
  shape   spaceFull
}

// Legitimate full-set read — compliant, marked + auditable.
@unbounded("provider catalog is a small bounded set — never more than a handful of rows")
query provider allProviders {
  filter  isActiveRecord
  shape   providerFull
}

// VIOLATION — list read with no bound. Pulls the whole table.
query widget allWidgets {
  filter  ownerUserId==args.ownerUserId
  shape   widgetFull
}
```

**Runtime backstop (always on, ships independently).** Even if an
unmarked list query slips past authoring, the engine applies an implicit
**`LIMIT 50`** to any query that arrives with no explicit window — no
`paginate` and no `sort` — so nothing pulls unbounded. A query that
paginates / sorts states its own window and is unaffected; an
`@unbounded("reason")` query is rewritten to an explicit large paginate
and bypasses the 50-cap (still clamped to `MEMQL_MEMORY_ENGINE_MAX_WINDOW`).
The cap is tunable via `MEMQL_MEMORY_ENGINE_DEFAULT_LIST_CAP` (clamped to
`<= MEMQL_MEMORY_ENGINE_MAX_RESULTS`). Lives in
`component/memql/engine.go` (`defaultListLimit`) +
`component/memql/config.go`.

**`@unbounded` is mutually exclusive** with `paginate`, `sort`, and
`count` — a query that already paginates / sorts is bounded, and a
`count` returns an aggregate. The rewriter rejects the combination at
load time.

**Audit + enforcement.** The classifier lives in
`component/language/pagination` and is the single source of truth for
the rule. Two consumers derive from it:

- `go run ./scripts/audit-pagination` — the live report. Lists every
  unmarked-list query (the backfill work item) and every `@unbounded`
  query with its reason. `--json` / `--unmarked` / `--unbounded` /
  `--strict` / `--domain=<x>`.
- `TestPaginationAuthoringRule` in `test/dslconformance/conformance_test.go` — the gate.
  **Enforcing** (the issue 5.3 backfill, memql#1967, marked every list
  query in the tree and flipped this gate on in the same merge, so `main`
  stayed green). It hard-fails on any unmarked list query, asserts the
  classifier detects a brand-new unmarked list query, and checks every
  `@unbounded` mark carries a non-empty reason.

**Why it bites you.** Without a bound, a query against a growing concept
(participants, events, plans) starts cheap and silently degrades into a
full-table scan as the data grows — no error, just creeping latency and
memory. The runtime cap bounds the blast radius; the authoring rule makes
the author think about the bound up front.

---

## Result caching: `@cache(N)` on hot reads

**Caching is ON by default, and an annotation changes the number rather
than switching the feature on.** A pure read carrying no annotation is
cached for 60 seconds (memql#1970). Write `@cache(N)` to choose a
different TTL and `@nocache` to opt out.

`N` is a **whole number of SECONDS**, written positionally:
`@cache(300)`. That is the preferred form since memql#2618 and the form
every live call site in this repository uses. The older keyword spelling
`@cache(ttl="300")` still parses -- the parser reads the `ttl` argument
first, then a bare string, then a positional number -- so an old diff
showing it is not broken; it is simply not what to write. A duration
string (`@cache("5m")`) is not a supported form.

```memql
@cache(300)
query agentRole activeAgentRoles {
  filter  isActiveRecord
  shape   agentRoleFull
}
```

**When to reach for it.** Read-heavy queries whose underlying rows change
rarely: bounded catalogs / registries (role / skill catalogs, router
budgets) get long TTLs; hot append-only streams (the per-space utterance
list) get short ones and lean on invalidation.

**When to reach for `@nocache`.** Rarely, and never on a hunch. Because
every write publishes an invalidation for the concept it wrote, the
common worry -- "this read must not go stale" -- is usually already
answered. Reach for it when the read's answer can change with **no write
to a concept the read depends on**, because that is the one case
invalidation structurally cannot cover. Two shapes qualify:

- **Time-boundary reads.** A filter like `timeoutAt < now` admits new
  rows as the clock advances, with nothing written. The TTL is then the
  only freshness mechanism there is.
- **Rows that arrive without an engine write.** Anything written by raw
  SQL rather than through a mutation publishes no invalidation event --
  the observability rollups are the standing example.

Every `@nocache` carries a one-line reason comment saying which of these
applies. "It felt risky" is not one: a `@nocache` on a hot read is a
permanent cost paid on every call, and the reason is what a later reader
needs in order to remove it safely.

**Correctness — invalidation (5.4).** A write to a cached query's read
concept evicts the dependent cached results, so a cached read never
outlives a row it depends on. You do not annotate the eviction; it is
keyed off the concept the query reads.

**Correctness — cross-node.** Each node runs its OWN result cache, so a
write on one replica has to evict the cached read on every sibling or a
stale answer survives there. Epic 5 issue 5.5 handled this with a
routing rule PER cached concept -- an author adding `@cache` had to also
register a `node.RegisterRoutingRule` forwarding that concept's
`graph.node.created/updated/deleted` writes, or the eviction silently
stayed local. **That per-concept rule is retired (issue 5.6,
memql#1970)** and pinned as gone by
`TestEvaluateRouting_PerConceptCacheRulesRetired`
(`component/node/routing_test.go`).

> **How that test measures it changed in memql#4542, and the change is
> worth knowing about if you read it.** It used to assert the invariant
> through a proxy -- it listed the topics the retired rules forwarded and
> required all of them to be dark -- which was sound only while nothing
> else wanted them. memql#4542 added browser-reach rules for `v1:agents:*`
> and for cognition deletes, so several of those topics forward again for
> a reason that has nothing to do with caching. The invariant is unchanged;
> `v1:router:budget` is now the witness, being the one concept in the
> retired set that no surface subscribes to. **A forward rule for a
> concept is no longer evidence that somebody is caching it** -- do not
> read one that way.

Every graph write now also
publishes a dedicated `cache.invalidate.<concept>` event
(`MemQLEngine.InvalidateCacheForConcept`), and ONE broadcast routing rule
(`{Pattern: "cache.invalidate.*", TargetType: ""}` in
`component/node/routing.go`) forwards that channel to every node type --
pinned by `TestEvaluateRouting_CacheInvalidateBroadcast`. So `@cache` now
Just Works cross-node for any concept, with **no routing rule to add**:
the broadcast channel is what evicts the cached read on every sibling
replica. A single-node green test still would not have caught the old
per-concept gap, which is why both the retirement and the broadcast
replacement are gated by name rather than left to review.

**Correctness — the writing node.** A write evicts its concept's
dependent entries on the node that handled it SYNCHRONOUSLY, before the
write's response is observable, so a client that re-reads immediately
after its own write is never served the pre-write result (memql#4531).
The eviction is surgical: a cached read of a different concept survives
the write. It used to be a full `cache.Clear()`, which made the writing
node's cache useless and hid the read-your-writes question entirely.

**Keyset cursors.** The cache key includes the paginated query's cursor
(`engine.go` `cacheKey`), so distinct continuation pages of a `@cache`'d
query key independently — page 2 never collides with the cached page 1.

**Actor identity.** When a plan depends on the caller — an owned read,
a role-gated read, or any read row-authz enforcement has narrowed — the
resolved actor is folded into the cache key. Two users can never share
one entry for a query that answers differently for each of them.

**Is it working?** `curl :PORT/metrics | grep memql_result_cache` gives
hits, misses, evictions, and whether invalidation is reaching this
replica; `query_reads_total{query="..."}` gives one query's hit ratio
(memql#4532). `/metrics` is in-cluster-only by design.

See `component/memql/result_cache.go` and `result_cache_policy.go` for the
cache's shape and instrumentation, and
[caching-and-live-data-architecture-design.md](../../superpowers/specs/2026-08-25-caching-and-live-data-architecture-design.md)
for the reviewed adoption table -- which reads carry an explicit TTL
today and why the rest ride the default.

---

## 24. Changing the grammar requires negative-syntax cases

**Rule.** Any change to the DSL grammar, a construct parser, or the
load pipeline -- a new construct kind, a new/removed annotation, a new
or retired operator, a changed body / signature / args rule, a new
invocation form -- MUST land with **negative-syntax cases** proving the
malformed or now-illegal forms FAIL loud. "Fail loud" means one of: a
parse error, a `memqllint` / `dslimports.Load` diagnostic, or a
WARN-level load skip. Silent acceptance is never acceptable -- a typo,
a stale annotation, or a retired operator must produce an error that
names the construct and (where the pipeline supports one) a position.

**Where the cases go** (the systematic negative-syntax conformance
suite, epic #2351 / memql#2383):

- `component/language/parser/negative_grammar_test.go` -- parser /
  expression-level rejections (malformed bodies via the `Parse<Kind>Decl`
  entry points, body-rule + signature-arity violations, unknown
  annotations, invocation-site errors, trailing tokens, word logical
  operators). This package cannot import `component/language/compiler`
  (import cycle), so rewriter-family kinds (query / mutate / logic /
  automation) are exercised via `NormaliseAll` + `ParseFile`.
- `component/memql/negative_load_test.go` -- load / lint-level and
  slicer-level rejections driven through `dslimports.Load` (the same
  path `cmd/memqllint` and engine startup take): a malformed body per
  construct kind, unbalanced braces, a typo'd top-level keyword, a
  construct nested at the wrong depth, and duplicate names (caught as a
  WARN-level runtime-loader skip).

Both files need no database, but both live in workspace modules
(`component/language`, `component/memql`) that a bare `go test ./...`
from the repo root does not compile into its test binary (memql#4032) --
run `make test` instead, which is what CI actually runs. If a change
reveals a NEW silent-acceptance hole
the change itself does not close, pin it as an explicit `t.Skip` case
marked `HOLE` (with an issue pointer) so the gap is tracked and visible
rather than forgotten.

**Why it bites you.** The 2026-07-03 syntax audit (epic #2351) found
every hole in this class *empirically* -- garbage spec / shape bodies
loading silently, unknown kind prefixes dropping calls, trailing tokens
accepted -- because no test ever asked "does this malformed input
error?". A grammar change with only positive ("this valid form parses")
tests re-opens exactly that gap: the happy path stays green while a typo
becomes a silent semantic change. The negative suite is the standing
question "does bad input fail?" -- keep it answering "yes" for every
kind you touch.

**Enforcement.** The two suites above are the gate; a construct kind or
operator added without a matching negative case is the defect this rule
targets. See also [#22](#22-tree-wide-conformance-gates-dslconformance_testgo)
for the complementary tree-wide gates that scan the live `.memql` tree.

---

## How to add a new entry

When you discover a new gotcha:

1. Add a numbered section here.
2. Include: the rule, the wrong example, the right example, the
   actual error message you saw, and a one-line "why it bites you".
3. Reference any code paths that enforce / exhibit the rule.
4. Cross-link from the directory-specific CLAUDE.md if relevant.

If a rule starts feeling like architecture (rather than a trap),
promote it to `docs/public/concepts/architecture.md` or `docs/public/language/memql.md` and leave
a stub here pointing to it.

## Grammar versioning + the migration channel (S6, memql#2361)

The engine grammar has an explicit epoch: `parser.GrammarVersion`
(`component/language/parser/grammar_version.go`), printable via
`memqlmigrate --grammar-version`. It is stamped into:

- **authored-construct rows** (`v1:authoring:construct.grammarVersion`) at
  promote time -- re-hydration on a NEWER engine quarantines a mismatched
  row with an error naming the migration command, instead of the stale
  source degrading into an arbitrary parse warning;
- **release lockfiles** (`releases/<ver>.yaml` `grammarVersion:`), so a
  pack or deploy consumer can detect which grammar a release's DSL was
  authored under.

**The migration channel is the codemod-per-epic pattern.** Every grammar
epic (a change that retires or reshapes an authored form) MUST:

1. bump `parser.GrammarVersion` (the pinned keyword-fingerprint test
   forces this when the invocation surface changes);
2. ship a `memqlmigrate --rewrite=<epic>` mode that mechanically migrates
   authored source (precedents: `scripts/migrations/construct_invocation/`,
   `scripts/migrations/event_payload_args/`);
3. reject the retired form at parse time with a hint naming that command.

A stale pack or bundle is therefore always detectable (version stamp),
diagnosable (rejection-with-hint), and mechanically fixable (the rewrite
mode) -- never a silent soft-skip.

## Reserved args-field names

`now`, `actor`, `partition`, `config`, and `trace` are ambient top-level
identifiers every body may read; an `args { }` field of the same name is
REJECTED at parse time (rename it -- e.g. `asOf` for a caller-passed
evaluation instant). `event` is additionally reserved for AUTOMATION args
by the load-time shadow check (G2, memql#2364).

---

## 25. Import scope: same-domain constructs are ambient (#2617)

**Rule.** A construct never imports its OWN domain. Constructs of the
file's containing `dsl/<domain>/` directory are ambient -- in scope
with no `use` line: the loader seeds the concept-resolution namespace
hint from the directory, canonicalId() accepts ambient same-domain
concept short-names, and the editor suppresses same-domain import
suggestions. `use` stays required cross-domain, and is now enforced
(#2755) by `test/dslconformance/cross_domain_use_test.go`.

The asymmetry is about information, not consistency. By the flattened
one-file-per-kind layout a same-domain name can only come from one
place, so the import carries nothing -- but nothing about
`isActiveRecord` tells you it lives in `common`, so there the import is
the only thing that says so.

The cross-domain gate checks the references whose bare spelling really
is a construct reference: a trait or spec invoked in a `filter` clause,
and a shape named in a `shape` projection clause. It deliberately does
NOT check bare identifiers that resolve to a concept -- a bare name in a
filter clause is a payload field (`filter surface!=""` reads the payload
field `surface`, not the `v1:actions:surface` concept), and 10 construct
names double as payload field names across the tree.

A reference to a construct declared NOWHERE in the tree is skipped
rather than flagged: a product bundle mounts extra domains at boot via
`MEMQL_DSL_PATH`, and you cannot import what does not exist at lint
time. The rule is "if the tree can see it in another domain, name it" --
never "every reference must resolve here".

```memql fragment
// dsl/planner/queries.memql -- `plan` is ambient (v1:planner:plan),
// even though v1:harness:plan shares the trailing segment.
query plan plansForSpace {
  ...
}

// Cross-domain names still need the import:
use cognition.concepts.{ space }
```

**Constraints.**

- **Explicit wins.** A file-top import of a name always beats the
  ambient resolution -- importing `harness.concepts.{ plan }` from
  inside `dsl/planner/` binds the harness concept.
- **Ambiguity stays an error.** When the domain hint cannot single
  out one concept (colon-scoped sub-namespaces can still collide),
  the loader reports the ambiguity by name -- exactly as it always
  did for unhinted collisions.
- **Explicit same-domain imports keep parsing** (the rule is
  additive), but the tree gate (`test/dslconformance/no_same_domain_use_test.go`,
  which runs the `memqlmigrate --rewrite=same-domain-use` codemod)
  keeps them out of the shipped corpus.

---

## 26. Reading `actor.*` requires `@actor` (#2621)

**Rule.** A query / mutation / logic / automation whose body reads the
auth envelope (`actor.*`) must declare `@actor` in its preamble --
used-but-undeclared is a file-attributed load error AND an edit-time
`actor-undeclared` Error squiggle (#2622, same shared detection).
Declared-but-unused is legal. Spec/trait bodies keep the inverse rule
(direct reads rejected; bind an `@actor` shape); shapes use `@actor`
as their kind marker; the seed-file `@actor("system")` is a different
construct. An unknown member (`actor.displayName`) is likewise a load
error and an `actor-unknown-property` squiggle (#2625): the envelope is
a closed set, and both layers read the same canonical table.

---

## 27. `!=` matches rows where the field is ABSENT (#1685 / #2783)

**Rule.** A `!=` predicate is null-safe: a row whose payload lacks the
field **matches**. A `==` predicate does not. The one exception is
`!= ""`, which excludes absent fields.

```memql fragment
filter deleted != true      // matches rows with NO `deleted` key
filter status == "active"   // does NOT match rows with no `status` key
filter consumedAt != ""     // does NOT match rows with no `consumedAt` key
```

**Why.** Both directions were bugs before they were rules:

- Plain SQL `<>` yields NULL, not true, when the field is missing, so
  `deleted != true` silently DROPPED every row that never had a
  `deleted` key -- the concept `@default` is not always stamped
  (#1685). Hence `IS DISTINCT FROM`.
- An absent string field is logically equal to `""` -- both mean "not
  set" -- and `!= ""` is the canonical *is set* idiom
  (`deletionScheduledAt != ""`, `consumedAt != ""`). Under the bare
  #1685 rule those returned every unset row (#1708 / #1714). Hence the
  `COALESCE(expr, '') <> ''` carve-out.

The SQL push-down and the in-process post-filter implement both rules
identically, and must continue to: a combined-filter query scans in SQL
and then re-filters in process, so any disagreement means the rows you
get depend on which path ran. `absent_field_comparison_test.go` pins
all of it, including that agreement.

**The trap this creates.** A misspelled property in a `!=` predicate is
ABSENT, so it matches **every row**:

```memql fragment
filter delted != true       // typo -- matches everything, including deleted rows
```

The failure direction is the dangerous one. The same typo in `==`
returns zero rows and someone notices immediately; in `!=` on an
authorization- or deletion-scoped filter it quietly serves rows that
were meant to be excluded.

Note the rule is specific to `!=`. `not in` and the ordered
comparisons (`<`, `>`, `<=`, `>=`) all treat an absent field as a
NON-match, on both paths -- so `deleted not in [true]` does NOT behave
like `deleted != true`.

**What to do about it.** Prefer the trait over an inline predicate --
`isNotDeleted` rather than `deleted != true`. The conformance gate
already requires this wherever a trait exists (rule 22,
`TestNoInlineTraitablePredicates`).

**A misspelled FIELD is now caught before it ships**, and so is a
misspelled trait in a `use` line. Three referential lanes cover those
three positions, each verified by injecting the typo into a shipped
construct. A misspelled trait at a *call site* is the position they
miss -- see below:

| you misspell | reported as |
|---|---|
| a payload field in a `filter` (#2781) | `query "expiredWorkerInvocations": filter compares field "actionTYPO", which concept "invocation" does not declare` |
| a field inside a spec body (#2804) | `spec "requiresAdmin": body reads field "rolle", which shape "actorEnvelope" does not declare` |
| a trait's name in a cross-domain `use` | `use common.traits: "isNotDeletd" is not declared in common/traits.memql` |

**Where these run matters.** The field lanes (rows 1 and 2) live in
`memqllint` and in the real-tree test that drives them
(`TestVerifyReferentialIntegrity_RealDSLTree`, in
`component/memql/dslimports`). `component/memql/dslimports` sits in a
workspace module that `go test ./...` from the repo root does not
compile into its test binary (memql#4032, see the CLAUDE.md Testing
section) -- so `make test` catches these two lanes but
`go test ./dsl/...` alone stays green, and the `dsl` package is the
natural place to look. Row 3 is caught by both, since the cross-domain
import gate (rule 25) also runs in `./dsl/...`.

**The typo shape that is still silent is a misspelled trait at the CALL
SITE.** Row 3 catches a name misspelled in the `use` line. It does not
check that call sites match what was imported -- so this is silent:

```memql fragment
use common.traits.{ isNotDeleted }        // correct

filter  planId==args.planId && isNotDeletd    // typo -- nothing reports it
filter  ownerUserId==args.ownerUserId && isNotDeleted   // sibling keeps the import "used"
```

Verified: `memqllint`, `go test ./dsl/...` and the referential tests all
stay green. It surfaces only when the typo orphans the import entirely
(every call site misspelled), which reports the import as never
referenced. A same-domain trait is the same hole with no import at all
to orphan -- rule 25 (#2617) makes it ambient, so there is nothing for
the import lane to inspect.

It does fail **closed**: at query-parse on first call the query errors
`unknown spec "isNotDeletd"` rather than quietly returning every row. A
loud runtime error, not a build-time gate.

**What field validation structurally cannot see** is a field that is real
and declared but scoped from the wrong *source*: `filter
row.id==args.userId` is a well-formed reference to an existing property
-- it just trusts an argument where it should have trusted the token.
That is #2799 / #2800 / #2803, not this rule. (Note the `row.` prefix:
the bare-intrinsic spelling is retired by rule 22.)

So the fail-open direction above is real but mostly out of reach of a
typo now. It remains reachable by a caller-supplied id, and by a field
declared on the concept that is simply absent on a given row -- which is
the case the null semantics get deliberately right.

## 28. Whitespace changes what `-` means; a fraction needs its leading `0` (memql#3624)

Two lexical traps in the same neighbourhood, one fixed and one still live.

**Fixed: a fraction MUST be written with a leading digit.** `.5` used to
lex as an identifier, so it reached the comparison as the *string* `".5"`
while `0.5` reached it as the float:

```memql retired
filter score > .5       // REJECTED at load since memql#3624
filter score > 0.5      // correct -- and always was
```

Error: `a number cannot start with '.' at line N, column M: a decimal
literal needs a leading digit (write "0.5", not ".5")`. Rejecting rather
than accepting `.5` as a second spelling keeps one canonical form per
construct, matching the rest of this document.

**Still live: `-` glued to an identifier is absorbed into the name.**
`-` is a legal identifier character (the seed catalog's kebab-case slugs,
`seed skill workbench-baseline`, and the capability-script argument names
in `dsl/install/actions.memql`), so a hyphen with no surrounding space
becomes part of the identifier instead of an operator:

```memql fragment
filter remaining == total-used     // compares against the STRING "total-used"
filter remaining == total - used   // subtraction -- what was probably meant
```

The neighbourhood is asymmetric, and only the middle row is silent:

| spelling | tokens | outcome |
|---|---|---|
| `a - b`, `a -b` | `a` `-` `b` | subtraction |
| `a-b` | `a-b` | **silent identifier** |
| `a- b` | `a-` `b` | loud |
| `a -5`, `5-3` | two operands | loud |

**Why it bites you.** The two readings are character-for-character
identical; only *position* separates a name from a subtraction, and the
lexer does not know position. No lexical rule can tell them apart without
either breaking the 189 hyphenated names the tree already depends on or
silently flipping which reading wins -- both were measured in memql#3624
and rejected. **Always put spaces around a `-` you mean as an operator.**
The reasoning, the candidate rule that was tried, and what it broke are
recorded at `case '-'` in `component/language/parser/lexer.go`.

## 29. A prompt's declaration must cover its template AND name a real provider (memql#3616)

**Rule.** Two load-time checks now guard the `prompt` construct:

1. Every root-scope variable the `.tmpl` reads must be declared in the
   prompt body. A prompt whose template reads an undeclared input is
   **refused at load** (a strict-boot skip).
2. `@defaultProvider("...")` must name a `provider` the DSL tree
   declares. A name nothing declares -- a typo, or a **policy** slug in
   the provider slot -- **refuses boot**.

**Why the first one bites.** A prompt's input schema compiles with
`additionalProperties: false`, and `aiRuntime.Invoke` validates the
caller's data **before** rendering the template. So a field the template
reads but the body omits is not "an input the prompt happens to ignore"
-- it is a field **no caller can ever supply**:

```
data validation failed: ... additionalProperties 'phase',
  'agentTurnCounts', 'threadHolder', 'timeSinceLastHuman' not allowed
```

`cognitionPrediction` declared two fields while `component/polyphon`
passed nine, and the caller swallowed the error into its pattern-based
fallback -- so the entire predictive-cognition LLM path was dead, and
looked like normal operation the whole time. The same class had already
been fixed once by hand (the `directive` field on `cognitionReply`).

The check is one-way: template reads must be declared, but a declared
field the template never reads is inert and allowed. A prompt declaring
**no** fields compiles to a nil schema, so nothing is rejected and the
check does not apply.

**Why the second one bites.** `@defaultProvider`, a policy slug and a
provider name are all bare identifiers. A dangling name does **not**
error at call time: `resolveProviderName` hands it through,
`ChatStructuredProviderByName` misses, and the call falls through to the
default provider -- so the prompt quietly runs on a model its author did
not choose, leaving one INFO line on the structured path and nothing at
all on the plain chat path.

```memql retired
@defaultProvider("strongReasoning")     // WRONG -- that is a `policy`
@defaultProvider("streamClaudeSonnet")  // right -- a `provider`
```

A `@disabled` provider is still **declared**, so pointing at one stays
legal: dependents degrading to the default is the documented lifecycle
contract (#1081), not a mistake. The check is about the name existing,
not about the lane being on.

**Where it is enforced.** `validatePromptTemplateFields`
(`component/memql/prompt_template_fields.go`), called from
`LoadUnifiedPrompts`; and `ValidatePromptDefaultProviders`
(`component/memql/prompt_default_provider.go`), called from engine
bootstrap once both registries exist. Caller-side payload contracts are
pinned next to the callers -- `integrations/agents/factory_prompt_contract_test.go`
and `test/dslconformance/prompt_caller_payload_test.go`.

## 30. `??` is BLANK-coalescing, not null-coalescing (#1614 / memql#3627)

**Rule.** `a ?? b` (and its longhand `coalesce(a, b)`) falls through to
`b` when `a` is absent, null, **or a string that is empty or contains
only whitespace**. It does *not* fall through for `false`, `0`, `[]` or
`{}` — those are values.

```memql fragment
// with the caller passing v:
//   false   -> false        kept
//   0       -> 0            kept
//   []      -> []           kept
//   {}      -> {}           kept
//   ""      -> "DEFAULT"    REWRITTEN
//   " "     -> "DEFAULT"    REWRITTEN   <-- the sharp edge
//   "\t\n"  -> "DEFAULT"    REWRITTEN
//   "value" -> "value"      kept
insert { v: args.v ?? "DEFAULT" }
```

**Why it bites you.** The operator is called null-coalescing everywhere,
including in this repo, so nothing prepares you for the third line: **a
user who deliberately clears a text field gets the default written back
over their clearing**, and a field holding a single space is treated as
absent when it is not absent by any reading.

**Why it stays this way.** The empty-string behaviour is deliberate
(#1614): `f: args.f ?? ""` has to be able to land an explicit empty
string, because a `null` there fails JSON-schema validation on a
non-required string field. The whole corpus is written against that
rule, and the ARGUMENT position has no other spelling — `@default` on an
args field is rejected at load (#991), so `??` is the only mechanism
that fills a value. Changing the operator under the corpus to settle a
naming complaint would be the larger defect.

**What to do about it.** When a stored value must survive a caller
sending blank, that is `@noUnset("field")` on the mutation (memql#3415)
— the targeted opt-out. It drops a named field from the delta when the
incoming value is empty and the stored one is not, which is exactly the
"do not let a blank overwrite this" rule that `??` does not express.

**One rule, one implementation.** Both spellings resolve through
`coalesceSelect` in `component/memql/mutation_templates.go`: the FINAL
arm is the ultimate fallback and is returned even when blank, while
every NON-final arm is skipped when nil, missing, or blank. They were
two implementations that disagreed on a blank middle arm until
memql#3627 (`coalesce(args.a, "", args.c)` with nothing resolving gave
`""` from a payload slot and `nil` from an `id:` slot). Pinned by
`coalesce_array_missing_3627_test.go`.

**A missing arg contributes NOTHING to either container.** An absent
optional arg omits its key from an object literal and omits its element
from an array literal — `{ v: [args.a, args.b, "c"] }` with only `b`
supplied renders `{"v":["B","c"]}`, not a `null` hole (memql#3627). An
explicit `null` the author wrote is preserved in both.

---

## 31. `use` and namespaces: what an import means, per construct kind

**A directory is a namespace.** Every `.memql` file in one directory shares a
namespace and its constructs reference each other freely. A **subdirectory is a
different namespace** — `dsl/agents/roles/` is `agents/roles`, not `agents`.
Anything from another namespace must be imported with `use`, whatever kind it
is. Name collisions across namespaces are resolved by **aliasing**
(`use harness.concepts.{ plan as harnessPlan }`, memql#3802).

That is the model. memql#3803 closed the enforcement half and memql#3897 the
registry half, so it now holds for every construct kind rather than for
`concept` alone. memql#4051 then moved the enforcement onto the **boot** path:
the rule is corpus-level (it asks where a name is *declared*), boot ran a
per-file scan, so for a while it was checked by CI over this repository's `dsl/`
and by nothing at all over a product bundle mounted at `MEMQL_DSL_PATH`. It now
lands on the `LoadReport` like every other contract gate, so a bundle carrying
an unimported cross-namespace reference is refused by strict boot and surfaced
offline by `cmd/memqllint`.

### The model is Go's, and a namespace is a PATH

Worth stating plainly, because it settles the questions the rest of this section
answers. In Go a directory is a package; a **subdirectory is a different,
unrelated package** with no privileged access to its parent; and a symbol's
global identity is the full **import path** plus the name —
`example.com/m/agents/tools.Widget`, not `tools.Widget`. The package *name* is
only a file-local qualifier, which is exactly why two packages may both be named
`tools` and why collisions inside one file are fixed by aliasing the import.

MemQL follows that. A namespace is the whole directory path, so a concept
declared in `dsl/agents/tools/` assembles as:

```
v1:agents/tools:widget:a9f3b7c2...
\_/ \__________/ \____/ \_______/
 v    namespace   name   shortId
```

**The path goes inside the domain segment, not as an extra one** (memql#3898).
`v1:agents:tools:widget` is the same idea and breaks the id contract:
`core/id.ParseNodeId` defines a concept as the version segment plus *exactly
two* more, and that arity is unrecoverable from the string — `v1:agents:tools:widget:abc`
is indistinguishable from concept `v1:agents:tools` with shortId `widget:abc`
without consulting a registry. Every component splits node ids through that
function. Keeping the path inside the domain leaves `version:domain:entity`
intact, so nothing downstream changes.

Three consequences worth knowing:

- **A parent's `namespace.pin` does not reach a subdirectory.** A pin is
  per-directory, the way `package cluster` in `deployment/` does not name the
  package in `deployment/sub/`. A subdirectory that wants one carries its own.
- **A nested file must import its parent's constructs.** This is the change an
  author meets. It is not the same-namespace import memql#2617 bans — the file's
  namespace is `agents/roles` and the import names `agents`, which is a different
  one. The corpus already agreed: 17 of the 23 nested files were writing
  `use agents.concepts.{ agentRole }` before anything required it.
- **No existing id moved.** Every file that declares a concept in this tree is
  flat, and for a flat file the namespace is exactly what the root domain was.
  That is what made the decision cheap to take when it was taken, and it gets
  expensive the first time a concept lands in a subdirectory (memql#3898,
  reconciling memql#3026).

### Does `use` participate in resolution?

| Construct kind | Registry | Two domains may share a name? | Is `use` required for a cross-namespace reference? |
|---|---|---|---|
| `concept` | namespaced, `v1:<ns>:<name>` | **yes** — 4 do today | **YES**, and it is also the disambiguator: an unimported bare name resolves *ambiently* to this namespace, and a foreign one is a hard error naming the import |
| `query`, `mutation`, `logic`, `spec`, `trait`, `shape`, `tool`, `prompt`, `provider`, `builtin`, `policy`, `seed` | namespaced, `<ns>.<name>` (memql#3897) | **yes** — the S5 gate narrowed to per-namespace | **YES** — enforced by the `cross-namespace-import` contract gate (memql#3803), and it now *disambiguates* too: an unimported bare name resolves in this namespace, and an ambiguous one is refused naming both candidates |

Before memql#3803 the second row read "no": a cross-domain reference to
`statusIsActive` — which lives in `common.traits` — resolved from a bundle with
no import at all. For twelve kinds the import was documentation that nothing
checked, and for one it was load-bearing resolution.

**That conflation was not cosmetic.** memql#2617 banned same-namespace imports
on the premise that they are "pure ceremony" — true of the twelve, false of the
one — and wrote the rule across all thirteen. Both bugs that followed trace to
it: memql#3800 (the authoring path could not do ambient resolution, so 45
constructs compiled at boot and were refused by every editor) and memql#3802 (a
foreign import silently captured every bare use of a name and bound the wrong
concept with `OK=true`).

### The cost of enforcing it was one line

Measured across `dsl/` at **resolution sites only** — a call, a `shape` clause,
a bare filter conjunct — with comments **and string literals** stripped:

| kind | unimported | imported |
|---|---|---|
| builtin | 1 | 0 |
| query | 0 | 1 |
| shape | 0 | 3 |
| spec | 0 | 7 |
| trait | 0 | 102 |
| **total** | **1** | **113** |

The tree was already 113/114 compliant *by habit*: authors have been writing
the imports the engine never asked for. The single holdout was
`builtin cognitionTrackPresence(...)` in `dsl/cognition/automations.memql`, a
file that carried no `use` block at all.

> **A naive count says 345.** Word-boundary matching over raw source reports
> `builtin agent`, `builtin error`, `builtin tools`, `builtin help` and
> `builtin concepts` — ordinary English in doc comments and `@description`
> text. Two further traps: an *indented* `provider enum("heygen", ...)` is a
> concept **field**, not a `provider` construct named `enum` (registering it
> turns every `@enum(...)` in the tree into a cross-namespace call, worth 45
> phantom findings); and strings must be stripped **before** comments, or a
> `//` inside a URL in a `@description` eats that string's closing quote and
> desynchronises every string after it. Both are pinned by tests in
> `component/memql/dslgate/imports_test.go`.

### The flat kinds are namespaced too — BUILT (memql#3897)

Every construct kind is per-namespace. The registry key is
`<namespace>.<name>` — a dot rather than a colon, because a colon is the
*concept id* separator and a flat construct is not a concept. The namespace
comes from the file's origin through the same `dslfs.NamespaceFromFilePath`
that ambient concept resolution and canonical-id assembly use, so a construct's
namespace, a concept's namespace and the ambient scope are one answer to one
question.

**So a product DSL bundle may now declare a `shape`, `spec`, `query`, `trait`
or any other flat-kind construct whose name a core construct already uses.**
That was a load-time error the product could not resolve except by renaming its
own construct — and since a product *is* a DSL bundle plus a client
(memql#2472), it was the primary delivery path hitting a constraint with no way
around it.

**And aliasing means something for these kinds now.** `use x.y.{ n as m }`
(memql#3802) was inert here: two same-named flat constructs could not coexist,
so there was nothing to alias between. That is the real answer to "why can't I
alias a shape".

Resolution order, which is Go's:

1. the referencing file's **own namespace** — no import, same-package
2. an explicit `use` import, including an alias

A **bare** name still resolves when it is unambiguous, and that is deliberate
rather than a leftover: a reference inside a compiled body is looked up at
*execution* time from a context that has no file, so the bare floor is what
keeps every existing reference working while load-time resolution qualifies
them. An **ambiguous** bare name is refused, naming both candidates —
resolving it to one of them would be exactly the silent capture memql#3802
fixed for concepts.

The S5 uniqueness gate (memql#2360) narrowed to match: the same name in two
namespaces is legal; the same name twice in **one** namespace is still the
silent last-wins overwrite it always was, and is still refused.

---

## 32. `startsWith` is a selection, never a pass-through (memql#4208)

**Rule.** `<field> startsWith <prefix>` matches a row whose string
property begins with the prefix, or with ANY prefix in a list. The
right-hand side is a string literal, a list of string literals, or an
`args.<field>` that resolves to either at call time. Two inputs match
NOTHING, and that is the contract rather than an edge case:

- an **empty list** (`args.prefixes` bound to `[]`);
- a **blank prefix** (`""` or whitespace), on its own or inside a list --
  blanks are dropped, and a list of nothing but blanks is an empty list.

Right -- a prefix-scoped read that cannot widen (the read behind
memql#4208, `dsl/observability/queries.memql`):

```memql fragment
filter  bucket==args.bucket && (when(args.codeReference) { codeReference==args.codeReference } || codeReference startsWith args.prefixes)
```

Wrong -- expecting Go's `strings.HasPrefix(s, "")`:

```memql fragment
filter  codeReference startsWith args.prefix   // args.prefix == "" returns NO rows, not every row
```

**Why.** Every language's HasPrefix says the empty string is a prefix of
everything, and in a query filter that is exactly the fail-open shape this
document keeps recording (`!= ""` as the is-set idiom in #27, `??`
blank-coalescing in #30): a selection that admits every row on a blank
input. `codeReference startsWith args.prefixes` is safe to hand whatever
list the caller holds because neither an empty list nor a list of blanks
can turn it into a cluster-wide scan. The engine rule lives in
`normalizePrefixValues` (`component/memql/executor_filter.go`) and every
evaluator reads it -- the SQL compile (`((payload #>> '{f}') ^@
ANY(?::text[]))`, or the constant `FALSE` for an empty list), the
in-process post-filter every SQL candidate goes through, collection
lambdas and shape-template matches -- so they cannot disagree.

**What it does NOT do.**

- It is not a pattern. `^@` is Postgres `starts_with()` as an operator, a
  byte-prefix test; `%` and `_` in a prefix are literal, and nothing is
  escaped or concatenated -- one `text[]` parameter is bound whatever the
  right-hand shape was.
- It does not drop on absence. `when(args.x) { ... }` is the "no
  constraint when the arg is absent" form; a blank is a VALUE, and as a
  prefix it is not one. Declare the list arg `[]string!` when an absent
  list must be a refused call rather than an unconstrained one.
- It is not a runtime-condition operator. Filters and spec bodies compile
  it; an automation cond-step condition or a trigger `@filter` is refused
  by name (`component/automations/evaluator.go`), because that string
  grammar would otherwise read the whole condition as a non-empty --
  truthy -- string.
- `not startsWith` is not a form; the left side must be a row field
  (`args.x startsWith ...` is refused); a bare identifier on the right is
  refused rather than read as its own literal text; a row intrinsic
  (`row.id startsWith`) is refused at compile.

**Tested by.** `component/language/parser/startswith_test.go` (grammar and
its negative cases), `component/memql/executor_filter_startswith_test.go`
(SQL + in-process agreement), and
`component/memql/code_metrics_in_window_db_test.go` (the memql#4208 read
against a real Postgres, db-gated).
