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

```memql
use cognition.concepts.{ space }

// Right -- one bare insert. The target concept comes from the
// `mutation <Concept> <name>` signature; restating it is retired.
mutation space mutationCreateSpace {
  args { name string @required }
  insert {
    name: args.name
    status: "active"
    createdAt: now
    createdBy: actor.userId
  }
}

// Wrong -- two writes in one body. The parser rejects it.
mutation space mutationCreateSpaceAndGrantOwner {
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
use identity.mutations.{ mutationGrantPartitionAccess }

// 1. The product calls this mutation. (`@default` is not valid on
//    an args field -- apply defaults in the body via coalesce().)
mutation partition mutationCreatePartition {
  args {
    name      string  @required
    type      string
  }
  insert {
    name: args.name
    partitionType: coalesce(args.type, "standard")
    status: "active"
    createdAt: now
    createdBy: actor.userId
  }
}

// 2. An automation fires on the row landing and grants the
//    creating user owner access. Note the step calls the logic
//    construct by its bare (un-prefixed) name.
@enabled
@trigger(event="node.created", concept="v1:platform:partition", partition="*")
@description("Grant the partition creator owner access on first landing.")
automation autoBootstrapWorkspaceOwnerAccess {
  step grant {
    logic grantOwnerOnPartitionCreate { event: event }
  }
}

logic logicGrantOwnerOnPartitionCreate {
  args { event object @required }
  body {
    return mutationGrantPartitionAccess({
      userId:      args.event.payload.createdBy,
      partitionId: args.event.payload.id,
      role:        "owner"
    })
  }
}
```

The product calls `mutationCreatePartition` once. The automation
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

```memql
use cognition.queries.{ queryActiveSpaceIds }

// `sort` is not a registered function -- engine init fails.
logic logicListSpacesSorted {
  args { event object @required }
  body {
    return sort(queryActiveSpaceIds({}), "name", "asc")
  }
}
```

**Right -- struct queries have dedicated clauses.** Sorting,
windowing, and latest-per-id snapshots are `sort` / `paginate` /
`asOf` clauses on the struct query itself, not directive calls
(live examples: `queryLatestSpaceContextForSpace` in
`dsl/cognition/queries.memql`, `queryStaleClusterNodes` in
`dsl/cluster/queries.memql`):

```memql
use cognition.concepts.{ context }

@enabled
@description("Latest space-context row for a space.")
query context queryLatestSpaceContextForSpace {
  args {
    spaceId  string  @required
  }
  filter  spaceId == args.spaceId
  sort    "createdAt", "desc"
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

## 2. Function-call argument keys are bare identifiers

**Rule.** Function call argument object keys are **bare identifiers**,
not quoted strings. Values follow the standard MemQL grammar (quoted
strings, numbers, booleans, null, nested objects, arrays).

**Canonical:**
```memql
mutationCreatePartition({name: "test", partitionType: "standard"})
querySpaceParticipants({spaceId: "space-123", participantType: "si"})
```

Quoted string keys are also accepted so JSON-serialized tool calls
that arrive through the same parser path keep working:

```memql
mutationCreatePartition({"name": "test", "partitionType": "standard"})
```

Both forms parse identically. Mixed is fine too. The public RPC
(`ExecuteQuery`), the CLI/SDK call builders, the function-definition
parser, and the automation-DSL parser all use the same rule now --
there is no strict vs. relaxed split anymore.

If you build a call string from a Go template, either form works;
the bare-identifier form is easier to read.

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

```memql
// REJECTED -- concept-level @scope is gone.
@scope("global")
concept node { ... }
```

The full concept-annotation author surface is `@description`,
`@version`, `@namespace`, `@type`, and `@displayCard` -- see
[#7](#7-annotations-on-concepts-where-to-put-new-ones).

(Unrelated: **seed** constructs have their own `@scope("perUser")`
annotation -- see `dsl/agents/assistant.memql`. That is a seed
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
to plain queries. Use `asOf("2026-01-01T00:00:00Z")` from the
top-level parser if you need a historical snapshot; struct queries
carry an `asOf latest` clause for explicit latest-per-id reads (see
`queryStaleClusterNodes` in `dsl/cluster/queries.memql`).

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
{partition}:{concept}:{id-segment}
```

