---
title: MemQL Attribute Matrix
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Attribute Matrix

> **Last Updated:** 2026-06-11

This document defines the `@attribute` decorators available on the
function-style constructs (queries, mutations, automations) and points
at the annotation surfaces of the other construct kinds. The Yes/No
columns reflect the **load-time allow-list** — `annotations.ByReceiver`
in `component/language/annotations/registry.go`, the authoritative gate
`ValidateConstructAnnotations` enforces. A `Yes` means the construct
actually accepts the annotation at load; a `No` means it is rejected
(even where the parser still folds it into an AST field — several
annotations were removed from the allow-lists in #989 and are load-
rejected, shown as `No` here). `TestAttributeMatrixMatchesAllowLists`
pins this table to the allow-lists so the two cannot drift.

---

## Attribute Applicability Matrix

| Attribute | Query | Mutation | Automation | Description |
|-----------|:-----:|:--------:|:----------:|-------------|
| **Lifecycle** |
| `@enabled` | Yes | Yes | Yes | Accepted no-op; definitions are enabled by default (#2609) |
| `@disabled` | Yes | Yes | Yes | Explicitly disables the definition |
| `@deprecated` | No | No | No | Not accepted on any function-style construct -- removed from the allow-lists (#989); the parser still folds it but the load gate rejects it. Use `@disabled` to deactivate |
| `@version("v1")` | No | No | No | Version metadata -- valid on **seeds and concept definitions only**, rejected at load on query/mutation/automation |
| **Documentation** |
| `@description("...")` | Yes | Yes | Yes | Human-readable description (fallback; prefer `///` doc comments, #2601) |
| **Access Control** |
| `@public` | Yes | Yes | No | Authz-classification opt-out: declares that no caller-scope check applies (see below) |
| `@actor` | Yes | Yes | Yes | Declares the body reads the auth envelope (`actor.*`); used-but-undeclared fails load (#2621) |
| `@permission("...")` | -- | -- | -- | BURIED (#2713); never enforced. See below |
| **Performance** |
| `@timeout("30s")` | No | No | No | Not accepted -- removed from the allow-lists (#989); rejected at load |
| `@cache(300)` | Yes | No | No | Result-cache TTL in whole seconds; positional preferred (#2618), `ttl="300"` keeps parsing |
| **Reliability** |
| `@retry(count=3)` | No | No | No | Not accepted -- removed from the allow-lists (#989); rejected at load |
| `@idempotent` | No | No | No | Not accepted -- removed from the mutation allow-list (#989); rejected at load |
| `@mergeFields("a", "b")` | No | Yes | No | Deep-merge the named object payload fields on update instead of replacing them |
| `@appendFields("a", "b")` | No | Yes | No | Append the named array payload fields' elements to the stored array on update instead of replacing them |
| `@createOnly("a", "b")` | No | Yes | No | Write the named payload fields only on create; preserve the stored value on an insert (upsert) onto an existing id |
| **Auditing** |
| `@audit` | No | No | No | Not accepted -- removed from the allow-lists (#989); rejected at load |
| **Triggers (Automation Only)** |
| `@trigger(event="...")` | No | No | Yes | Event-based trigger |
| `@trigger(schedule="...")` | No | No | Yes | Cron-based schedule (6-field, with seconds) |
| `@filter(...)` | No | No | Yes | Predicate over the triggering event's payload |
| `@schedule(cron="...")` | No | No | Yes | Accepted synonym for `@trigger(schedule="...")` |
| `@async` | No | No | No | Not accepted -- dead vocabulary; rejected at load. Automations run async by their event/schedule trigger |

---

## Attribute Definitions

### Lifecycle Attributes

#### `@enabled`
Accepted explicit no-op: definitions are enabled by default (functions since
#360, automations since #2604). Use `@disabled` to deactivate.

```memql
@enabled
query user activeUsers { ... }
```

#### `@disabled`
Explicitly disables the definition. The construct is parsed but not
loaded at runtime; it stays in the tree, is still maintained, and can
be re-enabled at any time. ("Deprecated / abandoned" as a distinct
lifecycle axis has no working annotation today -- `@deprecated` was
removed in #989.) For functions the gate is on execution as well as
discovery: a `@disabled` query, mutation, logic, or builtin (#2608) is
hidden from `functions()` and the MCP tool listing -- including the
`@mcp`-promoted first-class tool surface (#2647) -- (`help()` still
describes it, reporting `"enabled": false`) and rejected with
`function "name" is disabled` if called directly (#2605). Automations
carry the same pair (#2681): a `@disabled` `@mcp` automation is dropped
from the advertised MCP tool surface AND refused by the manual run path
(dry-run and live alike) rather than merely hidden. Tools,
seeds, prompts (#2606), providers, specs, and traits (#2607) are
skipped at load entirely: a `@disabled` tool never reaches the MCP
surface, a `@disabled` seed is never materialised, a `@disabled`
prompt never registers, and a `@disabled` spec or trait is validated
but never binds (its name stays reserved against authored promotion).
A `@disabled` capability (#2607) drops out of both the authored-import
crossref and the actions capability catalog: `use capabilities....`
imports stop resolving it and an action referencing it is rejected at
load with `capability "name" is @disabled`. A disabled capability must
still reconcile against the Go vocabulary -- `@disabled` is not a
validation bypass, so a decl parked ahead of its unshipped Go verb is
a hard load error; the `_disabled/` directory convention is the tool
for parking DSL ahead of Go.

AUTHORED constructs carry the same intentional-state semantics through
the authoring lifecycle (#2643). A session-authored `@disabled` spec or
trait passes Gate-1 (it must stay semantically valid) but cannot be
promoted: the promote is refused with `construct is @disabled; enable
it (remove @disabled from the source) before promoting` -- an authored
construct cannot be disabled in place as a soft-retire; the retire path
for a promoted construct is the demote. A STORED `@disabled` authored
row (persisted by an older engine or an activated bundle) re-hydrates
as an intentional skip on both the boot walk and the live propagation
walk: nothing registers and the row is reported as `skippedDisabled`
-- never through the quarantine channel (no ERROR log, no load-report
entry), which is reserved for rows that genuinely fail to recompile.
The skip also reserves the name, but only when the name is UNOWNED: a
stale `@disabled` row never disables a name that is live in the
registry (a core spec, or the author's own corrected re-promote on a
later walk tick) and never overwrites a core `@disabled` reservation,
which is permanent by design. The authored reservation, by contrast,
is releasable by its own lifecycle: promoting the corrected (enabled)
source re-enables the name, and the demote retires it. A bundle authoring a `@disabled` capability fails
Gate-1 with `capability "name" is @disabled` (it would compile to
nothing), not the misleading "no capability declaration found".

```memql
@disabled
query user legacyUsers { ... }
```

#### `@deprecated` (removed)
Removed from the allow-lists in #989 and **rejected at load** on every
construct -- it is in no receiver's allow-list. Use `@disabled` to take
a construct out of service.

#### `@version("...")`
Version metadata tag. Accepted on **seeds and concept definitions only**;
rejected at load on query / mutation / automation.

```memql
@version("v2")
seed defaultSettings { ... }
```

---

### Documentation Attributes

#### `@description("...")`
Human-readable description of the definition -- the **compatibility
fallback**. The preferred spelling since #2601 is a `///` doc-comment
block immediately above the declaration: it IS the description, needs no
quote-escaping, and **wins** over `@description` when both are present
(never concatenated). Attachment: a blank line breaks it, annotations
between the block and the declaration are transparent, consecutive `///`
lines join with single spaces, and a bare `///` line is a paragraph
break. Aim for ~500 characters (the editorial length target; sense emits a
hint-severity `description-length` diagnostic over the target -- never
a hard gate, #2703). The engine tree's conformance gate rejects the
redundant long form where `///` suffices; downstream trees convert with
`memqlmigrate --rewrite=doc-comment-descriptions` at their repin.

```memql
/// Returns all active user profiles with optional filters.
query user activeUsers { ... }
```

The fallback form stays valid wherever it parses today:

```memql
@description("Returns all active user profiles with optional filters")
query user activeUsers { ... }
```

---

### Access Control Attributes

#### `@internal` (retired)
Construct-level `@internal` was hard-retired under the 2026.08 epoch
(#2620 ruling / #2708). It only hid the construct from discovery
surfaces (tool listing, `@mcp` promotion) while leaving it callable --
one corpus use in its lifetime. The load gate now rejects it with a
migration hint; delete the annotation. Field-level `@internal` on
concept PROPERTIES is a different surface and remains fully supported --
and genuinely enforced: the field is rejected from caller args *and*
excluded from a shape's default projection.

Do not read that as a statement about the whole sensitivity family,
because it is not one (memql#2960). `@pii` is enforced in two places
(the `@scrubPii` update path, and the projection-authorization gate of
memql#2883).

`@secret` is **enforced on every validation surface that quotes a
rejected value**, and the boundary is still the part that matters. It
emits `x-secret`, which the engine reads to replace a rejected value
with `<redacted>` while the argument name and the declared constraint
(enum members, bounds, pattern) survive, so the diagnostic stays usable.

The list below comes from an **exhaustive enumeration** of validators —
every jsonschema `.Validate(` call site and every value-quoting
rejection message in the engine — not from adding one entry at a time.
That method is the point: three successive incremental passes each
shipped a scope paragraph that walked past a surface the next pass
found, and the exhaustive sweep found two more the epic's own
four-surface model did not contain. Extend this list by re-running the
enumeration, never by appending to it.

**Covered:**

- the **function-args validator** (`component/memql/function_validator.go`)
  — enum / minimum / maximum / pattern / date-time (memql#3036).
- the **tool-args validator** (`MemQLEngine.validateToolArgs`,
  `component/memql/tool_execution.go`), compiled from the *same* args
  schema that carries the secret flag and running **before** the
  function-args validator on the agent path, so it is the surface a
  rejected secret reaches first. Both exits redact (memql#3182): the
  message returned to the **model**, and the WARN, which now redacts
  per key from the args schema instead of serializing the **entire**
  args map — degrading to a values-free `argKeys` list when the tool's
  arguments cannot be classified.
- the **automation args binder**
  (`component/automations/args_binding.go`), the second validator
  mirroring this rule set over **event payloads** — a
  `graph.node.created` event carries the concept row's fields flattened
  into its payload. The loader stamps the secret flag from the trigger
  topic's concept, so the enum and pattern refusals **and the WARN log
  they are written to** all print `<redacted>`, closing the one path by
  which a row value could reach a **structured log** (memql#3183).
- **concept payload validation** (`Concept.validate`, so `Create` and
  `Delete`). This is where `@minimum` / `@maximum` / `@format` declared
  on the *concept* are actually enforced — they are checked nowhere
  else, so unlike the function-args validator this needs no automation
  and no matching argument name. Six jsonschema keywords interpolate
  the instance value (`minimum`, `maximum`, `exclusiveMinimum`,
  `exclusiveMaximum`, `multipleOf`, `format` — derived by reading the
  library, not guessed); all six redact, and every other message stays
  byte-identical to an unannotated field's (memql#3184).
- the DSL-callable **`validate` and `preflight` builtins**
  (`component/memql/executor_builtin.go`), which compile the same
  concept schema themselves and surface every leaf message in a
  **result payload returned to the caller** — worse than an error
  string, since no log escape is needed for the disclosure.

Redaction on the last two resolves secrecy through the **schema** at
the failing instance location, so it covers `@secret` at any nesting
depth and inherits it onto array elements — deliberately unlike
`Concept.SecretFields()`, which is **top-level only** and is not the
accessor those paths use.

**Matching is by argument NAME, not by write target** on the two
args-validator surfaces. An args field is redacted when its name
appears in the bound concept's `@secret` fields. A mutation writing
`apiKey: args.credential` into a `@secret` `apiKey` leaves `credential`
**unredacted** — and renaming between argument and field is the common
style in this corpus, so do not rely on the write target. (The
concept-payload and builtin surfaces are exempt: they resolve through
the schema, not through an argument name.)

It is **not** redacted from **query results** — a `@secret` value is
returned in full by any query that projects it. That is an
authorization decision: it needs a definition of "elevated" and
interacts with the per-row authz model deferred under memql#2803.

**Length is never redacted anywhere**, deliberately and uniformly:
`value too long (N runes, max M)` and the jsonschema `minLength` /
`maxLength` messages report a rune count for a secret field too. They
quote no value, but a length is a disclosure. Redacting it on one
surface would make that surface the sole diverging one while
withholding nothing the others already print.

**Unclassified — prompt input-schema validation**
(`PromptTemplate.ValidateData`, `component/memql/ai_prompts.go`). It
runs a jsonschema over caller data and interpolates the instance the
same way, but its schema is built from the prompt's own field list and
is not concept-derived, so there is no `x-secret` in it to read.
Nothing is redacted there — treat it as uncovered, not as safe.

So `@secret` stops a credential leaking through a validation
diagnostic. It is still **not** a general secrecy guarantee, and it is
not a reason to treat a credential in the graph as protected.

`@unique`, `@immutable` and `@default` remain **declared metadata**:
emitted, and read by nothing. Section 8 of
`dsl/_reference/_concept.memql` carries the per-annotation split.

`@default` is declared metadata **by decision, and it is now gated**
(memql#3038). It stays documentation rather than being applied on insert
(which would add an update-path question) or retired (the emitted
`default` keyword is read by the SDK, sense hover and form generators).
memql#3248 re-affirmed that decision and closed the one correctness hole
it named — see **Type-directed lowering** below. What changed with #3038
is that writing one and getting nothing is caught at authoring time:
`TestDefaultIsCoalescedOrStamped` (`dsl/default_stamped_test.go`) fails
when an **optional, top-level** concept field carries `@default` and no
mutation bound to that concept **stamps** it. Only a stamped value counts
— `f: args.f ?? "v"`, a literal, or a computed expression; `accept { f }`,
a bare `args.f` shorthand and a plain `f: args.f` all bind the field to a
caller argument, so omitting that argument still writes nothing.

Two things the gate does **not** cover, neither of them an oversight:

- **Runtime-mounted domains.** It scans the in-repo tree only, so a
  product bundle mounted through `MEMQL_DSL_PATH` is never scanned and its
  own `@default` mistakes go uncaught. That is the price of it being a
  conformance test instead of a load-time gate — a boot gate could refuse
  boot on a legitimate bundle topology, which is worse than the silent
  no-op it would replace.
- **Nested object leaves.** A `@default` inside an object block is out of
  scope, because no write form can stamp a single leaf: a mutation writes
  the parent object wholesale. Nothing is applied at **any** depth;
  stamping is the only mechanism that fills a value, and it can only be
  spelled for a top-level field.

memql#3248 asked whether the nested-leaf carve-out should be closed by
giving the grammar a leaf-stamping write form, or by applying nested
defaults on insert. **Decision: neither — the carve-out stands.** The
nested annotations are honest documentation, documented as such in three
places, and both alternatives cost a grammar or write-path change to
close a gap that breaks nothing today. What #3248 *did* close is the one
concrete defect it found while asking.

##### Type-directed lowering (memql#3248)

A `@default` literal is lowered against the field's **declared type**, and
one that could never be a value of that type is **refused at load**:

| declared type | accepted `@default` |
|---|---|
| `bool` | exactly `true` or `false` |
| `int` | a base-10 integer |
| `float` | a number (an integer literal is a valid float) |
| `datetime` | an RFC3339 timestamp, or `""` for unset |
| `string`, `enum` | the literal verbatim — **never coerced** |
| `object`, `array`, `map`, `any` | untyped lowering; the declaration does not narrow the literal to one reading |

Before this, the lowering guessed from the literal's *shape* and had no
datetime branch at all, so

```memql
whenField datetime @default("true")
```

emitted the **bool** `true` as the default of a field declared
`format: date-time`, in silence. The same guessing turned `@default("0")`
on a `string` field into the **number** `0`.

This matters even though nothing is applied on insert: the emitted
`default` keyword is consumed by the SDK, sense hover and the preferences
form generators, so a wrong one is wrong documentation shipped to three
consumers.

**Bare and quoted spellings are equivalent.** `@default(false)` and
`@default("false")` both mean the bool `false`. They are stored
differently by the parser — an unquoted token has no value to bind to, so
it is recorded as an argument *key* rather than as the attribute's value:

```
@default(false)     Value=<nil>    Args=map["false":true]
@default("false")   Value="false"  Args=map[]
```

The reader only understood the second shape, so the bare spelling was read
as the empty string. `isGroupGA bool @default(false)`
(`dsl/cognition/concepts.memql`) had therefore been emitting
`"default": ""` on a field declared `"type": "boolean"` since the line was
written. That is the same defect one layer earlier — the value read
without regard for how it was written, then lowered without regard for the
type it belongs to — and it was found because the type-directed lowering
turned a silent wrong answer into a load failure.

Guarded by `TestDefaultIsLoweredByItsDeclaredType`,
`TestBadDefaultRefusesToLoad` and `TestBareDefaultLiteralIsRead`
(`component/database/memory-nodes/declared_metadata_annotations_test.go`).
The second drives the real `BuildConceptFromDecl` path, so the check
cannot pass while unwired.

#### `@public`
Declares that the construct intentionally carries **no caller-scope
check**. The per-row authorization gate
(`TestPerRowAuthzClassification` in `dsl/conformance_test.go`)
classifies every query / mutation as owned (`actor.userId` reference),
admin (`actor.isClusterOwner == true`, or an admin context-spec -- the
recogniser also lists `requiresClusterOwner`, a #54 placeholder that is
not declared anywhere in `dsl/`), or public --
a construct that references user-scope fields without one of those
fails CI unless it carries `@public` with a comment explaining why.

```memql
// Concept catalog -- no per-user rows, safe to expose unscoped.
@public
query nodeType nodeTypes { ... }
```

#### `@role` (buried)
`@role("...")` was documented here as declarative access control, but
nothing ever enforced the value -- and since #989 the load gate has
rejected it outright (before then it parsed into a `Role` field that
only a dead help-payload branch ever read). Dead vocabulary wearing a
security costume. It was BURIED under the #2631 ruling (#2709): the
AST/parser/registry plumbing is deleted and the load gate emits a
pointed message. Access control lives at the actor layer (RBAC) plus
the `@public` per-row-authz classification.

NOTE: `@permission` (below) was the same documented-only class and has
been buried alongside it (#2713).

#### `@permission("...")` (buried)
`@permission("...")` was documented as requiring a caller permission,
but nothing ever enforced it: the load gate rejected it (absent from
the annotation registry) and its one reader -- a `help()`/`listFunctions`
payload branch -- was dead (the field it read was provably always
empty). The @role twin. It was BURIED under the #2631 ruling's
audited close-out (#2713): the AST/parser/registry plumbing is deleted
and the load gate emits a pointed message. Access control lives at the
actor layer (RBAC) plus the `@public` per-row-authz classification.

---

### Performance Attributes

#### `@timeout("...")` (removed)
Removed from the allow-lists in #989 and **rejected at load** on every
construct.

#### `@cache(N)`
Cache query results for N whole seconds. **Query only** - mutations
and automations should not be cached. Positional is preferred (#2618:
the single ttl arg makes position unambiguous); the keyword form
`@cache(ttl="300")` keeps parsing. Duration strings like "5m" were
doc drift -- the engine reads whole seconds.

```memql
@cache(300)
query nodeType nodeTypeCatalog { ... }
```

---

### Reliability Attributes

#### `@retry(count=N)` (removed)
Removed from the allow-lists in #989 and **rejected at load** on every
construct. The parser still folds it into an AST field, but no loader
reads it -- authoring it hard-drops the construct at load.

#### `@idempotent` (removed)
Removed from the mutation allow-list in #989 and **rejected at load**.

#### `@mergeFields("...")`
Opts an update-kind mutation into engine-side deep-merge for the named
object-typed payload fields: the partial object's keys merge into the
stored object instead of replacing it wholesale (the default contract
is top-level replace). Added for single-key preference writes that
would otherwise wipe sibling keys (memql#1339).

```memql
@mergeFields("preferences")
mutate user toggleComputerUseEnabled { ... }
```

#### `@appendFields("...")`
Opts an update-kind mutation into engine-side array APPEND for the named
array-typed payload fields: the partial array's elements are appended to
the stored array instead of replacing it wholesale (the default contract
is top-level replace). Lets a pure single-writer mutation accumulate list
elements -- e.g. attach one id to a request's attachmentIds -- without a
read-merge-append logic wrapper (memql#2240). Like `@mergeFields`, only
valid on update-kind mutations; not deduped (re-appending an existing
element yields a duplicate).

```memql
@appendFields("attachmentIds")
mutate request attachToRequest { ... }
```

#### `@createOnly("...")`
Opts an insert-kind (create-or-upsert) mutation into per-field create-only
semantics: the named payload fields are written ONLY when the mutation
creates the row. When the target id already exists, they are dropped from
the delta before the engine read-merge (memql#1709), so the stored value is
preserved instead of clobbered. This makes a deterministic-id re-stage
genuinely idempotent for lifecycle fields another writer owns after
creation -- e.g. `stageOutboundRequest` seeds `status`/`attempts` at birth
but must not reset a row the outbound worker has since moved to
sending/sent/failed (fylo#63). The inverse of `@mergeFields`/`@appendFields`:
only valid on insert-kind mutations (an update always targets an existing
row, so a create-only field could never be written).

```memql
@createOnly("status", "attempts")
mutate outboundRequest stageOutboundRequest { ... }
```

---

### Auditing Attributes

#### `@audit` (removed)
Removed from the allow-lists in #989 and **rejected at load** on every
construct.

---

### Trigger Attributes (Automation Only)

#### `@trigger(event="...", concept="...", partition="...")`
Event-based trigger. Graph events name the lifecycle
(`node.created` / `node.updated` / `node.deleted`) plus the concept;
the partition segment is a #56 phase-8 vestige -- always pass
`partition="*"`. Domain events (`cognition.response.requested`,
`system.startup`) use the bare `event=` form.

```memql
@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
automation bootstrapSession { ... }

@trigger(event="cognition.response.requested")
automation generateResponse { ... }
```

#### `@trigger(schedule="...")`
Cron-based schedule. The cron expression is **6-field** (seconds
first). `@schedule(cron="...")` is accepted as a synonym, but the
live tree uses the `@trigger(schedule=...)` form exclusively.

```memql
@trigger(schedule="0 0 2 * * *")
automation purgeExpiredArchivedSpaces { ... }

@trigger(schedule="0 */10 * * * *")
automation pruneStaleClusterNodes { ... }
```

#### `@filter(...)`
Predicate over the triggering event's  The automation only
fires when the predicate holds.

```memql
@trigger(event="node.created", concept="v1:cognition:space", partition="*")
@filter(active==true)
automation autoJoinSI { ... }
```

#### `@async` (removed)
Not accepted -- dead vocabulary, rejected at load (#2712), and its
`AutomationDef.Async` field was deleted (#2724). Automations already run
asynchronously off their event/schedule trigger.

---

## Examples by Type

### Query

```memql
use cognition.concepts.{ participant }
use common.traits.{ isActiveRecord }

@description("Get active human participants in a space")
query participant activeHumanParticipants {
  args {
    spaceId  string  @required
  }
  filter  spaceId==args.spaceId && participantType=="human" && isActiveRecord
  shape   participantFull
}
```

### Mutation

```memql
use cognition.concepts.{ space }

@description("Create a cognition space")
mutate space createSpace {
  args {
    spaceId  string  @required
    name     string  @required
  }
  insert {
    id:        args.spaceId
    name:      args.name
    status:    "active"
    createdAt: now
    createdBy: actor.userId
  }
}
```

### Automation

```memql
use cognition.logic.{ bootstrapSession }

@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
@description("Auto-creates a session when a participant joins a space")
automation bootstrapSession {
  step run {
    logic bootstrapSession { event: event }
  }
}
```

---

## Annotation Surfaces of the Other Constructs

The constructs outside this matrix carry their own closed annotation
sets; unknown annotations are rejected at load time:

- **Concepts**: `@description`, `@version`, `@namespace`, `@type`,
  `@displayCard`, `@rowAuthz`. (`@scope` is retired, #56.)
  `@rowAuthz(public|clusterOwner|owner="<field>"|via="<spec>")` declares
  WHO MAY SEE the concept's rows, once on the concept, instead of as an
  `actor.*` term every filter over it must remember to carry. Exactly
  one tier; `owner=` must name a field the concept declares, or the
  literal `id` for a self-owned concept whose owner is the row itself
  (memql#3029). PHASE 1 IS
  INERT (memql#2920) -- the tier is parsed, validated and carried, and
  nothing reads it at query time, so no result set changes. Undeclared
  is a boot warning, not yet an error. See
  [per-row-authz-audit.md](../operate/auth/per-row-authz-audit.md).
  `@version` is strict
  semver and absent means 1.0.0 (#2613) -- annotate only genuine
  non-defaults; the same default applies to seeds. `@namespace` absent
  DEFAULTS to the containing `dsl/<domain>/` directory (#2614): write it
  only for a colon-scoped sub-namespace (`cognition:client:tool`) or a
  deliberate divergence pinned by a one-line `namespace.pin` file in the
  domain directory (the id-preserving precedent: `dsl/deployment` pins
  `cluster`). Any other explicit-vs-directory mismatch is a load error.
  WARNING -- file location is id-bearing: moving a `.memql` file between
  domain directories changes every canonical id (`v1:<ns>:<concept>`) it
  declares; the load guard exists to catch exactly that. Field-level:
  `@required`, `@default`, `@description`, `@unique`, `@pattern`,
  `@minLength`, `@maxLength`, `@minimum`, `@maximum`, `@immutable`,
  `@secret`, `@pii`, `@internal`, `@serverSet`, `@variant`. See
  `dsl/_reference/_concept.memql`. (Field-level `@internal` is live --
  only the construct-level form was retired, #2708.)
- **Tools**: `@allowedRoles`, `@clientExecution`, `@description`,
  `@destructive`, `@disabled`, `@enabled`, `@executionTime`, `@handler`,
  `@rateLimit(maxCalls=N, periodSeconds=N)`, `@requiresConfirmation`,
  `@scopes` (`component/language/parser/tool_decl.go`).
- **Prompts**: `@description`, `@defaultProvider`, `@templateFile`,
  lifecycle flags.
- **Providers**: `@description`, `@extends`, `@model`, `@base`,
  `@type`, lifecycle flags.
- **Policies** (AI provider selection): `@primary`, `@fallback`,
  `@maxLatencyMs`, `@maxTimeToFirstTokenMs`, `@preferredRole`,
  `@description`.
- **Specs / traits**: `@description`, lifecycle flags. (A spec binds its
  shape/concept in the signature; the `@shape("name")` pin is removed.)
- **Shapes**: `@row` and/or `@actor` (kind declaration),
  `@description`.

**Retired annotations** (rejected at parse time with a migration
hint): the `@use*` family (`@useConcept`, `@useShape`, `@useQuery`,
...) -- use file-top `use <module>.{ ... }` imports; `@input` on
prompts -- the body IS the schema; `@template` on shapes -- shapes are
struct-form path lists; `@concepts("...")` on shapes -- the concept is
named by the `shape <Concept> <name>` signature; `@caller` -- use
`@actor`; the decision-policy attributes (`@tier`, `@audited`,
`@frontend_visible`) -- the decision-policy tier is retired (#984).

---

## Default Behaviors

| Aspect | Default | Notes |
|--------|---------|-------|
| Enabled state | **Enabled** | Use `@disabled` to deactivate; `@enabled` is an accepted no-op |
| Visibility | Public | Always discoverable (`@internal` retired, #2708) |
| Timeout | 30s | Platform default |
| Cache | None | No caching by default |
| Audit | Off | Must explicitly enable |
| Retry | 0 | No retries by default |
