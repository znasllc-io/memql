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
> **Last Updated:** June 11, 2026
> **Purpose:** Specification for the function-like DSL constructs in MemQL

---

## Overview

MemQL functions provide reusable, parameterized operations. Every
construct is declared in **struct form** -- an annotated block with a
keyword header -- and lives in a per-namespace, per-construct file
(`dsl/<namespace>/<construct>s.memql`, e.g. `dsl/cognition/queries.memql`).

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

- Query: what it returns (e.g. `activeSpaces`, `userById`)
- Mutation: the verb (e.g. `createSpace`, `archiveUser`)
- Logic: the verb (e.g. `bootstrapSession`, `generateResponse`)
- Spec / trait: the predicate (e.g. `isActiveRecord`, `requiresOwnerOrAdmin`)
- Prompt: descriptive name (e.g. `agentReply`, `cognitionCompaction`)
- Provider: provider name (e.g. `chat54Mini`, `streamClaudeSonnet`)
- Shape: `<concept><Projection>` (e.g. `spaceCard`, `participantFull`)

Gated by `TestNoKindPrefixInConstructNames` in `dsl/naming_conventions_test.go`.
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
The dotted path maps to a file on disk (`cognition.concepts` →
`dsl/cognition/concepts.memql`); the brace list names the constructs
pulled into local scope:

```memql
use cognition.concepts.{ participant, space }
use cognition.shapes.{ participantFull }
use common.traits.{ isActiveRecord }
```

The **concept a construct binds to is named in its signature**:
`query <Concept> <name>`, `mutation <Concept> <name>`,
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
calls (`authCheckPermission({ role: "admin" })`).

---

## Syntax Principles

### Consistent Accessor Pattern

Inputs are declared in an `args { ... }` block and read as `args.X`.
Engine-provided values are bare top-level names:

```memql
args.fieldName       -- Caller-passed argument
actor.userId         -- Resolved auth context (role, identityId, isClusterOwner, ...)
now                  -- RFC3339 timestamp captured at evaluation start
partition            -- Active partition for this call
config.X             -- Allow-listed config entry
fieldName            -- Row payload field (query filter / shape contexts)
row.id, row.createdAt, ...  -- Row intrinsics, via the `row.` namespace
```

> **Retired: the `ctx` envelope.** `ctx.input.X` and `ctx.X` are gone
> from the author surface; authors write `args.X`. The `node("...")`
> accessor wrapping used by legacy shape templates is also retired.

### Operators

One Go-style boolean grammar applies in every filter and expression
context:

| Operator | Meaning | Example |
|----------|---------|---------|
| `==` | Equal | `role=="admin"` |
| `!=` | Not equal | `status!="archived"` |
| `>` `>=` `<` `<=` | Comparisons | `count>=10` |
| `in` | Membership | `kind in ["a", "b"]`, `args.x in list` |
| `&&` | Logical AND | `spaceId==args.spaceId && isActiveRecord` |
| `\|\|` | Logical OR | `actor.role=="admin" \|\| actor.role=="owner"` |
| `!` | Logical NOT | `!hidden` |
| `( )` | Grouping (Go precedence: `!` > comparisons > `&&` > `\|\|`) | `(a \|\| b) && c` |
| `when(args.x) { ... }` | Arg-conditional predicate: when `args.x` is absent, the guarded block and its connective are dropped | `when(args.role) { role==args.role }` |

> **Retired operator forms** (all rejected at parse time and by the
> conformance suite, `TestNoRetiredOperatorForms`):
> - `;` AND separator → use `&&`
> - `,` OR separator → use `||`
> - `has` membership → use `in`
> - `?.` optional-chain prefix → use `when(args.x) { ... }`

---

## Query Functions

Queries are read-only struct constructs: signature-bound concept,
optional `args`, a `filter` clause, optional `sort` / `paginate`
directives, and a `shape` projection.

### Syntax

```memql
use cognition.concepts.{ participant }

@description("Get active human participants in a space")
query participant activeHumanParticipants {
  args {
    spaceId  string  @required
  }
  filter  spaceId==args.spaceId && participantType=="human" && statusIsActive && isActiveRecord
  shape   participantFull
}
```

Filter rules (enforced by `dsl/conformance_test.go`):