Where:
- `partition` = the resolved partition for the call
- `concept` = the concept bound by the `mutation <Concept> <name>`
  signature
- `id-segment` = the trimmed value of the `id:` field

If you omit `id:`, the engine derives a content hash from the 
Same payload twice ⇒ same id ⇒ a new time-series row under that id.
Different payload ⇒ different id ⇒ a different row.

**Common bug**: forgetting to set `id:` means duplicate inserts
create new ids instead of new versions of the same id.

---

## 10. Subscriptions and event topic shape

**Rule.** Event topics are **5 segments**:

```
graph.node.{created|updated|deleted}.{partition}.{concept}
```

Subscriptions can use `*` to match any single segment:

```
node.*.*.v1:cluster:node       # any partition, this concept
node.created.*.v1:cluster:node # only creates, any partition
```

The CLI prepends `graph.` automatically when subscription kind is
`SUBSCRIPTION_KIND_GRAPH_EVENTS` -- so the filter you pass is
`node.*.*.<concept>`, NOT `graph.node.*.*.<concept>`.

The partition segment is a #56 phase-8 vestige -- always use a `*`
wildcard for it (the same reason `@trigger(...)` patterns in
`dsl/*/automations.memql` carry `partition="*"`). A literal
partition match is a bug waiting for phase 8 to land.

---

## 11. Role enum: owner / admin / writer / reader

**Rule.** The unified role spectrum is **owner / admin / writer /
reader**. This applies to:

- `v1:identity:user.role` (cluster-wide)
- `v1:identity:partitionAccess.role` (per-partition)
- `v1:identity:delegation.roleCeiling`
- `v1:data:policy.revertMinRole`
- The `UserRole` proto enum
- `component/auth/rbac.go` (`RoleOwner`, `RoleAdmin`, `RoleWriter`,
  `RoleReader`)

The retired values **manager** and **user** are gone. Legacy data is
migrated at read time by `migrateRole` in `rbac.go`:

- `manager` -> `writer`
- `user` -> `reader`
- `developer` / `advocate` / `member` / `guest` -> the nearest match

**If you add a new concept with a role enum, use the four new values
only.** Don't add legacy values "for compatibility" -- the migrator
already handles old rows.

**Ordering.** `RoleLevel` returns: owner=0, admin=1, writer=2,
reader=3. Lower number = higher privilege. `RoleAtMost(a, b)` returns
the more-restrictive of the two (useful for delegation ceilings).

---

## 11b. cond() for conditional values -- not `if` at expression position

**Rule.** When you need a conditional value inside an expression (a
mutation payload, an argument, a function body), use `cond(predicate,
thenValue, elseValue)`. The `if` keyword is reserved for the
control-flow statement (`if condition { step }` in automations) and
does NOT work as a value-returning expression.

```memql
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

## 12. `partition` is a reserved payload field -- use `partitionName`

**Rule.** `partition` is one of the engine's reserved payload-level
fields (alongside `id`, `createdAt`, `createdBy`, `concept`, `payload`,
`schema`, `type`). Declaring a concept property named `partition`
fails `ensureReservedFieldsNotDeclared` at startup:

```
concept v1:identity:partitionAccess definition schema declares reserved property "partition"
```

Use `partitionName` (or similarly explicit) instead. `v1:identity:
partitionAccess.partitionName` is the canonical example.

**Why it bites you.** The PK for partition-scoped rows is
`(partition, id, createdAt)`. The engine uses the name `partition`
for the PK column, so any payload field with the same name would
shadow it in queries and confuse the schema check.

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

```memql
checkUser := queryUserById({ userId: args.event.payload.userId })

