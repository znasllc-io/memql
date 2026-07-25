---
title: MemQL Reserved Names
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Reserved Names

> **Last updated:** 2026-06-11

This document is the single index of every identifier MemQL reserves
in the author surface. Field names, arg names, imported names, and
function names that collide with one of these are rejected at load
time.

Authoring against this list is much cheaper than re-running the
engine to find out which name was reserved.

---

## 1. Engine-provided names (top-level)

These are available inside every body and cannot be redeclared as
local args / fields:

| Name | What | Where |
|------|------|-------|
| `args` | Caller-passed arguments object. Read fields as `args.X`. | every body |
| `actor` | The auth envelope. Closed member set (#2623, section 3 below): `userId`, `role`, `identityId`, `isClusterOwner`, `primaryEmail`, `now`, plus the legacy `isOwner` alias. Reading it requires `@actor` (#2621). | every body |
| `now` | RFC3339 timestamp captured at evaluation start. | every body |
| `partition` | Active partition for this call. | every body |
| `config` | Allow-listed config entries. See `component/config/policy_exposable.go` for the surface. Read as `config.X`. | every body |
| `trace` | Reserved engine name. (It carried the policy-trace handle of the decision-policy tier, retired in #984; the name stays reserved so resolution rules remain unambiguous.) | every body |

An args field whose name collides with any of these is a load-time
error. Defined in `component/memql/keyword_slices.go` and enforced
during args parsing.

---

## 2. Row intrinsics (concept rows)

Every persistent row has these engine-stamped fields. They are
**reserved on every concept's payload schema** -- redeclaring one in
a `concept` body is a hard error. They are also the canonical names
the executor reads at SQL push-down time.

| Field | Type | Stamped by | Notes |
|-------|------|-----------|-------|
| `id` | string | mutation execution | Canonical id format: `{concept}:{shortId}` (the `{partition}:` prefix was retired in #56). Canonical internally; bare-ified toward clients. See [identifiers.md](../concepts/identifiers.md). |
| `concept` | string | mutation execution | The concept id, e.g. `v1:cognition:participant`. |
| `type` | string | concept declaration | Currently mirrors `concept`; reserved for future versioning differences. |
| `createdAt` | datetime | mutation execution | RFC3339 timestamp at insert time. |
| `createdBy` | string | mutation execution | Stamped from the request actor's identity. |
| `partition` | string | mutation execution | Envelope partition for partition-scoped concepts; `_system` for global-scoped concepts. |
| `schema` | string | concept declaration | Concept schema version hash (engine-derived). |
| `provenance` | object | mutation execution | Origin metadata: `{kind, name, trigger, via}`. Supports nested paths. |

Defined in `component/memql/intrinsic_fields.go`. Cross-referenced
in `docs/public/language/authoring-rules.md` (gotcha #19).

### Naming an intrinsic in a filter: the `row.` namespace

In a query `filter` clause an intrinsic is addressed through the `row.`
namespace -- `row.id`, `row.createdAt`, `row.provenance.kind` -- never
bare (memql#2779, `TestFilterIntrinsicsUseRowNamespace`). Payload
properties in the same clause are bare, so the namespace is what keeps
the two surfaces apart:

```memql
filter  row.id == args.spaceId && status == "active"
//      ^^^^^^ row envelope      ^^^^^^ payload property
```

`row.` accepts only the intrinsics the filter compiler pushes down:
`id`, `concept`, `type`, `createdAt`, `createdBy`, and
`provenance.<leaf>`. `row.<anything else>` is an error rather than a
silent fall-through to a payload lookup -- that fall-through is the
defect the namespace closed. `partition` and `schema` are intrinsics on
the row but are not filter-comparable, so they have no `row.` form.

A **sort key** takes the same namespace -- `sort "row.createdAt", "desc"` --
and rejects a non-sortable leaf rather than silently ordering on a JSONB path
that does not exist. Authored sort keys are gated exactly like filter
predicates (memql#2786): the tree is migrated, and naming an intrinsic bare in
a `.memql` sort clause fails CI. `provenance` has no sort form -- it is
object-valued with no ordering, so `row.provenance` is rejected.

The bare spelling remains valid at RUNTIME, where callers pass sort keys in
through the SDK and the query API; the gate covers authored `.memql` only.

Other surfaces are unchanged: a shape body already projects `row.id` /
`row.createdAt`; a spec/trait body reads its signature-bound fields bare
and rejects `row.*` (epic #2281); a mutation `insert`/`update` block
writes `id:` / `createdAt:` as target keys rather than references.

> One consequence worth stating: `row` is now a reserved head in a filter, so
> a concept that declares a payload property literally named `row` cannot be
> filtered on it. No concept in the engine tree does, but a product bundle
> mounted via `MEMQL_DSL_PATH` should avoid the name.

---

## 3. Actor-envelope fields

Inside an `@actor` shape or a context-spec, the engine exposes a
fixed envelope. Field paths under `actor.` are restricted to:

| Path | What |
|------|------|
| `actor.userId` | The acting user's id. |
| `actor.role` | Cluster role: `owner` / `admin` / `writer` / `reader`. |
| `actor.identityId` | The credential row (token, magic-link, PAT). |
| `actor.isClusterOwner` | Bool short-circuit; bypasses the per-partition ACL. |
| `actor.primaryEmail` | The acting user's primary email address. |
| `actor.now` | RFC3339 timestamp at evaluation start (the shape-body spelling of the reserved `now`). |
| `actor.isOwner` | Legacy alias of `actor.isClusterOwner`. |

This is the ONE canonical envelope (#2623), uniform across query
filters, mutation values, logic bodies, and context-specs; the
engine-side table is `auth.ActorEnvelopeFields` and the dslspec
editor table is drift-pinned to it. `actor.config.<key>` is retired
from this list -- no resolver ever implemented it; read allow-listed
config through the bare reserved `config.<key>` instead.

The fields are the only names valid under `actor.` -- a typo like
`actor.userid` (lowercase) is a hard error AT LOAD (#2625: it was a
runtime error on the query/mutation surfaces and a SILENT NIL on the
logic/spec surfaces before), and an `actor-unknown-property` squiggle
at edit time. The `caller.X` / `@caller`
spellings are retired in #221; the parser rejects them with a
migration hint pointing at the canonical `actor.X` / `@actor`.

Reading the envelope is a DECLARED capability (#2621): a query,
mutation, logic, or automation whose body references `actor.*` must
carry a bare `@actor` annotation in its preamble, or it fails load
with a file-attributed error (used-requires-declared, the same shape
as the logic event-binding rule). Declared-but-unused is legal. Spec
and trait bodies keep the inverse rule -- direct `actor.*` reads are
load-rejected there; bind an `@actor` shape instead. The seed-file
`@actor("system")` (a seed-write identity) is a different construct
and is unaffected.

---

## 4. Imported names (`use` declarations)

Cross-file dependencies are declared with file-top `use` imports:

```memql
use cognition.concepts.{ participant, space }
use common.traits.{ traitIsActiveRecord }
```

The dotted path maps to a file on disk (`cognition.concepts` →
`dsl/cognition/concepts.memql`); the brace list names the constructs
pulled into local scope **under their declared names** -- there is no
aliasing clause. Every imported name shares the same identifier
namespace as the top-level engine-provided names in section 1, so the
five reserved names (`actor`, `now`, `partition`, `config`, `trace`)
can never be construct names and can never enter scope through an
import.

Resolution lives in `component/memql/dslimports/`. (An earlier
design sketch for path-based `import "./foo" as <name>` aliasing was
superseded by the `use` model that shipped with the import-model
pivot, memql PRs #47 / #48 / #49.)

---

## 5. Reserved keywords (declaration headers)

Each construct's declaration header uses a reserved keyword. These
cannot be used as identifier names anywhere in the author surface:

| Keyword | Construct |
|---------|-----------|
| `concept` | Row schema declaration. |
| `shape` | Reusable field projection. |
| `spec` | Atomic boolean predicate. |
| `trait` | Concept-agnostic atomic predicate. |
| `query` | Read function. |
| `mutation` | Write function. |
| `logic` | Imperative orchestration block. |
| `automation` | Event-triggered workflow. |
| `tool` | AI-callable surface. |
| `prompt` | AI prompt template. |
| `provider` | AI vendor + model config. |
| `builtin` | Go-backed operation wrapper. |
| `seed` | Declarative row template. |
| `policy` | AI provider-selection record (`@primary` / `@fallback`). The decision-policy tier is retired (#984). |

Plus body-level keywords inside specific constructs: `args`, `body`,
`filter`, `shape`, `insert`, `update`, `step`, `params`, `auth`,
`include`. Their reservation is scoped to the construct that defines
them.

---

## 6. Reserved annotation names

The full annotation surface is per-construct and enforced by each
parser's allow-list (search `allowedXAnnotations` in
`component/memql/`). Cross-construct annotations:

| Annotation | Where it applies |
|------------|------------------|
| `@description("...")` | Every construct. |
| `@enabled` / `@disabled` | Lifecycle on queries / mutations / logic / automations / traits / tools / builtins / prompts / providers / specs / seeds. Enabled is the default on every kind (#2604-#2608); `@enabled` is an accepted no-op. `@disabled` means "not loaded right now", not "deprecated" -- that axis is `@deprecated`. `@disabled` on a `@base` provider propagates to every child that `@extends` it. |
| `@deprecated("hint")` | Removed from the allow-lists (#989) and rejected at load on every construct; the parser still folds it but no loader reads it. Use `@disabled` to deactivate. |
| `@internal` | RETIRED at construct level under the 2026.08 epoch (#2620 ruling / #2708) -- it only hid a callable construct from discovery surfaces; the load gate rejects it with a migration hint. Field-level `@internal` on concept properties (the `@secret`/`@pii` sensitivity family) remains live. |

**Cross-construct dependencies do NOT go through annotations.** The
legacy `@useConcept` / `@useShape` / `@useQuery` / `@useMutation` /
`@useLogic` / `@useBuiltin` / `@useSpec` / `@useTrait` / `@useTool` /
`@usePrompt` / `@useProvider` / `@useAutomation` family was retired
in the import-model pivot (memql PRs #47 / #48 / #49, 2026-05-19) and
is rejected at parse time. The canonical post-migration shape:

- **File-top Form B imports** declare cross-file dependencies:
  `use cognition.concepts.{ participant }`,
  `use common.traits.{ traitIsActiveRecord }`.
- **Concept binding lives in the construct signature** for seeds /
  queries / mutations / shapes:
  `query <Concept> <name> { ... }`, `mutation <Concept> <name> { ... }`,
  `shape <Concept> <name> { ... }`, `seed <Concept> <name> { ... }`.

The legacy `@input` wrapper and `@template` body annotation are also
retired -- the parser rejects them with a migration hint.

---

## 7. Where this list lives in code

This file is documentation; the source of truth is split across
several Go files. When the list below changes, update this doc:

| Item | Source |
|------|--------|
| Top-level engine names | `component/memql/keyword_slices.go` |
| Row intrinsics | `component/memql/intrinsic_fields.go` |
| Caller envelope | `component/memql/sense/builtins.go` + `runtime_evaluator.go` |
| Construct keywords | per-construct parser allow-lists in `component/memql/` |
| Annotation allow-lists | per-construct parser allow-lists in `component/memql/` |
| Imported names (`use`) | `component/memql/dslimports/dslimports.go` |

---

## Why a doc index instead of just code?

Because authors write `.memql` files in editors first and read engine
errors second. The fast way to write a correct file is to know the
rule before it bites; the slow way is to fix the file after the
engine boots and rejects it. This doc closes that gap.
