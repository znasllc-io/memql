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

// 1. The product calls this mutation.
mutation partition mutationCreatePartition {
  args {
    name      string  @required
    type      string  @default("standard")
  }
  insert {
    name: args.name
    partitionType: args.type
    status: "active"
    createdAt: now
    createdBy: actor.userId
  }
}

// 2. An automation fires on the row landing and grants the
//    creating user owner access.
@enabled
@trigger(event="node.created", concept=platform.partition, partition="_system")
@description("Grant the partition creator owner access on first landing.")
automation autoBootstrapWorkspaceOwnerAccess {
  step grant {
    logic logicGrantOwnerOnPartitionCreate { event: event }
  }
}

logic logicGrantOwnerOnPartitionCreate {
  args { event object @required }
  body {
    return mutationGrantPartitionAccess({
      userId:      args.event.payload.createdBy,
      partitionId: args.event.payload.id,
      role:        "owner",
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
(Phase 5 Step 34). The rules live in
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
and `shape()` are query-level *directives*. They wrap an entire
expression at the **outermost** layer of a query string and only work
when called by the top-level query parser. The **function-loader
validator** (which validates `.memql` function definitions at engine
init) treats every bare call name in a function body as a reference to
another registered function -- and since `sort` / `paginate` / etc.
aren't registered functions, the engine init fails with:

```
function "<name>" references unknown function "sort"
```

If you put a directive inside a function body, the entire engine
refuses to start. The primary node crashes. Cognition / agent / planner
can't attach. Whole cluster bricked.

**Wrong:**

```memql
use platform.partition

@enabled
func (Query) queryListPartitions(_ any) (any, error) {
  return sort(concept, "payload.name", "asc"), nil
}
```

**Right:**

```memql
use platform.partition

@enabled
func (Query) queryListPartitions(_ any) (any, error) {
  return concept, nil
}
```

Sort the result on the client. The CLI does this in
`ensureDefaultPartition` (`cli/app.go`), which pins `default` first
and `sort.Slice`s the rest by name.

The same constraint is called out in `queries/v1/cluster/queryClusterNodes.memql`
for `asOf(..., latest)`. Treat sort/paginate/asOf/select/withDepth/shape
the same way.

**Where directives DO work**: in raw query strings sent through
`MemqlClientMessage.Stream` (the public RPC), e.g.
`sort(concept==v1:cluster:node, "payload.name", "asc")`. That goes
through the top-level parser, which knows about directives.

---

## 2. Function-call argument keys are bare identifiers

**Rule.** Function call argument object keys are **bare identifiers**,
not quoted strings. Values follow the standard MemQL grammar (quoted
strings, numbers, booleans, null, nested objects, arrays).

**Canonical:**
```memql
createPartition({name: "test", partitionType: "standard"})
querySpaceParticipants({spaceId: "space-123", participantType: "si"})
```

Quoted string keys are also accepted so JSON-serialized tool calls
that arrive through the same parser path keep working:

```memql
createPartition({"name": "test", "partitionType": "standard"})
```

Both forms parse identically. Mixed is fine too. The public RPC
(`ExecuteQuery`), the CLI/SDK call builders, the function-definition
parser, and the automation-DSL parser all use the same rule now --
there is no strict vs. relaxed split anymore.

If you build a call string from a Go template, either form works;
the bare-identifier form is easier to read.

---

## 3. Concept scope: `@scope("global")` for system data

**Rule.** Concepts default to **partition-scoped**: rows stamp the
request envelope's `partition`, queries auto-filter on it. Tenant
data lives this way.

For infrastructure concepts that should be visible from every tenant
(cluster topology, partition registry, system bookkeeping), add
`@scope("global")`:

```memql
@description("A registered node in the memQL cluster.")
@scope("global")
@cache(ttl=0)
concept Node { ... }
```

Effect: rows live in the reserved `_system` partition regardless of
envelope. Reads always target `_system`. Events fire under
`graph.node.created._system.<concept>` (subscribers using
`graph.node.*.*.<concept>` wildcards still match).

**Globals as of writing**:
- `v1:cluster:node`, `v1:cluster:nodeType`, `v1:cluster:spawnEvent`,
  `v1:cluster:cluster`, `v1:cluster:database`,
  `v1:cluster:identityProvider`
- `v1:platform:partition` (the partition registry itself is global so
  you can list partitions from any partition)

If you add a new infrastructure concept, mark it `@scope("global")`
or it'll vanish whenever the user switches partition.

---

## 4. Mutation functions can't be wrapped with directives

**Rule.** You cannot wrap a mutation function call with `shape()`,
`paginate()`, `sort()`, `select()`, `asOf()`, or `withDepth()`. The
parser rejects it.

This is documented in `queries/arch.md:145`. Mutations return a
single inserted node, not a queryable result set.

---

## 5. `concept==X` returns the LATEST version per id

**Rule.** When you query a concept without `asOf()`, the engine
internally calls `loadLatestNodes` and returns one row per id
(the latest by `createdAt`). The time-series of historical versions
is preserved in the database but not surfaced.

**Implication.** Re-inserting the same id appends a new row; the
new version becomes the visible one, the old version is invisible
to plain queries. Use `asOf("2026-01-01T00:00:00Z")` from the
top-level parser if you need a historical snapshot.

The CLI defensively dedupes the result of `concept==X` queries
anyway (in `parseClusterNodes`, `parsePartitions`) -- the engine
might surface multiple historical rows in some shape paths.

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

The CLI enforces this at keystroke time and on save via
`cli/ui/validate.go` (`ValidateName`, `IsNameChar`,
`NormalizeName`). Server side, an `args { name string @required }`
declaration only checks type -- the validation is currently a
CLI-side contract. **Don't trust the wire.** If you write a
server-side mutation that takes a name, also validate the shape on
the server before persisting.

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

Existing concept annotations: `@description`, `@cache(ttl=N)`,
`@skipDeleted`, `@enforceRequired`, `@defaultFilter`, `@scope("...")`,
`@type`.

---

## 8. The `_system` partition is reserved

**Rule.** Partition names starting with `_` are reserved. The CLI's
name validator (`dnsLabelRE`) explicitly rejects leading underscores,
so users can't choose `_system` (or `_anything`) for their partition.

`_system` is where global-scoped concept rows live. Treat it as
internal -- never surface it in user-facing partition lists.

---

## 9. Insert id semantics: explicit vs derived

**Rule.** When you write `insert("v1:foo:bar", id=ctx.name, payload=...)`,
the engine computes the storage id as:

```
{partition}:{concept}:{id-segment}
```

Where:
- `partition` = global-scope-resolved partition (envelope or `_system`)
- `concept` = `"v1:foo:bar"`
- `id-segment` = the trimmed value of the `id=` field

If you omit `id=`, the engine derives a content hash from the payload.
Same payload twice ⇒ same id ⇒ a new time-series row under that id.
Different payload ⇒ different id ⇒ a different row.

**Common bug**: forgetting to pass `id=` means duplicate inserts
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

For global-scoped concepts, the partition segment will always be
`_system`. A wildcard subscription matches; a literal-`default`
subscription does NOT.

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

- Inside a mutation payload (`insert("v1:foo", payload={x:
  cond(...)})`): the mutation-template evaluator handles it.
- As a function-call arg in an automation step
  (`createUser({role: cond(...)})`): the function-step arg resolver
  resolves it at arg-resolution time before renderMemQLValue quotes
  the result for the outgoing query. See
  `automations/steps/function.go::resolveArgValueRef`.

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
As of Phase 1 of the language-improvements plan, the check also runs at
mutation time (`executor.executeInsert`) -- so `insert("v1:foo",
payload={partition: "..."})` now fails with the same error shape
instead of silently stripping the field.

---

## 13. Step references are validated + topologically sorted at compile time

**Rule.** A step's condition or arg referencing another step's result
(`foo.First().payload.x`, `first(foo).x`, `foo.Empty()`, etc.) is
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
checkUser := userById({ userId: event.payload.userId })

result := if cehckUser.Empty() {   # typo: cehckUser -> checkUser
  createUser({...})
}
```

The compiler emits:

```
automation "bootstrapUser": step "result" references unknown step "cehckUser" -- check for a typo, or add the step
```

Example of a cycle (would deadlock at runtime):

```memql
a := if b.Empty() { queryFoo({}) }
b := if a.Empty() { queryBar({}) }
```

The compiler emits:

```
automation "test": dependency cycle among steps [a b]
```

---

## 14. Function naming: filename NO prefix, function name WITH prefix

**Rule.** The artifact type (`query` / `mutation` / `spec`) is named by
the directory, so filenames MUST NOT carry the type prefix. Function
declarations inside those files MUST carry a matching prefix
(`queryActiveSpaces`, `mutationCreateSpace`, `specIsActiveRecord`). The
first letter of the bare name is uppercased when forming the prefixed
name.

```
queries/v1/cognition/activeSpaces.memql       # filename: no prefix
    func (Query) queryActiveSpaces(args any)   # function: prefixed

mutations/v1/cognition/createSpace.memql       # filename: no prefix
    func (Mutation) mutationCreateSpace(args)  # function: prefixed

specs/v1/common/isActiveRecord.memql           # filename: no prefix
    spec specIsActiveRecord { ... }            # function: prefixed (struct form)
```

**Why it bites you.** Callers (CoPresent frontend, automations, Go
integration code) name functions as a string. A mixed convention means
every caller has to guess whether to add a prefix. Pre-rename, the
frontend hit runtime "function not found" errors because half the
backend had prefixed names and half didn't.

Enforcement lives in two places:

- **Loader** (`component/memql/function_loader.go`,
  `expectedFunctionNameFromFile`): derives the expected function name
  from the filename + directory, rejects legacy prefixed filenames
  (`queryActiveSpaces.memql`) with a clear message telling you what to
  rename the file to, and requires the function declaration to match
  the derived prefixed name exactly.
- **Linter** (`component/language/compiler/linter.go`): emits
  `naming.query-prefix` / `naming.mutation-prefix` / `naming.spec-prefix`
  warnings when a function of the given kind is declared without the
  prefix. With `StrictWarnings: true` in the compiler config, these
  become hard errors.

Automations (`func (Automation) ...`) are event-triggered, not called
by name, so their naming convention is unchanged. The same is true for
`(Builtin)`, `(Tool)`, `(Prompt)`, `(Provider)`, and `(Shape)`; those
are out of scope for this rule and can use their own conventions.

---

## 15. Mutation payload shorthand: `ctx.ident` infers the key

**Rule.** Inside a `payload={...}` object literal (and any other
`{...}` map the mutation-template parser handles), a bare `ctx.ident`
with no `key:` prefix is shorthand for `ident: ctx.ident`. The key
is taken from the ctx-path's final segment.

`args.ident` is retired (rejected at parse time as of Phase 1
of the policies+DSL hygiene initiative). The equivalent form is
`ctx.ident`. The same shorthand applies — `{ctx.name}` expands to
`{name: ctx.name}`. Only single-segment paths are eligible;
`ctx.user.id` falls through to the verbose `key: ctx.user.id`
form.

```memql
// Verbose -- still valid, still works.
payload={
  name:        ctx.name,
  region:      ctx.region,
  environment: ctx.environment,
}

// Shorthand -- equivalent.
payload={
  ctx.name,
  ctx.region,
  ctx.environment,
}
```

Mix the two freely when it reads better:

```memql
payload={
  ctx.nodeType,
  address:  coalesce(ctx.address, ""),
  parentId: "",
  ctx.health,
  lastSeen: coalesce(ctx.lastSeen, timestamp()),
}
```

**Constraints.**

- **Simple identifier only.** The arg path must match
  `[A-Za-z_][A-Za-z0-9_]*`. Dotted paths (`args.user.id`) are NOT
  eligible; write those as `userId: args.user.id` explicitly. The
  parser rejects shorthand with dotted paths instead of inventing a
  garbage field named `user.id`.
- **Bare `arg(...)` only.** `coalesce(args.x, default)`,
  `concat(args.a, ":", args.b)`, `cond(...)`, and other wrapping
  expressions keep the explicit `key:` prefix. Only a plain
  `args.name` expression can be shorthand.
- **No effect on the `args { ... }` block.** That block is a type
  declaration, not a value map; its lines stay in the
  `<name> <type> [@required] ...` form.

**Why it bites you (if you don't know about it).** Reviewing PRs
you'll see some mutations declaring 20-field payloads and some
declaring 20-field payloads with half the repetition. Both are valid
and equivalent. Shorthand support lives in three parsers, one per
context:
`component/memql/mutation_templates.go::tryParseShorthandArg`
(mutation `insert()` payloads),
`component/memql/shape_parser.go::tryParseNodeShorthand`
(`@template({...})` blocks in Shape files), and
`component/language/parser/parser.go::parseObject` together with
`component/language/compiler/automation_generator.go::tryParseBarePathShorthand`
(automation step-args like
`mutationCreateUser({event.payload.subject, event.payload.email})`).

---

## 16. Shape template shorthand: `node("path.ident")` infers the key

**Rule.** Inside a Shape's `@template({...})` block, a bare
`node("path.ident")` or `node("ident")` with no `key:` prefix is
shorthand for `ident: node("path.ident")`. The key is taken from the
**terminal segment** of the path.

```memql
// Verbose -- still valid, still works.
func (Shape) agentFull {
  @template({
    id:          node("id"),
    name:        node("payload.name"),
    description: node("payload.description"),
    createdAt:   node("createdAt")
  })
}

// Shorthand -- equivalent.
func (Shape) agentFull {
  @template({
    node("id"),
    node("payload.name"),
    node("payload.description"),
    node("createdAt")
  })
}
```

**Constraints.**

- **`node(...)` only.** Other functions (`shape(...)`, `select(...)`,
  `concat(...)`, etc.) keep the explicit `key:` prefix.
- **Single quoted argument.** `node("payload.name")` is eligible;
  multi-arg calls are not.
- **Terminal segment must be a simple identifier.** Paths whose last
  segment is not `[A-Za-z_][A-Za-z0-9_]*` fall back to the verbose
  form. In practice this only matters for deeply nested accessors
  like `node("payload.transcription.text")` where you want the key
  to be `transcription` rather than `text`: stick with the verbose
  form there.

---

## 17. Automation step-args shorthand: bare dotted path infers the key

**Rule.** Inside an automation step's function-call args, a bare
dotted path like `event.payload.spaceId` or
`registerNode.result.node.id` with no `key:` prefix infers the
key from the path's **terminal segment**.

```memql
// Verbose -- still valid, still works.
mutationSendTextUtterance({
  spaceId:       event.payload.spaceId,
  participantId: event.payload.siParticipantId,
  text:          siResponse
})

// Shorthand -- terminal segments become the keys.
mutationSendTextUtterance({
  event.payload.spaceId,
  participantId: event.payload.siParticipantId,  // different key name
  text:          siResponse                        // wrapped value
})
```

**Constraints.**

- **At least two dotted segments required.** Single identifiers like
  `allAgents` are NOT eligible -- they'd collide with step-reference
  semantics where `allAgents` means "the `allAgents` step's result".
  Use `allAgents.Nodes()` in a `for` loop, not inside an object arg.
- **Every segment must be a simple identifier.** Method calls
  (`.Nodes()`), index access (`.Nodes()[0]`), and call arguments
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
// Correct
@template({
  id: node("id"),
  spaceId: node("payload.spaceId"),
  createdAt: node("createdAt")
})

payload={
  name: ctx.name,
  active: true,
}

// Wrong -- unnecessary quotes on simple-identifier keys
@template({
  "id": node("id"),
  "spaceId": node("payload.spaceId")
})
```

**Why it bites you.** Mixed quoting styles in the same codebase make
every review a guessing game. All .memql files before this rule had
unquoted keys except a handful in inline `shape(...)` templates that
used JSON-style quoting; the blast radius on a frontend/Go consumer
is small because the parsers accept both, but the inconsistency is
what blocked us from spotting earlier bugs (quoted keys don't
participate in the `node("X")` shorthand from rule #16 because
shorthand only triggers when the key is absent).

**Exception.** Quoted keys are accepted when the key content isn't a
valid identifier -- for example a key with a hyphen or space, or
JSON blobs embedded verbatim in a string value (those aren't
MemQL-parsed at all). Reach for quoted keys ONLY when the name cannot
be expressed as `[A-Za-z_][A-Za-z0-9_]*`; everything else is a
style violation.

Where the three parsers stand today:

- `component/memql/mutation_templates.go::parseObjectKey` -- accepts
  both; prefer unquoted.
- `component/memql/shape_parser.go::parseKey` -- accepts both;
  prefer unquoted.
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

- Don't pass `createdBy=` to `insert()` -- the engine ignores it (or
  rejects it, depending on path). Whoever fires the mutation IS the
  recorded creator.
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
# Wrong -- bare-vs-canonical input shape changes the participant id
id = concat("participant-", hash(concat(args.spaceId, ":", args.userId)))

# Right -- canonicalId() collapses both forms to the same string. The
# second argument is the imported concept short-name (resolved against
# the file-top `use ...concepts.{ space, user }` imports).
id = concat("participant-", hash(concat(
  canonicalId(args.spaceId, space), ":",
  canonicalId(args.userId,  user)
)))
```

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
`component/memql/partition_context.go`), so `payload.userId == arg(...)`
queries work with canonical-stored values. But the id derivation
runs BEFORE the payload auto-canon, so `canonicalId()` in the id
template is still required for stable deterministic ids.

Affected mutations (audit done 2026-05-06):
`joinSpaceAsHuman`, `joinSpaceAsSI`, `createGreetingUtterance`,
`createSessionForParticipant`, `sendTextUtterance`,
`sendSpeechUtterance`, `sendActionUtterance`,
`sendRealtimeTranscriptUtterance`. Plus
`automations/v1/cognition/autoJoinSI` (computes the GA agent id
from `hash(actor)`) and
`automations/v1/copresent/onUnmetCapability` (hashes spaceId +
utteranceId).

The `concat("ga-", hash(actor))` pattern in autoJoinSI is wrapped in
`canonicalId(...)` to canonicalize the AGENT id (not the actor) for
the `mutationJoinSpaceAsSI` call -- the actor itself is already
canonical post the JWT verifier fix.

---

## 21. Argument resolution: `args.X` for caller-passed, bare names for engine

**Rule.** Every DSL construct declares its inputs through one of
three canonical forms:

- **Struct query / mutation**: `args { ... }` sub-block INSIDE the
  construct body.
- **Procedural function / automation / policy**: file-top
  `args { ... }` block ABOVE the `func (...)` declaration.
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
use cognition.utterance

@description("Insert a chat utterance")
mutation mutationSendUtterance {
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

use cognition.space

@description("Active spaces visible to caller")
query queryActiveSpaces {
  args {
    ownerId  string  @required
  }
  filter  payload.ownerId == args.ownerId && specIsActiveRecord
  shape   spaceFull
}

// Spec — struct form. No args, no return.
spec specIsHumanParticipant {
  payload.participantType == "human"
}

// No-input policy — single parameter must be `_`, not `ctx`.
@tier("bff")
@frontend_visible
func (Policy) alwaysAllow(_ any) bool {
  return true
}
```

**Wrong (rejected at registration):**

```memql
// Legacy func (Spec) form — specs are struct-form now.
func (Spec) example(ctx any) bool {
  return true
}

// args.X is the only way to reach caller-passed fields.
mutation example {
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
filters: `payload.spaceId == args.spaceId` reads correctly without
needing the reader to guess which side is concept-field vs caller-arg.

**For automations:** the triggering event payload is bound as
`args`, so `args.topic`, `args.kind`, and `args.payload.<field>`
reach the event from inside the automation body.

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