- Payload fields are `<field>` -- bare, never `payload.<field>` or
  `<conceptName>.<field>`.
- Row intrinsics go through the `row.` namespace: `row.id`,
  `row.concept`, `row.type`, `row.createdAt`, `row.createdBy`,
  `row.provenance.<leaf>`. The bare spelling is retired (memql#2779) --
  it left a row intrinsic indistinguishable from a payload property even
  though the two compile to entirely different SQL:

  ```memql
  filter  row.id == args.spaceId   // the row envelope (a table column)
  filter  status == args.status    // a payload property (a JSONB path)
  ```
- Named trait / spec predicates are called bare
  (`isActiveRecord`), and are **mandatory** where a trait covers
  the predicate -- inline `active==true` is rejected when
  `isActiveRecord` exists.

### Optional Filters with `when()`

A `when(args.x) { ... }` guard applies its predicate only when the
argument is provided:

```memql
@description("Active spaces, optionally narrowed to a creator")
query space activeSpaces {
  args {
    userId  string
  }
  filter  isActiveRecord && statusIsActive && when(args.userId) { row.createdBy==args.userId }
  shape   spaceFull
}
```

**Calling patterns:**
```memql
activeSpaces()                      -- No optional filter applied
activeSpaces({"userId": "u-1"})     -- Creator filter applied
```

### Sorting and Pagination

```memql
query context latestSpaceContextForSpace {
  args {
    spaceId  string  @required
  }
  filter  spaceId==args.spaceId
  sort    "row.createdAt", "desc"
  paginate 1
  shape   spaceContextFull
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
  filter  isActiveRecord
  count
}
```

`count` is mutually exclusive with `shape`, `sort`, and `paginate`
(a count has no projection, ordering, or window). The count reflects
the deduped, latest-version, post-filtered set -- the same row
pipeline a normal query uses -- so it is correct under the
time-series versioning model. Callers read `count` off the returned
object rather than taking `len()` on a row array.

---

## Mutation Functions

Mutations write exactly one row of their signature-bound concept.

### Execution Constraints

- Exactly **one** bare `insert { ... }` OR `update { ... }` block per
  mutation body. `update` is the partial-update counterpart for
  read-merge-write flows.
- Mutation functions can only be invoked as a **top-level** expression:
  `myMutation({ ... })`.
- Mutation functions cannot be wrapped with directives like `shape()`,
  `paginate()`, `sort()`, `select()`, `asOf()`, or `withDepth()`.
- Queries and specs cannot call mutations (compile-time CQS check).

### Syntax

```memql
use cognition.concepts.{ space }

@description("Create a cognition space")
mutate space createSpace {
  args {
    spaceId  string  @required
    name     string  @required
  }
  insert {
    id:          args.spaceId
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
> null-coalescing operator, or use a concept-field `@default`
> (a concept-field `@default` is NOT a substitute -- it is never applied on insert either, memql#2959):

```memql
insert {
  id:      args.guideId
  kind:    args.kind ?? "walkthrough"
  version: args.version ?? 1
  active:  args.active ?? true
}
```

`a ?? b ?? c` folds to exactly what `coalesce(a, b, c)` produces, so the
two spellings are interchangeable to the engine. The shorthand is the
authored form -- `dsl/no_coalesce_longhand_test.go` gates the corpus on
it, and `memqlmigrate --rewrite=null-coalesce` converts the call form.

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
use common.builtins.{ ensureDailySpaceForUser }

@description("On user creation, ensure today's daily space exists.")
logic provisionDailySpaceOnUserCreate {
  args {
    event object @required
  }
  body {
    return ensureDailySpaceForUser({ userId: args.event.payload.id })
  }
}
```

### Multi-statement bodies

Intermediate steps are `name := <call>` assignments; steps execute in
dependency order, and the trailing `return <expr>` is the function's
return value. A step can be guarded with `if <cond> { ... }` so it
only fires when the condition holds:

```memql
body {
  getUser := userById({ userId: args.event.payload.ownerUserId })
  activeAssistantId := coalesce(getUser.first().payload.preferences.activeAssistantId, "")

  getActiveGA := if activeAssistantId != "" {
    agentById({ agentId: activeAssistantId })
  }
  getFallbackGA := if activeAssistantId == "" {
    assistantAgentForUser({ ownerUserId: args.event.payload.ownerUserId })
  }

  return coalesce(getActiveGA, getFallbackGA)
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
use cognition.logic.{ bootstrapSession }

@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
@description("On participant creation, open the session that participant needs.")
automation bootstrapSession {
  step decide {
    logic bootstrapSession ( event )
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
    logic pruneStaleClusterNodes { event: event }
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
    check: $config.MEMQL_ENV == "staging"
    literal: MEMQL_ENV
    description: "Only drive the staging deploy spine in staging."
  }
  precondition digestPinned {
    check: exists(args.imageDigest)
    literal: imageDigest
  }
  step run {
    logic driveDeploy ( event )
  }
}
```

The `check` expression uses the same grammar as `Step.Condition` and
trigger `@filter` (`$event.*`, `$config.*`, `$var.*`, `exists(...)`,
comparisons, `&&` / `||` / `!`). Prefer `exists(args.X)` (the G5 typed
contract binds the payload to the automation's args) over
`X != ""` — the condition evaluator treats a present-but-empty value as
"not exists", so `exists(...)` is the reliable presence check.

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

| Attribute | Arguments | Description |
|-----------|-----------|-------------|
| `@enabled` / `@disabled` | none | Lifecycle: enabled by default, `@enabled` an accepted no-op; `@disabled` constructs stay in the tree, are not loaded |
| `@trigger` | `event="..."`, `concept="..."`, `partition="*"` | Event-based trigger; lifecycle events like `system.startup` / `system.shutdown` take `event` only |
| `@trigger` | `schedule="..."` | Six-field cron schedule |
| `@filter` | `(<predicate>)` | Event-payload predicate gating the trigger, e.g. `@filter(active==true)` |
| `@description` | `"..."` | Human-readable description |
| `precondition NAME { ... }` | `check:` (req), `literal:`, `description:` | First-class deterministic check; a miss aborts the run + emits `healing.precondition.missed` (Epic 4 self-healing) |

---

## Helper Functions Reference

Verified author-surface helpers (see `component/language/parser`):

### Data Access

| Function | Description | Example |
|----------|-------------|---------|
| `args.name` | Caller-passed argument | `args.spaceId` |
| `actor.X` | Auth context (`userId`, `role`, `identityId`, `isClusterOwner`) | `actor.userId` |
| `now` | Eval-start timestamp (bare name) | `createdAt: now` |
| `config.X` | Allow-listed config entry | `config.someKey` |
| `var("NAME")` | Named configuration variable (`v1:platform:variable` / `v1:platform:partitionVariable`) | `var("LOG_LEVEL")` |

### Logic

| Function | Description | Example |
|----------|-------------|---------|
| `coalesce(a, b, ...)` | First non-null | `coalesce(args.name, "default")` |
| `cond(pred, then, else)` | Conditional value | `cond(args.flag, "yes", "no")` |

### Strings and Ids

| Function | Description | Example |
|----------|-------------|---------|
| `concat(a, b, ...)` | Concatenate | `concat("si-", hash(args.agentId))` |
| `lower(str)` / `upper(str)` | Case conversion | `lower(args.email)` |
| `trim(str)` | Remove whitespace | `trim(args.input)` |
| `contains(str, sub)` | Substring check | `contains(args.email, "@company.com")` |
| `hash(str)` | SHA256 hash | `hash(args.email)` |
| `canonicalId(shortId, concept)` | Expand a short id to the canonical row id of an imported concept | `canonicalId(args.spaceId, space)` |
| `toString(x)` | Stringify | `toString(args.count)` |

### Time

| Function | Description |
|----------|-------------|
| `now` | Current ISO timestamp — the bare reserved primitive (no call parens; `now()` / `timestamp()` are retired). See Data Access above. |
| `addDuration(ts, dur)` | Timestamp arithmetic (a leading-sign negative duration subtracts, e.g. `addDuration(ts, "-PT2H")`) |
| `daysBetween(a, b)` | Whole-day difference |

The calendar extractors and predicates that once sat alongside these
(`year` / `quarter` / `month` / `dayOfMonth` / `isAnniversary` /
`isFirstDayOfQuarter` / `subtractTimestamps` / `memqlVersion`) were
hard-retired with zero corpus uses under the 2026.08 grammar epoch
(#2620 ruling / #2707); the parser rejects them with a migration hint.
`subtractTimestamps`'s replacement is `addDuration` with a negative
duration (leading sign: `"-P1D"`, `"-PT2H"`). `memqlVersion()` survives as a client meta-command (the
registry builtin `serviceVersion`, alias `memqlVersion`), not as an
expression builtin.

### AI

| Function | Description | Example |
|----------|-------------|---------|
| `si(promptName, data)` | Blocking LLM call through a named prompt | `si("consolidateMemory", {episodes: cluster})` |
| `agent("name", "prompt", spaceId)` | Async agent invocation through the planner | see `dsl/agents/builtins.memql` |

---

## Prompt Functions

Prompts define AI templates with typed input schemas and a default
provider. Struct form: the body is a **bare field list** -- it IS the
input schema. Declared in `dsl/<namespace>/prompts.memql`; the
rendered template is a Go text/template file named by `@templateFile`.

### Syntax

```memql
@defaultProvider("chat54Mini")
@templateFile("prompts/cognitionCompaction.tmpl")
@description("Summarize older conversation messages into a rolling summary.")
prompt cognitionCompaction {
  entries          []object  @required @description("Conversation messages, oldest first.")
  previousSummary  string              @description("Prior rolling summary; empty on first compaction.")
}
```

> **Retired prompt forms** (both rejected at parse time):
> - `func (Prompt) name(args any) { ... }` -- receiver-function wrapping.
> - `@input { ... }` -- body-level wrapper around the field list. The
>   field list is the body now.

### Attributes

| Attribute | Description |
|-----------|-------------|
| `@description` | Human-readable description of the prompt |
| `@defaultProvider` | Default AI provider name to use |
| `@templateFile` | Go text/template file for the prompt |

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
    apiKey  env("MEMQL_AI_ANTHROPIC_API_KEY")
  }
}
```

> **Retired:** `func (Provider) name { ... }` is rejected at parse
> time with a migration hint.

### Attributes

| Attribute | Description |
|-----------|-------------|
| `@type` | Provider type (e.g. `OpenAI`, `OpenAIStream`, `Anthropic`, `AnthropicStream`). Optional when `@extends` is used. |
| `@model` | Model identifier |
| `@extends` | Inherits `auth` and `@type` from a named base provider |
| `@base` | Marks a vendor-level base provider definition |
| `@default` | Marks this provider as the fallback for callers that do not pick one explicitly |
| `@enabled` / `@disabled` | Lifecycle: enabled by default, `@enabled` an accepted no-op. `@disabled` skips registration entirely (no auth resolution attempted); `@disabled` on a `@base` propagates to every child that `@extends` it. |

### Blocks

| Block | Description |
|-------|-------------|
| `auth` | Credentials via `env()` environment-variable references. Inherited from the base when using `@extends`. |
| `params` | Provider-specific parameters (contextWindow, maxCompletionTokens, voice, etc.) |

---

## Tool Functions

Tools are the AI-callable surface over queries, mutations, and
builtins, declared in `dsl/<namespace>/tools.memql`. The body is the
tool's input schema; `@handler` binds it to the operation it runs:

```memql
@handler(type="query", query="findEvents({\"title\": \"$args.title\"})")
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
hint.

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
use cognition.concepts.{ space }

@description("Space summary card")
@row
shape space spaceCard {
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

### Composition

A shape can `include` another shape (transitive; cycles and field
collisions are errors):

```memql
@row
shape space spaceCardAlias {
  include spaceCard
}
```

### Usage in Queries

Struct queries reference a shape by name in their `shape` clause:

```memql
query participant spaceParticipants {
  args {
    spaceId  string  @required
  }
  filter  spaceId==args.spaceId && isActiveRecord
  shape   participantFull
}
```

> **Retired shape forms** (rejected at parse time):
> `func (Shape) name { ... }` receiver wrapping, the
> `@concepts("v1:...")` binding annotation, the `@template({...})`
> body annotation, and `node("path")` accessors. Shapes have no
> inputs and no return; the body is a path list plus optional
> `include` statements.
