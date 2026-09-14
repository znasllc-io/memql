---
title: MemQL Function Language Specification
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Function Language Specification

> **Status:** Stable
> **Last Updated:** September 13, 2026
> **Purpose:** Specification for the function-like DSL constructs in MemQL

---

## Overview

MemQL functions provide reusable, parameterized operations. Every
construct is declared in **struct form** -- an annotated block with a
keyword header -- and lives in a per-namespace, per-construct file
(`dsl/<namespace>/<construct>s.memql`, e.g. `dsl/library/queries.memql`).

| Construct | Purpose |
|-----------|---------|
| **Query** | Read rows of a concept with a filter and a shape projection |
| **Mutation** | Write one row (`insert` or `update`) of a concept |
| **Logic** | Imperative orchestration block called from automation steps |
| **Automation** | Event- or schedule-triggered workflow |
| **Prompt** | AI prompt template with a typed input schema |
| **Provider** | AI vendor + model configuration |
| **Shape** | Reusable field-projection template |
| **Tool** | AI-callable surface over a query / mutation / builtin |
| **Builtin** | Go-backed operation behind a declarative schema |

> **Retired: receiver-function constructs.** The legacy
> `func (Query|Mutation|Spec|Tool|Prompt|Provider|Builtin|Automation|Shape|Policy) ...`
> receiver syntax is retired and **rejected at parse time** with a
> migration hint. The struct forms documented below are the only
> accepted author surface.

## Naming Convention