result := if cehckUser.empty() {   // typo: cehckUser -> checkUser
  mutationCreateUser({...})
}
```

The compiler emits:

```
automation "bootstrapUser": step "result" references unknown step "cehckUser" -- check for a typo, or add the step
```

Example of a cycle (would deadlock at runtime):

```memql
a := if b.empty() { queryFoo({}) }
b := if a.empty() { queryBar({}) }
```

The compiler emits:

```
automation "test": dependency cycle among steps [a b]
```

---

## 14. Function naming: construct name carries the kind prefix

**Rule.** Query / mutation / spec / trait / logic constructs are
named with a kind prefix: `queryActiveSpaces`, `mutationCreateSpace`,
`specIsHumanParticipant`, `traitIsActiveRecord`, `logicAutoJoinSI`.
Constructs live in one consolidated file per kind per namespace
(`dsl/<namespace>/<construct>s.memql`), so the file name never
carries an individual construct's name.

```
dsl/cognition/queries.memql     query space queryActiveSpaces { ... }
dsl/cognition/mutations.memql   mutation space mutationCreateSpace { ... }
dsl/cognition/specs.memql       spec specIsHumanParticipant { ... }
dsl/common/traits.memql         trait traitIsActiveRecord { ... }
dsl/cognition/logic.memql       logic logicAutoJoinSI { ... }
```

**Why it bites you.** Callers (the product frontend, automations, Go
integration code) name functions as a string. A mixed convention means
every caller has to guess whether to add a prefix. Pre-rename, the
frontend hit runtime "function not found" errors because half the
backend had prefixed names and half didn't.

Enforcement: the **linter** (`component/language/compiler/linter.go`)
emits `naming.query-prefix` / `naming.mutation-prefix` /
`naming.spec-prefix` warnings when a construct of the given kind is
declared without the prefix. With `StrictWarnings: true` in the
compiler config, these become hard errors. (The old filename-derived
enforcement in the function loader was retired with the flattened
per-construct tree.)

One wrinkle: automation **step bodies reference logic constructs by
the bare, un-prefixed name** -- `step run { logic autoJoinSI { event:
event } }` resolves to `logic logicAutoJoinSI` through the file-top
`use cognition.logic.{ logicAutoJoinSI }` import (see
`dsl/cognition/automations.memql`).

Automations are event-triggered, not called by name, so they use
verb-first names with no prefix (`autoJoinSI`, `bootstrapSession`,
`purgeExpiredArchivedSpaces`). Builtins, tools, prompts, providers,
and shapes are out of scope for this rule and use their own
conventions (shapes are conventionally `<concept><Projection>`, e.g.
`participantFull`, `spaceCard`).

---

## 15. Write-block shorthand: bare `args.ident` infers the key

**Rule.** Inside a mutation's `insert { ... }` / `update { ... }`
block, a bare `args.ident` with no `key:` prefix is shorthand for
`ident: args.ident`. The key is taken from the arg path's final
segment. Only single-segment paths are eligible; `args.user.id`
falls through to the verbose `userId: args.user.id` form.

```memql
// Verbose -- still valid, still works.
insert {
  spaceId:     args.spaceId
  agentId:     args.agentId
  displayName: args.displayName
}

// Shorthand -- equivalent.
insert {
  args.spaceId
  args.agentId
  args.displayName
}
```

Mix the two freely when it reads better (live example:
`mutationAddAgentToSpace` in `dsl/cognition/mutations.memql`):

```memql
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
@description("Full agent projection")
shape agent agentFull {
  row.id
  name
  description
  row.createdAt
}
```

A shape can `include` another shape for composition; transitive
inclusion is supported, cycles + field collisions are errors.

**Retired forms (rejected at parse time):** the receiver form and
its template wrapper are gone --

```memql
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

