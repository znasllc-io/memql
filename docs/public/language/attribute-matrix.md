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
| `@enabled` | Yes | Yes | Yes | Activates the definition (required to use it) |
| `@disabled` | Yes | Yes | Yes | Explicitly disables the definition |
| `@deprecated` | Yes | Yes | Yes | Marks as deprecated with optional message |
| `@version("v1")` | Yes | Yes | Yes | Version tag for the definition |
| **Documentation** |
| `@description("...")` | Yes | Yes | Yes | Human-readable description |
| **Access Control** |
| `@internal` | Yes | Yes | Yes | Not exposed to external API |
| `@public` | Yes | Yes | No | Authz-classification opt-out: declares that no caller-scope check applies (see below) |
| `@role("admin")` | Yes | Yes | Yes | Restrict to users with specified role |
| `@permission("...")` | Yes | Yes | No | Require specific permission |
| **Performance** |
| `@timeout("30s")` | Yes | Yes | Yes | Maximum execution time |
| `@cache(ttl="5m")` | Yes | No | No | Cache results for duration |
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
discovery: a `@disabled` query, mutation, or logic is hidden from
`functions()` and the MCP tool listing (`help()` still describes it,
reporting `"enabled": false`) and rejected with
`function "name" is disabled` if called directly (#2605). Tools,
seeds, prompts (#2606), providers, specs, and traits (#2607) are
skipped at load entirely: a `@disabled` tool never reaches the MCP
surface, a `@disabled` seed is never materialised, a `@disabled`
prompt never registers, and a `@disabled` spec or trait is validated
but never binds (its name stays reserved against authored promotion).
A `@disabled` capability (#2607) drops out of both the authored-import
crossref and the actions capability catalog: `use capabilities....`
imports stop resolving it and an action referencing it is rejected at
load with `capability "name" is @disabled`.

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
Human-readable description of the definition.

```memql
@description("Returns all active user profiles with optional filters")
query user queryActiveUsers { ... }
```

---

### Access Control Attributes

#### `@internal`
Marks the definition as internal-only. Not exposed to the external API.

```memql
@internal
query node querySystemMetrics { ... }
```

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

#### `@role("...")`
Restricts access to users with the specified role
(owner / admin / writer / reader).

```memql
@role("admin")
query auditEvent queryAdminDashboard { ... }
```

#### `@permission("...")`
Requires the caller to have the specified permission.
Query / mutation only.

```memql
@permission("read:users")
query user queryUserProfiles { ... }
```

---

### Performance Attributes

#### `@timeout("...")`
Maximum execution time. Supports duration formats: `"30s"`, `"5m"`, `"1h"`.

```memql
@timeout("30s")
query record queryHeavyReport { ... }
```

#### `@cache(ttl="...")`
Cache query results for the specified duration. **Query only** -
mutations and automations should not be cached.

```memql
@cache(ttl="5m")
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

@enabled
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

@enabled
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

@enabled
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
  `@displayCard`. (`@scope` is retired, #56.) Field-level:
  `@required`, `@default`, `@description`, `@unique`, `@pattern`,
  `@minLength`, `@maxLength`, `@minimum`, `@maximum`, `@immutable`,
  `@secret`, `@variant`. See `dsl/_reference/_concept.memql`.
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
| Visibility | Public | Use `@internal` to hide from API |
| Timeout | 30s | Platform default |
| Cache | None | No caching by default |
| Audit | Off | Must explicitly enable |
| Retry | 0 | No retries by default |
