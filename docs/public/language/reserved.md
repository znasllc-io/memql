---
title: MemQL Reserved Names
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Reserved Names

> **Last updated:** 2026-05-19

This document is the single index of every identifier MemQL reserves
in the author surface. Field names, arg names, import aliases, and
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
| `actor` | Resolved auth context. Fields: `userId`, `role`, `identityId`, `isClusterOwner`, `partitions`. | every body |
| `now` | RFC3339 timestamp captured at evaluation start. | every body |
| `partition` | Active partition for this call. | every body |
| `config` | Allow-listed config entries. See `component/config/policy_exposable.go` for the surface. Read as `config.X`. | every body |
| `trace` | Policy-trace handle (`trace.persist`, `trace.note`). | policies + logic invoked from policies |

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
| `id` | string | mutation execution | Canonical id format: `{partition}:{concept}:{contentHash}`. See [identifiers.md](../concepts/identifiers.md). |
| `concept` | string | mutation execution | The concept id, e.g. `v1:cognition:participant`. |
| `type` | string | concept declaration | Currently mirrors `concept`; reserved for future versioning differences. |
| `createdAt` | datetime | mutation execution | RFC3339 timestamp at insert time. |
| `createdBy` | string | mutation execution | Stamped from the request actor's identity. |
| `partition` | string | mutation execution | Envelope partition for partition-scoped concepts; `_system` for global-scoped concepts. |
| `schema` | string | concept declaration | Concept schema version hash (engine-derived). |
| `provenance` | object | mutation execution | Origin metadata: `{kind, name, trigger, via}`. Supports nested paths. |

Defined in `component/memql/intrinsic_fields.go`. Cross-referenced
in `docs/public/language/authoring-rules.md` (gotcha #19).

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
| `actor.now` | RFC3339 timestamp at evaluation start. |
| `actor.config.<key>` | Allow-listed config entries. |

The fields are the only names valid under `actor.` -- a typo like
`actor.userid` (lowercase) is a hard error. The `caller.X` / `@caller`
spellings are retired in #221; the parser rejects them with a
migration hint pointing at the canonical `actor.X` / `@actor`.

---

## 4. Import aliases (DSL import-model refactor)

When the import-model refactor lands ([dsl-import-model-refactor.md](../../internal/design/dsl-import-model-refactor.md)),
import aliases default to the basename of the imported file. The
following are **reserved alias names** -- attempting
`import "./foo" as <name>` for one of these is a load-time error:

| Reserved alias | Reason |
|----------------|--------|
| `actor` | Top-level engine-provided name. |
| `now` | Top-level engine-provided name. |
| `partition` | Top-level engine-provided name. |
| `config` | Top-level engine-provided name. |
| `trace` | Top-level engine-provided name. |

The basename rule applies first: `import "./foo"` resolves to alias
`foo`. If the resulting basename collides with another import's
basename, the author must add `as <name>`. Files whose basename is
not a legal identifier (regex `^[a-z][a-zA-Z0-9_]*$`) require an
explicit `as` clause too.

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
| `tool` | SI-callable surface. |
| `prompt` | SI prompt template. |
| `provider` | SI vendor + model config. |
| `builtin` | Go-backed operation wrapper. |
| `seed` | Declarative row template. |
| `policy` | Cross-cutting decision OR SI-router routing config. |

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
| `@enabled` / `@disabled` | Lifecycle on functions / automations / traits / tools / builtins / policies. **Not on specs** -- the engine controls spec lifecycle. |
| `@deprecated("hint")` | Functions, automations, tools, builtins. |
| `@internal` | Functions; hides from SI tool surfaces + external docs. |

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
| Import aliases | `component/memql/dslimports/dslimports.go` (post-refactor) |

---

## Why a doc index instead of just code?

Because authors write `.memql` files in editors first and read engine
errors second. The fast way to write a correct file is to know the
rule before it bites; the slow way is to fix the file after the
engine boots and rejects it. This doc closes that gap.