```memql
// Correct -- mutation write block
insert {
  name: args.name
  spaceId: args.spaceId
  active: true
}

// Correct -- step-call args in an automation / logic body
mutationCreateUser({ userId: args.event.payload.subject, email: args.event.payload.email })

// Wrong -- unnecessary quotes on simple-identifier keys
mutationCreateUser({ "userId": args.event.payload.subject, "email": args.event.payload.email })
```

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
 Declaring any of them as a payload property in a concept
schema is rejected at concept-load time by
`ensureReservedFieldsNotDeclared`:

```
concept v1:foo:bar definition schema declares reserved property "createdBy"
```

If a single concept fails to load, the whole concept loader bails --
which means **no concepts get registered**, the BFF can't serve any
graph queries, and the entire cluster is bricked at startup.

The reserved set today: `id`, `createdAt`, `createdBy`, `partition`,
`concept`, `payload`, `schema`, `type`. Full list in
`component/database/memory-nodes/constants.go`.

Practical consequences for concept authors:

- **`createdBy`**: never declare it. The engine sets it from the
  request actor on every insert. If you need a separate
  "issued by some other actor" field (a grant is created by an admin
  but stamped on a different user), use a payload field with a
  distinct name like `grantedBy`. See
  `v1:identity:partitionAccess.grantedBy` for the canonical example.
