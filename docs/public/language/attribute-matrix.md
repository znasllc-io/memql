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
at the annotation surfaces of the other construct kinds. Every row
below is backed by the parser (`component/language/ast/ast.go` +
`component/language/parser/parser.go`); anything not listed is either
construct-specific (see the end of this document) or rejected.

---

## Attribute Applicability Matrix

| Attribute | Query | Mutation | Automation | Description |
|-----------|:-----:|:--------:|:----------:|-------------|
| **Lifecycle** |
| `@enabled` | Yes | Yes | Yes | Accepted no-op; definitions are enabled by default (#2609) |
| `@disabled` | Yes | Yes | Yes | Explicitly disables the definition |
| `@deprecated` | Yes | Yes | Yes | Marks as deprecated with optional message |
| `@version("v1")` | Yes | Yes | Yes | Version metadata tag (no load-time default for these constructs) |
| **Documentation** |
| `@description("...")` | Yes | Yes | Yes | Human-readable description (fallback; prefer `///` doc comments, #2601) |
| **Access Control** |
| `@public` | Yes | Yes | No | Authz-classification opt-out: declares that no caller-scope check applies (see below) |
| `@actor` | Yes | Yes | Yes | Declares the body reads the auth envelope (`actor.*`); used-but-undeclared fails load (#2621) |
| `@permission("...")` | No | No | No | Documented-only, never enforced; load-rejected. Bury tracked as #2713 |
| **Performance** |
| `@timeout("30s")` | Yes | Yes | Yes | Maximum execution time |
| `@cache(300)` | Yes | No | No | Result-cache TTL in whole seconds; positional preferred (#2618), `ttl="300"` keeps parsing |
| **Reliability** |
| `@retry(count=3)` | No | Yes | Yes | Retry on failure |
| `@idempotent` | No | Yes | No | Safe to retry without side effects |
| `@mergeFields("a", "b")` | No | Yes | No | Deep-merge the named object payload fields on update instead of replacing them |
| `@appendFields("a", "b")` | No | Yes | No | Append the named array payload fields' elements to the stored array on update instead of replacing them |
| `@createOnly("a", "b")` | No | Yes | No | Write the named payload fields only on create; preserve the stored value on an insert (upsert) onto an existing id |
| **Auditing** |
| `@audit` | No | Yes | Yes | Log all executions for audit trail |
| **Triggers (Automation Only)** |
| `@trigger(event="...")` | No | No | Yes | Event-based trigger |
| `@trigger(schedule="...")` | No | No | Yes | Cron-based schedule (6-field, with seconds) |
| `@filter(...)` | No | No | Yes | Predicate over the triggering event's payload |
| `@schedule(cron="...")` | No | No | Yes | Accepted synonym for `@trigger(schedule="...")` |
| `@async` | No | No | Yes | Run asynchronously when triggered |

---

## Attribute Definitions

### Lifecycle Attributes

#### `@enabled`
Accepted explicit no-op: definitions are enabled by default (functions since
#360, automations since #2604). Use `@disabled` to deactivate.

```memql
@enabled
query user queryActiveUsers { ... }
```

#### `@disabled`
Explicitly disables the definition. The construct is parsed but not
loaded at runtime; it stays in the tree, is still maintained, and can
be re-enabled at any time. ("Deprecated / abandoned" is the separate
`@deprecated` axis.) For functions the gate is on execution as well as
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
query user queryLegacyUsers { ... }
```

#### `@deprecated`
Marks the definition as deprecated. Optionally includes a message.

```memql
@deprecated
query user queryOldUsers { ... }

@deprecated("Use queryActiveUsers instead")
query user queryUsers { ... }
```

#### `@version("...")`
Version tag for the definition.

```memql
@version("v2")
query user queryActiveUsers { ... }
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
break. Aim for ~200 characters (the editorial length target; sense emits a
hint-severity `description-length` diagnostic over the target -- never
a hard gate, #2703). The engine tree's conformance gate rejects the
redundant long form where `///` suffices; downstream trees convert with
`memqlmigrate --rewrite=doc-comment-descriptions` at their repin.

```memql
/// Returns all active user profiles with optional filters.
query user queryActiveUsers { ... }
```

The fallback form stays valid wherever it parses today:

```memql
@description("Returns all active user profiles with optional filters")
query user queryActiveUsers { ... }
```

---

### Access Control Attributes

#### `@internal` (retired)
Construct-level `@internal` was hard-retired under the 2026.08 epoch
(#2620 ruling / #2708). It only hid the construct from discovery
surfaces (tool listing, `@mcp` promotion) while leaving it callable --
one corpus use in its lifetime. The load gate now rejects it with a
migration hint; delete the annotation. Field-level `@internal` on
concept PROPERTIES (the `@secret` / `@pii` sensitivity family) is a
different surface and remains fully supported.

#### `@public`
Declares that the construct intentionally carries **no caller-scope
check**. The per-row authorization gate
(`TestPerRowAuthzClassification` in `dsl/conformance_test.go`)
classifies every query / mutation as owned (`actor.userId` reference),
admin (`actor.isClusterOwner` / `requiresClusterOwner`), or public --
a construct that references user-scope fields without one of those
fails CI unless it carries `@public` with a comment explaining why.

```memql
// Concept catalog -- no per-user rows, safe to expose unscoped.
@public
query nodeType queryNodeTypes { ... }
```

#### `@role` (buried)
`@role("...")` was documented here as declarative access control, but
the load gate always rejected it and nothing ever enforced the value
-- dead vocabulary wearing a security costume. It was BURIED under the
#2631 ruling (#2709): the AST/parser/registry plumbing is deleted and
the load gate emits a pointed message. Access control lives at the
actor layer (RBAC) plus the `@public` per-row-authz classification.

NOTE: `@permission` (below) is the same documented-only class -- it is
parsed into a field nothing reads. It is a candidate for the same
audit-and-bury treatment; see the #2709 close-out.

#### `@permission("...")` (documented-only; bury pending)
Documented here as requiring a caller permission, but the load gate
rejects it on every construct (absent from the annotation registry)
and nothing reads the value -- the same never-enforced class as the
buried `@role`. Audited during #2709; its bury is tracked as #2713.
Do not author it.

---

### Performance Attributes

#### `@timeout("...")`
Maximum execution time. Supports duration formats: `"30s"`, `"5m"`, `"1h"`.

```memql
@timeout("30s")
query record queryHeavyReport { ... }
```

#### `@cache(N)`
Cache query results for N whole seconds. **Query only** - mutations
and automations should not be cached. Positional is preferred (#2618:
the single ttl arg makes position unambiguous); the keyword form
`@cache(ttl="300")` keeps parsing. Duration strings like "5m" were
doc drift -- the engine reads whole seconds.

```memql
@cache(300)
query nodeType queryNodeTypeCatalog { ... }
```

---

### Reliability Attributes

#### `@retry(count=N)`
Retry the operation on failure. **Mutation and Automation only**.

```memql
@retry(count=3)
mutation user mutationCreateUser { ... }
```

#### `@idempotent`
Marks the mutation as safe to retry without side effects. **Mutation only**.

```memql
@idempotent
mutation user mutationUpsertUser { ... }
```

#### `@mergeFields("...")`
Opts an update-kind mutation into engine-side deep-merge for the named
object-typed payload fields: the partial object's keys merge into the
stored object instead of replacing it wholesale (the default contract
is top-level replace). Added for single-key preference writes that
would otherwise wipe sibling keys (memql#1339).

```memql
@mergeFields("preferences")
mutation user mutationToggleComputerUseEnabled { ... }
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
mutation request attachToRequest { ... }
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
mutation outboundRequest stageOutboundRequest { ... }
```

---

### Auditing Attributes

#### `@audit`
Log all executions for audit trail. **Mutation and Automation only**.

```memql
@audit
mutation user mutationDeleteUser { ... }
```

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

#### `@async`
Run the automation asynchronously when triggered. The caller doesn't
wait for completion.

```memql
@async
@trigger(event="report.requested")
automation generateReport { ... }
```

---

## Examples by Type

### Query

```memql
use cognition.concepts.{ participant }
use common.traits.{ traitIsActiveRecord }

@description("Get active human participants in a space")
query participant queryActiveHumanParticipants {
  args {
    spaceId  string  @required
  }
  filter  spaceId==args.spaceId && participantType=="human" && traitIsActiveRecord
  shape   participantFull
}
```

### Mutation

```memql
use cognition.concepts.{ space }

@description("Create a cognition space")
@audit
@retry(count=3)
mutation space mutationCreateSpace {
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
use cognition.logic.{ logicBootstrapSession }

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
  `@displayCard`. (`@scope` is retired, #56.) `@version` is strict
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