**Construct names carry no kind prefix** (memql#2853). Name a construct for
what it does; the keyword already states what it is.

- Query: what it returns (e.g. `activeFolders`, `userById`)
- Mutation: the verb (e.g. `createFolder`, `archiveUser`)
- Logic: the verb (e.g. `bootstrapSession`, `generateResponse`)
- Spec / trait: the predicate (e.g. `isActiveRecord`, `requiresOwner`)
- Prompt: descriptive name (e.g. `agentReply`, `consolidateMemory`)
- Provider: provider name (e.g. `chat54Mini`, `streamClaudeSonnet`)
- Shape: `<concept><Projection>` (e.g. `folderCard`, `artifactFull`)

Gated by `TestNoKindPrefixInConstructNames` in `test/dslconformance/naming_conventions_test.go`.
Full rationale and history: [naming-conventions.md](naming-conventions.md).

This page previously mandated kind prefixes and claimed the compiler emitted
naming diagnostics for mismatches. Both were false: 0 of 1091 shipped
declarations carried a prefix, and the naming lint was retired in epic #2031 --
`TestCompileSource_NoNamingWarnings` now fails the build if any `naming.*`
diagnostic is emitted at all.

(Deliberately paraphrased rather than quoted: `TestNamingDocsDoNotMandateAPrefix`
blocks the old mandate as a plain substring, so quoting it verbatim here would
trip the gate on this very page.)

## Imports and Concept Binding

Cross-file dependencies are declared with **file-top `use` imports**.
The dotted path maps to a file on disk (`library.concepts` →
`dsl/library/concepts.memql`); the brace list names the constructs
pulled into local scope:

```memql fragment
use library.concepts.{ artifact, folder }
use library.shapes.{ artifactFull }
use common.traits.{ isActiveRecord }
```

The **concept a construct binds to is named in its signature**:
`query <Concept> <name>`, `mutate <Concept> <name>`,
`shape <Concept> <name>`, `seed <Concept> <name>`. The short concept
name resolves through the file's `use ...concepts.{ ... }` import.

> **Retired: the `@use*` annotation family and `@concepts(...)`.**
> `@useConcept`, `@useShape`, `@useQuery`, `@useMutation`,
> `@useLogic`, `@useBuiltin` (and the rest of the `@use*` family),
> plus the `@concepts("v1:...")` shape binding, are retired and
> rejected at parse time. Use file-top `use` imports + signature
> binding instead.

## Builtin Functions (Registry-Driven)

Builtins are declared in the DSL like every other construct -- in
`dsl/<namespace>/builtins.memql`, struct form. The body's field list
is the input schema; the implementation is the Go integration named
by `@executor`:

```memql
@executor("integration.auth.checkPermission")
@args(profile="object")
@description("Check if the current authenticated user has a specific role. Returns boolean result.")
builtin authCheckPermission {
  role  string  @required
}
```

At runtime, parser resolution and executor dispatch are registry-driven
from these declarations -- builtins resolve through the same function
registry as user-defined functions, so they look like regular DSL
calls with named arguments (`authCheckPermission(role: "admin")`). The
object-literal call form (`authCheckPermission({ role: "admin" })`) is
refused (memql#2335).

---

## Syntax Principles

### Consistent Accessor Pattern

Inputs are declared in an `args { ... }` block and read as `args.X`.
Engine-provided values are bare top-level names, and a row is read through
the lambda parameter that names it:

```text
args.fieldName       -- Caller-passed argument
actor.userId         -- Resolved auth context (role, identityId, isClusterOwner, ...)
now                  -- RFC3339 timestamp captured at evaluation start
partition            -- Active partition for this call
config.X             -- Allow-listed config entry
row.fieldName        -- Row payload field, through the parameter (filter row => ...)
row.id, row.createdAt, ...  -- Row intrinsics, through the same parameter
```

A shape body is a path list rather than an expression, so there a bare
`fieldName` is the payload field.

> **Retired: the `ctx` envelope.** `ctx.input.X` and `ctx.X` are gone
> from the author surface; authors write `args.X`. The `node("...")`
> accessor wrapping used by legacy shape templates is also retired.

### Operators

One expression language applies in every filter, spec, condition and value:
[memql.md](memql.md#expressions) is the reference, with the full operator table,
the [precedence](memql.md#operator-precedence) and the table of
[absent values](memql.md#absent-values).

| Operator | Meaning | Example |
|----------|---------|---------|
| `==` / `!=` | Typed equality; `!=` is true on an unset field | `row.status != "archived"` |
| `>` `>=` `<` `<=` | Comparisons | `row.count >= 10` |
| `in` | Membership | `row.kind in ["a", "b"]`, `args.tag in row.tags` |
| `startsWith` | String prefix (ANY of a list); an empty list and a blank prefix match nothing ([authoring rules](authoring-rules.md)) | `row.codeReference startsWith args.prefixes` |
| `&&` / `\|\|` / `!` | Boolean connectives and negation | `row.folderId == args.folderId && isActiveRecord(row)` |
| `??` | Blank-coalescing | `args.kind ?? "walkthrough"` |
| `c ? a : b` | Conditional value | `args.flag ? "yes" : "no"` |
| `+` | Adds numbers, joins strings | `"si-" + hash(args.agentId)` |
| `( )` | Grouping | `(a \|\| b) && c` |

A condition is boolean: there is no truthiness, and `!` negates exactly. An
optional argument is a plain predicate, `args.x == nil || row.f == args.x`,
which the engine folds before the query runs. The retired spellings (`when(...)`,
`;` and `,` as connectives, `has`, `?.`, `cond(...)`, `coalesce(...)`,
`concat(...)`, ...) are refused at parse wherever a `.memql` file writes them;
the full list is [memql.md](memql.md#retired-spellings), and
`memqlmigrate --rewrite=expressions` rewrites them.

---

## Query Functions

Queries are read-only struct constructs: signature-bound concept,
optional `args`, a `filter` clause, optional `sort` / `paginate`
directives, and a `shape` projection.

### Syntax

```memql
use library.concepts.{ artifact }

@description("Get live document artifacts filed under a folder")
query artifact activeDocumentArtifacts {
  args {
    folderId  string  @required
  }
  filter  row => row.folderId == args.folderId
              && row.kind == "document"
              && statusIsActive(row)
              && isActiveRecord(row)
  shape   artifactFull
}
```

Filter rules:

- The filter is a lambda, and every field is read through its parameter:
  a payload field is `row.status`, never a bare `status`,
  `payload.status` or `<conceptName>.status`.
- A row intrinsic is read through the same parameter: `row.id`,
  `row.concept`, `row.type`, `row.createdAt`, `row.createdBy`,
  `row.provenance.<leaf>`. Intrinsics are reserved field names, so
  `row.id` (a table column) and `row.status` (a JSONB path) never collide.
- A trait or spec is applied to the row: `isActiveRecord(row)`. Where a
  trait covers the predicate it is **mandatory** -- the inline
  `row.active == true` is rejected when `isActiveRecord` exists
  (`test/dslconformance/conformance_test.go`).
- A long filter continues on lines that open with `&&`, `||` or `??`.

### Optional filters

An optional argument is a plain predicate: `args.x == nil || <predicate>`.
Nothing in it reads the row until the second half, so the engine computes
`args.x == nil` once before the query runs: the clause becomes true when the
argument is unset (left out, null or `""`) and the predicate alone when it is
set. It replaces the retired `when(args.x) { ... }` guard, and under `||` the
form is `args.x != nil && <predicate>`:

```memql
@description("Active folders, optionally narrowed to a creator")
query folder activeFolders {
  args {
    userId  string
  }
  filter  row => isActiveRecord(row)
              && statusIsActive(row)
              && (args.userId == nil || row.createdBy == args.userId)
  shape   folderFull
}
```

**Calling patterns** (the internal query form a client sends):
```text
activeFolders()                     -- No optional filter applied
activeFolders(userId: "u-1")        -- Creator filter applied
```

### Sorting and Pagination

```memql
query run latestRunForGoal {
  args {
    goalId  string  @required
  }
  filter  row => row.goalId == args.goalId
  sort    "row.createdAt", "desc"
  paginate 1
  shape   workRunFull
}
```

A sort key names either a payload property (`"version"`, bare) or a row
intrinsic (`"row.createdAt"`, namespaced). The two compile to different
`ORDER BY` expressions -- a table column vs a JSONB path -- so in an authored
`.memql` file the intrinsics take the `row.` namespace, exactly as they do in a
filter predicate. See [Reserved identifiers](reserved.md) for the accepted
leaves.

### Counting

A `count` clause makes the query return the cardinality of the
matching set as a self-describing `{count: N}` aggregate computed
server-side, instead of the rows themselves:

```memql
query user userCount {
  filter  row => isActiveRecord(row)
  count
}
```

`count` is mutually exclusive with `shape`, `sort`, and `paginate`
(a count has no projection, ordering, or window). The count reflects
the deduped, latest-version, post-filtered set -- the same row
pipeline a normal query uses -- so it is correct under the
time-series versioning model. Callers read `count` off the returned
object rather than counting a row array.

---

## Mutation Functions

Mutations write exactly one row of their signature-bound concept.

### Execution Constraints

- Exactly **one** bare `insert { ... }` OR `update { ... }` block per
  mutation body. `update` is the partial-update counterpart for
  read-merge-write flows.
- Mutation functions can only be invoked as a **top-level** call, with
  named arguments: `createFolder(folderId: "f-1", name: "Inbox")` from a
  client, `mutation createFolder(folderId: args.folderId, name: args.name)`
  from a body.
- Mutation functions cannot be wrapped with directives like `shape()`,
  `paginate()`, `sort()`, `select()`, `asOf()`, or `withDepth()`.
- Queries and specs cannot call mutations (compile-time CQS check).

### Syntax

```memql
use library.concepts.{ folder }

@description("Create a Library folder")
mutate folder createFolder {
  args {
    folderId  string  @required
    name      string  @required
  }
  insert {
    id:          args.folderId
    name:        args.name
    status:      "active"
    ownerUserId: actor.userId
    createdAt:   now
  }
}
```

The write target comes from the signature -- the body never restates
the concept id, and the named-write form (`insert <concept> { ... }`)
is rejected (`TestNoRetiredBindingForms`).

### Args Annotations and Defaults

`args { ... }` fields take `@required`, `@enum("a", "b")`,
`@description("...")`, `@maxLength(N)`, `@pattern("re")`.

> **Retired: `@default` on args fields.** It was never applied and is
> rejected at load time. Apply a default in the body with the `??`
> blank-coalescing operator (it falls through on a blank or
> whitespace-only string as well as on absent/null -- see
> [authoring-rules.md §28](authoring-rules.md)). A concept-field
> `@default` is NOT a substitute -- it is never applied on insert
> either (memql#2960):

```memql fragment
insert {
  id:      args.guideId
  kind:    args.kind ?? "walkthrough"
  version: args.version ?? 1
  active:  args.active ?? true
}
```

`a ?? b ?? c` takes the first operand that is set, and the last one
otherwise. `??` is the one spelling: the `coalesce(a, b, c)` call is retired,
and `memqlmigrate --rewrite=expressions` rewrites it.

`??` binds **tighter than comparison** and **looser than arithmetic**, so
`args.stage ?? "" == "active"` means `(args.stage ?? "") == "active"`,
and `args.n ?? 0 + 1` means `args.n ?? (0 + 1)` -- parenthesise when you
want the sum coalesced.

---

## Logic Functions

Logic blocks are the imperative tier: called from automation steps,
they declare `args { ... }` and a `body { ... }` that is a sequence of
named statements ending in `return <expr>`.

### Single-statement form (the common case)

```memql
use common.builtins.{ ensureKnowledgeBridge }

@description("On document creation, make sure its knowledge bridge exists.")
logic provisionBridgeOnDocumentCreate {
  args {
    event object @required
  }
  body {
    return ensureKnowledgeBridge(documentId: args.event.payload.id)
  }
}
```

### Multi-statement bodies

Intermediate steps are `name := <call>` assignments; steps execute in
dependency order, and the trailing `return <expr>` is the function's
return value. A step can be guarded with `if <cond> { ... }` so it
only fires when the condition holds:

```memql fragment
body {
  getUser := query userById(userId: args.event.payload.ownerUserId)
  activeAssistantId := getUser.first().payload.preferences.activeAssistantId ?? ""

  getActiveGA := if activeAssistantId != "" {
    query agentById(agentId: activeAssistantId)
  }
  getFallbackGA := if activeAssistantId == "" {
    query assistantAgentForUser(ownerUserId: args.event.payload.ownerUserId)
  }

  return getActiveGA ?? getFallbackGA
}
```

Step results are referenced by their **bare step name**; result
navigation uses the lowercase accessors `step.first()`, `step.empty()`,
`step.count()` (and `step.Ran()` for whether a guarded step executed).
A step result is also a collection you can run the collection/lambda
library over (`where` / `select` / `count` / ...) — see
[memql.md](memql.md#collection--lambda-library). The capitalized
`.First()` / `.Empty()` / `.Nodes()` / `.Len()` / `.Count()` / `.Last()`
accessors are retired.

---

## Automation Functions

Automations are event- or schedule-triggered workflows. The canonical
body is one or more `step` blocks, each invoking a logic function with
the triggering event:

### Event-Triggered

```memql
use library.logic.{ indexArtifact }

@trigger(event="node.created", concept="v1:library:file", partition="*")
@description("On file creation, index it into the Library.")
automation indexArtifact {
  step decide {
    logic indexArtifact(event: event)
  }
}
```

Inside the logic function, the triggering event is bound as `args`, so
`args.event.payload.<field>` is how the body reaches the event data. The
event is a first-class, in-scope value the engine threads into EVERY nested
step's argument resolution, and it binds identically across every invocation
surface -- a real graph event, the live `run_automation` path, and the
`run_automation` dry-run preview (memql#1727). Run a logic without an event
in scope (a misconfigured/direct call) and `event.*` references degrade to
empty rather than erroring.

### Scheduled

`@trigger(schedule="...")` takes a six-field cron expression
(sec min hour dom mon dow):

```memql
@trigger(schedule="0 */10 * * * *")
@description("Every 10 min: mark departed cluster nodes as health='stopped'.")
automation pruneStaleClusterNodes {
  step run {
    logic pruneStaleClusterNodes(event: event)
  }
}
```

### Preconditions (self-healing)

An automation may declare one or more first-class `precondition` blocks
alongside its `step` blocks. A precondition is a **deterministic boolean
check** (no LLM) evaluated at the start of the run — after the trigger
fires and the input query (if any) loads, but **before any step executes**.

```memql
automation deployStaging {
  precondition envIsStaging {
    check: config.MEMQL_ENV == "staging"
    literal: MEMQL_ENV
    description: "Only drive the staging deploy spine in staging."
  }
  precondition digestPinned {
    check: args.imageDigest != nil
    literal: imageDigest
  }
  step run {
    logic driveDeploy(event: event)
  }
}
```

The `check` expression is an automation condition, written in the same
expression language as every other: `event.payload.<field>`,
`config.<key>`, `var("NAME")`, `args.<field>` (the G5 typed contract binds
the payload to the automation's args), comparisons, `&&` / `||` / `!`. It
must be boolean. `args.X != nil` is the presence check: an absent field, a
null and an empty string are one unset value, so it is false for all three
(the retired `exists(...)` read a blank the same way).

A precondition that evaluates false is a **miss**:

1. The run aborts cleanly — **no step fires**, the execution is recorded
   as `skipped`.
2. The harness emits a structured `healing.precondition.missed` event
   (see [events](../concepts/events.md#self-healing-events)) carrying the
   automation + precondition identity, the failed `check`, the asserted
   `literal`, and the triggering event 

A miss is **both** the clean self-healing repair trigger and the
cross-machine portability mechanism: a literal asserted by a precondition
that does not hold on this machine is, by definition, a precondition that
misses here. Fields:

| Field | Required | Purpose |
|-------|----------|---------|
| `check` | yes | The deterministic boolean expression that must hold |
| `literal` | no | Names the machine-specific literal asserted (path / id / endpoint) — the portability hint the repair loop relativizes |
| `description` | no | Human-readable context surfaced in the miss signal |

Preconditions are evaluated in declaration order; the first miss wins and
aborts the run. They are deterministic by design — they guard the
authored/deterministic deploy spine but are never themselves LLM-healed.

### Attribute Reference

Every annotation an automation accepts, and how each is written, is listed
under [automation](attribute-matrix.md#automation) in the attribute matrix,
which is generated from the annotation registry. Each links to its entry,
which gives its keys (the `@trigger` keys among them) and what it does.

---

## Catalog

Every function and method an expression can call is one entry of the catalog
in `component/language/functions`, and the table below is that catalog. Operators
are not here (`p ? a : b`, `a + b`, `a ?? b`): see [memql.md](memql.md#operators).
Neither are the reserved roots, spec and trait applications, or construct calls
(`query activeUsers(...)`).

- *Pushed down* entries compile to SQL. A relationship traversal selects rows,
  so it lives in a query filter or spec body.
- *Pushed down on a row field* entries compile to SQL when their input is a
  field of the row (`row.title.includes(args.q)`, `row.tags.any(t => ...)`)
  and run in process on any other value.
- *In process* entries run in the engine. In a filter or spec body they take
  only values that do not read the row, which are computed once before the query
  ([plan constants](memql.md#plan-constants)): `row.expiresAt < addDuration(now, "P1D")`
  pushes down, `lower(row.email) == args.email` is refused.

A method is listed as `list.<name>` or `string.<name>` and called on its receiver:
`row.tags.any(t => t == "urgent")`, `args.title.count()`. An absent receiver or
argument does what each entry says.

<!-- BEGIN GENERATED: function catalog. Do not edit: go test github.com/znasllc-io/memql/component/language/functions -run Published -update-docs -->

| Signature | Runs | What it does | Replaces |
|---|---|---|---|
| `addDuration(ts datetime, dur duration) datetime` | In process | Returns ts moved by the ISO 8601 duration dur, as an RFC 3339 timestamp; a leading minus sign moves it back. An absent or unparseable argument is an error. |  |
| `aliasOf(label? string, match lambda) rows` | Pushed down | Returns the rows sharing an alias group with the rows selected by match; a leading label follows only the edges whose `as` label it names. A traversal that finds nothing returns no rows, and a pointer that is absent or blank is skipped. |  |
| `canonicalId(value string, concept string) string` | In process | Returns value as a canonical id of the named concept, whether it arrives as a bare short id or already canonical. An absent or blank value yields the empty string, and a value canonical for a different concept is an error. |  |
| `childOf(label? string, match lambda) rows` | Pushed down | Returns the children of the rows selected by match, the rows whose parent edge points at one of them; a leading label follows only the edges whose `as` label it names. A traversal that finds nothing returns no rows, and a pointer that is absent or blank is skipped. |  |
| `contains(label? string, match lambda) rows` | Pushed down | Returns the members of the collection rows selected by match, following their contains edges (the graph traversal: the substring test is string.includes); a leading label follows only the edges whose `as` label it names. A traversal that finds nothing returns no rows, and a pointer that is absent or blank is skipped. |  |
| `createdBy(label? string, match lambda) rows` | Pushed down | Returns the creators of the rows selected by match, following their createdBy edges; a leading label follows only the edges whose `as` label it names. A traversal that finds nothing returns no rows, and a pointer that is absent or blank is skipped. |  |
| `daysBetween(a datetime, b datetime) number` | In process | Returns the number of whole days from a to b, truncated toward zero and negative when b is earlier. An absent or unparseable argument is an error. |  |
| `equals(label? string, match lambda) rows` | Pushed down | Returns the rows joined to the rows selected by match by an equals edge; a leading label follows only the edges whose `as` label it names. A traversal that finds nothing returns no rows, and a pointer that is absent or blank is skipped. |  |
| `error(message string) any` | In process | Raises an error carrying message and ends the evaluation, so it never returns a value. |  |
| `hash(value string) string` | In process | Returns the SHA-256 digest of value as 64 lowercase hexadecimal characters. An absent value hashes as the empty string, so the result is always 64 characters wide. |  |
| `ids(match lambda) rows` | Pushed down | Returns the rows selected by match as id-only rows, without payload or schema. It follows no edge, so it takes no label. |  |
| `lower(value string) string` | In process | Returns value with its letters lowercased. An absent value yields the empty string. |  |
| `owns(label? string, match lambda) rows` | Pushed down | Returns the rows joined to the rows selected by match by an owns edge, in either direction; a leading label follows only the edges whose `as` label it names. A traversal that finds nothing returns no rows, and a pointer that is absent or blank is skipped. |  |
| `parentOf(label? string, match lambda) rows` | Pushed down | Returns the parents of the rows selected by match, following their parent edges; a leading label follows only the edges whose `as` label it names. A traversal that finds nothing returns no rows, and a pointer that is absent or blank is skipped. |  |
| `references(label? string, match lambda) rows` | Pushed down | Returns the rows joined to the rows selected by match by a references edge; a leading label follows only the edges whose `as` label it names. A traversal that finds nothing returns no rows, and a pointer that is absent or blank is skipped. |  |
| `secret(name string) string` | In process | Returns the decrypted value of the secret named name, looked up among the partition secrets and then the global ones. A name that matches no secret is an error, and the value must never be logged. |  |
| `shortId(value string) string` | In process | Returns the bare short id of value by stripping one canonical concept prefix; a value that is already bare comes back unchanged. An absent or blank value yields the empty string. |  |
| `systemSecret(name string) string` | In process | Returns the decrypted value of the global secret named name, with no partition lookup. A name that matches no secret is an error, and the value must never be logged. |  |
| `systemVar(name string) string` | In process | Returns the plaintext value of the global configuration variable named name, with no partition lookup. A name that matches no variable is an error. |  |
| `toString(value any) string` | In process | Returns value as text; a string comes back unchanged. An absent value yields the empty string. |  |
| `trim(value string) string` | In process | Returns value without its leading and trailing whitespace. An absent value yields the empty string. |  |
| `upper(value string) string` | In process | Returns value with its letters uppercased. An absent value yields the empty string. |  |
| `var(name string) string` | In process | Returns the plaintext value of the configuration variable named name, looked up among the partition variables and then the global ones. A name that matches no variable is an error. |  |
| `list.all(pred lambda) bool` | Pushed down on a row field, in process otherwise | Reports whether pred holds for every element of the list. An empty or absent list answers true. |  |
| `list.any(pred lambda) bool` | Pushed down on a row field, in process otherwise | Reports whether pred holds for at least one element of the list. An empty or absent list answers false. |  |
| `list.avg(fn lambda) number` | In process | Returns the mean of fn over the elements of the list. An empty or absent list yields an absent value, and fn returning a non-number is an error. |  |
| `list.count() number` | Pushed down on a row field, in process otherwise | Returns the number of elements in the list. An absent list counts as zero. Over a row field it counts what is stored: an array's elements, a string's characters, and zero for anything else. | `len(x)`, `count(x)` |
| `list.distinct(key? lambda) list` | In process | Returns the list with repeated elements dropped, keeping the first of each; with key, two elements repeat when key gives both the same value. An absent list yields an empty list. |  |
| `list.empty() bool` | In process | Reports whether the list has no elements. An absent list is empty. |  |
| `list.first() any` | In process | Returns the first element of the list. An empty or absent list yields an absent value; to take the first match, filter first: `xs.where(x => p).first()`. | `first(x)` |
| `list.groupBy(key lambda) list` | In process | Returns one group per distinct value of key, in first-seen order, each a map holding key and items. An absent list yields an empty list. |  |
| `list.last() any` | In process | Returns the last element of the list. An empty or absent list yields an absent value. | `last(x)` |
| `list.max(fn lambda) number` | In process | Returns the largest value of fn over the elements of the list. An empty or absent list yields an absent value, and fn returning a non-number is an error. |  |
| `list.min(fn lambda) number` | In process | Returns the smallest value of fn over the elements of the list. An empty or absent list yields an absent value, and fn returning a non-number is an error. |  |
| `list.nodes() list` | In process | Returns the rows of a query result as a list; a list comes back unchanged. An absent value yields an empty list. |  |
| `list.orderBy(key lambda) list` | In process | Returns the list sorted ascending by key, elements with equal keys keeping their order. An absent list yields an empty list. |  |
| `list.orderByDesc(key lambda) list` | In process | Returns the list sorted descending by key, elements with equal keys keeping their order. An absent list yields an empty list. |  |
| `list.reduce(seed any, fn lambda) any` | In process | Returns seed folded through the list, calling fn with the accumulator and each element in turn. An empty or absent list returns seed unchanged. |  |
| `list.select(fn lambda) list` | In process | Returns fn applied to each element of the list, in order. An absent list yields an empty list. |  |
| `list.single() any` | In process | Returns the one element of the list. Any other number of elements, none included, is an error; to take the one match, filter first: `xs.where(x => p).single()`. |  |
| `list.skip(n number) list` | In process | Returns the list without its first n elements; a negative n skips none. An absent list yields an empty list. |  |
| `list.sum(fn lambda) number` | In process | Returns the sum of fn over the elements of the list. An empty or absent list sums to zero, and fn returning a non-number is an error. |  |
| `list.take(n number) list` | In process | Returns the first n elements of the list, or all of them when there are fewer; a negative n takes none. An absent list yields an empty list. |  |
| `list.where(pred lambda) list` | In process | Returns the elements of the list that pred holds for, in order. An absent list yields an empty list. |  |
| `string.count() number` | In process | Returns the number of characters in the string, counting Unicode code points rather than bytes. An absent string counts as zero. |  |
| `string.includes(sub string) bool` | Pushed down on a row field, in process otherwise | Reports whether sub occurs in the string. A blank sub matches nothing, as a blank prefix does for startsWith, and an absent string or sub answers false. | `contains(s, sub)` |

<!-- END GENERATED: function catalog -->

The spellings the catalog replaces with an operator (`cond`, `concat`,
`coalesce`, `exists`, ...) are listed with every other retired form in
[memql.md](memql.md#retired-spellings).

### Roots

These are names, not calls:

| Name | Description | Example |
|------|-------------|---------|
| `args.name` | Caller-passed argument, as declared in `args { }` | `args.folderId` |
| `actor.X` | Auth context (`userId`, `role`, `identityId`, `isClusterOwner`) | `actor.userId` |
| `now` | Eval-start timestamp (bare name) | `createdAt: now` |
| `config.X` | Allow-listed config entry | `config.someKey` |
| `event` | The triggering event, in an automation | `event.payload.status` |

`var("NAME")` reads a named configuration variable (`v1:platform:variable` /
`v1:platform:partitionVariable`); it is in the catalog above.

### AI

| Function | Description | Example |
|----------|-------------|---------|
| `si(promptName, data)` | Blocking LLM call through a named prompt | `si("consolidateMemory", {episodes: cluster})` |
| `agent("name", "prompt", partitionId)` | Async agent invocation through the planner | see `dsl/agents/builtins.memql` |

---

## Prompt Functions

Prompts define AI templates with typed input schemas and a default
provider. Struct form: the body is a **bare field list** -- it IS the
input schema. Declared in `dsl/<namespace>/prompts.memql`; the
rendered template is a Go text/template file named by `@templateFile`.

### Syntax

```memql
@defaultProvider("chat54Mini")
@templateFile("prompts/consolidateMemory.tmpl")
@description("Summarize older conversation messages into a rolling summary.")
prompt consolidateMemory {
  entries          []object  @required @description("Conversation messages, oldest first.")
  previousSummary  string              @description("Prior rolling summary; empty on first compaction.")
}
```

> **Retired prompt forms** (both rejected at parse time):
> - `func (Prompt) name(args any) { ... }` -- receiver-function wrapping.
> - `@input { ... }` -- body-level wrapper around the field list. The
>   field list is the body now.

### Attributes

Every annotation a prompt accepts (`@level` is required on every prompt) and
every annotation its input fields accept are listed under
[prompt](attribute-matrix.md#prompt) and
[prompt field](attribute-matrix.md#prompt-field) in the attribute matrix, which
is generated from the annotation registry.

### Input Field Types

| Type | Description |
|------|-------------|
| `string` / `int` / `float` / `boolean` | Scalars |
| `object` | JSON object |
| `[]object` | Array of JSON objects |
| `@required` | Field modifier marking the field as required |
| `@description("...")` | Per-field documentation surfaced to the AI layer |

---

## Provider Functions

Providers define AI vendor + model configurations (OpenAI and
Anthropic are the supported vendors). Struct form, consolidated in
`dsl/providers/providers.memql`.

### Syntax

```memql
@extends("openai")
@model("gpt-5.4-mini")
@description("OpenAI GPT-5.4 Mini -- balanced cost/latency chat")
provider chat54Mini {
  params {
    contextWindow        128000
    maxCompletionTokens  16384
  }
}
```

Base providers carry vendor-level auth and type; children inherit via
`@extends`:

```memql
@base
@type("Anthropic")
provider anthropic {
  auth {
    federationRuleId   env("MEMQL_AI_ANTHROPIC_FEDERATION_RULE_ID")
    organizationId     env("MEMQL_AI_ANTHROPIC_ORGANIZATION_ID")
    serviceAccountId   env("MEMQL_AI_ANTHROPIC_SERVICE_ACCOUNT_ID")
    identityTokenFile  env("MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE")
  }
}
```

> **Retired:** `func (Provider) name { ... }` is rejected at parse
> time with a migration hint.

### Attributes

Every annotation a provider accepts, and how each is written, is listed under
[provider](attribute-matrix.md#provider) in the attribute matrix, which is
generated from the annotation registry. `@disabled` on a provider skips it at
load with no auth resolution attempted, and on a `@base` it skips every child
that `@extends` it.

### Blocks

| Block | Description |
|-------|-------------|
| `auth` | Credentials via `env()` environment-variable references. Inherited from the base when using `@extends`. |
| `params` | Provider-specific parameters (contextWindow, maxCompletionTokens, cost, etc.) |

---

## Tool Functions

Tools are the AI-callable surface over queries, mutations, and
builtins, declared in `dsl/<namespace>/tools.memql`. The body is the
tool's input schema; `@handler` binds it to the operation it runs:

```memql
@handler(type="query", query="query findEvents(title: args.title)")
@executionTime("fast")
@description("Find the caller's calendar events by exact title.")
tool calendarFind {
  title  string  @required @description("Exact event title to look up.")
}
```

Tool body fields take `@required`, `@default("...")`, `@enum`, and
`@description`. (Tool fields are the one place `@default` is valid --
it is rejected on query / mutation `args` fields.) The legacy
`func (Tool)` form is retired; the parser rejects it with a migration
hint. What a query handler's call and a webhook's url may be written
as, and the retired `$args.` placeholder, are in
[the language reference](memql.md#tools).

---

## Shape Functions

Shapes are reusable field-projection templates, declared in struct
form in `dsl/<namespace>/shapes.memql`. Each shape declares its
**kind** via `@row` (concept payload + row intrinsics) and/or
`@actor` (auth-context envelope); at least one is required. The body
is a path list -- each path becomes a template entry keyed by the
path's terminal segment.

### Row Shapes

The bound concept is named by the signature `shape <Concept> <name>`
(resolved through the file-top concept import):

```memql
use library.concepts.{ folder }

@description("Folder summary card")
@row
shape folder folderCard {
  row.id
  name
  description
  row.createdAt
}
```

### Actor Shapes

Project the engine envelope; no signature concept. Closed field set:
`actor.userId` / `actor.role` / `actor.identityId` /
`actor.isClusterOwner` / `actor.primaryEmail` / `actor.now` (the
`isOwner` spelling is a legacy alias of `isClusterOwner`;
`actor.config.<key>` is retired -- read config through the bare
reserved `config.<key>`, #2623):

```memql
@description("Actor identity envelope")
@actor
shape actorEnvelope {
  actor.userId
  actor.role
  actor.identityId
  actor.isClusterOwner
}
```

### No composition

`include` is not a shape verb. It was documented here for a long time
and never implemented (memql#3621): a shape body is a path list, so
`include folderCard` parsed as two payload properties and projected two
always-null keys. It is rejected at load now. To share a projection,
repeat the paths, or drop the body entirely and take the default
projection over the bound concept (memql#2035).

### What the loader checks

Every shape is validated against the concept it binds (memql#3621):

- a bare payload property must be a **declared field** of the bound
  concept -- `createdAt` written bare is `payload.createdAt`, which is
  almost certainly the row intrinsic `row.createdAt` misspelled;
- the bound concept must **resolve**; an ambiguous bare name (`plan`,
  `request`, `call`, `invocation` all collide across namespaces)
  disambiguates through the shape's own domain;
- two paths may not collapse onto the **same terminal key** -- every
  path is keyed by its last segment, so `row.id` + `id` used to yield
  one entry and lose the row id;
- the **declared kind** must match the body: `actor.*` needs `@actor`,
  `row.*` / bare payload needs `@row`, at least one is required, and
  `actor.*` members are the closed envelope set (#2623).

### Usage in Queries

Struct queries reference a shape by name in their `shape` clause:

```memql
query artifact folderArtifacts {
  args {
    folderId  string  @required
  }
  filter  row => row.folderId == args.folderId && isActiveRecord(row)
  shape   artifactFull
}
```

> **Retired shape forms** (rejected at parse time):
> `func (Shape) name { ... }` receiver wrapping, the
> `@concepts("v1:...")` binding annotation, the `@template({...})`
> body annotation, and `node("path")` accessors. Shapes have no
> inputs and no return; the body is a path list.