- **`partition`**: see [#12](#12-partition-is-a-reserved-payload-field----use-partitionname).
  Use `partitionName` instead.
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
delete plus dropping the matching `mutationCreatePartition` arg.

---

## 20. Foreign-key id derivation: use `canonicalId()` before hashing

When a mutation derives a deterministic id by hashing foreign-key
args (the participant id pattern: `id = hash(spaceId + ":" + userId)`),
the args MUST go through `canonicalId(value, "<conceptType>")` first.
The hash is byte-level, so two callers passing the same logical
reference under different shapes (`"user-abc"` vs
`"_system:v1:identity:user:user-abc"`) hash to different strings and
produce DUPLICATE rows with distinct ids.

```memql
// Wrong -- bare-vs-canonical input shape changes the participant id
insert {
  id: hash(concat(args.spaceId, ":", args.userId))
  ...
}

// Right -- canonicalId() collapses both forms to the same string. The
// second argument is the imported concept short-name (resolved against
// the file-top `use ...concepts.{ space, user }` imports).
insert {
  id: hash(concat(
    canonicalId(args.spaceId, space), ":",
    canonicalId(args.userId,  user)
  ))
  ...
}
```

(Don't prefix the hash with the concept name -- `id:
concat("participant-", hash(...))` duplicates information already in
the canonical id position, and `dsl/conformance_test.go`'s
`TestNoShortIdConceptPrefix` rejects known concept-name prefixes
outright. The shortId is the bare hash / uuid / slug.)

`canonicalId(value, concept)` -- `concept` is an imported concept
short-name (the stringly-typed `"v1:ns:name"` literal is retired):

- bare slug → prepends `<partition>:<concept>:` (engine reads the
  concept's `@scope` to pick `_system` for global concepts, otherwise
  the request envelope's partition)
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

Affected mutations (audit done 2026-05-06; all live under
`dsl/cognition/mutations.memql` today):
`joinSpaceAsHuman`, `joinSpaceAsSI`, `createGreetingUtterance`,
`createSessionForParticipant`, `sendTextUtterance`,
`sendSpeechUtterance`, `sendActionUtterance`,
`sendRealtimeTranscriptUtterance`.

The historical `concat("ga-", hash(actor))` pattern in autoJoinSI is
gone entirely: `logicAutoJoinSI` (`dsl/cognition/logic.memql`) now
resolves the assistant via `queryAssistantAgentForUser` + the space
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

**Right (struct form — the canonical author surface):**

```memql
use cognition.concepts.{ utterance, space }
use common.traits.{ traitIsActiveRecord }

@description("Insert a chat utterance")
mutation utterance mutationSendUtterance {
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

@description("Active spaces visible to caller")
query space queryActiveSpaces {
  args {
    ownerId  string  @required
  }
  filter  ownerId == args.ownerId && traitIsActiveRecord
  shape   spaceFull
}

// Spec — struct form. Binds one shape XOR concept in the signature;
// the body returns a boolean over bare field names. No args.
use cognition.concepts.{ participant }

spec participant specIsHumanParticipant {
  return participantType == "human"
}
```

**Policies take no args at all.** The live `policy` construct is an
empty-bodied AI provider-selection record (the decision-policy tier
that once carried `func (Policy)` bodies with `@tier` / `@audited`
is retired, #984 — caller-context boolean checks belong in
context-specs called via `spec("name")`):

```memql
@primary("streamClaudeSonnet")
@fallback("stream54Pro")
@description("Default chat policy for non-operator agents.")
policy balancedChat { }
```

**Wrong (rejected at registration):**

```memql
// Legacy func (Spec) form — specs are struct-form now.
func (Spec) example(ctx any) bool {
  return true
}

// args.X is the only way to reach caller-passed fields.
mutation space mutationExample {
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
block, the keys ARE bare field names of the row's  Saying
`spaceId: args.spaceId` keeps the LHS (concept payload key) and RHS
(caller arg) visually distinct. The same precedent applies to query
filters: `spaceId == args.spaceId` reads correctly without
needing the reader to guess which side is concept-field vs caller-arg.

**For automations:** the triggering event payload is bound as
`args`, so `args.topic`, `args.kind`, and `args.payload.<field>`
reach the event from inside the automation body.

---

## 22. Tree-wide conformance gates (`dsl/conformance_test.go`)

CI enforces a set of static rules over every loaded `.memql` file.
A PR that violates any of them fails before the engine ever parses
the change. The gates, with their test names:

- **Canonical filter prefixes** (`TestFilterSyntaxCanonical`).
  Filter predicates reference payload fields as `<field>`
  and row intrinsics (`id`, `concept`, `createdAt`, `createdBy`,
  `partition`, `type`, `schema`) by their bare names. The
  `<conceptName>.<field>` alias form is rejected.
- **Mandatory trait specs** (`TestNoInlineTraitablePredicates`).
  When a trait in `dsl/common/traits.memql` covers a predicate, the
  filter must call the trait, not inline the comparison:
  `traitIsActiveRecord` (not `active == true`),
  `traitIsNotDeleted` (not `deleted != true`),
  `traitStatusIsActive` (not `status == "active"`), and so
  on for the status / identity-type / deletion-scheduled traits.
  Concept-specific predicates (`ownerUserId == args.userId`)
  stay inline.
- **No concept-name shortId prefixes** (`TestNoShortIdConceptPrefix`).
  Derived ids are the bare unique part (uuid / hash / slug) — never
  `concat("agent-", ...)` or another concept-name / sub-type prefix.
  See [#20](#20-foreign-key-id-derivation-use-canonicalid-before-hashing).
- **Typed @relationship targets** (`TestRelationshipTargetsUseImports`,
  memql#1067). `@relationship(..., target=user, ...)` names an
  imported concept; the `target="v1:..."` canonical-string form is
  rejected.
- **Per-row authz classification** (`TestPerRowAuthzClassification`).
  Every query / mutation that touches a user-scope field
  (`ownerUserId`, `userId`, `createdBy`, ...)
  must either carry a caller-scope check (`actor.userId` in the
  filter / write), an admin gate (`actor.isClusterOwner` or a
  `requiresClusterOwner` spec), or an explicit `@public` annotation
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
  `dsl/no_retired_operators_test.go`): filters use the single Go
  boolean grammar — `&&` / `||` / `!` with parens. The `;`-AND and
  `,`-OR separators, the `has` membership operator (use `in`), and
  the `?.` optional-chain prefix (use `when(args.x) { ... }`) are
  rejected.
- `TestNoInfixWordAndOr` (#973,
  `dsl/no_word_logical_operators_test.go`): the English `and` / `or`
  infix forms are rejected.
- `TestNoRetiredBindingForms` (#988, `dsl/no_named_writes_test.go`):
  named writes (`insert <concept> {` / `update <concept> {`) are
  rejected — the write target comes from the
  `mutation <Concept> <name>` signature, the block is bare
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

- **Single-row read — EXEMPT.** The filter contains a bare `id == <expr>`
  equality on the row's primary intrinsic. It reads at most one row, so
  it is not a list. A *guarded* `when(args.x) { id == ... }` does **not**
  count — the id filter is conditional, so the query can still return
  the full set when the arg is omitted.
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
// Single-row read — exempt (id == equality).
query space querySpaceMeta {
  args { spaceId string @required }
  filter  id==args.spaceId
  shape   spaceFull
}

// Bounded list — compliant (paginate window).
query space queryFirstTenSpaces {
  filter  active==true
  paginate 10
  shape   spaceFull
}

// Legitimate full-set read — compliant, marked + auditable.
@unbounded("provider catalog is a small bounded set — never more than a handful of rows")
query provider queryAllProviders {
  filter  traitIsActiveRecord
  shape   providerFull
}

// VIOLATION — list read with no bound. Pulls the whole table.
query widget queryAllWidgets {
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
and bypasses the 50-cap (still clamped to `MEMORY_ENGINE_MAX_WINDOW`).
The cap is tunable via `MEMORY_ENGINE_DEFAULT_LIST_CAP` (clamped to
`<= MEMORY_ENGINE_MAX_RESULTS`). Lives in
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
- `TestPaginationAuthoringRule` in `dsl/conformance_test.go` — the gate.
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

## Result caching: `@cache(ttl="N")` on hot reads

A query can opt into the engine's result cache with `@cache(ttl="N")` on
the query declaration. `N` is a **whole number of SECONDS, string-valued**
(`@cache(ttl="300")`, not `@cache(ttl=300)` and not `@cache(ttl="5m")` —
the bare-number and duration forms do not parse). `@cache(ttl="0")` is the
explicit "never cache" escape.

```memql
@cache(ttl="300")
query agentRole queryActiveAgentRoles {
  filter  traitIsActiveRecord
  shape   agentRoleFull
}
```

**When to reach for it.** Read-heavy queries whose underlying rows change
rarely: bounded catalogs / registries (role / skill catalogs, router
budgets) get long TTLs; hot append-only streams (the per-space utterance
list) get short ones and lean on invalidation.

**Correctness — invalidation (5.4).** A write to a cached query's read
concept evicts the dependent cached results, so a cached read never
outlives a row it depends on. You do not annotate the eviction; it is
keyed off the concept the query reads.

**Correctness — cross-node (REQUIRED).** Each node runs its OWN result
cache. The invalidation subscriber only fires on a node when the graph
write reaches it, and the default routing rules forward only a fixed set
of namespaces (`v1:cluster:*`, `v1:cognition:*`, `v1:planner:*`). For
**every** concept you `@cache`, its `graph.node.created/updated/deleted`
writes MUST be forwarded to peers by a `node.RegisterRoutingRule`
(`component/node/routing.go`) — otherwise the write evicts the cache on
the writing node but the cached read goes **stale on its siblings**, a
silent cross-node stale read. A single-node green test will not catch
this. The cached-concept forward rules are pinned by
`TestEvaluateRouting_CachedConceptForwarding` in `component/node`.

**Keyset cursors.** The cache key includes the paginated query's cursor
(`engine.go` `cacheKey`), so distinct continuation pages of a `@cache`'d
query key independently — page 2 never collides with the cached page 1.

See `docs/internal/planning/cache-audit-phase-0.md` for the cache's shape
and instrumentation.

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

Both files need no database and run as part of the standard
`go test ./...` CI job. If a change reveals a NEW silent-acceptance hole
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
