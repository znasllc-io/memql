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
`mutation <Concept> <name>` signature; restating it is retired.

```memql
use library.concepts.{ folder }

mutation folder createFolder {
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
mutation folder createFolderAndGrantOwner {
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

The canonical worked example is **the Library index**: a to-do is one
row, and the Library row that indexes it is a second, written by the
automation that fires when the first lands:

```memql
use todos.concepts.{ todo }

// 1. The product calls this mutation: one row, the to-do.
/// Create a to-do for the caller.
@actor
mutation todo createTodo {
  args {
    todoId                  string!
    title                   string!
    dueAt                   datetime
    priority                enum("low", "medium", "high")
    sourceResponsibilityId  string
  }
  insert {
    accept { title, dueAt, priority, sourceResponsibilityId }
    stamp {
      id: args.todoId
      ownerUserId: actor.userId
      done: false
    }
  }
}

// 2. An automation fires on the row landing and writes the second row.
//    The trigger payload is bound into its args block; every call names
//    its kind and passes named arguments.
/// On to-do creation, promote it into the Library Records lens.
@trigger(event="node.created", concept="v1:todos:todo")
automation indexTodoOnCreate {
  args {
    id any
    ownerUserId any
    title any
  }

  persist := mutation createArtifact(
    sourceConceptRef: args.id,
    ownerUserId:      args.ownerUserId,
    lens:             "record",
    kind:             "todo",
    source:           "agent_generated",
    title:            args.title ?? "Untitled to-do",
    live:             false
  )
}
```

The product calls `createTodo` once. The automation takes care of the
second write. The user gets one product action; the engine gets two
atomic rows with clean audit trails.

**Cross-references**: see `dsl/library/automations.memql` and
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
when called by the top-level query parser. In a `logic` or `automation`
body, a bare call inside an expression must be a catalog function or a
spec or trait predicate. `sort` / `paginate` / etc. are neither, so the
load refuses the body:

```
logic listFoldersSorted: `sort(...)` is not a function or a predicate known here, so the expression fails when it runs: call a catalog function or a spec or trait the corpus declares [body_call_unknown]
```

If you put a directive inside a function body, the entire engine
refuses to start. The primary node crashes. Agent / planner / workbench
/ mcp / edge can't attach. Whole cluster bricked.

**Wrong:**

```memql retired
use library.queries.{ activeFolderIds }

// `sort` is a directive, not a function: the load refuses the body.
logic listFoldersSorted {
  folders := query activeFolderIds()
  return sort(folders, "name", "asc")
}
```

**Right -- struct queries have dedicated clauses.** Sorting,
windowing, and latest-per-id snapshots are `sort` / `paginate` /
`asOf` clauses on the struct query itself, not directive calls
(live examples: `libraryArtifacts` in
`dsl/library/queries.memql`, `staleClusterNodes` in
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
  filter  row => row.clusterId == args.clusterId
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
use library.concepts.{ artifact }

/// Latest artifact row filed under a folder.
query artifact latestArtifactForFolder {
  args {
    folderId  string  @required
  }
  filter  row => row.folderId == args.folderId
  sort    "row.createdAt", "desc"
  paginate 1
  shape   artifactFull
}
```

(The historical receiver form `func (Query) queryListPartitions(_ any)
(any, error) { return sort(...), nil }` hit this trap constantly; the
receiver form itself is now retired and rejected at parse time, so
the directive-in-body variant of the bug can only appear in `logic`
bodies.)

**Where directives do work**: in the internal query form, the raw
string an SDK sends through `MemqlClientMessage.Stream` (the public
RPC), e.g. `sort(concept==v1:cluster:node, "name", "asc")`. That string
goes through the top-level parser, which knows about directives. It is
a wire contract rather than an authoring surface, and it keeps its own
grammar: see [memql.md](memql.md#the-internal-query-form).

---

## 2. Function-call arguments are named, not an object literal

**Rule.** A call to a construct passes named arguments,
`fn(key: value, key2: value2)`, and an empty call is `fn()`. Inside a
body the call also names the construct's kind:

```memql fragment
rows := query folderArtifacts(folderId: "folder-123", kind: "document")
created := mutation createFolder(folderId: "folder-123", name: "Inbox")
```

This section used to document a bare-vs-quoted distinction between two
spellings of an object-literal call (`createFolder({name: "Inbox"})`
vs `createFolder({"name": "Inbox"})`). That premise is gone: an
argument list is not an object literal, so there is no key to quote.
Each key is a bare name, matched by name against the callee's declared
args. A quoted key is a parse error, not an alternate spelling:

```memql retired
// Refused: object-literal call args are retired; this parses as
// neither a named-arg call nor a valid expression.
createFolder({"folderId": "folder-123", "name": "Inbox"})
```

**The argument pun is retired.** A bare name in argument position,
`logic composeTitle(folderId)` meaning
`logic composeTitle(folderId: folderId)` (G3, memql#2365), is retired
with the other step-reference shorthands (D12 of the
[language freeze record](../../superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md)).
Name the argument and write its value: `logic composeTitle(folderId: args.folderId)`.
The parser refuses the pun (`body_positional_argument`), and
`memqlmigrate --rewrite=bodies` rewrites it.

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

```memql fragment
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
- `concept` = the concept bound by the `mutation <Concept> <name>`
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
mutation desktop saveMyDesktop {
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

The DSL trigger surface matches: an automation's
`@trigger(event="node.created", concept="v1:notes:note")` names the
concept and no partition, and a `partition=` kwarg on `@trigger` is
refused at parse (`trigger_partition_retired`).

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

## 11b. A conditional value is `p ? a : b`, not `if` at expression position

**Rule.** A value that depends on a condition is written `p ? a : b`:
in a mutation value, a call's argument, a statement. `if` guards
statements -- `if condition { ... }` -- and is not an expression, so it
cannot stand where a value goes.

```memql fragment
role: existingOwners.empty() ? "owner" : "reader"
```

```memql retired
role: if existingOwners.empty() { "owner" }             // refused: `if` is a statement
role: cond(existingOwners.empty(), "owner", "reader")   // refused: cond() is retired
```

`cond(p, a, b)` is retired: the parser refuses it with
`cond(p, a, b) is retired in edition 2026: write p ? a : b
(memqlmigrate --rewrite=expressions rewrites it)`. Both branches are
required, as they were for `cond()`.

What the ternary does, from `component/memql/expr_eval.go`:

- **The condition is a boolean.** There is no truthiness, so a string
  or a number there is refused with `condition_not_boolean`; an absent
  condition reads as false, and so does a row field holding a value of
  another type ([27](#27-unset-is-one-value-in--and--1685--2783)).
  Compare a flag that may arrive as a string explicitly:
  `args.flag == true ? "on" : "off"`.
- **Only the chosen branch runs.** The other may hold an `error(...)`
  or a read that is only meaningful on its own side.
- **It binds loosest of the value operators** (level 9 of the
  [precedence table](memql.md#operator-precedence)), so
  `row.x == 1 ? a : b` tests `row.x == 1`, `p && q ? a : b` tests
  `p && q`, and a nested ternary groups to the right:
  `a ? b : c ? d : e` is `a ? b : (c ? d : e)`.

In a filter or spec body a ternary over the row must be boolean-valued:
it lowers to `(c && p) || (!c && q)`, and the lowering refuses a
ternary that chooses a value by the row (the manifest records the rule
in `component/language/tiers/manifest.go`). Choose the value before the
query instead, where it is a plan constant
([21d](#21d-a-subexpression-that-does-not-read-the-row-is-a-plan-constant)):
`row.stage == (args.urgent ? "now" : "later")`.

---

## 11c. One expression grammar in every position (#2542)

**Rule.** Every place an expression is written reads the same grammar: a
`return`, a `name := ...` statement, an `if` or `for ... if`
condition, a lambda body, a map value, a mutation value, a call's
argument, a filter, a spec body. What differs between positions is where
the expression runs and what it may call, and
[memql.md](memql.md#where-each-expression-runs) prints that table from
the tier manifest.

Before edition 2026 each value position of a logic body had its own
grammar, and this rule kept a table of what worked where (#2542, #2655,
#2693, #3024). One grammar with one precedence table closes those cases:

| Written | Before edition 2026 | Edition 2026 |
|---|---|---|
| `a - b > 0` | parsed as `a - (b > 0)` and refused at load | `(a - b) > 0`: arithmetic binds tighter than comparison |
| `return args.n == 5` | refused at load: an identifier-led comparison in value position was run as a store query | a comparison is a boolean value in any position |
| `p && q` as a conditional's condition | refused inside a `cond(...)` argument | `p && q ? a : b`: `? :` binds loosest |
| a bare name nothing binds (`role == "x"`) | refused at load; before #3024 it constant-folded | `unknown_name`: a bare name resolves to a lambda parameter, a root, a local, a predicate or a function, or to nothing ([21c](#21c-a-predicate-names-its-receiver-the-lambda-parameter-and-bare-names)) |

What still differs by position:

- **Where it runs.** A filter or spec body pushes down to SQL; a
  condition, a body, a value and a `refine` clause run in process. In a
  pushdown position an in-process
  function, arithmetic, `??` or a map literal is legal only on values
  that do not read the row
  ([21d](#21d-a-subexpression-that-does-not-read-the-row-is-a-plan-constant)).
- **Construct calls.** `query`, `mutation`, `logic` and `builtin` calls
  are legal only as a statement of their own in a logic or automation
  body -- the whole right-hand side of `:=`, the whole value of
  `return`, or a line by itself -- because each call is a step a run
  journals. A condition, a
  mutation value, a trigger filter, a prompt input and a `refine` clause
  may not call a construct (`construct_call_not_allowed`), and a filter
  or spec body never may.

Notes that hold in every in-process position:

- **Conditions are booleans.** `if`, `&&`, `||`, `!`, `? :` and the
  lambdas of `where`, `any` and `all` take a boolean; an absent value
  reads as false, and so does a row field holding a value of another
  type ([27](#27-unset-is-one-value-in--and--1685--2783)); anything else
  is refused with `condition_not_boolean`.
  There is no truthiness: `"false"`, `0` and `""` are not conditions.
- **Integer arithmetic stays integer.** Two integers divide as integers
  (`7 / 2` is `3`), and `.count()` returns an integer, so
  `good.count() * 100 / total.count()` is a whole percent; multiply by
  `1.0` for a fraction. A number decoded from JSON (a payload field, an
  argument a client sent) is a float, so it divides as one. Division or
  remainder by zero is `division_by_zero`, an out-of-range result is
  `arithmetic_overflow`, and `%` needs whole numbers.
- **The ambient roots evaluate.** `actor.role == "owner"`, a
  `config.<key>` comparison and `now` discriminate exactly like an
  `args.` comparison (#3024). With no authenticated caller the actor
  envelope denies rather than leaving keys absent (#2801), because an
  absent key is what makes a negated gate read true.
- **A reserved root is not a readable one.** `trace` is reserved, so no
  local or field may shadow it, but nothing supplies it: a `trace.` read
  is `unknown_name`. The same goes for an `actor.` member outside the
  closed envelope (#2623) and a `config.` key outside the
  `policy_exposable.go` allow-list.

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

## 13. Statements run in the order written; a name is read after it is bound

**Rule.** A body's statements run in the order they are written. Nothing
is reordered by dependency, so a name can only be read after the
statement that binds it. The load refuses a name read before its
statement, naming both lines, and a name bound nowhere, which catches a
typo:

```memql fragment
logic bootstrapUser {
  args {
    userId string
  }
  checkUser := query userById(userId: args.userId)
  if cehckUser.empty() {   // typo: cehckUser -> checkUser
    mutation createUser(userId: args.userId)
  }
  return checkUser.count()
}
```

```
logic bootstrapUser, line 6:6: `cehckUser` is not a statement name, a loop variable or a root here [body_unknown_name]
```

```memql fragment
logic readsAhead {
  total := subtotal + 1
  subtotal := 2
  return total
}
```

```
logic readsAhead, line 2:12: `total` reads `subtotal`, which is bound on line 3, after it: move line 3 above line 2 [body_forward_reference]
```

**Why.** Before edition 2026 the compiler sorted steps by the dotted
references it could see, and ran co-released steps in map order. A step
that read a later step's name by another spelling ran first and read
nothing, and the order a reader saw was not the order that ran.
`memqlmigrate --rewrite=bodies` writes each migrated body in the order
the old engine ran it, and comments every statement it moved.

---

## 14. Function naming: the construct name says what it does

**Rule.** A construct is named for what it does, not for its kind --
the declaration keyword already carries that: `libraryArtifacts`,
`moveArtifactToFolder`, `isArchivedArtifact`, `isActiveRecord`,
`indexArtifact`. No `query*` / `mutation*` / `logic*` / `spec*` /
`trait*` / `seed*` prefix -- settled in #2853, which measured 0 of 1091
shipped declarations carrying one. (See naming-conventions.md.)
Constructs live in one consolidated file per kind per namespace
(`dsl/<namespace>/<construct>s.memql`), so the file name never
carries an individual construct's name.

```
dsl/library/queries.memql       query folder activeFolders { ... }
dsl/library/mutations.memql     mutation folder createFolder { ... }
dsl/deployment/specs.memql      spec actorEnvelope requiresOwner = actor => ...
dsl/common/traits.memql         trait isActiveRecord = row => ...
dsl/library/logic.memql         logic indexArtifact { ... }
```

The declaration keyword and the call kind are one word, `mutation`
(D13 of the language freeze record): `mutation folder createFolder { ... }`
declares it and `mutation createFolder(...)` calls it. The old declaration
keyword `mutate` is refused at parse, and `memqlmigrate --rewrite=bodies`
rewrites it.

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

An automation calls a logic construct by the name the file-top import
gives it, with its kind and a named argument:
`decide := logic indexArtifact(event: event)` resolves through
`use library.logic.{ indexArtifact }`. The pun `logic indexArtifact ( event )`
is retired ([#2](#2-function-call-arguments-are-named-not-an-object-literal)).

Automations are event-triggered, not called by name, so they use
verb-first names with no prefix (`indexFileOnCreate`,
`archiveFileOnArtifactArchive`, `releaseWorkspaceOnRunTerminal`).
Builtins, tools, prompts, providers, and shapes are out of scope for
this rule and use their own conventions (shapes are conventionally
`<concept><Projection>`, e.g. `artifactFull`, `folderCard`).

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
  id: "filed-" + hash(
    canonicalId(args.artifactId, "artifact") + ":" +
    canonicalId(args.folderId, "folder")
  )
  args.folderId
  args.artifactId
  kind: "document"
  args.displayName
  status: "active"
}
```

**Constraints.**

- **Simple identifier only.** The arg path must match
  `[A-Za-z_][A-Za-z0-9_]*`. A dotted path (`args.user.id`) is not
  eligible; write it as `userId: args.user.id`. The parser refuses the
  dotted shorthand rather than inventing a field named `user.id`.
- **Bare `args.X` only.** `args.x ?? "default"`, `args.a + ":" + args.b`,
  `p ? a : b` and every other expression keep the explicit `key:`
  prefix. Only a plain `args.name` can be shorthand.
- **No effect on the `args { ... }` block.** That block is a type
  declaration, not a value map; its lines stay in the
  `<name> <type> [@required] ...` form.

**Why it bites you (if you don't know about it).** Reviewing PRs
you'll see some mutations declaring 20-field payloads and some
declaring 20-field payloads with half the repetition. Both are valid
and equivalent. Under the hood the struct-form rewriter translates
`args.X` to the engine-internal `ctx.X` and the expansion lives in
the mutation-template parser
(`component/memql/mutation_templates.go`). The bare mirror belongs to
the write block alone: a map literal in a call's argument has no
shorthand key (see [#17](#17-automation-arguments-named-with-no-shorthand-keys)).
Authors never write `ctx.X` -- it is not part of the author surface.

---

## 16. Shape bodies: the key comes from the path's terminal segment

**Rule.** Shapes are struct-form path lists. Each body line is a
projection path. A payload property is written by **bare name**
(`name`, `description`) -- the concept is bound by the
`shape <Concept> <name>` signature, so the `payload.` prefix is
removed; row metadata stays `row.X` (`row.id`, `row.createdAt`) and the
auth envelope stays `actor.X` (`actor.userId`). The projected field is
keyed by the path's **terminal segment**. Every shape declares its
kind via `@row` (concept payload + row intrinsics) and/or `@actor`
(engine envelope, no signature concept). The explicit `payload.X`
form is rejected at load. A shape body is a path list, not an
expression: the lambda parameter of rule
[21c](#21c-a-predicate-names-its-receiver-the-lambda-parameter-and-bare-names)
does not apply to it.

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

## 17. Automation arguments: named, with no shorthand keys

**Rule.** An automation declares a typed `args { }` contract, and the
trigger binds the event payload into it before any statement runs; a
payload that violates the contract refuses the run rather than binding
part of it (G1, `component/automations/args_binding.go`). A statement
reads a declared arg as `args.<field>`, reads an earlier statement's
value by its name, and passes every argument by name:

```memql
@trigger(event="node.created", concept="v1:knowledge:document")
automation indexNewDocument {
  args {
    folderId     string @required
    documentRef  string @required
  }
  title := logic composeTitle(folderId: args.folderId)
  mutation createArtifact(
    folderId:         args.folderId,
    sourceConceptRef: args.documentRef,
    title:            title
  )
}
```

These spellings are gone:

- **`event.<field>` reads** in an automation body are refused at load
  (G5, memql#2367): declare the field in `args { }` and read
  `args.<field>`. To hand the whole event to a logic, pass it:
  `logic record(event: event)`.
- **Step-result reads.** `steps.decide.result` and `decide.result` read
  a step's record; a statement's name is its value, so write `decide`.
  `steps.<id>` is refused at parse.
- **The step bodies' accessors.** `step("decide")`, `input()`, `item()`
  and `index()` are refused at parse (`body_accessor_retired`): a
  statement's name is its value, an argument is `args.<name>`, and a loop
  names its element, `for x in s`, and has no index.
- **A bare argument.** `folderId` for `args.folderId` (G2, memql#2364)
  is refused (`body_unknown_name`, whose hint says `args.folderId`): an
  argument reads the same way in every position, and a bare name is a
  statement's or a loop's.
- **The argument pun**, `logic composeTitle(folderId)`, is retired
  ([#2](#2-function-call-arguments-are-named-not-an-object-literal)):
  write `folderId: args.folderId`.
- **Shorthand keys in a map literal.** A map entry is `key: value`, and
  a key is one name. `{ registerNode.result.node.id }` once inferred the
  key `id` from the path's last segment
  (`automation_generator.go::tryParseBarePathShorthand`); the
  edition-2026 parser refuses it (`a map key is one name, got
  registerNode.result.node.id: nest a map for a path`), and a bare
  `{ folderId }` likewise (`a map entry is written key: value`). Write
  `{ nodeId: registerNode.node.id }`.

---

## 18. Object-literal keys: unquoted identifiers only

**Rule.** Inside a MemQL `{...}` literal a key is an unquoted name
(`name:`, `folderId:`, `createdAt:`). Quoted keys (`"name":`) were
accepted for JSON interop and are not written in new code.

```memql fragment
// Correct -- mutation write block
insert {
  name: args.name
  folderId: args.folderId
  active: true
  metadata: { source: "import" }   // unquoted key in a nested map value
}

// Wrong -- unnecessary quotes on simple-identifier keys
insert {
  "name": args.name
  metadata: { "source": "import" }
}
```

Where each parser stands:

| Where the literal is | A quoted key |
|---|---|
| A map literal in an edition-2026 expression: a mutation value such as `metadata: { ... }`, a call's argument, a statement | refused: `map keys are unquoted names (authoring rule 18)` (`parseV1Map` in `component/language/parser/v1_expr.go`) |
| The write block itself, `insert { ... }` / `update { ... }` | still accepted by the mutation parser (`component/memql/mutation_templates.go::parseObjectKey`); do not write one |

In an edition-2026 map literal a key is one name: a hyphenated key is an
identifier there (`{ user-agent: args.ua }`), a dotted key is refused,
and a key with a space cannot be written at all.

Call arguments are not an object literal and never were in this rule's
sense: an argument list is named args (`fn(key: value)`), see
[#2](#2-function-call-arguments-are-named-not-an-object-literal).

**Why it bites you.** Mixed quoting styles make every review a guessing
game, and a quoted key opts out of the bare-`args.X` shorthand of
[#15](#15-write-block-sugar-accept-----stamp----and-the-bare-mirror-shorthand),
which triggers only when the key is absent.

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
authorization-relevant (the pre-edition filter `actor.userId == args.v`
const-folded to true whenever the caller passed their own id, so the
predicate contributed nothing and the query returned every row).
Matching is case-insensitive and by whole name, so `Provenance` is
refused while `arguments`, `metadata`, and `rowCount` are ordinary
properties.

Edition 2026 reads a payload field through the lambda parameter
(`row.actor`), so a filter can no longer confuse the field with the
root. The names stay reserved all the same, and
`ensureReservedFieldsNotDeclared` still refuses them.

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
The cognition node (a node type since removed), agent and planner all
dropped off the mesh because the primary couldn't serve queries. The fix
was a one-line concept-schema delete plus dropping the matching
`createPartition` arg.

---

## 20. Foreign-key id derivation: normalise before hashing

When a mutation derives a deterministic id by hashing foreign-key
args (the filing id pattern: `id = hash(folderId + ":" + userId)`),
normalise the args first, with `canonicalId(value, "concept")` by
default -- see below for when `shortId(value)` is the right choice
instead, and what it does not give you. The hash is byte-level, so two
callers passing the same logical reference under different shapes
(`"user-abc"` vs `"_system:v1:identity:user:user-abc"`) hash to
different strings and produce duplicate rows with distinct ids.

```memql retired
// Wrong -- bare-vs-canonical input shape changes the derived id
insert {
  id: hash(args.folderId + ":" + args.userId)
  ...
}

// Right -- canonicalId() collapses both forms to the same string, and
// each part is hashed before joining so the composite cannot alias.
// The second argument names an imported concept, as a string.
insert {
  id: hash(
    hash(canonicalId(args.folderId, "folder")) +
    hash(canonicalId(args.userId, "user"))
  )
  ...
}

// Wrong -- joining with a separator first. `hash(a + ":" + b)` aliases
// whenever a part can contain the separator: ("chat", "k:1") and
// ("chat:k", "1") derive one id, so two distinct rows collapse into one.
// A canonicalId() part happens to be safe today only because its fixed
// `v1:ns:concept:` prefix makes the split recoverable -- that is a
// property of the data shape, not a constraint, and it stops holding the
// moment a part is caller-supplied (memql#3009).
```

(Don't prefix the hash with the concept name -- `id: "folder-" + hash(...)`
duplicates information already in the canonical id position, and
`test/dslconformance/conformance_test.go`'s `TestNoShortIdConceptPrefix`
rejects known concept-name prefixes outright. The shortId is the bare
hash / uuid / slug.)

`canonicalId(value, "concept")` -- the concept is a string naming an
imported concept, `"folder"`. An edition-2026 expression resolves no
bare concept name, so the older `canonicalId(value, folder)` spelling is
quoted by `memqlmigrate --rewrite=expressions`; a canonical
`"v1:ns:name"` string is refused by the binding gate (rule 22). The
catalog entry (`component/language/functions/catalog.go`) and
`EvalExpr` give it this behaviour:

- bare slug → prepends `<concept>:` (no partition prefix -- partitioning
  and `@scope` were both retired in #56; the composed form is the plain
  `{concept}:{shortId}` shape, see `component/memql/partition_context.go`'s
  `canonicalizeIdValue`)
- already-canonical, matching concept → returns as-is
- canonical for a different concept → an error (catches type-tag typos
  like passing a user id to `canonicalId(..., "folder")`)
- absent or blank value → the empty string (an optional foreign key
  stays empty)
- a missing or blank concept argument → an error (`invalid_argument`)

The engine also canonicalizes `@relationship`-tagged payload fields at
insert time (`canonicalizeRelationshipFields` in
`component/memql/partition_context.go`), so `row.userId == args.userId`
works against canonical-stored values. But the id derivation runs
before that, so `canonicalId()` in the id value is still what makes a
deterministic id stable.

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

Compliant mutations (audit done 2026-05-06), in the cognition mutations
file, since removed with that tree:
`joinSpaceAsHuman`, `joinSpaceAsSI`, `createGreetingUtterance`,
`createSessionForParticipant`, `sendTextUtterance`,
`sendSpeechUtterance`, `sendActionUtterance`,
`sendRealtimeTranscriptUtterance`. The roster is kept because it is
what the audit measured; do not read it as a list of constructs to go
and look at.

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
`hash(a + ":" + b)` is not injective:

```
("d:x", "y")   and   ("d", "x:y")   ->   hash("d:x:y")
```

Two different pairs, one id, one timeline. Whichever wrote last wins and
the reader silently gets the wrong row.

**The rule: in `hash(a + sep + b)`, every part after the first must be
free of `sep`.** The *leading* part may contain it freely. With the
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
  id: hash(shortId(args.deploymentId) + ":" + args.nodeType)
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
when it is not -- the derivation memql#3009 landed, in a construct since
removed with the cognition tree:

```memql fragment
id: "utt-" + hash(
  hash(args.partitionId) +
  hash(canonicalId(args.participantId, "participant")) +
  hash(args.action.type) +
  hash(args.action.idempotencyKey)
)
```

`hash()` is sha256-hex, so every part renders to exactly 64 characters
and the joined string has exactly one decomposition. No separator, no
constraint on what a caller may send, injective by construction.
`recordReputationWindow` (`dsl/campaigns/mutations.memql`) is a live
construct built the same way.

`sendActionUtterance` (in the cognition mutations file, since removed
with that tree) needed it because the constraint was **both unavailable
and wrong**. Unavailable: `action` was declared `object!`, so
`type` and `idempotencyKey` are nested in an unstructured object and
`validateArgsField` only matches `patternRegex` on *declared* fields —
there is nowhere to hang the annotation and nothing would enforce it.
Wrong: `idempotencyKey` is a caller-chosen opaque string, so banning a
colon rejects `"order:123"` to work around an internal encoding choice.
**Do not push the engine's hashing problem into the caller's key space.**

Construction is also the only answer when a part is engine-derived but
separator-bearing. `hash(args.nodeType + ":" + now)` aliased with no
caller involvement at all, because an RFC3339 timestamp always carries
colons.

**Where the tree stands.** The memql#3009 conversion put every composite
id derivation in `dsl/cognition/` and `dsl/cluster/` on construction; the
cognition half went with that tree, so `dsl/cluster/` is what survives of
it. The two in `dsl/deployment/` use the constraint (memql#2980). Both are
gated — `TestConvertedIdDerivationsKeepPerPartHashing` and
`TestCompositeHashedIdTrailingPartRejectsTheSeparator` — and **both
gates check by path, not by shape**. A new file adopting the separator
form trips neither. The tree-wide shape detector (find every `id:`
value that hashes joined parts, require its parts constrained or
digested) is the durable answer, has its own false-positive design
problem, and is not built.

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
live examples of the hazard rather than of the fix.** `dsl/` carries
about ten `id:` values that hash joined parts. Several are safe only
incidentally — the trailing part is a `canonicalId()` result whose fixed
`v1:<ns>:<concept>:` prefix happens to make the split recoverable — and
the one that was measurably not safe was `sendActionUtterance` (in the
cognition mutations file, since removed with that tree): it hashed
`args.action.type` and `args.action.idempotencyKey`, which lived inside
an untyped `object!` and therefore **could not** carry `@pattern` at
all, so `("chat", "k:1")` and `("chat:k", "1")` derived one id from
client-supplied input. Tracked in memql#3009; do not read this section
as a statement that the tree complies with it -- that construct is gone,
the missing detector is not, and the next one to be written this way
will be found the same way.

A shape detector — find every `id:` value that hashes joined parts and
require its trailing parts to be constrained — is the gate that would
make the rule true tree-wide. It does not exist yet.

The historical `"ga-" + hash(actor)` pattern in the auto-join
path was gone before the tree that held it was: the cognition logic file
resolved the assistant via `assistantAgentForUser` + the space row's
`ownerUserId` instead (memql#273, locked in at the time by
`TestAutoJoinSILocksInOwnerUserIdResolution`), and shortId prefixes
like `ga-` are banned tree-wide by `TestNoShortIdConceptPrefix`. That
last ban is the part still standing; the auto-join path and its test
went with the cognition tree.

---

## 21. Argument resolution: `args.x`, the engine roots, and the lambda parameter

**Rule.** A construct declares its inputs in one of two places:

- **Query, mutation, logic, automation**: an `args { ... }` block inside
  the construct body (ahead of the statements in a logic or an
  automation). An automation's args are bound from the triggering
  event's payload at fire time and validated against the block; a
  violation refuses the run (rule [#17](#17-automation-arguments-named-with-no-shorthand-keys)).
- **Builtin, tool, prompt**: the body's fields directly -- the body is
  the schema, with no `args` wrapper.

An expression reads three kinds of name -- the declared arguments
(`args.x`), the engine roots (`actor`, `now`, `config`, `event`), and a
lambda parameter (`row.x`) -- and never a bare payload field:

| Written | Is | Read in |
|---|---|---|
| `args.folderId` | a declared argument | any body, and a query filter (where it is a plan constant) |
| `actor.userId`, `actor.role`, `actor.isClusterOwner`, ... | the resolved auth envelope (a closed set, #2623) | any body and a query filter, once the construct declares `@actor` (rule 26); in a spec, only as the parameter of a spec bound to an `@actor` shape |
| `now` | the RFC 3339 timestamp captured when evaluation began | any expression |
| `config.<key>` | an allow-listed configuration value (`component/config/policy_exposable.go`) | any body, and a query filter (a plan constant) |
| `event` | the triggering event envelope | an automation body; pass it whole, `logic record(event: event)`, and read `args.event.payload.<field>` in the logic |
| `row.status`, `row.id` | a payload field or an intrinsic of the row, through the lambda parameter | a filter, a spec or trait body, a trigger filter, a `refine` clause, and a collection method's lambda (`t => t.status`) |

A payload field is always reached through the parameter that names the
row: `filter row => row.folderId == args.folderId` reads the field on
the left and the argument on the right, and neither side can be
mistaken for the other. How a bare name resolves is rule
[#21c](#21c-a-predicate-names-its-receiver-the-lambda-parameter-and-bare-names).

**Reserved names.** `now`, `actor`, `partition`, `config` and `trace`
are reserved: an args field named one of them is refused at load, and
so is a call-site argument of that name (memql#3626). `event` is
reserved too: in an automation it is the trigger root, and no statement
or loop variable may take any reserved name (`body_reserved_name`). `trace` is reserved
without being readable: nothing binds it, so reading it is refused
(`body_unknown_name`).

**An arg description is a `///` doc comment, never `@description`**
(memql#3336). The `///` block on the line(s) immediately above an
`args { ... }` field is that argument's description: it lands on the
field's AST slot, and it is what the corpus, the SDK generator, and the
LSP's `memql/runnableConstructs` read. `@description` on an args field
is refused at load -- there is no AST slot for it, so it used to be
accepted and then silently thrown away. The identical annotation on a
`tool` / `prompt` / `builtin` field is untouched: those bodies are the
schema and keep it. `@default` on an args field is refused the same way
(#991); apply a default in the body with `args.x ?? <default>`.

```memql retired
query folder activeFolders {
  args {
    /// The owner whose folders to list.
    ownerId string @required                        // correct
    limit   number @description("page size")        // refused at load
  }
  filter  row => row.ownerId == args.ownerId && isActiveRecord(row)
  shape   folderFull
}
```

The declaration-level `@description` on the construct itself stays
load-bearing (it is the fallback for the construct's own `///` block).
To fix an existing file, run `memqlmigrate --rewrite=args-description`,
which strips the annotation; re-add the prose as a `///` comment.

**Right:**

```memql
use library.concepts.{ artifact, folder }
use common.traits.{ isActiveRecord }

/// Insert a Library artifact
mutation artifact recordArtifact {
  args {
    folderId  string  @required
    title     string  @required
  }
  insert {
    folderId:  args.folderId
    title:     args.title
    createdAt: now
    createdBy: actor.userId
  }
}

/// Active folders visible to caller
query folder activeFolders {
  args {
    ownerId  string  @required
  }
  filter  row => row.ownerId == args.ownerId && isActiveRecord(row)
  shape   folderFull
}

// A spec binds one concept or shape in its signature, and its lambda
// names the row it reads. A spec takes no args.
spec artifact isArchivedArtifact = row => row.archived == true
```

**Policies take no args at all.** The live `policy` construct is an
empty-bodied AI provider-selection record (the decision-policy tier
that once carried `func (Policy)` bodies with `@tier` / `@audited`
is retired, #984). A caller-context check is a context-spec applied to
the actor in a filter, `requiresOwner(actor)`:

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
mutation folder example {
  args { x string @required }
  insert {
    field: ctx.x   // ctx is not in scope inside struct-form bodies
  }
}
```

**The procedural form is internal.** The struct-form rewriter emits a
`func (Receiver) NAME(ctx any) (any, error) { return <expr>, nil }`
shape for a query and a mutation, for the engine's parser, and authors
never write it; `ctx` is not part of the author surface. A logic's statements follow its `args { }`
block and end with `return <expr>`, which returns the value directly,
with no `ctx.output = ...`.

---

## 21b. Nested object blocks are CLOSED

A nested block that declares sub-fields rejects undeclared keys, exactly as the
top level always has (memql#3641):

```memql
concept user {
  preferences {
    theme               enum("light", "dark", "system")
    computerUseEnabled  bool
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

---

## 21c. A predicate names its receiver: the lambda parameter and bare names

**Rule.** Every predicate position names the value it reads with a
lambda parameter, and every field is read through it (D1 of the
[language freeze record](../../superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md)):

```memql fragment
filter  row => (row.ownerUserId == actor.userId || actor.isClusterOwner == true) && (args.status == nil || row.status == args.status)
spec sendJob isDrainableSendJob = row => row.status == "queued" || row.status == "running"
spec actorEnvelope requiresOwner = actor => actor.role == "owner"
trait isActiveRecord = row => row.active == true
@filter(row => row.status == "archived")
```

The lines are the edition-2026 spellings of `campaigns` and
`isDrainableSendJob` (`dsl/campaigns/`), `requiresOwner`
(`dsl/deployment/specs.memql`), `isActiveRecord`
(`dsl/common/traits.memql`) and an account automation's trigger filter
(`dsl/accounts/automations.memql`).

- **The parameter is the author's name.** Any name that is not a
  reserved root will do, and `row` is the convention -- it is what
  `memqlmigrate --rewrite=expressions` writes. A spec bound to an
  `@actor` shape names its parameter `actor`, and there the parameter is
  the actor envelope itself.
- **A spec or trait is applied to its receiver**: `isActiveRecord(row)`,
  `requiresOwner(actor)`. A bare `isActiveRecord` in a filter is the
  pre-edition spelling, and `spec isX` / `trait isX` as a reference is
  refused, naming `isX(row)`.
- **A nested lambda names its element**: `row.tags.any(t => t == "urgent")`,
  `args.members.where(m => m.active)`, and `(acc, x) => ...` for the two
  parameters of `reduce`. A parameter is in scope inside its own lambda
  body and nowhere else.
- **A canonical id in an expression is a string.** `row.concept == v1:crm:lead`
  is refused (`a canonical id in an expression is written as a string`);
  write `"v1:crm:lead"`.

**How a bare name resolves**, in order:

1. a lambda parameter in scope, innermost first;
2. a reserved root: `args`, `actor`, `now`, `config`, `partition`, and
   in an automation `event`;
3. a statement name or a loop variable bound earlier in the same body
   (`rows := query ...`);
4. when called, a catalog function ([functions.md](functions.md#catalog))
   and then a spec or trait -- the catalog is consulted first, so a
   predicate cannot shadow a function by taking its name
   (`component/memql/expr_eval.go`).

A name that is none of these is `unknown_name`. A payload field is never
a bare name: `status` alone is `unknown_name`, not the row's status.

---

## 21d. A subexpression that does not read the row is a plan constant

**Rule.** In a filter or a spec body, a subexpression that does not read
the row parameter is computed once per call, before the query runs, and
bound as a parameter of the SQL. So an in-process function, arithmetic,
`??` or a map literal is legal there on values that do not read the row:

```memql fragment
filter  row => row.expiresAt < addDuration(now, "P1D")
filter  row => row.rank > args.floor * 2
filter  row => row.stage == (args.stage ?? "active")
```

A subexpression that does read the row has to push down. The load
refuses one that cannot, naming the node, the position and the nearest
spelling that pushes down: `lower(row.email) == args.email` is refused
in a query filter, and `row.email == lower(args.email)` computes the
lower-case value before the query instead (the same test only if the
stored emails are already lower-case). The table of what pushes down,
per position, is [memql.md](memql.md#where-each-expression-runs).

**This is what replaces `when(args.x) { ... }`.** In
`filter row => (args.x == nil || row.f == args.x)` the test
`args.x == nil` reads no row, so it is a plan constant: an absent
argument folds the whole group to `TRUE` and the predicate drops, a
present one folds it to `row.f = $1`. Under `||` the guard is written
the other way round, as `(args.x != nil && row.f == args.x)`. The
observability window read is the tree's `||` case
(`dsl/observability/queries.memql`):

```memql fragment
filter  row => row.bucket == args.bucket
            && row.windowStart >= args.windowStart
            && row.windowStart < args.windowEnd
            && ((args.codeReference != nil && row.codeReference == args.codeReference) || row.codeReference startsWith args.prefixes)
```

One consequence of the unset rule
([#27](#27-unset-is-one-value-in--and--1685--2783)): `args.x == nil` is
also true for an argument sent as `""`, so a blank argument drops the
predicate exactly like an absent one. The `when(args.x)` guard dropped
only on absent or `null` and kept `row.f == ""` for a blank.

A collection method in a filter needs a bounded source: a row array
field (`row.tags.any(t => t == "urgent")` lowers to SQL) or a plan
constant (`args.tags.any(...)`). A query result is unbounded and is
refused there.

---

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
  A filter reads a payload field through its lambda parameter,
  `row.status`. The `payload.<field>` and `<conceptName>.<field>` forms
  are both rejected, and a bare field name is not a field at all in
  edition 2026 (rule
  [#21c](#21c-a-predicate-names-its-receiver-the-lambda-parameter-and-bare-names)).
- **Intrinsics and payload fields share one spelling**
  (`TestFilterIntrinsicsUseRowNamespace`, memql#2779). A filter mixes
  two field surfaces -- the row's columns and its JSON payload -- that
  compile to entirely different SQL (a table column vs a JSONB path).
  Before edition 2026 payload fields were bare, so a bare `id` looked
  exactly like a payload field while compiling to a column; the `row.`
  namespace marked the intrinsic, and this gate enforced it. Under the
  lambda parameter both are
  read through `row` (rule 21c), and the intrinsic names -- `id`, `concept`,
  `type`, `createdAt`, `createdBy`, `provenance` -- are reserved field
  names (rule 19), so the two can never collide:

  ```memql fragment
  filter  row => row.id == args.clusterId      // an intrinsic: the row's id column
  filter  row => row.region == args.region     // a payload field
  ```

  A spec or trait body reads through its own parameter the same way,
  `spec registration isRevoked = row => row.revoked == true`. Mutation
  `insert` / `update` blocks write `id:` / `createdAt:` as target keys
  rather than references, and are unaffected. Sort keys are covered by
  their own gate, below.
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
  filter applies the trait rather than inlining the comparison:
  `isActiveRecord(row)` (not `row.active == true`),
  `isNotDeleted(row)` (not `row.deleted != true`),
  `statusIsActive(row)` (not `row.status == "active"`), and so on for
  the status / identity-type / deletion-scheduled traits.
  Concept-specific predicates (`row.ownerUserId == args.userId`) stay
  inline.
- **No concept-name shortId prefixes** (`TestNoShortIdConceptPrefix`).
  Derived ids are the bare unique part (uuid / hash / slug) — never
  `"agent-" + ...` or another concept-name / sub-type prefix.
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
  context-spec such as `requiresOwner` applied at the top level of the
  filter, `requiresOwner(actor)`),
  an actor-gate ANNOTATION (`@requiresRank` / `@requiresCapability` — see
  [#33](#33-requiresrank-and-requirescapability-epic-memql4832--memql5166)), or an explicit `@public`
  annotation acknowledging the intent. Anything else hard-fails.
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
  `test/dslconformance/no_retired_operators_test.go`): the retired
  connectives -- the `;`-AND and `,`-OR separators, the `has`
  membership operator, and the `?.` conditional prefix -- are refused
  by a line-oriented text scan over the tree. It is a text scan because
  the pre-edition parser accepted all four and the engine computed `;`
  as AND and `,` as OR, which is how a `,` inside parentheses became an
  authorization bypass, closed here rather than in the grammar
  (memql#3612). The scan runs at load over whatever tree the node
  mounted, so a product bundle at `MEMQL_DSL_PATH` is covered too
  (memql#3629); this test runs it over the embedded corpus. The
  edition-2026 parser now refuses each of the four itself, naming the
  replacement and `memqlmigrate --rewrite=expressions`
  ([memql.md](memql.md#retired-spellings)), and `!` is legal in every
  expression position.
- `TestNoInfixWordAndOr` (#973,
  `test/dslconformance/no_word_logical_operators_test.go`): the English `and` / `or`
  infix forms are rejected.
- `TestNoRetiredBindingForms` (#988, `test/dslconformance/no_named_writes_test.go`):
  named writes (`insert <concept> {` / `update <concept> {`) are
  rejected — the write target comes from the
  `mutation <Concept> <name>` signature, the block is bare
  `insert {` / `update {`. A canonical concept id passed to
  `canonicalId` (`canonicalId(x, "v1:ns:name")`) and a hand-built
  `"v1:ns:concept:"` prefix are rejected — name the imported concept,
  `canonicalId(x, "folder")`.

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
  it is not a list. An optional-arg guard,
  `(args.id == nil || row.id == args.id)`, does **not** count — the id
  comparison applies only when the arg is present, so the query can
  still return the full set when it is omitted.
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
query folder folderMeta {
  args { folderId string @required }
  filter  row => row.id == args.folderId
  shape   folderFull
}

// Bounded list — compliant (paginate window).
query folder firstTenFolders {
  filter  row => isActiveRecord(row)
  paginate 10
  shape   folderFull
}

// Legitimate full-set read — compliant, marked + auditable.
@unbounded("provider catalog is a small bounded set — never more than a handful of rows")
query provider allProviders {
  filter  row => isActiveRecord(row)
  shape   providerFull
}

// VIOLATION — list read with no bound. Pulls the whole table.
query widget allWidgets {
  filter  row => row.ownerUserId == args.ownerUserId
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
(artifacts, events, plans) starts cheap and silently degrades into a
full-table scan as the data grows — no error, just creeping latency and
memory. The runtime cap bounds the blast radius; the authoring rule makes
the author think about the bound up front.

---

## 23b. `refine` narrows a page in process, after `paginate`

**Rule.** A predicate over the row that cannot push down -- it calls an
in-process function on a field, or runs a collection method over
something other than a row array -- is not a filter. Write it as a
`refine` clause: `refine row => <expression>`. It runs in process, on
the page `paginate` already read, and keeps the rows the expression is
true for.

```memql
use library.concepts.{ artifact }
use library.shapes.{ artifactFull }
use common.traits.{ isActiveRecord }

/// Artifacts in a folder whose title contains a search term, any case.
query artifact searchFolderArtifacts {
  args {
    folderId  string!
    q         string!
  }
  filter   row => row.folderId == args.folderId && isActiveRecord(row)
  paginate 50
  refine   row => lower(row.title).includes(lower(args.q))
  shape    artifactFull
}
```

- **`refine` requires `paginate`.** It only ever runs over a bounded
  page; a `refine` with no `paginate` is refused at load.
- **A page may come back short.** The clause removes rows after the
  page is read, so a page can hold fewer rows than `paginate` asked
  for, or none; a short page is not the end of the set.
- **It is the in-process tier.** Every catalog function and collection
  method is legal in it, and a construct call is not. Its parameter
  reads the query's bound concept, like the filter's.

`refine` is a named clause on purpose (D11 of the
[language freeze record](../../superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md)):
an expression never falls back to in-process evaluation on its own. A
filter that cannot push down is refused at load and names the fix;
moving the predicate into `refine` is a decision the author makes and a
reviewer can see.

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
  filter  row => isActiveRecord(row)
  shape   agentRoleFull
}
```

**When to reach for it.** Read-heavy queries whose underlying rows change
rarely: bounded catalogs / registries (role / skill catalogs, router
budgets) get long TTLs; hot append-only streams (the per-folder artifact
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
> and for cognition deletes -- the cognition half went with that tree, the
> `v1:agents:*` half did not -- so several of those topics forward again
> for a reason that has nothing to do with caching. The invariant is unchanged;
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
  (import cycle), so the struct-form kinds (query / mutation / logic /
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
targets. See also [#22](#22-tree-wide-conformance-gates-testdslconformanceconformance_testgo)
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

## Grammar versioning and the migration channel

Three labels name the language a tree is written in, coarse to fine
(`component/language/parser/edition.go` and `grammar_version.go`; epic
memql#5356):

- **The language line** -- `memql = "1.0"` in each domain's `memql.toml`
  (see [The language line](memql.md#the-language-line)). The engine refuses
  a domain that declares none, or a newer line than it speaks, and later
  versions of the language key meaning on the line a tree declares.
- **The edition** -- `edition = "2026"` beside it, naming the parser front
  end the tree is written for. An edition never forks the parser: there is
  one lexer, one parser, one AST and one compiler, and a front end is a thin
  source-to-source step that brings one edition's spelling to that core. The
  current edition's step is the identity; a later edition that changes a
  spelling registers a front end that rewrites the old spelling into the new.
- **`parser.GrammarVersion`** -- the fine label, printable with
  `memqlmigrate --grammar-version`. It moves on every change to the authored
  surface inside an edition, and it ends with the 8-hex digest of that
  surface: `TestGrammarVersionCarriesTheSurfaceDigest`
  (`grammar_surface_drift_test.go`) recomputes the digest from a behavioural
  accept/reject corpus, the struct-body parsers' clause arms and the
  invocation keywords, so a change to what an author may write cannot land
  without editing the constant. It is stamped on authored-construct rows
  (`v1:authoring:construct.grammarVersion`) at promote; re-hydration recompiles
  a stored row first and uses a stale stamp only to explain a failure, so a
  bump never unregisters a construct whose source still parses.

**The migration channel is `memqlmigrate`.** Every rewrite is registered in
one registry, keyed by the edition it moves a tree onto and the epic that
shipped it (`cmd/memqlmigrate/rewrites.go`), and reached with `--edition`
(default: the edition this engine writes) and `--rewrite=<name>`;
`--rewrite=language-line` is the one that declares the line in every domain
that has none, and `--rewrite=expressions` moves filters, spec and trait
bodies, conditions and values onto the edition-2026 expression grammar. In
VS Code the language server makes the same `--rewrite=expressions` edit one
construct at a time, as the **Rewrite to edition 2026** quick fix on a retired
spelling's diagnostic ([Sense](sense.md#the-rewrite-quick-fix)).

A rewrite is **required** when a narrowing can strand source someone else
holds: the retired form has in-tree usage, or plausible usage in a
`MEMQL_DSL_PATH` bundle or a durably-promoted authored row. The epic that
narrows the language then ships the rewrite in the same PR and rejects the
retired form at parse time with a hint naming the command. A rewrite is not
required for a narrowing with no usage and no stored-row exposure, nor for a
widening. Both still bump `GrammarVersion`: the version records what the
grammar is, not whether anyone was inconvenienced.

A stale bundle is therefore detectable (the line it declares, and the refusal
naming it), diagnosable (a rejection with a hint), and mechanically fixable
(the rewrite) -- never a silent soft-skip.

**Rejecting at parse time is one of two narrowings, and the harsher one.** Since
language 1.0 was frozen, a form a bundle outside this repo could plausibly be
holding leaves through a **deprecation window** instead: it keeps loading, every
load warns naming the replacement and the release it stops loading at, every use
is counted on `memql_dsl_deprecated_uses_total{rule}`, and only after at least
two minor releases does the parser refuse it. The rewrite requirement above is
unchanged -- a window with no rewrite is a warning with no way out -- and the
forms currently in a window are in
[memql.md](memql.md#forms-in-a-deprecation-window). Reject at parse time where
nothing outside this repo could have written the form; deprecate where it could.

## Reserved args-field names

`now`, `actor`, `partition`, `config`, and `trace` are reserved
top-level identifiers; an `args { }` field of the same name is refused at
parse time (rename it -- e.g. `asOf` for a caller-passed evaluation
instant). `event` is reserved too: in an automation it is the trigger
root, and no statement or loop variable may take a reserved name
(`body_reserved_name`).

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

The cross-domain gate checks the references whose spelling really is a
construct reference: a trait or spec applied in a `filter` clause
(`isNotDeleted(row)`), and a shape named in a `shape` projection clause.
It does not check a name that merely matches a concept: a field is read
through the lambda parameter (`filter row => row.surface != ""` reads
the payload field `surface`, not the `v1:actions:surface` concept), and
10 construct names double as payload field names across the tree.

A reference to a construct declared NOWHERE in the tree is skipped
rather than flagged: a product bundle mounts extra domains at boot via
`MEMQL_DSL_PATH`, and you cannot import what does not exist at lint
time. The rule is "if the tree can see it in another domain, name it" --
never "every reference must resolve here".

```memql fragment
// dsl/worker/queries.memql -- `invocation` is ambient
// (v1:worker:invocation), even though v1:observability:invocation
// shares the trailing segment.
query invocation invocationsForUser {
  ...
}

// Cross-domain names still need the import:
use planner.concepts.{ plan }
```

**Constraints.**

- **Explicit wins.** A file-top import of a name always beats the
  ambient resolution -- importing `observability.concepts.{ invocation }`
  from inside `dsl/worker/` binds the observability concept.
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
Declared-but-unused is legal. A spec or trait body does not read the
`actor` root: a spec that asks about the actor binds an `@actor` shape,
and its lambda parameter, spelled `actor`, is the envelope
(`spec actorEnvelope requiresOwner = actor => actor.role == "owner"`).
A spec or trait over rows that reads it is refused at load
(`lower_actor_in_row_predicate`): an ownership test belongs in the query
filter, `row.ownerUserId == actor.userId`, where row-authz reads it.
Shapes use `@actor` as their kind marker; the seed-file
`@actor("system")` is a different construct. An unknown member (`actor.displayName`) is likewise a load
error and an `actor-unknown-property` squiggle (#2625): the envelope is
a closed set, and both layers read the same canonical table.

---

## 27. Unset is one value in `==` and `!=` (#1685 / #2783)

**Rule.** A missing field, a JSON `null`, the `nil` literal and the empty
string `""` are one value, unset, when `==` or `!=` compares them. `!=`
is the exact negation of `==`, so `!=` against a set value matches an
unset field, and `!= ""` and `!= nil` are the "is set" test.

```memql fragment
filter  row => row.deleted != true      // matches rows with no `deleted` key
filter  row => row.status == "active"   // does not match rows with no `status` key
filter  row => row.consumedAt != ""     // does not match rows with no `consumedAt` key
filter  row => row.consumedAt != nil    // the same test: nil and "" are one value
```

The whole table, as `EvalExpr` implements it
(`component/memql/expr_eval.go`, pinned row by row by
`TestEvalExprAbsenceTable`):

| Expression | `x` missing or `null` | `x` is `""` |
|---|---|---|
| `x == nil`, `x == ""` | true | true |
| `x != nil`, `x != ""` | false | false |
| `x == "open"` | false | false |
| `x != "open"` | true | true |
| `x < "m"` (and `<=`, `>`, `>=`) | false | compared as a string: `"" < "m"` is true |
| `x in ["open", ""]` | true | true |
| `x in ["open", "held"]` | false | false |
| `"open" in x` | false | refused: `in_requires_list` |
| `x startsWith "op"` | false | false |
| `x.includes("op")` | false | false |
| `!(x == "open")` | true | true |
| `x ?? "none"` | `"none"` | `"none"` |
| `x.count()` | `0` | `0` |
| `x.?f` | absent | absent |

Five more facts the table relies on:

- **Whitespace is a value.** `" "` is not unset: `" " == ""` is false.
  Only `??` reads a whitespace-only string as blank
  ([#30](#30--is-blank-coalescing-not-null-coalescing-1614--memql3627)).
- **Equality is typed.** `1 == "1"` is false, numbers compare
  numerically across integers and floats, and `0` and `false` are
  values, never unset.
- **`!` negates exactly.** Every predicate answers true or false, an
  unset operand included, so `!(x == v)` is `x != v` for every `v`.
  An ordered comparison is false for an absent field in both
  directions, so `!(row.n < 5)` is true for a row with no `n` while
  `row.n >= 5` is false: negating an ordered comparison is not the
  reversed comparison.
- **A stored value of the wrong type answers.** Read from the row, a
  value whose type does not fit the operation is not equal, not
  ordered, not a member and not true: over a stored `"true"`,
  `row.flag` is false and `!row.flag` true, and `"a" in row.tags` over
  a stored string is false. `x.count()` counts what is stored -- an
  array's elements, a string's characters, zero for anything else. SQL
  cannot refuse one row mid-scan, and one corrupt row must not fail a
  whole read, so the table's refusals are for a computed value only
  (an argument, a call's result).
- **Each rule has one SQL twin**, named beside it in `expr_eval.go`:
  `IS DISTINCT FROM` for `!=` against a set value,
  `COALESCE(x, '') = ''` (or `<> ''`) for a comparison against unset,
  `NOT COALESCE((e), FALSE)` for `!`. Text orders by byte
  (`COLLATE "C"`), so RFC 3339 UTC timestamps order as instants and no
  locale folds `"é"` into `"e"`. The differential lane holds the SQL and
  the in-process answer to each other, row by row.

**Why.** Both halves of the old rule were bugs before they were rules:

- Plain SQL `<>` yields NULL, not true, when the field is missing, so
  `deleted != true` silently DROPPED every row that never had a
  `deleted` key -- nothing stamps a default on insert at all, which is
  why the concept-field `@default` that used to suggest otherwise is
  retired (#1685, epic memql#5375). Hence `IS DISTINCT FROM`.
- An absent string field is logically equal to `""` -- both mean "not
  set" -- and `!= ""` is the canonical is-set idiom
  (`deletionScheduledAt != ""`, `consumedAt != ""`). Under the bare
  #1685 rule those returned every unset row (#1708 / #1714).
- `== ""`, the *is NOT set* idiom, is the same fact read the other way,
  and until memql#5366 it disagreed with it: a plain `=` against a
  missing key is NULL, so `revokedAt == ""` excluded every session that
  had never been revoked -- no writer stamps `revokedAt` until a
  revocation does. The push-down now spells every unset test
  `COALESCE(expr, '') = ''` (or `<> ''`), for `""` and `nil` alike.

**What changed on 2026-09-13.** The two carve-outs became one rule
(D8 of the
[language freeze record](../../superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md)):
unset is one value, so `==` is symmetric and `!=` is its exact negation
for every pair. Four results differ from before:

- `row.f == ""` matches a row with no `f`. It did not before: only
  `!= ""` carried the carve-out.
- `row.f == nil` matches a blank `f`, and `row.f != nil` excludes it.
  `nil` was a separate presence test (`IS NULL`); now `x != nil` keeps
  the blank rule `exists(x)` had, which is why `exists(x)` retires to it.
- `!(row.f in list)`, the replacement for the retired `not in`, is true
  for an absent `f`, exactly as `row.f != v` is. `not in` treated an
  absent field as a non-match, so `deleted not in [true]` did not
  behave like `deleted != true`; its replacement does.
- A comparison is typed: a literal compares only with a stored value of
  its own JSON type, so a stored `"1"` is not equal to `1` and a stored
  `"true"` is not `true`. The push-down guards each comparison on the
  stored type, so a mistyped or malformed stored value is not equal, not
  ordered and not a member. It used to be cast, and `'abc'::numeric`
  failed the whole read with a Postgres error.

**The trap this creates.** A misspelled field in a `!=` predicate is
absent, so it matches every row:

```memql fragment
filter  row => row.delted != true       // typo -- matches everything, including deleted rows
```

The failure direction is the dangerous one. The same typo in
`== true` returns zero rows and someone notices immediately; in `!=` on
an authorization- or deletion-scoped filter it quietly serves rows that
were meant to be excluded.

**What to do about it.** Prefer the trait over an inline predicate --
`isNotDeleted(row)` rather than `row.deleted != true`. The conformance
gate already requires this wherever a trait exists (rule 22,
`TestNoInlineTraitablePredicates`).

**A misspelled field is caught before it ships**, and so is a misspelled
trait in a `use` line. Three referential lanes cover those positions,
each verified by injecting the typo into a shipped construct. A
misspelled trait at a call site is the position they miss -- see below:

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

**A misspelled trait at the call site was the silent shape.** Row 3
catches a name misspelled in the `use` line. It does not check that call
sites match what was imported:

```memql fragment
use common.traits.{ isNotDeleted }        // correct

filter  row => row.planId == args.planId && isNotDeletd(row)             // typo
filter  row => row.ownerUserId == args.ownerUserId && isNotDeleted(row)  // a sibling keeps the import "used"
```

Measured on the pre-edition spelling (a bare `isNotDeletd` conjunct):
`memqllint`, `go test ./dsl/...` and the referential tests all stayed
green. It surfaced only when the typo orphaned the import entirely
(every call site misspelled), which reports the import as never
referenced. A same-domain trait is the same hole with no import at all
to orphan -- rule 25 (#2617) makes it ambient, so there is nothing for
the import lane to inspect.

It does fail closed: on first call the query errors
`unknown spec "isNotDeletd"` rather than quietly returning every row. A
loud runtime error, not a build-time gate.

**What field validation structurally cannot see** is a field that is
real and declared but scoped from the wrong source:
`filter row => row.id == args.userId` is a well-formed reference to an
existing property -- it just trusts an argument where it should have
trusted the token. That is #2799 / #2800 / #2803, not this rule.

So the fail-open direction above is real but mostly out of reach of a
typo now. It remains reachable by a caller-supplied id, and by a field
declared on the concept that is simply absent on a given row -- which is
the case the unset rule gets deliberately right.

---

## 27b. `.?` reads through an object that may be absent

**Rule.** Where the object a member path passes through may be absent --
an optional object field, an optional arg, an untyped value -- write
`.?` after it. The load requires `.?` there and refuses `.` with the
`.?` spelling in the message:

```memql fragment
filter  row => row.?lineage.originatingRunId == args.runId
@filter(row => row.?preferences.computerUseEnabled == false)
```

`lineage` on `v1:agents:agent` and `preferences` on `v1:identity:user`
are optional object blocks, so a row may carry neither.

At run time `.` and `.?` behave the same: a read through an absent
object is absent, never an error, and the rest of the chain stays
absent. The unset rule (#27) then decides the comparison, so the
trigger filter above fires only for a user whose preferences say
`false`, not for one who never set them. What `.?` adds is the
statement, checked at load, that the author knows the object may be
missing.

`.?` is one token written after the object, and it binds like `.`. It
is not the retired `?.` prefix, which was the optional-argument guard:
`?.status == args.x` is refused naming
`(args.x == nil || <predicate>)`, and `?.` written after an object is
refused with `optional member access is written .? after the object,
as in x.?field`.

---

## 28. Whitespace changes what `-` means; a fraction needs its leading `0` (memql#3624)

Two lexical traps in the same neighbourhood, both now refused.

**A fraction is written with a leading digit.** `.5` used to lex as an
identifier, so it reached the comparison as the *string* `".5"` while
`0.5` reached it as the float:

```memql retired
filter  row => row.score > .5       // refused at load since memql#3624
filter  row => row.score > 0.5      // correct -- and always was
```

Error: `a number cannot start with '.' at line N, column M: a decimal
literal needs a leading digit (write "0.5", not ".5")`. Rejecting rather
than accepting `.5` as a second spelling keeps one canonical form per
construct, matching the rest of this document.

**A hyphen glued between two names is one name.** `-` is a legal
identifier character (the seed catalog's kebab-case slugs,
`seed skill workbench-baseline`, and the capability-script argument
names in `dsl/install/actions.memql`), so `total-used` lexes as a single
identifier. Before edition 2026 a filter compared against it and matched
nothing, silently:

```memql retired
filter  remaining == total-used     // pre-edition: compared against the string "total-used"
```

The lexer cannot tell a name from a subtraction -- only *position*
separates them, and the lexer does not know position. No lexical rule
can tell them apart without either breaking the 189 hyphenated names the
tree depends on or silently flipping which reading wins; both were
measured in memql#3624 and rejected, and the reasoning is recorded at
`case '-'` in `component/language/parser/lexer.go`. The edition-2026
parser does know position, so it closes the case there: in an
expression a hyphenated name is refused
(`` `row.total-used` reads as one name; write `row.total - used` (spaces) for subtraction ``),
while a named argument or a map key, which is never an expression, may
still be hyphenated. Put spaces around a `-` you mean as an operator:

```memql fragment
filter  row => row.remaining == row.total - row.used
```

| spelling | tokens | in an edition-2026 expression |
|---|---|---|
| `a - b`, `a -b` | `a` `-` `b` | subtraction |
| `a-b` | `a-b` | refused: one name (silent before edition 2026) |
| `a- b` | `a-` `b` | refused |
| `a -5`, `5-3` | two operands | refused: put a space after `-` |

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

The prompt this was found on, `cognitionPrediction`, declared two fields
while `component/polyphon` passed nine, and the caller swallowed the
error into its pattern-based fallback -- so the entire
predictive-cognition LLM path was dead, and looked like normal operation
the whole time. The same class had already been fixed once by hand (the
`directive` field on `cognitionReply`). Both prompts and the caller have
since been removed with the cognition tree; the check they motivated
runs on every prompt in the tree today.

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

**Rule.** `a ?? b` falls through to `b` when `a` is missing, `null`,
**or a string that is empty or contains only whitespace**. It does not
fall through for `false`, `0`, `[]` or `{}` -- those are values. `??` is
the only spelling: `coalesce(a, b)` is retired and refused, naming
`a ?? b` and `memqlmigrate --rewrite=expressions`.

```memql fragment
// with the caller passing v:
//   false   -> false        kept
//   0       -> 0            kept
//   []      -> []           kept
//   {}      -> {}           kept
//   ""      -> "DEFAULT"    replaced
//   " "     -> "DEFAULT"    replaced   <-- the sharp edge
//   "\t\n"  -> "DEFAULT"    replaced
//   "value" -> "value"      kept
insert { v: args.v ?? "DEFAULT" }
```

`a ?? b ?? c` folds left to the first operand that is not blank, and the
last operand is returned even when it is blank. `??` binds tighter than
comparison, so `args.stage ?? "" == "active"` compares the coalesced
value, and `b` is not evaluated when `a` wins.

**Why it bites you.** The operator is called null-coalescing everywhere,
including in this repo, so nothing prepares you for the whitespace line:
**a user who deliberately clears a text field gets the default written
back over their clearing**, and a field holding a single space is
treated as absent when it is not absent by any reading. It is also the
one place whitespace counts as blank: to `==` a `" "` is a value
([#27](#27-unset-is-one-value-in--and--1685--2783)).

**Why it stays this way.** The empty-string behaviour is deliberate
(#1614): `f: args.f ?? ""` has to be able to land an explicit empty
string, because a `null` there fails JSON-schema validation on a
non-required string field. The whole corpus is written against that
rule, and NO field position has another spelling — `@default` is rejected
at load on an args field (#991) and, since epic memql#5375, on a concept
field too. Neither was ever applied on insert, so a field carrying one
did not default; the concept-field form was published as the
JSON-Schema `default` keyword, which no validator applies. `??` is the
only mechanism that fills a value. (`@default` DOES stay on a `tool` /
`prompt` / `builtin` field, where the body IS the schema handed to the
model and `default` is a value the model reads.) Changing the operator under the corpus to settle a
naming complaint would be the larger defect.

**What to do about it.** When a stored value must survive a caller
sending blank, that is `@noUnset("field")` on the mutation (memql#3415)
— the targeted opt-out. It drops a named field from the delta when the
incoming value is empty and the stored one is not, which is exactly the
"do not let a blank overwrite this" rule that `??` does not express. Empty
means nil, a blank or whitespace-only string, or an empty array or object;
a numeric or boolean zero is a value, so `0` and `false` still write (see
[`@noUnset`](attribute-matrix.md#nounset)).

**One rule, one implementation.** Every evaluator of `??` resolves
through `coalesceSelect` in `component/memql/mutation_templates.go`,
`EvalExpr` included: the final arm is the ultimate fallback and is
returned even when blank, while every non-final arm is skipped when nil,
missing, or blank. The two pre-edition spellings, `??` and
`coalesce(...)`, were two implementations that disagreed on a blank
middle arm until memql#3627 (`coalesce(args.a, "", args.c)` with nothing
resolving gave `""` from a payload slot and `nil` from an `id:` slot).
Pinned by `coalesce_array_missing_3627_test.go`.

**A missing arg contributes nothing to either container.** An absent
optional arg omits its key from a map literal and omits its element
from a list literal — `{ v: [args.a, args.b, "c"] }` with only `b`
supplied renders `{"v":["B","c"]}`, not a `null` hole (memql#3627). An
explicit `nil` the author wrote is kept in both.

In a filter or a spec body `??` is legal only on values that do not read
the row: `row.stage == (args.stage ?? "active")` computes the fallback
before the query
([21d](#21d-a-subexpression-that-does-not-read-the-row-is-a-plan-constant)),
and `row.alias ?? "x"` is refused there.

---

## 31. `use` and namespaces: what an import means, per construct kind

**A directory is a namespace.** Every `.memql` file in one directory shares a
namespace and its constructs reference each other freely. A **subdirectory is a
different namespace** — `dsl/agents/roles/` is `agents/roles`, not `agents`.
Anything from another namespace must be imported with `use`, whatever kind it
is. Name collisions across namespaces are resolved by **aliasing**
(`use observability.concepts.{ invocation as codeInvocation }`, memql#3802).

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
`builtin cognitionTrackPresence(...)` in the cognition automations file
(since removed with that tree), which carried no `use` block at all.
The measurement is what it was; the file it names is gone.

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

### A core reference to a runtime-declared name is the late-binding seam (memql#4882)

The cross-namespace-import gate reads the MERGED tree at boot (memql#4051):
embedded core plus every runtime domain a product bundle mounts at
`MEMQL_DSL_PATH`. A reference in the core tree can therefore be declared
elsewhere by construction, and the case that found it was the cognition logic
file (since removed with that tree) calling
`mutation mutationCreateCanvasState(...)`, which the engine documented as
"supplied by a product bundle at runtime". The `use` the gate would ask for
cannot be written -- the core file does not know the product namespace exists
-- and a violation lands on the load report as a skip, which strict boot
refuses. So a bundle that did exactly what the engine asks refused every node
that mounted it.

The gate exempts that direction and no other: a CORE file referencing a name
declared only by a NON-core domain is not reported. Runtime -> core, runtime
-> runtime and core -> core still need their `use`. The verdict on which
domains are core arrives through `dslgate.Options.CoreDomain`
(`contract_gates.go` passes the embedded tree's own directory list); a caller
that passes nothing gets the whole rule, which is the fail-closed direction.

## 32. `startsWith` is a selection, never a pass-through (memql#4208)

**Rule.** `s startsWith p` is true when the string `s` begins with the
prefix `p`, or with any prefix in a list of them. The right-hand side is
a string, a list of strings, or an expression that yields either
(`args.prefixes`). Two inputs match nothing, and that is the contract
rather than an edge case:

- an **empty list** (`args.prefixes` bound to `[]`);
- a **blank prefix** (`""` or whitespace), on its own or inside a list --
  blanks are dropped, and a list of nothing but blanks is an empty list.

`s.includes(sub)`, the substring test that replaced `contains(s, sub)`,
follows the same rule: a blank `sub` matches nothing, because `""`
occurs in every string and a blank search must not widen a selection to
every row.

Right -- a prefix-scoped read that cannot widen (the read behind
memql#4208, `dsl/observability/queries.memql`):

```memql fragment
filter  row => row.bucket == args.bucket
            && ((args.codeReference != nil && row.codeReference == args.codeReference) || row.codeReference startsWith args.prefixes)
```

Wrong -- expecting Go's `strings.HasPrefix(s, "")`:

```memql fragment
filter  row => row.codeReference startsWith args.prefix   // args.prefix == "" returns no rows, not every row
```

**Why.** Every language's HasPrefix says the empty string is a prefix of
everything, and in a query filter that is exactly the fail-open shape
this document keeps recording (`!= ""` as the is-set idiom in #27, `??`
blank-coalescing in #30): a selection that admits every row on a blank
input. `row.codeReference startsWith args.prefixes` is safe to hand
whatever list the caller holds because neither an empty list nor a list
of blanks can turn it into a cluster-wide scan. The engine rule lives in
`normalizePrefixValues` (`component/memql/executor_filter.go`) and every
evaluator reads it -- the SQL compile (`((payload #>> '{f}') ^@
ANY(?::text[]))`, or the constant `FALSE` for an empty list), the
in-process post-filter every SQL candidate goes through, and `EvalExpr`
(`exprStartsWith`), so they cannot disagree.

**What it does not do.**

- It is not a pattern. `^@` is Postgres `starts_with()` as an operator,
  a byte-prefix test; `%` and `_` in a prefix are literal, and nothing
  is escaped or concatenated -- one `text[]` parameter is bound whatever
  the right-hand shape was.
- It does not drop on absence. `(args.x == nil || ...)` is the "no
  constraint when the arg is absent" form
  ([21d](#21d-a-subexpression-that-does-not-read-the-row-is-a-plan-constant));
  a blank is not a prefix. Declare the list arg `[]string!` when an
  absent list must be a refused call rather than an unconstrained one.
- It needs a string subject. An absent or non-string left side is
  false, not an error; a right side that is neither a string nor a list
  of strings is refused.
- `not startsWith` is not a form: negate the comparison,
  `!(row.codeReference startsWith p)`.

**Tested by.** `component/language/parser/startswith_test.go` (grammar and
its negative cases), `component/memql/executor_filter_startswith_test.go`
(SQL + in-process agreement), `TestEvalExprAbsenceTable` in
`component/memql/expr_eval_test.go` (the in-process rule over absent,
null, blank and set subjects), and
`component/memql/code_metrics_in_window_db_test.go` (the memql#4208 read
against a real Postgres, db-gated).

## 33. `@requiresRank` and `@requiresCapability` (epic memql#4832 / memql#5166)

A construct states who may CALL it. Two annotations, on a `query`, a `mutation` or
a `logic`, and they answer different questions:

```memql fragment
@requiresRank("developer")                    // a FLOOR on the cluster's ladder
@requiresCapability("update", "principal")    // a GRANT a role was given
```

**A rank is a floor and a capability is a grant, and a cluster can hold one
without the other.** `developer` ranks 300 above `admin`'s 200 and holds
strictly fewer verbs on `principal`, so the two have opposite answers on the
pair that matters most. Reach for the FLOOR when the admitted set is a
contiguous top of the ladder ("who works on this cluster"); reach for the GRANT
when what excludes somebody is a permission rather than a rung ("who edits
credentials"). Declared together, BOTH must pass.

Both are validated at LOAD -- a rank naming no role in `dsl/rbac`, or a verb
outside the five, or a resource no role holds a grant on, refuses boot with the
known set in the message. Both are enforced at execution, on the direct call and
on every plan that EXPANDS the construct: a floor checked only on the direct
call is bypassed by a query that expands the floored construct.

They gate WHO MAY CALL. `@rowAuthz` still decides WHICH ROWS come back, and a
floor that also narrowed rows would be a second answer to a question the tier
already answers.

**They replaced three specs, and the replacement is not a rename.**
`requiresAdmin`, `requiresOwnerOrAdmin` and `requiresDeveloperOrAbove` compared
the actor's role STRING against literals. Three faults, all closed here:

- a slug comparison cannot see a custom role at all -- `role == "admin"` is
  false for a rank-250 role holding every principal verb, so every role a
  cluster authored for itself was refused every surface those gated;
- the names went stale silently: `requiresDeveloperOrAbove` reads as a floor and
  WAS a three-value set;
- a misspelled spec name is a missing conjunct nothing notices, because
  `dslgate`'s recogniser knows gates BY NAME and a gate it does not know is not
  a gate.

A ROLE COMPARISON IS NOT A SPEC ANY MORE. What still belongs in a context-spec
is a caller predicate no rank and no grant can express -- a question about the
actor themselves.

## 34. `account="<field>"` is one argument for two field shapes (epic memql#5165)

`@rowAuthz(owner="<field>", account="<field>")` is the **account grant**: the
named field holds the account a row belongs to, and reads *and writes* widen to
"the owner, OR anyone whose group ties them to this row's account".

```memql fragment
@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")   // one account
@rowAuthz(owner="ownerUserId", clusterOwner, account="accountIds")  // several
```

**Both spellings are the same argument, deliberately.** "Which accounts is this
row for" is one question, and the lowering answers a `string` and a `[]string`
with a single jsonb containment test — so nothing you write says which shape it
is, and the concept's own field declaration decides.

### The field's TYPE is checked at load, and that is not tidiness

`string` or `[]string`, and nothing else. The containment test the lowering uses
also matches a top-level **key** of a jsonb object, so a field declared `object`
would admit any row whose map happened to carry an account id as a key — a
widening nobody wrote and nothing else would catch. A field the concept does not
declare is refused for the reason an unknown `owner=` field is: it lowers to a
scope that matches nothing, which is a gate that reads like a widening and
grants nobody anything.

Declaring `account=` on the OWNER field is also refused: it would compare a user
id against an account id, and the author meant one of the two.

### It widens WRITES, and the verb is decided elsewhere

This is the one place the program widens the rank record's read-only-peer rule.
A member may write a tied row whatever the owner's rank, **if their role holds
the verb** — decided upstream by the data-plane capability gate and the
mutation's own annotations. The row gate answers *which rows*, never *may this
actor write at all*.

`rankStrict` is untouched: it withdraws the cluster-owner write escape, not the
account branch.

### A row with no account is not a tied row

An absent or empty account field is admitted by **no** account branch, including
staff's. Such a row is somebody's own work, and the owner and cluster-owner
branches still decide it.

### It widens the TIER, not a query that narrows itself

The tier's predicate is **ANDed** into a bound read. A query whose own filter
already says `row.ownerUserId == actor.userId` therefore stays owner-scoped no matter
what its concept declares — an AND with a hand-written owner conjunct cannot be
widened by anything.

`sitesForAccount` is exactly that shape and predates the grant, so a member of
Acme's group reads Acme's site through the generic path and **not** through that
query. If you want a named read to serve members, its filter has to stop saying
who the caller must be and let the tier decide.

### The generic browse caches for 60 seconds

An unbound read (the generic concept browse, the internal query form
`concept=="v1:platform:site"`)
injects no tier predicate — admission is the per-row egress gate alone — so its
plan carries no account term, and the account fingerprint joins the cache
signature only when one is present. The signature folds in the ACTOR, so no
caller ever sees another's rows; what it does not fold in is that caller's
MEMBERSHIPS. A freshly-placed member therefore waits out the 60-second TTL on
that path.

Named reads are unaffected: they bind a concept, so the tier is injected, the
plan carries the account term, and the fingerprint is in the key. This is the
rank scope's own pre-existing shape rather than anything the account grant
introduced.

### If you add the argument to an existing concept

Every read of that concept widens the moment you do, and nothing narrows. Check
the concept's CHILDREN too: a member who can read a campaign has to be able to
read its deliveries, and a child with no `accountId` of its own stays
owner-only — which reads as a permissions bug in a screen that half works. The
four campaigns children gained a stamped-from-parent `accountId` for exactly
that reason, with no backfill for the rows written before it.

## Automation loops

An automation that writes what it triggers on needs a filter the write cannot
satisfy, or a converging `@loop` annotation whose `until` predicate is negated in
the filter. Directory-mode `memqllint` rejects uncovered cycles. Prefer a
before-write automation for adjusting fields on the triggering row: it changes
the incoming version without issuing another write. See
[loop protection](memql.md#loop-protection) and
[before-write adjustments](memql.md#before-write-adjustments).
